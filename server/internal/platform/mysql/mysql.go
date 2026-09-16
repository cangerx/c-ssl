// Package mysql 负责 MySQL 连接池的创建与健康探测。
//
// 业务代码不直接持有 *sql.DB —— 只有各域的 Repository 与 bootstrap 会依赖本包。
package mysql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	mysqldriver "github.com/go-sql-driver/mysql"
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

// MySQL 服务端错误码。
//
// 集中在这里是因为「哪些错误值得重试」是 MySQL 的领域知识，
// 仓储层与事务管理器都需要判断，不该各自抄一份常量。
const (
	ErrCodeDeadlock    = 1213 // Deadlock found when trying to get lock
	ErrCodeLockTimeout = 1205 // Lock wait timeout exceeded
)

// IsRetryable 判断错误是否值得重试。
//
// 只认死锁与锁等待超时：它们意味着「这次撞上了并发」，重来一次就会成功。
// 业务错误（余额不足、唯一键冲突）绝不重试——重试改变不了结果，
// 还会把本该立刻返回的错误拖成超时。
func IsRetryable(err error) bool {
	var mysqlErr *mysqldriver.MySQLError
	if !errors.As(err, &mysqlErr) {
		return false
	}
	return mysqlErr.Number == ErrCodeDeadlock || mysqlErr.Number == ErrCodeLockTimeout
}
