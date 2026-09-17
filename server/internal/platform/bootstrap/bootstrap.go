// Package bootstrap 负责依赖的组装与生命周期管理。
//
// 三个入口（api / worker / cron）共用同一套装配逻辑，
// 差别只在启动后运行什么。
package bootstrap

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"

	goredis "github.com/redis/go-redis/v9"

	"github.com/cangerx/c-ssl/server/internal/config"
	"github.com/cangerx/c-ssl/server/internal/platform/logger"
	"github.com/cangerx/c-ssl/server/internal/platform/mysql"
	platformredis "github.com/cangerx/c-ssl/server/internal/platform/redis"
	"github.com/cangerx/c-ssl/server/internal/upstream/foxssl"
	"github.com/cangerx/c-ssl/server/internal/upstream/payment"
	"github.com/cangerx/c-ssl/server/internal/version"
)

// App 持有进程级依赖。
type App struct {
	Config *config.Config
	Logger *slog.Logger
	DB     *sql.DB
	Redis  *goredis.Client

	// PaymentChannels 是已注册的支付渠道，供路由装配注入给充值域。
	PaymentChannels []payment.Channel
	// MockPaymentChannel 非空表示启用了 Mock 渠道。
	// 开发环境据此注册模拟回调接口；本包不判断环境，环境判断集中在配置层。
	MockPaymentChannel *payment.MockChannel

	// FoxSSLClient 是证书上游适配器，供路由装配注入给订单域。
	//
	// 接口而不是具体类型：订单域只依赖 foxssl.Client，接入真实上游时
	// 这里换一个构造分支即可，订单域一行都不用改。
	FoxSSLClient foxssl.Client
	// MockFoxSSLClient 非空表示启用了 Mock 上游，订单域据此注册
	// 「推进模拟上游」的开发辅助接口。与 MockPaymentChannel 同一套模式：
	// 本包不判断环境，环境判断集中在配置层（生产环境不允许 mock）。
	MockFoxSSLClient *foxssl.MockClient
}

// New 按顺序加载配置、初始化日志、连接 MySQL 与 Redis。
//
// 任何一步失败都立即返回错误，不带着半成品依赖继续启动——
// 这类问题在启动阶段暴露远比在第一个请求时暴露便宜。
func New(ctx context.Context) (*App, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, fmt.Errorf("加载配置失败: %w", err)
	}

	log := logger.New(cfg.LogLevel, cfg.AppEnv)
	slog.SetDefault(log)

	app := &App{Config: cfg, Logger: log}

	// 支付渠道在启动阶段构造，而不是在每个请求里判断：
	// 密钥缺失、配置了未实现的渠道这类问题必须在启动时就暴露，
	// 而不是等到用户点开支付页、或者渠道回调进来时才发现。
	if app.PaymentChannels, app.MockPaymentChannel, err = buildPaymentChannels(cfg); err != nil {
		return nil, err
	}

	// 证书上游同样在启动阶段构造。配置了未实现的上游时拒绝启动，
	// 而不是悄悄回退到 Mock——那会让「以为在下真单」的环境
	// 一直签发着浏览器不信任的证书，而且全程不报错。
	if app.FoxSSLClient, app.MockFoxSSLClient, err = buildFoxSSL(cfg); err != nil {
		return nil, err
	}

	if app.DB, err = mysql.Open(ctx, cfg.MySQLDSN); err != nil {
		return nil, err
	}

	if app.Redis, err = platformredis.Open(ctx, cfg.RedisAddr, cfg.RedisPassword, cfg.RedisDB); err != nil {
		app.Close()
		return nil, err
	}

	return app, nil
}

// buildPaymentChannels 构造支付渠道注册表。
//
// 只有一个渠道时不引入「注册表」这层抽象是更省事的写法，但 webhook 的路由
// 需要一个「按标识取渠道」的能力，而这个能力本身就是注册表。
// 现在写出来，接入第二个渠道时不需要改动调用方。
func buildPaymentChannels(cfg *config.Config) ([]payment.Channel, *payment.MockChannel, error) {
	switch cfg.PaymentProvider {
	case payment.NameMock:
		ch, err := payment.NewMockChannel(cfg.PaymentWebhookSecret, cfg.AppBaseURL)
		if err != nil {
			return nil, nil, fmt.Errorf("构造 Mock 支付渠道失败: %w", err)
		}
		return []payment.Channel{ch}, ch, nil

	default:
		// 真实渠道在 Phase 5 接入（见 docs/06 第 10 节）。
		// 配置了未实现的渠道时直接拒绝启动，而不是悄悄回退到 Mock——
		// 那会让「以为在收真钱」的环境实际一分钱没收到，而且不会报错。
		return nil, nil, fmt.Errorf(
			"未实现的支付渠道 %q：当前仅支持 %s", cfg.PaymentProvider, payment.NameMock)
	}
}

// buildFoxSSL 构造证书上游适配器。
//
// 与支付渠道一样，只有一个实现时这层 switch 看起来是多余的。
// 保留它是为了给「配置了未实现的上游」一个明确的失败点：
// 少了它，FOXSSL_PROVIDER=real 会被静默忽略，服务照常启动、照常用 Mock 签单。
//
// 第二个返回值是具体的 Mock 客户端，仅在启用 Mock 上游时非空。
// 返回具体类型而不是接口，是为了让调用方能安全地判空——见 router 里的说明。
func buildFoxSSL(cfg *config.Config) (foxssl.Client, *foxssl.MockClient, error) {
	switch cfg.FoxSSLProvider {
	case foxssl.NameMock:
		client, err := foxssl.NewMockClient(
			cfg.FoxSSLAPIKey, cfg.FoxSSLWebhookSecret, cfg.FoxSSLBaseURL)
		if err != nil {
			return nil, nil, fmt.Errorf("构造 Mock 证书上游失败: %w", err)
		}
		return client, client, nil

	case foxssl.NameHTTP:
		// 真实上游**不接收 FoxSSLWebhookSecret**：上游文档明确回调验签
		// 用的就是 API Key（见 foxssl.HTTPOptions.APIKey）。把它传进去
		// 会让所有回调因为签名不匹配被拒，而错误信息只会说「签名不匹配」。
		//
		// 那个配置项仍然保留：Mock 上游自己签自己验，用独立密钥是合理的。
		client, err := foxssl.NewHTTPClient(foxssl.HTTPOptions{
			BaseURL: cfg.FoxSSLBaseURL,
			APIKey:  cfg.FoxSSLAPIKey,
		})
		if err != nil {
			return nil, nil, fmt.Errorf("构造真实证书上游失败: %w", err)
		}
		// 第二个返回值为 nil：真实上游没有 Mock 的控制方法，
		// 开发辅助接口会因此不注册（见 router 里的说明）。
		return client, nil, nil

	default:
		return nil, nil, fmt.Errorf(
			"未实现的证书上游 %q：当前仅支持 %s 与 %s",
			cfg.FoxSSLProvider, foxssl.NameMock, foxssl.NameHTTP)
	}
}

// StartupLog 输出启动摘要。敏感字段已脱敏。
func (a *App) StartupLog(service string) {
	attrs := []any{
		"service", service,
		"version", version.String(),
		"build_time", version.BuildTime,
	}
	attrs = append(attrs, a.Config.LogSummary()...)
	a.Logger.Info("服务启动", attrs...)
}

// Close 释放全部依赖，可重复调用。
func (a *App) Close() {
	if a.Redis != nil {
		if err := a.Redis.Close(); err != nil {
			a.Logger.Error("关闭 Redis 失败", "error", err)
		}
		a.Redis = nil
	}
	if a.DB != nil {
		if err := a.DB.Close(); err != nil {
			a.Logger.Error("关闭 MySQL 失败", "error", err)
		}
		a.DB = nil
	}
}

// DBVersion 返回 MySQL 服务端版本，用于启动日志中提示版本偏差。
func (a *App) DBVersion(ctx context.Context) string {
	v, err := mysql.Version(ctx, a.DB)
	if err != nil {
		return "unknown"
	}
	return v
}
