// Package logger 构建结构化日志器。
//
// 日志中的 trace_id 由中间件写入 context，由这里的 handler 自动附加，
// 业务代码不需要手动传 trace_id。
package logger

import (
	"context"
	"log/slog"
	"os"
	"strings"
)

type ctxKey int

const traceIDKey ctxKey = iota

// WithTraceID 把 trace ID 写入 context。
func WithTraceID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, traceIDKey, id)
}

// TraceID 从 context 取出 trace ID，不存在时返回空串。
func TraceID(ctx context.Context) string {
	id, _ := ctx.Value(traceIDKey).(string)
	return id
}

// New 按环境构建日志器。
// 开发环境用文本格式便于阅读，其他环境用 JSON 便于采集。
func New(level, env string) *slog.Logger {
	var lv slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lv = slog.LevelDebug
	case "warn", "warning":
		lv = slog.LevelWarn
	case "error":
		lv = slog.LevelError
	default:
		lv = slog.LevelInfo
	}

	opts := &slog.HandlerOptions{Level: lv}

	var base slog.Handler
	if strings.EqualFold(env, "development") {
		base = slog.NewTextHandler(os.Stdout, opts)
	} else {
		base = slog.NewJSONHandler(os.Stdout, opts)
	}

	return slog.New(traceHandler{Handler: base})
}

// traceHandler 在每条日志上自动附加当前 context 的 trace_id。
type traceHandler struct {
	slog.Handler
}

func (h traceHandler) Handle(ctx context.Context, r slog.Record) error {
	if id := TraceID(ctx); id != "" {
		r.AddAttrs(slog.String("trace_id", id))
	}
	return h.Handler.Handle(ctx, r)
}

func (h traceHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return traceHandler{Handler: h.Handler.WithAttrs(attrs)}
}

func (h traceHandler) WithGroup(name string) slog.Handler {
	return traceHandler{Handler: h.Handler.WithGroup(name)}
}

// Sensitive 是日志中必须脱敏的字段名。
//
// 上游密钥、支付私钥、口令一旦进入日志就等于泄露，这里集中登记，
// 由调用方在记录请求/响应体前先过一遍。
var Sensitive = map[string]struct{}{
	"password":            {},
	"passwd":              {},
	"api_key":             {},
	"apikey":              {},
	"apiKey":              {},
	"secret":              {},
	"private_key":         {},
	"privateKey":          {},
	"token":               {},
	"access_token":        {},
	"refresh_token":       {},
	"authorization":       {},
	"X-Webhook-Signature": {},
}

// IsSensitive 判断字段名是否属于需要脱敏的敏感字段。
func IsSensitive(key string) bool {
	_, ok := Sensitive[key]
	return ok
}

// Redact 对敏感值做遮蔽，保留长度信息便于排查。
func Redact(value string) string {
	if value == "" {
		return ""
	}
	if len(value) <= 8 {
		return "***"
	}
	return value[:4] + "***" + value[len(value)-2:]
}
