package middleware

import (
	"log/slog"
	"net/http"
	"runtime/debug"

	"github.com/cangerx/c-ssl/server/internal/domain/errs"
	"github.com/cangerx/c-ssl/server/internal/server/httpx"
)

// Recover 捕获 panic，记录堆栈并返回统一错误响应，避免单个请求打挂整个进程。
func Recover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			rec := recover()
			if rec == nil {
				return
			}

			// http.ErrAbortHandler 是标准库约定的静默中止信号，不应吞掉
			if rec == http.ErrAbortHandler {
				panic(rec)
			}

			slog.ErrorContext(r.Context(), "请求处理发生 panic",
				"panic", rec,
				"method", r.Method,
				"path", r.URL.Path,
				"stack", string(debug.Stack()),
			)

			// 响应已开始写出时无法再改状态码，只能记录
			if recorder, ok := w.(*responseRecorder); ok && recorder.wrote {
				return
			}

			httpx.Fail(w, r, errs.New(errs.CodeInternal))
		}()

		next.ServeHTTP(w, r)
	})
}
