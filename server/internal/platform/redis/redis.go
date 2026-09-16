// Package redis 负责 Redis 客户端的创建与健康探测。
//
// 本地实例可能被多个项目共用，因此连接必须显式指定 db index，
// 不允许依赖默认的 db0。
package redis

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	poolSize     = 20
	minIdleConns = 5
	dialTimeout  = 3 * time.Second
	readTimeout  = 3 * time.Second
	writeTimeout = 3 * time.Second
	pingTimeout  = 3 * time.Second
)

// Open 建立客户端并立即探活。
func Open(ctx context.Context, addr, password string, db int) (*redis.Client, error) {
	if addr == "" {
		return nil, fmt.Errorf("未配置 redis 地址")
	}

	client := redis.NewClient(&redis.Options{
		Addr:         addr,
		Password:     password,
		DB:           db,
		PoolSize:     poolSize,
		MinIdleConns: minIdleConns,
		DialTimeout:  dialTimeout,
		ReadTimeout:  readTimeout,
		WriteTimeout: writeTimeout,
	})

	pingCtx, cancel := context.WithTimeout(ctx, pingTimeout)
	defer cancel()

	if err := client.Ping(pingCtx).Err(); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("redis 探活失败: %w", err)
	}

	return client, nil
}

// Health 探测连接可用性，返回耗时。
func Health(ctx context.Context, client *redis.Client) (time.Duration, error) {
	if client == nil {
		return 0, fmt.Errorf("redis 未初始化")
	}

	probeCtx, cancel := context.WithTimeout(ctx, pingTimeout)
	defer cancel()

	start := time.Now()
	err := client.Ping(probeCtx).Err()
	return time.Since(start), err
}
