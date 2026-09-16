// Package token 负责访问令牌与刷新令牌的签发和校验。
//
// 两种令牌刻意采用不同形态：
//
//   - 访问令牌（Access Token）是无状态 JWT。它短命（默认 15 分钟）、
//     每次请求都要校验，不能查库，所以自包含声明，由签名保证不可伪造。
//
//   - 刷新令牌（Refresh Token）是不透明随机串，形如 "<会话ID>.<密钥>"。
//     它长命（默认 7 天）且必须可撤销——用户改密、退出登录、管理员封禁
//     都要立即失效，所以状态放服务端，只把密钥的 SHA-256 存库。
//     这样即使数据库被读走，也无法反推出可用的令牌。
package token

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// 令牌用途。写进 JWT 的 typ 声明，防止把刷新令牌当访问令牌用。
const (
	TypeAccess = "access"
)

// issuerName 是 JWT 的 iss 声明值，不对外暴露。
const issuerName = "c-ssl"

// 解析或校验失败时返回的错误。
var (
	ErrMalformedToken = errors.New("令牌格式非法")
	ErrTokenExpired   = errors.New("令牌已过期")
	ErrTokenInvalid   = errors.New("令牌无效")
	ErrWrongTokenType = errors.New("令牌用途不匹配")
)

// ── 访问令牌 ──────────────────────────────────────

// Claims 是访问令牌携带的声明。
type Claims struct {
	jwt.RegisteredClaims
	// Type 固定为 access，避免其他用途的令牌被当作访问令牌接受。
	Type string `json:"typ"`
	// SessionID 是签发该访问令牌的会话标识，对应 user_sessions.session_id。
	//
	// 把它放进访问令牌，是为了让"退出登录"能精确定位到当前这条会话，
	// 只撤销本设备而不影响用户的其他设备。没有它就只能全量撤销。
	SessionID string `json:"sid"`
}

// UserID 从声明中取出用户 ID。解析失败返回 0。
func (c *Claims) UserID() int64 {
	id, err := strconv.ParseInt(c.Subject, 10, 64)
	if err != nil {
		return 0
	}
	return id
}

// Signer 负责签发与校验访问令牌。
type Signer struct {
	secret    []byte
	accessTTL time.Duration
}

// NewSigner 构造令牌签发器。secret 为空会直接 panic——
// 这属于配置错误，宁可启动失败也不要签发任何人都能伪造的令牌。
func NewSigner(secret string, accessTTL time.Duration) *Signer {
	if secret == "" {
		panic("token: 签发密钥不能为空")
	}
	if accessTTL <= 0 {
		panic("token: 访问令牌有效期必须为正")
	}
	return &Signer{secret: []byte(secret), accessTTL: accessTTL}
}

// AccessTTL 返回访问令牌有效期，供接口回填 expiresIn。
func (s *Signer) AccessTTL() time.Duration { return s.accessTTL }

// IssueAccess 为用户签发访问令牌，同时返回其 jti 与过期时间。
//
// sessionID 是这条令牌所属的会话，用于退出登录时定位会话。
// jti 用于日志与链路追踪，不参与鉴权——访问令牌是无状态的。
func (s *Signer) IssueAccess(userID int64, sessionID string) (raw string, jti string, expiresAt time.Time, err error) {
	jti, err = randomString(16)
	if err != nil {
		return "", "", time.Time{}, err
	}

	now := time.Now()
	expiresAt = now.Add(s.accessTTL)

	claims := Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    issuerName,
			Subject:   strconv.FormatInt(userID, 10),
			ID:        jti,
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(expiresAt),
		},
		Type:      TypeAccess,
		SessionID: sessionID,
	}

	raw, err = jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(s.secret)
	if err != nil {
		return "", "", time.Time{}, fmt.Errorf("签发访问令牌失败: %w", err)
	}
	return raw, jti, expiresAt, nil
}

// ParseAccess 校验访问令牌并返回声明。
//
// 只接受 HS256：显式校验签名算法，防止算法混淆攻击
// （例如把 alg 改成 none，或换成非对称算法后拿公钥当密钥）。
func (s *Signer) ParseAccess(raw string) (*Claims, error) {
	claims := &Claims{}

	_, err := jwt.ParseWithClaims(raw, claims, func(t *jwt.Token) (any, error) {
		if t.Method != jwt.SigningMethodHS256 {
			return nil, fmt.Errorf("%w: 签名算法 %v", ErrTokenInvalid, t.Header["alg"])
		}
		return s.secret, nil
	},
		jwt.WithIssuer(issuerName),
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
	)
	if err != nil {
		if errors.Is(err, jwt.ErrTokenExpired) {
			return nil, ErrTokenExpired
		}
		return nil, fmt.Errorf("%w: %v", ErrTokenInvalid, err)
	}

	if claims.Type != TypeAccess {
		return nil, fmt.Errorf("%w: 期望 %s，实际 %s", ErrWrongTokenType, TypeAccess, claims.Type)
	}
	if claims.UserID() <= 0 {
		return nil, fmt.Errorf("%w: 用户标识缺失", ErrTokenInvalid)
	}
	return claims, nil
}

// ── 刷新令牌 ──────────────────────────────────────

// RefreshToken 是一条刷新令牌的构成。
type RefreshToken struct {
	// SessionID 是公开的会话标识，用于查库定位会话。它不是秘密。
	SessionID string
	// Secret 是秘密部分，只在签发时存在于内存中，绝不入库。
	Secret string
}

// String 拼出交给客户端的完整刷新令牌。
func (t RefreshToken) String() string { return t.SessionID + "." + t.Secret }

// NewRefreshToken 生成一条新的刷新令牌。
func NewRefreshToken() (RefreshToken, error) {
	sessionID, err := randomString(16)
	if err != nil {
		return RefreshToken{}, err
	}
	secret, err := randomString(32)
	if err != nil {
		return RefreshToken{}, err
	}
	return RefreshToken{SessionID: sessionID, Secret: secret}, nil
}

// ParseRefreshToken 拆解客户端提交的刷新令牌。
//
// 只做格式拆分，不校验密钥是否正确——那需要与库中哈希比对。
//
// 两部分都必须是 URL 安全 base64 字符集，且密钥部分不能含点。
// 否则 "a.b.c" 会被切成 sessionID="a"、secret="b.c"，虽然校验必然失败，
// 但会让畸形输入白白走一次数据库查询，不如在解析阶段就挡住。
func ParseRefreshToken(raw string) (RefreshToken, error) {
	sessionID, secret, ok := strings.Cut(raw, ".")
	if !ok || sessionID == "" || secret == "" {
		return RefreshToken{}, ErrMalformedToken
	}
	if !isBase64URL(sessionID) || !isBase64URL(secret) {
		return RefreshToken{}, ErrMalformedToken
	}
	return RefreshToken{SessionID: sessionID, Secret: secret}, nil
}

// isBase64URL 判断字符串是否只由 URL 安全 base64 的字符组成。
func isBase64URL(s string) bool {
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '-', r == '_':
		default:
			return false
		}
	}
	return true
}

// HashRefreshSecret 计算刷新令牌密钥的哈希，用于入库比对。
//
// 这里用 SHA-256 而非 Argon2：密钥是 32 字节的高熵随机串，
// 不存在被字典攻击的可能，不需要慢哈希；而每次刷新都要校验，
// 慢哈希会白白增加延迟。
func HashRefreshSecret(secret string) []byte {
	sum := sha256.Sum256([]byte(secret))
	return sum[:]
}

// VerifyRefreshSecret 恒定时间比对密钥哈希。
func VerifyRefreshSecret(secret string, wantHash []byte) bool {
	got := HashRefreshSecret(secret)
	return subtle.ConstantTimeCompare(got, wantHash) == 1
}

// ── 辅助 ──────────────────────────────────────────

// randomString 生成 n 字节熵的 URL 安全随机串。
func randomString(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("生成随机串失败: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}
