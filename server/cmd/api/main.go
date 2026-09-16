// Command api 启动 HTTP 服务。
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/cangerx/c-ssl/server/internal/platform/bootstrap"
	"github.com/cangerx/c-ssl/server/internal/server"
	"github.com/cangerx/c-ssl/server/internal/version"
)

const shutdownTimeout = 15 * time.Second

func main() {
	if err := run(); err != nil {
		slog.Error("api 服务异常退出", "error", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	app, err := bootstrap.New(ctx)
	if err != nil {
		return err
	}
	defer app.Close()

	app.StartupLog("api")
	app.Logger.Info("数据库版本", "mysql_version", app.DBVersion(ctx))

	router := server.NewRouter(server.Deps{
		Config:  app.Config,
		DB:      app.DB,
		Redis:   app.Redis,
		Version: version.String(),
	})
	srv := server.NewHTTPServer(app.Config.Addr(), router)

	serveErr := make(chan error, 1)
	go func() {
		app.Logger.Info("HTTP 服务监听", "addr", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
		}
	}()

	select {
	case err := <-serveErr:
		return fmt.Errorf("HTTP 服务异常: %w", err)
	case <-ctx.Done():
		app.Logger.Info("收到退出信号，开始优雅关闭")
	}

	// 关闭时用独立的 context：此时 ctx 已被信号取消，不能再用于超时控制
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("优雅关闭失败: %w", err)
	}

	app.Logger.Info("服务已退出")
	return nil
}
