package httpx

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"

	"github.com/cangerx/c-ssl/server/internal/domain/errs"
)

// MaxBodyBytes 是请求体的最大字节数。
//
// 限制请求体大小是廉价的 DoS 防护：没有上限时，一个声明
// Content-Length 为几 GB 的请求就能把内存吃光。
const MaxBodyBytes = 1 << 20 // 1 MiB

// DecodeJSON 解析请求体中的 JSON。
//
// 解析失败返回带字段详情的 1000 错误，调用方直接 Fail 即可。
// 刻意不允许未知字段报错——契约新增字段时不应让旧客户端立刻失败。
func DecodeJSON(w http.ResponseWriter, r *http.Request, dst any) *errs.Error {
	if r.Body == nil {
		return errs.New(errs.CodeInvalidParam).WithField("body", "请求体不能为空")
	}

	// 用 MaxBytesReader 而不是 io.LimitReader：后者会静默截断，
	// 超长请求体可能被当成合法 JSON 解析出一半内容。
	r.Body = http.MaxBytesReader(w, r.Body, MaxBodyBytes)

	decoder := json.NewDecoder(r.Body)
	if err := decoder.Decode(dst); err != nil {
		if errors.Is(err, io.EOF) {
			return errs.New(errs.CodeInvalidParam).WithField("body", "请求体不能为空")
		}
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			return errs.New(errs.CodeInvalidParam).WithField("body", "请求体过大")
		}
		return errs.New(errs.CodeInvalidParam).WithField("body", "JSON 格式不正确")
	}

	// 拒绝尾随内容：`{"a":1}{"b":2}` 这种输入应当报错，
	// 而不是静默接受第一个对象。
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errs.New(errs.CodeInvalidParam).WithField("body", "请求体只能包含一个 JSON 对象")
	}
	return nil
}

// ClientIP 提取客户端 IP。
//
// trustProxy 为 false 时只看 RemoteAddr，忽略 X-Forwarded-For——
// 该头是客户端可以随便伪造的，直接信任等于让限流形同虚设。
// 只有当服务确实部署在可信反向代理之后时才应打开。
func ClientIP(r *http.Request, trustProxy bool) string {
	if trustProxy {
		if forwarded := r.Header.Get("X-Forwarded-For"); forwarded != "" {
			// 取最左侧一项，即最初发起请求的客户端
			if first, _, ok := strings.Cut(forwarded, ","); ok {
				return strings.TrimSpace(first)
			}
			return strings.TrimSpace(forwarded)
		}
		if realIP := r.Header.Get("X-Real-IP"); realIP != "" {
			return strings.TrimSpace(realIP)
		}
	}

	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		// RemoteAddr 不含端口时按原样返回
		return r.RemoteAddr
	}
	return host
}

// ── 请求身份 ──────────────────────────────────────
//
// 放在 httpx 而不是 middleware，是为了让业务 handler 不必依赖 middleware 包：
// 依赖方向保持 handler → httpx，middleware → httpx，不会形成环。

type identityKey struct{}

// Identity 是当前请求已认证的身份。
type Identity struct {
	UserID int64
	// SessionID 是访问令牌的 jti，仅用于日志关联。
	SessionID string
}

// WithIdentity 把认证身份写入上下文。
func WithIdentity(ctx context.Context, id Identity) context.Context {
	return context.WithValue(ctx, identityKey{}, id)
}

// IdentityFromContext 取出认证身份。
func IdentityFromContext(ctx context.Context) (Identity, bool) {
	id, ok := ctx.Value(identityKey{}).(Identity)
	return id, ok
}

// UserIDFromContext 取出当前用户 ID。未认证时返回 (0, false)。
func UserIDFromContext(ctx context.Context) (int64, bool) {
	id, ok := IdentityFromContext(ctx)
	if !ok || id.UserID <= 0 {
		return 0, false
	}
	return id.UserID, true
}

// MustUserID 取出当前用户 ID，未认证时返回 0。
// 仅用于已经过 Auth 中间件的路由。
func MustUserID(ctx context.Context) int64 {
	id, _ := UserIDFromContext(ctx)
	return id
}
