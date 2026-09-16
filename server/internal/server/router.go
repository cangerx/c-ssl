// Package server 组装 HTTP 路由与中间件。
package server

import (
	"context"
	"database/sql"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	goredis "github.com/redis/go-redis/v9"

	"github.com/cangerx/c-ssl/server/internal/config"
	"github.com/cangerx/c-ssl/server/internal/domain/errs"
	"github.com/cangerx/c-ssl/server/internal/platform/mysql"
	platformredis "github.com/cangerx/c-ssl/server/internal/platform/redis"
	"github.com/cangerx/c-ssl/server/internal/product"
	"github.com/cangerx/c-ssl/server/internal/server/httpx"
	"github.com/cangerx/c-ssl/server/internal/server/middleware"
)

const (
	// APIPrefix 是业务接口前缀。健康检查不在其下，便于探针直接访问。
	APIPrefix = "/api/v1"

	healthTimeout = 5 * time.Second
	readTimeout   = 15 * time.Second
	writeTimeout  = 30 * time.Second
	idleTimeout   = 60 * time.Second
)

// Deps 是路由所需的外部依赖。
type Deps struct {
	Config  *config.Config
	DB      *sql.DB
	Redis   *goredis.Client
	Version string
}

// NewRouter 构建路由。中间件顺序为 Trace → Log → Recover，
// 这样 panic 恢复后的 500 响应也能被访问日志记录到。
func NewRouter(deps Deps) http.Handler {
	r := chi.NewRouter()

	r.Use(middleware.Trace)
	r.Use(middleware.Log)
	r.Use(middleware.Recover)

	r.NotFound(func(w http.ResponseWriter, req *http.Request) {
		httpx.Fail(w, req, errs.New(errs.CodeNotFound))
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, req *http.Request) {
		httpx.Fail(w, req, errs.Newf(errs.CodeInvalidParam, "该接口不支持 %s 方法", req.Method))
	})

	r.Get("/health", deps.handleHealth)

	r.Route(APIPrefix, func(r chi.Router) {
		// 垂直切片逐个接入。每个域自带 Routes，路由表在此集中装配。
		product.NewHandler(product.NewService(product.NewRepository(deps.DB))).Routes(r)
	})

	return r
}

// NewHTTPServer 构建带超时配置的 http.Server。
func NewHTTPServer(addr string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:         addr,
		Handler:      handler,
		ReadTimeout:  readTimeout,
		WriteTimeout: writeTimeout,
		IdleTimeout:  idleTimeout,
	}
}

// ── 健康检查 ──────────────────────────────────────

// DependencyStatus 对应 openapi/components/schemas/common.yaml 的 DependencyStatus。
type DependencyStatus struct {
	Ok        bool   `json:"ok"`
	LatencyMs int32  `json:"latencyMs"`
	Error     string `json:"error"`
}

// Dependencies 对应契约中的 dependencies 对象。
type Dependencies struct {
	MySQL DependencyStatus `json:"mysql"`
	Redis DependencyStatus `json:"redis"`
}

// HealthStatus 对应契约中的 HealthStatus。
type HealthStatus struct {
	Status       string       `json:"status"`
	Version      string       `json:"version"`
	Dependencies Dependencies `json:"dependencies"`
}

func (d Deps) handleHealth(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), healthTimeout)
	defer cancel()

	status := HealthStatus{
		Status:  "ok",
		Version: d.Version,
		Dependencies: Dependencies{
			MySQL: probe(ctx, func(ctx context.Context) (time.Duration, error) {
				return mysql.Health(ctx, d.DB)
			}),
			Redis: probe(ctx, func(ctx context.Context) (time.Duration, error) {
				return platformredis.Health(ctx, d.Redis)
			}),
		},
	}

	if !status.Dependencies.MySQL.Ok || !status.Dependencies.Redis.Ok {
		status.Status = "degraded"
		httpx.Write(w, r, http.StatusServiceUnavailable, httpx.Envelope{
			Code:    errs.CodeInternal,
			Message: "服务依赖未就绪",
			Data:    status.Dependencies,
		})
		return
	}

	httpx.OK(w, r, status)
}

func probe(ctx context.Context, fn func(context.Context) (time.Duration, error)) DependencyStatus {
	latency, err := fn(ctx)
	result := DependencyStatus{
		Ok:        err == nil,
		LatencyMs: int32(latency.Milliseconds()),
	}
	if err != nil {
		result.Error = err.Error()
	}
	return result
}
