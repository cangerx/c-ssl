// Package ratelimit 提供基于 Redis 的固定窗口计数限流。
//
// 选择固定窗口而非滑动窗口或令牌桶：实现简单、只需两次 Redis 往返，
// 且对登录限流这个场景足够——它的目的是抬高暴力破解成本，
// 不是做精确的流量整形。固定窗口的边界突刺（窗口交界处可能放行 2 倍请求）
// 在这里可以接受。
package ratelimit

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"
)

// Limiter 是 Redis 支撑的计数器限流器。
type Limiter struct {
	rdb *redis.Client
}

// New 构造限流器。rdb 为 nil 时所有请求都放行（便于不依赖 Redis 的测试）。
func New(rdb *redis.Client) *Limiter { return &Limiter{rdb: rdb} }

// Result 是一次限流判定的结果。
type Result struct {
	// Allowed 表示本次请求是否放行。
	Allowed bool
	// Remaining 是本窗口内剩余可用次数，未放行时为 0。
	Remaining int
	// RetryAfter 是建议的重试等待时长，仅在被拒绝时有意义。
	RetryAfter time.Duration
}

// Allow 对 key 计数一次，返回是否放行。
//
// limit 为窗口内允许的最大次数；window 为窗口长度。
//
// Redis 不可用时**放行**并记警告，而不是拒绝：限流是纵深防御的一层，
// 不该因为它自身故障导致全站无法登录。真正兜底的是 users 表上的
// 失败计数与账号锁定，那一层不依赖 Redis。
func (l *Limiter) Allow(ctx context.Context, key string, limit int, window time.Duration) (Result, error) {
	if l.rdb == nil || limit <= 0 {
		return Result{Allowed: true, Remaining: limit}, nil
	}

	count, err := l.rdb.Incr(ctx, key).Result()
	if err != nil {
		slog.WarnContext(ctx, "限流计数失败，本次放行", "key", key, "error", err)
		return Result{Allowed: true, Remaining: limit}, nil
	}

	// 首次计数时设置过期，形成窗口。后续请求不刷新 TTL，
	// 否则持续请求会让窗口无限延长，等同于永久封禁。
	if count == 1 {
		if err := l.rdb.Expire(ctx, key, window).Err(); err != nil {
			slog.WarnContext(ctx, "设置限流窗口失败", "key", key, "error", err)
		}
	}

	if count > int64(limit) {
		ttl, ttlErr := l.rdb.TTL(ctx, key).Result()
		if ttlErr != nil || ttl < 0 {
			ttl = window
		}
		return Result{Allowed: false, Remaining: 0, RetryAfter: ttl}, nil
	}

	return Result{Allowed: true, Remaining: limit - int(count)}, nil
}

// Reset 清除某个 key 的计数，用于登录成功后立即解除该账号的限流。
func (l *Limiter) Reset(ctx context.Context, key string) error {
	if l.rdb == nil {
		return nil
	}
	if err := l.rdb.Del(ctx, key).Err(); err != nil {
		if errors.Is(err, redis.Nil) {
			return nil
		}
		return fmt.Errorf("清除限流计数失败: %w", err)
	}
	return nil
}

// 限流键前缀。集中定义避免各处拼错，也便于运维按前缀排查。
const (
	KeyPrefixLoginByIP    = "rl:login:ip:"
	KeyPrefixLoginByEmail = "rl:login:email:"
	KeyPrefixRegisterByIP = "rl:register:ip:"
)

// LoginByIPKey 构造按来源 IP 的登录限流键。
func LoginByIPKey(ip string) string { return KeyPrefixLoginByIP + ip }

// LoginByEmailKey 构造按目标账号的登录限流键。
func LoginByEmailKey(email string) string { return KeyPrefixLoginByEmail + email }

// RegisterByIPKey 构造按来源 IP 的注册限流键。
func RegisterByIPKey(ip string) string { return KeyPrefixRegisterByIP + ip }
