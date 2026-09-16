// Command cron 启动定时任务。
//
// Phase 0 只验证依赖装配与优雅退出，定时任务在 Phase 3 接入。
// 计划承担的任务见 docs/06 第 6 节：Webhook 兜底轮询、上游余额检查、
// 用户钱包与订单对账、长时间未验证提醒、证书到期提醒。
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/cangerx/c-ssl/server/internal/platform/bootstrap"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	app, err := bootstrap.New(ctx)
	if err != nil {
		slog.Error("cron 启动失败", "error", err)
		os.Exit(1)
	}
	defer app.Close()

	app.StartupLog("cron")
	app.Logger.Warn("cron 尚未接入定时任务，Phase 3 实现")

	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			app.Logger.Info("收到退出信号，cron 已退出")
			return
		case <-ticker.C:
			app.Logger.Debug("cron 心跳", "mysql_version", app.DBVersion(ctx))
		}
	}
}
