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
