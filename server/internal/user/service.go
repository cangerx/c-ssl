package user

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/cangerx/c-ssl/server/internal/domain/errs"
	"github.com/cangerx/c-ssl/server/internal/domain/password"
	"github.com/cangerx/c-ssl/server/internal/domain/token"
	"github.com/cangerx/c-ssl/server/internal/platform/ratelimit"
)

// 限流阈值。按 IP 与按账号两条线：前者拦单机扫号，后者拦针对特定账号的爆破。
const (
	loginIPLimit       = 30
	loginIPWindow      = 5 * time.Minute
	loginEmailLimit    = 10
	loginEmailWindow   = 5 * time.Minute
	registerIPLimit    = 10
	registerIPWindow   = time.Hour
	deviceLabelMaxRune = 128
)

// SessionReuseGrace 是轮换后仍视作"并发刷新"而非"令牌重放"的时间窗。
//
// 浏览器多标签页可能同时提交同一个刷新令牌，这是正常的，不该触发
// 全量会话撤销把用户踢下线。窗口取得短一些，把攻击者能利用的时间压到最小。
const SessionReuseGrace = 30 * time.Second

// ClientInfo 是请求来源信息，用于会话记录与限流。
type ClientInfo struct {
	IP        string
	UserAgent string
}

// Service 是用户域的业务入口。
type Service struct {
	repo       *Repository
	signer     *token.Signer
	limiter    *ratelimit.Limiter
	refreshTTL time.Duration

	// now 可在测试中替换，便于验证锁定与过期逻辑。
	now func() time.Time

	dummyHashOnce sync.Once
	dummyHash     string
}

// NewService 构造服务。
func NewService(repo *Repository, signer *token.Signer, limiter *ratelimit.Limiter, refreshTTL time.Duration) *Service {
	return &Service{
		repo:       repo,
		signer:     signer,
		limiter:    limiter,
		refreshTTL: refreshTTL,
		now:        time.Now,
	}
}

// AuthResult 是一次成功认证的结果。
type AuthResult struct {
	User         *User
	AccessToken  string
	RefreshToken string
	ExpiresIn    int32
}

// RegisterInput 是注册请求。
type RegisterInput struct {
	Email    string
	Password string
	Nickname string
}

// Register 创建账号并直接签发令牌。
func (s *Service) Register(ctx context.Context, in RegisterInput, client ClientInfo) (*AuthResult, error) {
	email := NormalizeEmail(in.Email)
	nickname := NormalizeNickname(in.Nickname)

	if !ValidateEmail(email) {
		return nil, errs.New(errs.CodeInvalidParam).
			WithField("email", "邮箱格式不正确")
	}
	if err := password.ValidatePlain(in.Password); err != nil {
		return nil, errs.New(errs.CodeInvalidParam).
			WithField("password", err.Error())
	}
	if len([]rune(nickname)) > 32 {
		return nil, errs.New(errs.CodeInvalidParam).
			WithField("nickname", "昵称不能超过 32 个字符")
	}

	limited, err := s.checkLimit(ctx,
		ratelimit.RegisterByIPKey(client.IP), registerIPLimit, registerIPWindow)
	if err != nil {
		return nil, err
	}
	if limited != nil {
		return nil, limited
	}

	hash, err := password.Hash(in.Password)
	if err != nil {
		return nil, errs.New(errs.CodeInternal).WithCause(err)
	}

	u, err := s.repo.CreateUser(ctx, email, hash, nickname)
	if errors.Is(err, ErrEmailTaken) {
		// 这里如实告知邮箱已注册。注册接口无法回避账号枚举——
		// 要么泄露存在性，要么让正常用户无法理解失败原因。
		// 缓解手段是限流，而不是模糊提示。
		return nil, errs.New(errs.CodeInvalidParam).
			WithField("email", "该邮箱已被注册")
	}
	if err != nil {
		return nil, errs.New(errs.CodeInternal).WithCause(err)
	}

	slog.InfoContext(ctx, "用户注册成功", "user_id", u.ID, "client_ip", client.IP)
	return s.issue(ctx, u, client)
}

// LoginInput 是登录请求。
type LoginInput struct {
	Email    string
	Password string
}

// Login 校验口令并签发令牌。
func (s *Service) Login(ctx context.Context, in LoginInput, client ClientInfo) (*AuthResult, error) {
	email := NormalizeEmail(in.Email)
	if email == "" || in.Password == "" {
		return nil, errs.New(errs.CodeInvalidParam).
			WithField("email", "邮箱与口令不能为空")
	}

	limited, err := s.checkLimit(ctx,
		ratelimit.LoginByIPKey(client.IP), loginIPLimit, loginIPWindow)
	if err != nil {
		return nil, err
	}
	if limited != nil {
		return nil, limited
	}

	limited, err = s.checkLimit(ctx,
		ratelimit.LoginByEmailKey(email), loginEmailLimit, loginEmailWindow)
	if err != nil {
		return nil, err
	}
	if limited != nil {
		return nil, limited
	}

	now := s.now()
	u, err := s.repo.GetByEmail(ctx, email)
	if err != nil {
		return nil, errs.New(errs.CodeInternal).WithCause(err)
	}

	// 账号不存在时也走一次同等的哈希计算再失败。
	// 否则"立即返回"与"算了 100ms 才返回"的时间差会让攻击者
	// 判断出邮箱是否已注册。
	if u == nil {
		s.equalizeTiming(in.Password)
		return nil, invalidCredentials()
	}

	if u.IsLocked(now) {
		remaining := u.LockRemaining(now)
		slog.WarnContext(ctx, "账号处于锁定期，拒绝登录",
			"user_id", u.ID, "client_ip", client.IP, "remaining", remaining.String())
		return nil, errs.New(errs.CodeTooManyRequests).
			WithField("lockedUntil", u.LockedUntil.Format(time.RFC3339))
	}

	ok, err := password.Verify(in.Password, u.PasswordHash)
	if err != nil {
		// 哈希串无法解析属于数据损坏，不能当成"口令错误"静默吞掉
		return nil, errs.New(errs.CodeInternal).WithCause(err)
	}
	if !ok {
		if err := s.repo.RecordLoginFailure(ctx, u.ID, now); err != nil {
			slog.ErrorContext(ctx, "记录登录失败次数出错", "user_id", u.ID, "error", err)
		}
		slog.WarnContext(ctx, "登录口令错误", "user_id", u.ID, "client_ip", client.IP)
		return nil, invalidCredentials()
	}

	// 状态检查放在口令校验之后：这样只有拿到正确口令的人
	// 才会得知账号被禁用，不会变成账号枚举的入口。
	if !u.IsActive() {
		return nil, errs.New(errs.CodeForbidden).
			WithField("status", "账号已被禁用，请联系客服")
	}

	if err := s.repo.RecordLoginSuccess(ctx, u.ID, now); err != nil {
		slog.ErrorContext(ctx, "记录登录成功状态出错", "user_id", u.ID, "error", err)
	}
	if err := s.limiter.Reset(ctx, ratelimit.LoginByEmailKey(email)); err != nil {
		slog.WarnContext(ctx, "清除账号登录限流失败", "user_id", u.ID, "error", err)
	}

	// 参数升级后静默重算哈希，用户无感
	if password.NeedsRehash(u.PasswordHash) {
		if newHash, hashErr := password.Hash(in.Password); hashErr == nil {
			if err := s.repo.UpdatePasswordHash(ctx, u.ID, newHash); err != nil {
				slog.WarnContext(ctx, "升级口令哈希失败", "user_id", u.ID, "error", err)
			}
		}
	}

	slog.InfoContext(ctx, "用户登录成功", "user_id", u.ID, "client_ip", client.IP)
	return s.issue(ctx, u, client)
}

// Refresh 用刷新令牌换取新的令牌对，并轮换刷新令牌。
func (s *Service) Refresh(ctx context.Context, rawToken string, client ClientInfo) (*AuthResult, error) {
	parsed, err := token.ParseRefreshToken(rawToken)
	if err != nil {
		return nil, errs.New(errs.CodeUnauthorized).WithCause(err)
	}

	now := s.now()
	session, err := s.repo.GetSession(ctx, parsed.SessionID)
	if err != nil {
		return nil, errs.New(errs.CodeInternal).WithCause(err)
	}
	if session == nil {
		return nil, errs.New(errs.CodeUnauthorized)
	}

	if !token.VerifyRefreshSecret(parsed.Secret, session.SecretHash) {
		// 会话标识存在但密钥不对：可能是有人在猜密钥。
		// 撤销该会话，把可疑凭据作废。
		slog.WarnContext(ctx, "刷新令牌密钥不匹配，撤销该会话",
			"session_id", parsed.SessionID, "user_id", session.UserID, "client_ip", client.IP)
		if revokeErr := s.repo.RevokeSession(ctx, session.SessionID, RevokeAdmin, now); revokeErr != nil {
			slog.ErrorContext(ctx, "撤销可疑会话失败", "session_id", session.SessionID, "error", revokeErr)
		}
		return nil, errs.New(errs.CodeUnauthorized)
	}

	if session.RevokedAt != nil {
		return nil, s.handleRevokedSession(ctx, session, now, client)
	}
	if !session.ExpiresAt.After(now) {
		return nil, errs.New(errs.CodeUnauthorized).WithCause(fmt.Errorf("会话已过期"))
	}

	u, err := s.repo.GetByID(ctx, session.UserID)
	if err != nil {
		return nil, errs.New(errs.CodeInternal).WithCause(err)
	}
	if u == nil {
		return nil, errs.New(errs.CodeUnauthorized)
	}
	if !u.IsActive() {
		return nil, errs.New(errs.CodeForbidden).
			WithField("status", "账号已被禁用，请联系客服")
	}

	next, err := s.newSession(u.ID, client, now)
	if err != nil {
		return nil, errs.New(errs.CodeInternal).WithCause(err)
	}

	if err := s.repo.RotateSession(ctx, session.SessionID, next.record, RevokeRotated, now); err != nil {
		if errors.Is(err, ErrSessionRotated) {
			// 并发刷新输掉了竞争，让客户端重试或重新登录，不牵连整个会话族
			return nil, errs.New(errs.CodeUnauthorized).
				WithCause(fmt.Errorf("会话已被另一次刷新轮换"))
		}
		return nil, errs.New(errs.CodeInternal).WithCause(err)
	}

	return s.issueWithRefresh(u, next.refreshToken, next.accessToken)
}

// handleRevokedSession 处理"使用已撤销刷新令牌"的情况。
//
// 正常客户端不会拿已撤销的令牌来刷新。出现这种情况，最可能是令牌被窃取后
// 攻击者先用了、真实用户拿着旧令牌再来——按 OAuth 2.0 安全实践，
// 此时应撤销该用户的全部会话，强制双方重新登录。
//
// 例外是刚轮换不久（SessionReuseGrace 内）的并发刷新，那是多标签页造成的，
// 不构成攻击信号。
func (s *Service) handleRevokedSession(ctx context.Context, session *Session, now time.Time, client ClientInfo) error {
	recentlyRotated := session.RevokedReason == RevokeRotated &&
		session.RevokedAt != nil &&
		now.Sub(*session.RevokedAt) <= SessionReuseGrace

	if recentlyRotated {
		slog.InfoContext(ctx, "并发刷新导致的旧令牌重放，忽略",
			"session_id", session.SessionID, "user_id", session.UserID, "client_ip", client.IP)
		return errs.New(errs.CodeUnauthorized).
			WithCause(fmt.Errorf("会话已被并发刷新轮换"))
	}

	slog.ErrorContext(ctx, "检测到已撤销刷新令牌被重放，撤销该用户全部会话",
		"session_id", session.SessionID,
		"user_id", session.UserID,
		"revoked_reason", session.RevokedReason,
		"client_ip", client.IP,
	)
	if err := s.repo.RevokeAllSessions(ctx, session.UserID, RevokeAdmin, now); err != nil {
		slog.ErrorContext(ctx, "撤销用户全部会话失败", "user_id", session.UserID, "error", err)
	}
	return errs.New(errs.CodeUnauthorized).
		WithCause(fmt.Errorf("刷新令牌已失效，请重新登录"))
}

// Me 返回当前登录用户。
func (s *Service) Me(ctx context.Context, userID int64) (*User, error) {
	u, err := s.repo.GetByID(ctx, userID)
	if err != nil {
		return nil, errs.New(errs.CodeInternal).WithCause(err)
	}
	if u == nil {
		return nil, errs.New(errs.CodeUnauthorized)
	}
	if !u.IsActive() {
		return nil, errs.New(errs.CodeForbidden).
			WithField("status", "账号已被禁用，请联系客服")
	}
	return u, nil
}

// Logout 撤销指定会话。传入空串时撤销该用户全部会话。
func (s *Service) Logout(ctx context.Context, userID int64, sessionID string) error {
	now := s.now()
	if sessionID == "" {
		if err := s.repo.RevokeAllSessions(ctx, userID, RevokeLogout, now); err != nil {
			return errs.New(errs.CodeInternal).WithCause(err)
		}
		return nil
	}
	if err := s.repo.RevokeSession(ctx, sessionID, RevokeLogout, now); err != nil {
		return errs.New(errs.CodeInternal).WithCause(err)
	}
	return nil
}

// ── 内部辅助 ──────────────────────────────────────

// sessionBundle 把会话记录与交给客户端的令牌绑在一起，
// 避免在多处重复传递容易弄混的原始字符串。
type sessionBundle struct {
	record       Session
	refreshToken token.RefreshToken
	accessToken  string
}

func (s *Service) newSession(userID int64, client ClientInfo, now time.Time) (sessionBundle, error) {
	refreshToken, err := token.NewRefreshToken()
	if err != nil {
		return sessionBundle{}, err
	}

	accessToken, _, _, err := s.signer.IssueAccess(userID, refreshToken.SessionID)
	if err != nil {
		return sessionBundle{}, err
	}

	return sessionBundle{
		record: Session{
			UserID:      userID,
			SessionID:   refreshToken.SessionID,
			SecretHash:  token.HashRefreshSecret(refreshToken.Secret),
			DeviceLabel: DeviceLabel(client.UserAgent, deviceLabelMaxRune),
			ClientIP:    client.IP,
			IssuedAt:    now,
			ExpiresAt:   now.Add(s.refreshTTL),
		},
		refreshToken: refreshToken,
		accessToken:  accessToken,
	}, nil
}

// issue 新建会话并签发令牌。
func (s *Service) issue(ctx context.Context, u *User, client ClientInfo) (*AuthResult, error) {
	bundle, err := s.newSession(u.ID, client, s.now())
	if err != nil {
		return nil, errs.New(errs.CodeInternal).WithCause(err)
	}
	if err := s.repo.CreateSession(ctx, bundle.record); err != nil {
		return nil, errs.New(errs.CodeInternal).WithCause(err)
	}
	return s.issueWithRefresh(u, bundle.refreshToken, bundle.accessToken)
}

func (s *Service) issueWithRefresh(u *User, refreshToken token.RefreshToken, accessToken string) (*AuthResult, error) {
	return &AuthResult{
		User:         u,
		AccessToken:  accessToken,
		RefreshToken: refreshToken.String(),
		ExpiresIn:    int32(s.signer.AccessTTL().Seconds()),
	}, nil
}

// checkLimit 做一次限流判定。被限流时返回非 nil 的业务错误。
func (s *Service) checkLimit(ctx context.Context, key string, limit int, window time.Duration) (*errs.Error, error) {
	result, err := s.limiter.Allow(ctx, key, limit, window)
	if err != nil {
		return nil, errs.New(errs.CodeInternal).WithCause(err)
	}
	if result.Allowed {
		return nil, nil
	}
	return errs.New(errs.CodeTooManyRequests).
		WithField("retryAfterSeconds", fmt.Sprintf("%d", int(result.RetryAfter.Seconds())+1)), nil
}

// equalizeTiming 对不存在的账号执行一次等价的口令校验，抹平响应时间差。
func (s *Service) equalizeTiming(plain string) {
	s.dummyHashOnce.Do(func() {
		// 这里必然成功：口令长度固定满足校验要求
		hash, err := password.Hash("timing-equalizer-placeholder")
		if err != nil {
			return
		}
		s.dummyHash = hash
	})
	if s.dummyHash == "" {
		return
	}
	_, _ = password.Verify(plain, s.dummyHash)
}

// invalidCredentials 返回统一的凭据错误。
//
// 不区分"邮箱不存在"与"口令错误"：区分开就等于提供了账号枚举接口。
func invalidCredentials() *errs.Error {
	return errs.New(errs.CodeUnauthorized).
		WithField("credentials", "邮箱或口令不正确")
}
