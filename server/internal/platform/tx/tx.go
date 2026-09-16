// Package tx 提供跨域事务。
//
// 设计文档 §3.3 规则 5：「跨域事务由发起方持有，事务句柄通过 context 传递」。
// 本包就是这句话的落地。
//
// 典型场景是支付回调：改充值单状态、给钱包加款、写账本必须原子完成。
// 钱包域自己开事务就做不到这一点——发起方必须是 payment 域，
// 钱包只能「加入」已经开着的事务。
//
// # 为什么用 context 而不是显式传参
//
// 跨域调用链是 Service → Service → Repository，中间可能隔好几层。
// 显式传 *sql.Tx 会让每一层的签名都多一个参数，而大部分调用根本不需要事务。
// context 让不需要事务的调用方保持原样，需要事务的调用方在上面套一层 Run。
//
// 代价是「有没有事务」变成隐式状态。因此每个 Repository 都必须用 Of()
// 取执行器，而不是直接用自己持有的 *sql.DB —— 直接用会静默跳过事务，
// 表现为「回调失败但钱已经加了」这类最难排查的问题。
package tx

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/cangerx/c-ssl/server/internal/platform/mysql"
)

// 事务重试参数。
//
// 重试的是整个事务体，不是某条语句——只有从头再来才能保证原子性。
// 因此 Run 的 fn 必须是「可以安全重放」的：只做数据库操作，
// 不调用外部接口、不发消息、不写文件。中途有副作用的话，
// 重放会让副作用发生两次。
//
// # 为什么必须重试，而不是把死锁当偶发错误抛出去
//
// 最典型的场景是「按唯一键找一行，找不到就插入」。InnoDB 在
// SELECT ... FOR UPDATE 找不到行时会在唯一索引的间隙上加间隙锁；
// 两个并发事务的间隙锁彼此不冲突，但随后的 INSERT 需要插入意向锁，
// 与对方的间隙锁互斥——于是互相等待，形成死锁，InnoDB 挑一个回滚。
//
// 这不是代码缺陷，是间隙锁的固有行为，从代码上无法消除。
// 而它偏偏命中「用户第一次操作」这条路径：并发的两次请求里
// 有一次随机失败，是不可接受的。
//
// 实测：12 个并发请求对同一个新账户首次入账，不做重试时 7 个失败于
// Error 1213 (40001): Deadlock found when trying to get lock。
// 这条路径由 wallet 包的 TestConcurrentFirstOperationsOnNewAccount 覆盖。
const (
	maxAttempts = 3
	retryDelay  = 20 * time.Millisecond
)

type ctxKey struct{}

// Manager 开启事务并把句柄放进 context。
//
// 由 bootstrap/router 构造一次后注入给需要事务编排的 Service。
// Service 持有 Manager 而不是 *sql.DB，连接池仍只由 bootstrap 管理。
type Manager struct {
	db *sql.DB
}

// NewManager 构造事务管理器。
func NewManager(db *sql.DB) *Manager { return &Manager{db: db} }

// Run 在事务中执行 fn，遇死锁或锁等待超时自动重试。
//
// fn 必须可以安全重放（见包注释）。返回 nil 则提交，返回错误则回滚。
//
// 若 context 中已经存在事务，Run 直接复用外层事务而不新开一个——
// 这样跨域调用链上只有最外层在管理事务边界，内层不会出现
// 「提交了内层、外层又回滚」导致的半提交。
func (m *Manager) Run(ctx context.Context, fn func(ctx context.Context) error) error {
	if _, ok := FromContext(ctx); ok {
		return fn(ctx)
	}

	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		err := m.runOnce(ctx, fn)
		if err == nil {
			return nil
		}
		if !mysql.IsRetryable(err) {
			return err
		}
		lastErr = err

		slog.WarnContext(ctx, "事务遇死锁，重试", "attempt", attempt, "error", err)

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(retryDelay):
		}
	}
	return fmt.Errorf("事务重试 %d 次后仍失败: %w", maxAttempts, lastErr)
}

func (m *Manager) runOnce(ctx context.Context, fn func(context.Context) error) error {
	handle, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("开启事务失败: %w", err)
	}
	defer func() { _ = handle.Rollback() }()

	if err := fn(WithTx(ctx, handle)); err != nil {
		return err
	}
	if err := handle.Commit(); err != nil {
		return fmt.Errorf("提交事务失败: %w", err)
	}
	return nil
}

// WithTx 把事务句柄放进 context。一般不需要直接调用，用 Run 即可。
func WithTx(ctx context.Context, handle *sql.Tx) context.Context {
	return context.WithValue(ctx, ctxKey{}, handle)
}

// FromContext 取出 context 中的事务句柄。
func FromContext(ctx context.Context) (*sql.Tx, bool) {
	handle, ok := ctx.Value(ctxKey{}).(*sql.Tx)
	return handle, ok
}

// Executor 是 *sql.DB 与 *sql.Tx 的公共子集。
//
// 注意 QueryRowContext 返回的是 *sql.Row 而不是接口，这是标准库的设计
// （*sql.Row 无法被外部实现），因此这里的签名是具体的。
type Executor interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Of 返回本次调用应当使用的执行器：context 中有事务就用事务，否则用默认连接池。
//
// 所有 Repository 的读写都应经过它。绕过它直接用 r.db 会让该操作脱离事务，
// 在被跨域事务包裹时产生半提交——这类 bug 不会报错，只会让数据对不上。
func Of(ctx context.Context, db *sql.DB) Executor {
	if handle, ok := FromContext(ctx); ok {
		return handle
	}
	return db
}

// InTransaction 判断当前是否处在事务中。
func InTransaction(ctx context.Context) bool {
	_, ok := FromContext(ctx)
	return ok
}

// ErrNoTransaction 表示期望在事务中执行，但 context 里没有事务。
var ErrNoTransaction = errors.New("当前不在事务中")
