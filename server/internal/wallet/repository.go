package wallet

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/cangerx/c-ssl/server/internal/domain/money"
	"github.com/cangerx/c-ssl/server/internal/platform/tx"
)

// Repository 负责钱包账户与账本的数据访问。
//
// 本类型刻意只提供「读账户、读账本、追加流水」三类方法，没有任何更新或删除
// 账本流水的方法。账本的不可变性由这个 API 面保证，而不是靠调用方自觉。
//
// 所有读写都经过 tx.Of(ctx, r.db) 取执行器，而不是直接用 r.db：
// 被跨域事务包裹时（支付回调要同时改订单、加款、写账本），
// 直接用 r.db 会让这一步脱离事务，产生「回调失败但钱已经加了」的半提交。
type Repository struct {
	db  *sql.DB
	txm *tx.Manager
}

// NewRepository 构造仓储。
func NewRepository(db *sql.DB) *Repository {
	return &Repository{db: db, txm: tx.NewManager(db)}
}

const accountColumns = `id, user_id, available_balance, frozen_balance, version, created_at, updated_at`

const ledgerColumns = `id, account_id, user_id, entry_no, op, biz_type, biz_no,
	available_delta, frozen_delta, available_after, frozen_after, remark, created_at`

// Apply 完成一次余额变更。
//
// 事务边界交给 platform/tx 决定，两种情形行为不同：
//
//   - 独立调用（context 中没有事务）：自开事务、负责提交，死锁自动重试；
//   - 被跨域事务包裹：直接加入外层事务，既不提交也不重试。
//     重试必须由发起方决定——内层重试会丢掉外层已经做过的改动。
func (r *Repository) Apply(ctx context.Context, req Request) (*Result, error) {
	availableDelta, frozenDelta, err := req.Op.Deltas(req.Amount)
	if err != nil {
		return nil, err
	}

	var result *Result
	err = r.txm.Run(ctx, func(ctx context.Context) error {
		applied, err := r.apply(ctx, tx.Of(ctx, r.db), req, availableDelta, frozenDelta)
		if err != nil {
			return err
		}
		result = applied
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// apply 执行一次余额变更。
//
// 执行顺序是刻意安排的：
//
//		锁定账户（不存在则创建） → 查幂等键 → 写账本 → 改余额
//
//	  - 先锁账户：同一账户的并发请求在此串行化。这一步不可省略——
//	    没有它，「读余额 → 判断够不够 → 写回」之间就有窗口，
//	    两个并发扣款会各自读到同一个旧余额、各自认为够扣，最终丢失一次更新。
//	    注意：这里的锁必须是 SELECT ... FOR UPDATE。普通 SELECT 在
//	    REPEATABLE READ 下读的是事务快照，读不到别人刚提交的余额。
//	  - 先写账本再改余额：账本的 entry_no 唯一索引是幂等闸门，
//	    重复请求在这里就被拦下，不会走到改余额那一步。
//	  - 余额不足的判定必须在这里做，因为只有这里持有行锁。
//
// 本方法不开事务也不提交——事务边界属于调用方（见 Apply）。
func (r *Repository) apply(
	ctx context.Context,
	exec tx.Executor,
	req Request,
	availableDelta, frozenDelta money.Amount,
) (*Result, error) {
	account, err := lockAccount(ctx, exec, req.UserID)
	if err != nil {
		return nil, err
	}

	entryNo := req.EntryNoValue()
	existing, err := findEntryByNo(ctx, exec, entryNo)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		if !sameRequest(existing, req, availableDelta, frozenDelta) {
			return nil, fmt.Errorf("%w: entry_no=%s", ErrIdempotencyKeyReused, entryNo)
		}
		// 重放：不再产生任何余额变动，原样返回首次执行的结果。
		//
		// 这里刻意不提交——即使外层是独立调用，提交也由 tx.Manager 负责。
		// 曾经这里有一句 tx.Commit()，注释是「提交只是为了释放行锁」：
		// 独立调用时无害，被跨域事务包裹时却会替调用方提交，
		// 外层后续失败再也回滚不了。见 TestDuplicateInsideTransactionDoesNotCommitEarly。
		return &Result{Account: account, Entry: existing, Duplicated: true}, nil
	}

	nextAvailable, err := account.AvailableBalance.Add(availableDelta)
	if err != nil {
		return nil, fmt.Errorf("计算可用余额失败: %w", err)
	}
	nextFrozen, err := account.FrozenBalance.Add(frozenDelta)
	if err != nil {
		return nil, fmt.Errorf("计算冻结余额失败: %w", err)
	}
	// 余额不可能为负。这里返回哨兵错误，由 Service 映射为业务错误码——
	// 仓储层不该依赖 HTTP 错误码，而判定又只能发生在持有行锁时。
	if nextAvailable.IsNegative() {
		return nil, ErrInsufficientAvailable
	}
	if nextFrozen.IsNegative() {
		return nil, ErrInsufficientFrozen
	}

	entry := &Entry{
		AccountID:      account.ID,
		UserID:         req.UserID,
		EntryNo:        entryNo,
		Op:             req.Op,
		BizType:        req.BizType,
		BizNo:          req.BizNo,
		AvailableDelta: availableDelta,
		FrozenDelta:    frozenDelta,
		AvailableAfter: nextAvailable,
		FrozenAfter:    nextFrozen,
		Remark:         req.Remark,
	}
	if err := insertEntry(ctx, exec, entry); err != nil {
		return nil, err
	}
	if err := updateBalance(ctx, exec, account.ID, nextAvailable, nextFrozen); err != nil {
		return nil, err
	}

	account.AvailableBalance = nextAvailable
	account.FrozenBalance = nextFrozen
	account.Version++
	return &Result{Account: account, Entry: entry}, nil
}

// lockAccount 锁定账户并返回其当前状态，账户不存在时先创建再锁定。
func lockAccount(ctx context.Context, exec tx.Executor, userID int64) (*Account, error) {
	account, err := selectAccountForUpdate(ctx, exec, userID)
	if err == nil {
		return account, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}

	// 账户按需创建：注册流程不必知道钱包的存在，历史用户缺失账户时也能自愈。
	//
	// 用 ON DUPLICATE KEY UPDATE 而不是 INSERT IGNORE：后者会把外键错误
	// 一并降级成警告，用户不存在时会静默跳过，问题被推迟到更难排查的地方。
	// 并发首次入账时只有一个事务真正插入，另一个走重复键分支后重新加锁读取。
	if _, err := exec.ExecContext(ctx,
		`INSERT INTO wallet_accounts (user_id, available_balance, frozen_balance)
		 VALUES (?, 0, 0)
		 ON DUPLICATE KEY UPDATE id = id`, userID); err != nil {
		return nil, fmt.Errorf("初始化钱包账户失败: %w", err)
	}
	return selectAccountForUpdate(ctx, exec, userID)
}

func selectAccountForUpdate(ctx context.Context, exec tx.Executor, userID int64) (*Account, error) {
	var account Account
	err := exec.QueryRowContext(ctx,
		`SELECT `+accountColumns+` FROM wallet_accounts WHERE user_id = ? FOR UPDATE`, userID).
		Scan(&account.ID, &account.UserID, &account.AvailableBalance, &account.FrozenBalance,
			&account.Version, &account.CreatedAt, &account.UpdatedAt)

	switch {
	case errors.Is(err, sql.ErrNoRows):
		// 原样返回，由调用方决定是否创建账户
		return nil, sql.ErrNoRows
	case err != nil:
		return nil, fmt.Errorf("锁定钱包账户失败: %w", err)
	}
	return &account, nil
}

// sameRequest 判断已入账的流水是否就是本次请求的首次执行结果。
//
// 幂等键相同但内容不同，说明调用方复用了单号（把两笔不同的订单当成同一笔），
// 必须报错而不是当成重放——静默返回旧结果会把这类 bug 藏到对账时才暴露。
func sameRequest(e *Entry, req Request, availableDelta, frozenDelta money.Amount) bool {
	return e.Op == req.Op &&
		e.BizType == req.BizType &&
		e.BizNo == req.BizNo &&
		e.AvailableDelta == availableDelta &&
		e.FrozenDelta == frozenDelta
}

func findEntryByNo(ctx context.Context, exec tx.Executor, entryNo string) (*Entry, error) {
	rows, err := exec.QueryContext(ctx,
		`SELECT `+ledgerColumns+` FROM wallet_ledger WHERE entry_no = ?`, entryNo)
	if err != nil {
		return nil, fmt.Errorf("查询账本流水失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	entries, err := scanEntries(rows)
	if err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		return nil, nil
	}
	return &entries[0], nil
}

func insertEntry(ctx context.Context, exec tx.Executor, e *Entry) error {
	result, err := exec.ExecContext(ctx,
		`INSERT INTO wallet_ledger
			(account_id, user_id, entry_no, op, biz_type, biz_no,
			 available_delta, frozen_delta, available_after, frozen_after, remark)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.AccountID, e.UserID, e.EntryNo, string(e.Op), e.BizType, e.BizNo,
		e.AvailableDelta, e.FrozenDelta, e.AvailableAfter, e.FrozenAfter, e.Remark)
	if err != nil {
		return fmt.Errorf("写入账本流水失败: %w", err)
	}

	id, err := result.LastInsertId()
	if err != nil {
		return fmt.Errorf("获取流水号失败: %w", err)
	}
	e.ID = id
	return nil
}

func updateBalance(
	ctx context.Context,
	exec tx.Executor,
	accountID int64,
	available, frozen money.Amount,
) error {
	_, err := exec.ExecContext(ctx,
		`UPDATE wallet_accounts
		 SET available_balance = ?, frozen_balance = ?, version = version + 1
		 WHERE id = ?`, available, frozen, accountID)
	if err != nil {
		return fmt.Errorf("更新钱包余额失败: %w", err)
	}
	return nil
}

// GetAccount 读取账户。只读，不创建账户——读接口不应产生写操作。
// 账户不存在时返回 (nil, nil)，由调用方决定如何呈现。
func (r *Repository) GetAccount(ctx context.Context, userID int64) (*Account, error) {
	var account Account
	err := tx.Of(ctx, r.db).QueryRowContext(ctx,
		`SELECT `+accountColumns+` FROM wallet_accounts WHERE user_id = ?`, userID).
		Scan(&account.ID, &account.UserID, &account.AvailableBalance, &account.FrozenBalance,
			&account.Version, &account.CreatedAt, &account.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("查询钱包账户失败: %w", err)
	}
	return &account, nil
}

// ListEntries 按流水号倒序分页读取账本。
//
// 多取一条来判断是否还有下一页，避免为了算总数再查一次。
func (r *Repository) ListEntries(ctx context.Context, userID int64, filter EntryFilter) (*EntryPage, error) {
	limit := filter.Limit
	if limit <= 0 {
		limit = DefaultEntryLimit
	}
	if limit > MaxEntryLimit {
		limit = MaxEntryLimit
	}

	query := strings.Builder{}
	query.WriteString(`SELECT ` + ledgerColumns + ` FROM wallet_ledger WHERE user_id = ?`)
	args := []any{userID}

	if filter.Cursor > 0 {
		query.WriteString(` AND id < ?`)
		args = append(args, filter.Cursor)
	}
	query.WriteString(` ORDER BY id DESC LIMIT ?`)
	args = append(args, limit+1)

	rows, err := tx.Of(ctx, r.db).QueryContext(ctx, query.String(), args...)
	if err != nil {
		return nil, fmt.Errorf("查询账本流水失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	entries, err := scanEntries(rows)
	if err != nil {
		return nil, err
	}

	page := &EntryPage{Items: entries}
	if len(entries) > limit {
		page.Items = entries[:limit]
		page.NextCursor = page.Items[len(page.Items)-1].ID
	}
	if page.Items == nil {
		// 保证序列化成 []，前端 .map() 不会因为 null 崩掉
		page.Items = []Entry{}
	}
	return page, nil
}

// Reconcile 对账：比对账户表上的余额与账本流水的累加值。
//
// 这是「余额与账本始终一致」这条不变量的机器可验证形式，
// 也是排查资金问题的第一手段：不一致时先看这里的差额。
func (r *Repository) Reconcile(ctx context.Context, userID int64) (*ReconcileResult, error) {
	var (
		result ReconcileResult
		// SUM 返回 DECIMAL，显式 CAST 成有符号整数才能直接扫进 int64
		ledgerAvailable int64
		ledgerFrozen    int64
	)
	result.UserID = userID

	err := r.db.QueryRowContext(ctx,
		`SELECT a.id, a.available_balance, a.frozen_balance, a.version,
		        CAST(COALESCE(SUM(l.available_delta), 0) AS SIGNED),
		        CAST(COALESCE(SUM(l.frozen_delta), 0) AS SIGNED),
		        COUNT(l.id)
		 FROM wallet_accounts a
		 LEFT JOIN wallet_ledger l ON l.account_id = a.id
		 WHERE a.user_id = ?
		 GROUP BY a.id, a.available_balance, a.frozen_balance, a.version`, userID).
		Scan(&result.UserID, &result.AccountAvailable, &result.AccountFrozen, &result.AccountVersion,
			&ledgerAvailable, &ledgerFrozen, &result.EntryCount)
	if errors.Is(err, sql.ErrNoRows) {
		// 没有账户：两侧都是 0，一致
		return &result, nil
	}
	if err != nil {
		return nil, fmt.Errorf("对账查询失败: %w", err)
	}

	result.AccountExists = true
	result.LedgerAvailable = money.Amount(ledgerAvailable)
	result.LedgerFrozen = money.Amount(ledgerFrozen)
	return &result, nil
}

func scanEntries(rows *sql.Rows) ([]Entry, error) {
	var entries []Entry

	for rows.Next() {
		var (
			e  Entry
			op string
		)
		err := rows.Scan(
			&e.ID, &e.AccountID, &e.UserID, &e.EntryNo, &op, &e.BizType, &e.BizNo,
			&e.AvailableDelta, &e.FrozenDelta, &e.AvailableAfter, &e.FrozenAfter,
			&e.Remark, &e.CreatedAt,
		)
		if err != nil {
			return nil, fmt.Errorf("解析账本流水失败: %w", err)
		}
		e.Op = Op(op)
		entries = append(entries, e)
	}
	return entries, rows.Err()
}
