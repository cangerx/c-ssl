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
	"github.com/cangerx/c-ssl/server/internal/domain/token"
	"github.com/cangerx/c-ssl/server/internal/order"
	"github.com/cangerx/c-ssl/server/internal/platform/mysql"
	"github.com/cangerx/c-ssl/server/internal/platform/ratelimit"
	platformredis "github.com/cangerx/c-ssl/server/internal/platform/redis"
	"github.com/cangerx/c-ssl/server/internal/product"
	"github.com/cangerx/c-ssl/server/internal/recharge"
	"github.com/cangerx/c-ssl/server/internal/server/httpx"
	"github.com/cangerx/c-ssl/server/internal/server/middleware"
	"github.com/cangerx/c-ssl/server/internal/upstream/foxssl"
	"github.com/cangerx/c-ssl/server/internal/upstream/payment"
	"github.com/cangerx/c-ssl/server/internal/user"
	"github.com/cangerx/c-ssl/server/internal/wallet"
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

	// PaymentChannels 是已注册的支付渠道，由 bootstrap 构造。
	PaymentChannels []payment.Channel
	// MockPaymentChannel 非空时注册开发环境的模拟回调接口。
	MockPaymentChannel *payment.MockChannel

	// FoxSSLClient 是证书上游适配器，由 bootstrap 构造。
	//
	// 类型是接口而不是 *foxssl.MockClient：路由层不需要知道上游是谁，
	// 测试也能注入一个自己控制状态的 Mock 实例来构造上游的推进与回调。
	FoxSSLClient foxssl.Client
	// MockFoxSSLClient 非空时注册「推进模拟上游」的开发辅助接口。
	//
	// 这里出现具体类型是有意的，与 MockPaymentChannel 同理：模拟上游的
	// 控制方法（把订单标成已签发）不属于 foxssl.Client——真实 CA 没有这个
	// 能力——所以它只能从具体类型上取，取不到就不注册这条路由。
	MockFoxSSLClient *foxssl.MockClient
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

	// 令牌签发器与限流器是横切依赖，在装配处构造一次后传给各业务域。
	signer := token.NewSigner(deps.Config.JWTSecret, deps.Config.JWTAccessTTL)
	limiter := ratelimit.New(deps.Redis)
	auth := middleware.Auth(signer)

	r.Route(APIPrefix, func(r chi.Router) {
		// 垂直切片逐个接入。每个域自带 Routes，路由表在此集中装配。
		user.NewHandler(
			user.NewService(user.NewRepository(deps.DB), signer, limiter, deps.Config.JWTRefreshTTL),
			auth,
			deps.Config.TrustProxy,
		).Routes(r)

		product.NewHandler(product.NewService(product.NewRepository(deps.DB))).Routes(r)

		wallet.NewHandler(wallet.NewService(wallet.NewRepository(deps.DB)), auth).Routes(r)

		// 充值域同时持有钱包服务：支付回调要在一个事务里
		// 改单状态 + 加款 + 写账本，加款必须走钱包域，不能自己动余额表。
		recharge.NewHandler(
			recharge.NewService(
				recharge.NewRepository(deps.DB),
				wallet.NewService(wallet.NewRepository(deps.DB)),
				deps.PaymentChannels,
				deps.Config.PaymentProvider,
				deps.Config.AppBaseURL,
			),
			auth,
			deps.MockPaymentChannel,
			deps.Config.TrustProxy,
		).Routes(r)

		// 订单域同时持有钱包服务与产品服务：
		//   钱包——下单要冻结、上游受理后转实扣、失败要解冻，
		//         余额一律由钱包域改，订单域不碰余额表；
		//   产品——价格与上游产品编号取自产品，之后一律用订单上的快照。
		//
		// 先判空再赋值，不能直接把 *foxssl.MockClient 塞进接口参数：
		// 值为 nil 的具体指针装进接口后，接口本身不是 nil，订单域里的
		// `if h.mock != nil` 会成立——于是生产环境也会注册那个开发辅助接口，
		// 而且第一次调用就会因为解引用空指针而 panic。
		var mockUpstream order.MockUpstream
		if deps.MockFoxSSLClient != nil {
			mockUpstream = deps.MockFoxSSLClient
		}
		order.NewHandler(
			order.NewService(
				order.NewRepository(deps.DB),
				wallet.NewService(wallet.NewRepository(deps.DB)),
				product.NewService(product.NewRepository(deps.DB)),
				deps.FoxSSLClient,
			),
			auth,
			deps.Config.TrustProxy,
			mockUpstream,
		).Routes(r)
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
