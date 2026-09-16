package middleware

import (
	"log/slog"
	"net/http"
	"time"
)

// responseRecorder 记录响应状态码与字节数，供访问日志使用。
type responseRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
	wrote  bool
}

func (r *responseRecorder) WriteHeader(code int) {
	if r.wrote {
		return
	}
	r.status = code
	r.wrote = true
	r.ResponseWriter.WriteHeader(code)
}

func (r *responseRecorder) Write(b []byte) (int, error) {
	if !r.wrote {
		r.WriteHeader(http.StatusOK)
	}
	n, err := r.ResponseWriter.Write(b)
	r.bytes += n
	return n, err
}

// Unwrap 让 http.ResponseController 能拿到原始 ResponseWriter，
// 否则流式响应与超时控制会失效。
func (r *responseRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// Log 记录访问日志。
//
// 只记录方法与路径，不记录查询串与请求体——订单接口的查询串可能含订单号，
// 请求体可能含口令与上游密钥。
func Log(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		recorder := &responseRecorder{ResponseWriter: w, status: http.StatusOK}

		next.ServeHTTP(recorder, r)

		level := slog.LevelInfo
		if recorder.status >= http.StatusInternalServerError {
			level = slog.LevelError
		} else if recorder.status >= http.StatusBadRequest {
			level = slog.LevelWarn
		}

		slog.Log(r.Context(), level, "http 请求",
			"method", r.Method,
			"path", r.URL.Path,
			"status", recorder.status,
			"bytes", recorder.bytes,
			"duration_ms", time.Since(start).Milliseconds(),
			"remote_addr", r.RemoteAddr,
		)
	})
}
