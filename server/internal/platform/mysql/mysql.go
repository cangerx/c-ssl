// Package mysql 负责 MySQL 连接池的创建与健康探测。
//
// 业务代码不直接持有 *sql.DB —— 只有各域的 Repository 与 bootstrap 会依赖本包。
package mysql

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	_ "github.com/go-sql-driver/mysql"
)

const (
	maxOpenConns    = 25
	maxIdleConns    = 10
	connMaxLifetime = 5 * time.Minute
	connMaxIdleTime = 1 * time.Minute
	pingTimeout     = 3 * time.Second
)

// Open 建立连接池并立即探活，探活失败直接返回错误，避免服务带着坏连接启动。
func Open(ctx context.Context, dsn string) (*sql.DB, error) {
	if dsn == "" {
		return nil, fmt.Errorf("MySQL DSN 未配置")
	}

	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, fmt.Errorf("打开 MySQL 连接失败: %w", err)
	}

	db.SetMaxOpenConns(maxOpenConns)
	db.SetMaxIdleConns(maxIdleConns)
	db.SetConnMaxLifetime(connMaxLifetime)
	db.SetConnMaxIdleTime(connMaxIdleTime)

	pingCtx, cancel := context.WithTimeout(ctx, pingTimeout)
	defer cancel()

	if err := db.PingContext(pingCtx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("MySQL 探活失败: %w", err)
	}

	return db, nil
}

// Health 探测连接可用性，返回耗时。
func Health(ctx context.Context, db *sql.DB) (time.Duration, error) {
	if db == nil {
		return 0, fmt.Errorf("MySQL 未初始化")
	}

	probeCtx, cancel := context.WithTimeout(ctx, pingTimeout)
	defer cancel()

	start := time.Now()
	err := db.PingContext(probeCtx)
	return time.Since(start), err
}

// Version 返回服务端版本号，用于环境自检时提示版本偏差。
func Version(ctx context.Context, db *sql.DB) (string, error) {
	probeCtx, cancel := context.WithTimeout(ctx, pingTimeout)
	defer cancel()

	var version string
	if err := db.QueryRowContext(probeCtx, "SELECT VERSION()").Scan(&version); err != nil {
		return "", fmt.Errorf("查询 MySQL 版本失败: %w", err)
	}
	return version, nil
}
