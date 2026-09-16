package user

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/cangerx/c-ssl/server/internal/domain/errs"
	"github.com/cangerx/c-ssl/server/internal/server/httpx"
)

// Handler 处理认证与会员相关的 HTTP 请求。
type Handler struct {
	svc *Service
	// auth 是认证中间件，由 router 注入，避免本包反向依赖 middleware 的具体实现。
	auth func(http.Handler) http.Handler
	// trustProxy 决定是否采信 X-Forwarded-For，见 httpx.ClientIP。
	trustProxy bool
}

// NewHandler 构造处理器。
func NewHandler(svc *Service, auth func(http.Handler) http.Handler, trustProxy bool) *Handler {
	return &Handler{svc: svc, auth: auth, trustProxy: trustProxy}
}

// Routes 注册认证与会员路由。挂载点已带 /api/v1 前缀。
//
// 需要登录的接口用 r.Group 就地声明，让"哪些接口要鉴权"在路由表上一目了然，
// 不必去中间件链里推断。
func (h *Handler) Routes(r chi.Router) {
	r.Post("/auth/register", h.register)
	r.Post("/auth/login", h.login)
	r.Post("/auth/refresh", h.refresh)

	r.Group(func(r chi.Router) {
		r.Use(h.auth)
		r.Post("/auth/logout", h.logout)
		r.Get("/me", h.me)
	})
}

// ── DTO ──────────────────────────────────────────
//
// 字段与 openapi/components/schemas/user.yaml 严格对应。
// 转换时逐个字段显式挑选，PasswordHash、失败计数、锁定时间等内部字段不外泄。

type userDTO struct {
	ID        int64  `json:"id"`
	Email     string `json:"email"`
	Nickname  string `json:"nickname"`
	Status    string `json:"status"`
	CreatedAt string `json:"createdAt"`
}

type authTokensDTO struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	ExpiresIn    int32  `json:"expiresIn"`
}

func toUserDTO(u *User) userDTO {
	return userDTO{
		ID:       u.ID,
		Email:    u.Email,
		Nickname: u.Nickname,
		Status:   string(u.Status),
		// 契约要求 date-time，统一用 RFC 3339
		CreatedAt: u.CreatedAt.Format(time.RFC3339),
	}
}

func toTokensDTO(result *AuthResult) authTokensDTO {
	return authTokensDTO{
		AccessToken:  result.AccessToken,
		RefreshToken: result.RefreshToken,
		ExpiresIn:    result.ExpiresIn,
	}
}

// ── 请求体 ────────────────────────────────────────
//
// 这些结构与服务层入参字段一一对应，但刻意分开声明：
// 线上格式（JSON 标签、字段增删）不应直接绑定到服务层 API 上。
// 转换时用类型转换而不是逐字段字面量，Go 的转换规则会忽略结构体标签。

type registerRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
	Nickname string `json:"nickname"`
}

type loginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

type refreshRequest struct {
	RefreshToken string `json:"refreshToken"`
}

// ── 处理函数 ──────────────────────────────────────

func (h *Handler) register(w http.ResponseWriter, r *http.Request) {
	var req registerRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.Fail(w, r, err)
		return
	}

	// 字段与 RegisterInput 完全一致，用转换而不是重复字面量
	result, err := h.svc.Register(r.Context(), RegisterInput(req), h.clientInfo(r))
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	httpx.OK(w, r, toTokensDTO(result))
}

func (h *Handler) login(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.Fail(w, r, err)
		return
	}

	result, err := h.svc.Login(r.Context(), LoginInput(req), h.clientInfo(r))
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	httpx.OK(w, r, toTokensDTO(result))
}

func (h *Handler) refresh(w http.ResponseWriter, r *http.Request) {
	var req refreshRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	if req.RefreshToken == "" {
		httpx.Fail(w, r, errs.New(errs.CodeInvalidParam).
			WithField("refreshToken", "刷新令牌不能为空"))
		return
	}

	result, err := h.svc.Refresh(r.Context(), req.RefreshToken, h.clientInfo(r))
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	httpx.OK(w, r, toTokensDTO(result))
}

func (h *Handler) logout(w http.ResponseWriter, r *http.Request) {
	identity, ok := httpx.IdentityFromContext(r.Context())
	if !ok {
		httpx.Fail(w, r, errs.New(errs.CodeUnauthorized))
		return
	}

	// 只撤销当前这条会话，用户在其他设备上的登录不受影响。
	// 访问令牌携带了 session_id，因此能精确定位（见 token.Claims.SessionID）。
	if err := h.svc.Logout(r.Context(), identity.UserID, identity.SessionID); err != nil {
		httpx.Fail(w, r, err)
		return
	}

	httpx.OK(w, r, nil)
}

func (h *Handler) me(w http.ResponseWriter, r *http.Request) {
	userID := httpx.MustUserID(r.Context())

	u, err := h.svc.Me(r.Context(), userID)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	httpx.OK(w, r, toUserDTO(u))
}

func (h *Handler) clientInfo(r *http.Request) ClientInfo {
	return ClientInfo{
		IP:        httpx.ClientIP(r, h.trustProxy),
		UserAgent: r.UserAgent(),
	}
}
