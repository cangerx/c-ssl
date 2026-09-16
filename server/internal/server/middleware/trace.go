// Package middleware 提供 HTTP 中间件。
//
// 依赖方向：本包只依赖 domain 与 platform，不依赖 server 包，
// 以免与 router 形成导入环。
package middleware

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/cangerx/c-ssl/server/internal/platform/logger"
)

// TraceHeader 是链路追踪 ID 的请求与响应头名称。
const TraceHeader = "X-Trace-Id"

const (
	maxTraceIDLen = 64
	traceIDBytes  = 16
)

// Trace 为每个请求确定 trace ID，写入 context 与响应头。
//
// 上游（网关、前端）传入的 X-Trace-Id 会被沿用，便于跨服务串联；
// 但会做长度与字符校验，防止把超长或含控制字符的值写进日志和响应头。
func Trace(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := sanitizeTraceID(r.Header.Get(TraceHeader))
		if id == "" {
			id = newTraceID()
		}

		w.Header().Set(TraceHeader, id)
		next.ServeHTTP(w, r.WithContext(logger.WithTraceID(r.Context(), id)))
	})
}

func sanitizeTraceID(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > maxTraceIDLen {
		return ""
	}
	for _, ch := range raw {
		isAllowed := (ch >= 'a' && ch <= 'z') ||
			(ch >= 'A' && ch <= 'Z') ||
			(ch >= '0' && ch <= '9') ||
			ch == '-' || ch == '_' || ch == '.'
		if !isAllowed {
			return ""
		}
	}
	return raw
}

func newTraceID() string {
	var buf [traceIDBytes]byte
	if _, err := rand.Read(buf[:]); err != nil {
		// crypto/rand 不可用属于系统级异常，退回时间戳保证链路不断
		return fmt.Sprintf("%016x", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf[:])
}
