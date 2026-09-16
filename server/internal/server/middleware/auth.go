package middleware

import (
	"errors"
	"net/http"
	"strings"

	"github.com/cangerx/c-ssl/server/internal/domain/errs"
	"github.com/cangerx/c-ssl/server/internal/domain/token"
	"github.com/cangerx/c-ssl/server/internal/server/httpx"
)

// Auth 校验 Authorization 请求头中的访问令牌，并把身份写入上下文。
//
// 访问令牌是无状态的：这里只验签名与过期时间，不查库。因此"用户被禁用"
// 这类状态变化不会立刻体现在已签发的令牌上，最长会滞后一个令牌有效期。
// 需要立即生效的场景（改密、封禁）依赖刷新令牌被撤销——用户无法续期后，
// 最多 15 分钟就会自然掉线。
func Auth(signer *token.Signer) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw := BearerToken(r)
			if raw == "" {
				httpx.Fail(w, r, errs.New(errs.CodeUnauthorized).
					WithCause(errors.New("请求未携带 Authorization 请求头")))
				return
			}

			claims, err := signer.ParseAccess(raw)
			if err != nil {
				httpx.Fail(w, r, errs.New(errs.CodeUnauthorized).WithCause(err))
				return
			}

			identity := httpx.Identity{
				UserID: claims.UserID(),
				// 注意是 SessionID（sid 声明）而不是 jti。
				// jti 只标识这一次令牌签发，与 user_sessions.session_id 无关；
				// 用错会导致退出登录撤销不到任何会话。
				SessionID: claims.SessionID,
			}
			next.ServeHTTP(w, r.WithContext(httpx.WithIdentity(r.Context(), identity)))
		})
	}
}

// bearerTokenPrefix 是 Authorization 头的前缀，按 RFC 6750 不区分大小写。
const bearerTokenPrefix = "bearer "

// BearerToken 从 Authorization 请求头提取令牌。未携带时返回空串。
func BearerToken(r *http.Request) string {
	header := r.Header.Get("Authorization")
	if len(header) <= len(bearerTokenPrefix) {
		return ""
	}
	if !strings.EqualFold(header[:len(bearerTokenPrefix)], bearerTokenPrefix) {
		return ""
	}
	return strings.TrimSpace(header[len(bearerTokenPrefix):])
}
