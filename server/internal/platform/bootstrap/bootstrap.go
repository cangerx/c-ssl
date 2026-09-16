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
	"github.com/cangerx/c-ssl/server/internal/version"
)

// App 持有进程级依赖。
type App struct {
	Config *config.Config
	Logger *slog.Logger
	DB     *sql.DB
	Redis  *goredis.Client
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

	if app.DB, err = mysql.Open(ctx, cfg.MySQLDSN); err != nil {
		return nil, err
	}

	if app.Redis, err = platformredis.Open(ctx, cfg.RedisAddr, cfg.RedisPassword, cfg.RedisDB); err != nil {
		app.Close()
		return nil, err
	}

	return app, nil
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
