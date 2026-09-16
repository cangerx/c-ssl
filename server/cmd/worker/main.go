// Command worker 启动队列消费者。
//
// Phase 0 只验证依赖装配与优雅退出，任务处理在 Phase 3 接入。
// 计划承担的任务见 docs/06 第 6 节：Webhook 事件处理、证书详情下载、用户通知、失败重试。
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
		slog.Error("worker 启动失败", "error", err)
		os.Exit(1)
	}
	defer app.Close()

	app.StartupLog("worker")
	app.Logger.Warn("worker 尚未接入任务处理，Phase 3 实现")

	// 占位循环：保持进程存活并定期探活，确保连接池不会因空闲被回收后静默失效
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			app.Logger.Info("收到退出信号，worker 已退出")
			return
		case <-ticker.C:
			app.Logger.Debug("worker 心跳", "mysql_version", app.DBVersion(ctx))
		}
	}
}
