package recharge

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/cangerx/c-ssl/server/internal/platform/tx"
	"github.com/cangerx/c-ssl/server/internal/upstream/payment"
)

// Repository 负责充值订单与渠道流水的数据访问。
//
// 所有读写都经过 tx.Of(ctx, r.db) 取执行器，而不是直接用 r.db：
// 支付回调要在同一个事务里改订单状态、写渠道流水、给钱包加款、写账本，
// 任何一步绕过事务都会产生半提交。
type Repository struct {
	db *sql.DB
}

// NewRepository 构造仓储。
func NewRepository(db *sql.DB) *Repository { return &Repository{db: db} }

const orderColumns = `id, order_no, user_id, amount, status, channel,
	channel_order_no, channel_trade_no, pay_url, paid_at, expires_at,
	remark, created_at, updated_at`

const transactionColumns = `id, channel, channel_trade_no, order_no, user_id,
	amount, status, paid_at, raw_payload, created_at`

// Create 插入一张充值订单。
func (r *Repository) Create(ctx context.Context, o *Order) error {
	result, err := tx.Of(ctx, r.db).ExecContext(ctx,
		`INSERT INTO recharge_orders
			(order_no, user_id, amount, status, channel, channel_order_no,
			 pay_url, expires_at, remark)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		o.OrderNo, o.UserID, o.Amount, string(o.Status), o.Channel,
		o.ChannelOrderNo, o.PayURL, o.ExpiresAt, o.Remark)
	if err != nil {
		return fmt.Errorf("创建充值订单失败: %w", err)
	}

	id, err := result.LastInsertId()
	if err != nil {
		return fmt.Errorf("获取充值订单 ID 失败: %w", err)
	}
	o.ID = id
	return nil
}

// GetByNo 按单号读取订单，不存在时返回 ErrOrderNotFound。
func (r *Repository) GetByNo(ctx context.Context, orderNo string) (*Order, error) {
	row := tx.Of(ctx, r.db).QueryRowContext(ctx,
		`SELECT `+orderColumns+` FROM recharge_orders WHERE order_no = ?`, orderNo)
	return scanOrder(row)
}

// LockByNo 按单号锁定订单，不存在时返回 ErrOrderNotFound。
//
// 必须是 SELECT ... FOR UPDATE：普通 SELECT 在 REPEATABLE READ 下读的是
// 事务快照，读不到并发事务刚提交的状态，状态机校验会基于过期数据通过。
func (r *Repository) LockByNo(ctx context.Context, orderNo string) (*Order, error) {
	row := tx.Of(ctx, r.db).QueryRowContext(ctx,
		`SELECT `+orderColumns+` FROM recharge_orders WHERE order_no = ? FOR UPDATE`, orderNo)
	return scanOrder(row)
}

func scanOrder(row *sql.Row) (*Order, error) {
	var (
		o      Order
		status string
		paidAt sql.NullTime
	)
	err := row.Scan(&o.ID, &o.OrderNo, &o.UserID, &o.Amount, &status, &o.Channel,
		&o.ChannelOrderNo, &o.ChannelTradeNo, &o.PayURL, &paidAt, &o.ExpiresAt,
		&o.Remark, &o.CreatedAt, &o.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrOrderNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("查询充值订单失败: %w", err)
	}

	o.Status = Status(status)
	if paidAt.Valid {
		t := paidAt.Time
		o.PaidAt = &t
	}
	return &o, nil
}

// ListByUser 按订单 ID 倒序分页读取某用户的充值订单。
//
// 多取一条来判断是否还有下一页，避免为了算总数再查一次。
func (r *Repository) ListByUser(ctx context.Context, userID int64, filter Filter) (*Page, error) {
	filter = filter.Normalize()

	query := `SELECT ` + orderColumns + ` FROM recharge_orders WHERE user_id = ?`
	args := []any{userID}
	if filter.Cursor > 0 {
		query += ` AND id < ?`
		args = append(args, filter.Cursor)
	}
	query += ` ORDER BY id DESC LIMIT ?`
	args = append(args, filter.Limit+1)

	rows, err := tx.Of(ctx, r.db).QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("查询充值订单列表失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	items := make([]Order, 0, filter.Limit)
	for rows.Next() {
		var (
			o      Order
			status string
			paidAt sql.NullTime
		)
		if err := rows.Scan(&o.ID, &o.OrderNo, &o.UserID, &o.Amount, &status, &o.Channel,
			&o.ChannelOrderNo, &o.ChannelTradeNo, &o.PayURL, &paidAt, &o.ExpiresAt,
			&o.Remark, &o.CreatedAt, &o.UpdatedAt); err != nil {
			return nil, fmt.Errorf("解析充值订单失败: %w", err)
		}
		o.Status = Status(status)
		if paidAt.Valid {
			t := paidAt.Time
			o.PaidAt = &t
		}
		items = append(items, o)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历充值订单失败: %w", err)
	}

	page := &Page{Items: items}
	if len(items) > filter.Limit {
		page.Items = items[:filter.Limit]
		page.NextCursor = page.Items[len(page.Items)-1].ID
	}
	return page, nil
}

// InsertTransaction 写入一条渠道流水。
//
// 重复的交易号会返回唯一索引冲突错误，由调用方翻译成「这是一次重放」。
// 这里刻意不先查后插：那中间有竞态窗口，并发的两次重复回调会双双通过检查。
func (r *Repository) InsertTransaction(ctx context.Context, t *Transaction) error {
	var paidAt any
	if t.PaidAt != nil {
		paidAt = *t.PaidAt
	}

	result, err := tx.Of(ctx, r.db).ExecContext(ctx,
		`INSERT INTO payment_transactions
			(channel, channel_trade_no, order_no, user_id, amount, status, paid_at, raw_payload)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		t.Channel, t.ChannelTradeNo, t.OrderNo, t.UserID, t.Amount,
		string(t.Status), paidAt, t.RawPayload)
	if err != nil {
		return fmt.Errorf("写入支付渠道流水失败: %w", err)
	}

	id, err := result.LastInsertId()
	if err != nil {
		return fmt.Errorf("获取渠道流水 ID 失败: %w", err)
	}
	t.ID = id
	return nil
}

// FindTransaction 按渠道交易号读取流水，不存在时返回 (nil, nil)。
//
// 这里刻意只提供加锁读（FOR SHARE）这一个版本，没有「普通读」的变体。
// 原因：判断「这是不是一次重放」必须看到最新已提交的数据，
// 而普通 SELECT 在 REPEATABLE READ 下读的是事务快照——快照的建立时机
// 取决于此前有没有发生过普通读，这个依赖是隐式且脆弱的：
// 只要将来有人在事务里先做一次普通查询（比如记一条日志），
// 重放就会被误判成「冲突了却查不到记录」。
//
// 只留加锁读，就让「选错」在代码结构上不可能发生，而不是靠注释提醒。
// 两者行为差异由 repository_test.go 的
// TestLockingReadSeesConcurrentCommit 固定下来。
func (r *Repository) FindTransaction(
	ctx context.Context,
	channel, tradeNo string,
) (*Transaction, error) {
	var (
		t      Transaction
		status string
		paidAt sql.NullTime
		raw    sql.NullString
	)
	err := tx.Of(ctx, r.db).QueryRowContext(ctx,
		`SELECT `+transactionColumns+` FROM payment_transactions
		 WHERE channel = ? AND channel_trade_no = ? FOR SHARE`, channel, tradeNo).
		Scan(&t.ID, &t.Channel, &t.ChannelTradeNo, &t.OrderNo, &t.UserID,
			&t.Amount, &status, &paidAt, &raw, &t.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("查询支付渠道流水失败: %w", err)
	}

	t.Status = payment.Status(status)
	t.RawPayload = raw.String
	if paidAt.Valid {
		v := paidAt.Time
		t.PaidAt = &v
	}
	return &t, nil
}

// MarkPaid 把订单置为已支付，并记录成功那笔支付的渠道交易号。
func (r *Repository) MarkPaid(ctx context.Context, orderNo, tradeNo string, paidAt time.Time) error {
	_, err := tx.Of(ctx, r.db).ExecContext(ctx,
		`UPDATE recharge_orders
		 SET status = ?, channel_trade_no = ?, paid_at = ?
		 WHERE order_no = ? AND status = ?`,
		string(StatusPaid), tradeNo, paidAt, orderNo, string(StatusPending))
	if err != nil {
		return fmt.Errorf("更新充值订单为已支付失败: %w", err)
	}
	return nil
}

// MarkFailed 把订单置为支付失败。
func (r *Repository) MarkFailed(ctx context.Context, orderNo string) error {
	_, err := tx.Of(ctx, r.db).ExecContext(ctx,
		`UPDATE recharge_orders SET status = ? WHERE order_no = ? AND status = ?`,
		string(StatusFailed), orderNo, string(StatusPending))
	if err != nil {
		return fmt.Errorf("更新充值订单为失败失败: %w", err)
	}
	return nil
}
