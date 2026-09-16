// Package httpx 提供统一响应外壳与错误输出。
//
// 单独成包是为了避免导入环：router 需要 middleware，而 middleware 的 panic 恢复
// 需要输出统一错误响应，若把响应逻辑放在 server 包就会形成 server → middleware → server 的环。
package httpx

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/cangerx/c-ssl/server/internal/domain/errs"
	"github.com/cangerx/c-ssl/server/internal/platform/logger"
)

// Envelope 是所有接口的统一响应外壳，对应 openapi/components/schemas/common.yaml 的 ApiEnvelope。
//
// Data 不使用 omitempty：契约要求成功与失败响应都带 data 字段（失败时为 null）。
type Envelope struct {
	Code    errs.Code `json:"code"`
	Message string    `json:"message"`
	Data    any       `json:"data"`
}

// OK 返回成功响应。
func OK(w http.ResponseWriter, r *http.Request, data any) {
	Write(w, r, http.StatusOK, Envelope{
		Code:    errs.CodeSuccess,
		Message: errs.CodeSuccess.Message(),
		Data:    data,
	})
}

// Write 以指定 HTTP 状态码输出响应外壳。
// 供健康检查等需要自定义状态码的场景使用，业务接口应优先用 OK 与 Fail。
func Write(w http.ResponseWriter, r *http.Request, status int, payload Envelope) {
	writeJSON(w, r, status, payload)
}

// Fail 返回错误响应。
//
// 只有 *errs.Error 携带的 Message 与 Fields 会返回给客户端；
// 底层原因（数据库报错、上游响应体等）仅写入服务端日志，避免内部细节泄露。
func Fail(w http.ResponseWriter, r *http.Request, err error) {
	e := errs.From(err)

	if e.Code == errs.CodeInternal || e.Code == errs.CodeUpstream {
		slog.ErrorContext(r.Context(), "请求处理失败",
			"code", e.Code,
			"message", e.Message,
			"method", r.Method,
			"path", r.URL.Path,
			"cause", causeString(e),
		)
	}

	var data any
	if len(e.Fields) > 0 {
		data = map[string]any{"fields": e.Fields}
	}

	writeJSON(w, r, e.Code.HTTPStatus(), Envelope{
		Code:    e.Code,
		Message: e.Message,
		Data:    data,
	})
}

func causeString(e *errs.Error) string {
	if cause := errors.Unwrap(e); cause != nil {
		return cause.Error()
	}
	return ""
}

func writeJSON(w http.ResponseWriter, r *http.Request, status int, payload Envelope) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)

	if err := json.NewEncoder(w).Encode(payload); err != nil {
		// 响应头已发出，无法再改状态码，只能记录
		slog.ErrorContext(r.Context(), "写入响应失败",
			"error", err,
			"trace_id", logger.TraceID(r.Context()),
		)
	}
}
