package wallet

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/cangerx/c-ssl/server/internal/domain/errs"
	"github.com/cangerx/c-ssl/server/internal/domain/money"
)

// Service 是钱包域的业务入口。其他域只能通过它动余额，
// 不允许直接访问 wallet_accounts 或 wallet_ledger。
type Service struct {
	repo *Repository
}

// NewService 构造服务。
func NewService(repo *Repository) *Service { return &Service{repo: repo} }

// ChangeInput 是一次余额变更的业务入参。
//
// 五个公开方法共用这个结构，而不是各自定义一个——它们的差别只在操作语义上，
// 字段完全一致，分开定义只会制造重复。
type ChangeInput struct {
	UserID  int64
	Amount  money.Amount
	BizType string
	BizNo   string
	// EntryNo 留空时由 BizType + BizNo + 操作类型推导，见 Request.EntryNo。
	EntryNo string
	Remark  string
}

// Recharge 充值入账：可用余额增加。
func (s *Service) Recharge(ctx context.Context, in ChangeInput) (*Result, error) {
	return s.apply(ctx, OpRecharge, in)
}

// Consume 直接扣款：可用余额减少。用于不需要冻结的即时扣款。
func (s *Service) Consume(ctx context.Context, in ChangeInput) (*Result, error) {
	return s.apply(ctx, OpConsume, in)
}

// Freeze 冻结：可用余额转入冻结余额，总额不变。
//
// 下单时先冻结，用户就无法在下单与支付之间把同一笔钱花到别处。
func (s *Service) Freeze(ctx context.Context, in ChangeInput) (*Result, error) {
	return s.apply(ctx, OpFreeze, in)
}

// Settle 结算：把冻结余额真正扣掉。只减少冻结余额，可用余额不变。
func (s *Service) Settle(ctx context.Context, in ChangeInput) (*Result, error) {
	return s.apply(ctx, OpSettle, in)
}

// Unfreeze 解冻：冻结余额退回可用余额。订单取消或失败时使用。
func (s *Service) Unfreeze(ctx context.Context, in ChangeInput) (*Result, error) {
	return s.apply(ctx, OpUnfreeze, in)
}

// Refund 退款：可用余额增加。与充值的区别是业务语义，
// 对账与报表需要区分「用户自己充的」和「平台退回来的」。
func (s *Service) Refund(ctx context.Context, in ChangeInput) (*Result, error) {
	return s.apply(ctx, OpRefund, in)
}

// apply 是所有余额变更的唯一入口。
//
// 这五个方法刻意不做成「传入 Op 字符串」的通用接口：那样调用方可以传任意操作，
// 冻结、结算这类方向性极强的操作就容易传错，而错了要等对账时才发现。
func (s *Service) apply(ctx context.Context, op Op, in ChangeInput) (*Result, error) {
	req := Request{
		UserID:  in.UserID,
		Op:      op,
		Amount:  in.Amount,
		BizType: in.BizType,
		BizNo:   in.BizNo,
		EntryNo: in.EntryNo,
		Remark:  in.Remark,
	}
	if err := req.Validate(); err != nil {
		return nil, err
	}

	result, err := s.repo.Apply(ctx, req)
	if err != nil {
		return nil, mapError(err)
	}
	return result, nil
}

// mapError 把仓储层的哨兵错误翻译成业务错误码。
//
// 余额不足的判定发生在仓储层的事务内（那里才持有行锁），
// 但 HTTP 语义属于服务层，所以在此处映射。
func mapError(err error) error {
	switch {
	case errors.Is(err, ErrInsufficientAvailable), errors.Is(err, ErrInsufficientFrozen):
		// 哨兵错误的文案由本包控制，不含任何内部细节，可以安全返回给客户端
		return errs.Newf(errs.CodeInsufficientBalance, "%s", err.Error()).WithCause(err)
	case errors.Is(err, ErrIdempotencyKeyReused):
		return errs.New(errs.CodeInvalidParam).
			WithField("entryNo", "该幂等键已被内容不同的请求使用").
			WithCause(err)
	default:
		return err
	}
}

// Get 返回账户视图。账户不存在时返回零值账户而不是报错：
// 新用户还没产生过任何资金往来，余额为 0 是正确呈现，不是异常。
func (s *Service) Get(ctx context.Context, userID int64) (*Account, error) {
	account, err := s.repo.GetAccount(ctx, userID)
	if err != nil {
		return nil, err
	}
	if account != nil {
		return account, nil
	}
	return &Account{UserID: userID, CreatedAt: time.Time{}, UpdatedAt: time.Time{}}, nil
}

// ListEntries 分页读取账本流水。
func (s *Service) ListEntries(ctx context.Context, userID int64, filter EntryFilter) (*EntryPage, error) {
	return s.repo.ListEntries(ctx, userID, filter)
}

// Reconcile 对账：比对账户余额与账本累加值。
//
// 结果不一致意味着有资金数据被绕过账本改动了，属于必须立即介入的严重问题，
// 因此这里记一条错误日志，让告警能抓到。
func (s *Service) Reconcile(ctx context.Context, userID int64) (*ReconcileResult, error) {
	result, err := s.repo.Reconcile(ctx, userID)
	if err != nil {
		return nil, err
	}
	if !result.Consistent() {
		available, frozen := result.Difference()
		slog.ErrorContext(ctx, "钱包对账不一致",
			"user_id", result.UserID,
			"account_available", result.AccountAvailable,
			"ledger_available", result.LedgerAvailable,
			"account_frozen", result.AccountFrozen,
			"ledger_frozen", result.LedgerFrozen,
			"diff_available", available,
			"diff_frozen", frozen,
			"entry_count", result.EntryCount,
		)
	}
	return result, nil
}
