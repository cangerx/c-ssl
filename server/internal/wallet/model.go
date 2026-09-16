// Package wallet 管理用户钱包账户与不可变账本。
//
// 资金安全的两条底线，本包所有代码都围绕它们组织：
//
//  1. 余额的任何变化都必须在同一事务内留下一条账本流水。
//     不允许出现「改了余额但没有流水」或「有流水但余额没动」的中间态。
//  2. 账本只追加，永不修改。Repository 刻意不提供任何更新或删除账本的方法——
//     历史一旦可改，对账就失去意义。
//
// 金额一律用 money.Amount（int64，单位分），全程不出现 float。
package wallet

import (
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/cangerx/c-ssl/server/internal/domain/errs"
	"github.com/cangerx/c-ssl/server/internal/domain/money"
)

// Op 是账本操作类型。
type Op string

const (
	// OpRecharge 充值入账：可用余额增加。
	OpRecharge Op = "recharge"
	// OpConsume 直接扣款：可用余额减少。用于不需要冻结的即时扣款。
	OpConsume Op = "consume"
	// OpFreeze 冻结：可用余额转入冻结余额，总额不变。
	// 下单时先冻结，避免用户在下单与支付之间把余额花掉。
	OpFreeze Op = "freeze"
	// OpSettle 结算：冻结余额真正扣除。只减少冻结余额。
	OpSettle Op = "settle"
	// OpUnfreeze 解冻：冻结余额退回可用余额。订单取消、失败时使用。
	OpUnfreeze Op = "unfreeze"
	// OpRefund 退款：可用余额增加。与充值的区别在于业务语义，
	// 对账与报表需要区分「用户自己充的」和「平台退回来的」。
	OpRefund Op = "refund"
)

var opDeltas = map[Op]func(money.Amount) (money.Amount, money.Amount){
	OpRecharge: func(a money.Amount) (money.Amount, money.Amount) { return a, 0 },
	OpRefund:   func(a money.Amount) (money.Amount, money.Amount) { return a, 0 },
	OpConsume:  func(a money.Amount) (money.Amount, money.Amount) { return -a, 0 },
	OpFreeze:   func(a money.Amount) (money.Amount, money.Amount) { return -a, a },
	OpSettle:   func(a money.Amount) (money.Amount, money.Amount) { return 0, -a },
	OpUnfreeze: func(a money.Amount) (money.Amount, money.Amount) { return a, -a },
}

// AllOps 返回全部合法操作类型，供校验与测试使用。
func AllOps() []Op {
	return []Op{OpRecharge, OpConsume, OpFreeze, OpSettle, OpUnfreeze, OpRefund}
}

// Valid 判断是否为已定义的操作。
func (o Op) Valid() bool {
	_, ok := opDeltas[o]
	return ok
}

// Deltas 返回该操作对可用余额与冻结余额的变动量。
//
// 冻结是最能说明「为什么不能只记一个带正负的 amount」的例子：
// 可用 -100 分、冻结 +100 分，总额不变，单一字段无法表达方向。
func (o Op) Deltas(amount money.Amount) (available, frozen money.Amount, err error) {
	fn, ok := opDeltas[o]
	if !ok {
		return 0, 0, fmt.Errorf("未知的账本操作类型: %q", o)
	}
	available, frozen = fn(amount)
	return available, frozen, nil
}

// ── 实体 ──────────────────────────────────────────

// Account 是钱包账户。可用余额与冻结余额之和即用户总资产。
type Account struct {
	ID               int64
	UserID           int64
	AvailableBalance money.Amount
	FrozenBalance    money.Amount
	// Version 是余额变更次数，每次写入 +1。它不参与并发控制
	// （并发由 SELECT ... FOR UPDATE 的行锁串行化），仅用于观测：
	// 并发测试里可以直接断言「N 次成功扣款后 version 恰好增加了 N」。
	Version   int64
	CreatedAt time.Time
	UpdatedAt time.Time
}

// TotalBalance 返回可用与冻结之和。
func (a Account) TotalBalance() money.Amount {
	return a.AvailableBalance + a.FrozenBalance
}

// Entry 是一条账本流水。写入后不可修改。
type Entry struct {
	ID        int64
	AccountID int64
	UserID    int64
	// EntryNo 是幂等键，全局唯一。重复请求由它拦下。
	EntryNo string
	Op      Op
	BizType string
	BizNo   string
	// AvailableDelta 与 FrozenDelta 是本次变动量，可为负。
	AvailableDelta money.Amount
	FrozenDelta    money.Amount
	// AvailableAfter 与 FrozenAfter 是记账后的余额快照，
	// 让任何一条流水都能独立还原当时的账户状态，不必从头累加。
	AvailableAfter money.Amount
	FrozenAfter    money.Amount
	Remark         string
	CreatedAt      time.Time
}

// ── 入参与出参 ────────────────────────────────────

// Request 描述一次余额变更请求。
type Request struct {
	UserID  int64
	Op      Op
	Amount  money.Amount
	BizType string
	BizNo   string
	// EntryNo 是幂等键。留空时由 BizType + BizNo + Op 推导，
	// 适用于「同一业务单号同一操作只应发生一次」的绝大多数场景。
	// 若同一单号确实需要多次同类型操作（例如分次退款），必须显式指定。
	EntryNo string
	Remark  string
}

// EntryNo 返回最终使用的幂等键。
func (r Request) EntryNoValue() string {
	if r.EntryNo != "" {
		return r.EntryNo
	}
	return BuildEntryNo(r.BizType, r.BizNo, r.Op)
}

// Result 是一次余额变更的结果。
type Result struct {
	Account *Account
	Entry   *Entry
	// Duplicated 为 true 表示该幂等键此前已经入账，本次没有产生任何余额变动，
	// Account 与 Entry 返回的是首次入账的结果。调用方据此区分「新入账」与「重放」。
	Duplicated bool
}

// EntryFilter 是账本分页查询条件。
type EntryFilter struct {
	// Cursor 为上一页最后一条的流水号，0 表示从头开始。
	// 用流水号而非 offset：账本持续追加，offset 分页会漏记录或重复。
	Cursor int64
	Limit  int
}

// EntryPage 是一页账本流水。
type EntryPage struct {
	Items []Entry
	// NextCursor 为 0 表示没有更多数据。
	NextCursor int64
}

// ReconcileResult 是一次对账结果。
type ReconcileResult struct {
	UserID int64
	// 账户表上记录的余额。
	AccountAvailable money.Amount
	AccountFrozen    money.Amount
	// 账本流水累加出来的余额。
	LedgerAvailable money.Amount
	LedgerFrozen    money.Amount
	EntryCount      int64
	AccountVersion  int64
	// AccountExists 为 false 表示该用户还没有钱包账户，
	// 此时两侧都应为 0，仍算一致。
	AccountExists bool
}

// Consistent 判断账户余额与账本累加值是否一致。
func (r ReconcileResult) Consistent() bool {
	return r.AccountAvailable == r.LedgerAvailable && r.AccountFrozen == r.LedgerFrozen
}

// Difference 返回账户余额与账本累加值的差额，不一致时用于定位问题。
func (r ReconcileResult) Difference() (available, frozen money.Amount) {
	return r.AccountAvailable - r.LedgerAvailable, r.AccountFrozen - r.LedgerFrozen
}

// ── 常量与校验 ────────────────────────────────────

const (
	// DefaultEntryLimit 是账本分页的默认条数。
	DefaultEntryLimit = 20
	// MaxEntryLimit 是账本分页的最大条数，防止客户端拉全表。
	MaxEntryLimit = 100

	entryNoMaxLen = 96
	bizTypeMaxLen = 32
	bizNoMaxLen   = 64
	remarkMaxLen  = 255
)

// 业务类型与单号限制为安全字符集，并排除 ':'。
// 幂等键由它们拼成，含 ':' 会让键空间出现歧义（"a:b" + "c" 与 "a" + "b:c" 撞车）。
var (
	bizTypePattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
	bizNoPattern   = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)
	entryNoPattern = regexp.MustCompile(`^[A-Za-z0-9_.:-]+$`)
)

// BuildEntryNo 由业务类型、单号与操作拼出幂等键。
func BuildEntryNo(bizType, bizNo string, op Op) string {
	return bizType + ":" + bizNo + ":" + string(op)
}

// 由 Repository 在事务内判定余额是否足够，Service 负责映射为业务错误码。
// 判定必须发生在持有行锁之后，因此不能提到 Service 层做。
var (
	ErrInsufficientAvailable = errors.New("可用余额不足")
	ErrInsufficientFrozen    = errors.New("冻结余额不足")
)

// ErrIdempotencyKeyReused 表示同一个幂等键被一次内容不同的请求复用了。
//
// 这种情况必须报错而不是当成重放：静默返回上一次的结果会把调用方的 bug
// （例如把两笔不同的订单用了同一个单号）藏起来，等对账时才发现。
var ErrIdempotencyKeyReused = errors.New("幂等键已被内容不同的请求使用")

// Validate 校验一次余额变更请求。返回的错误可直接交给 httpx.Fail。
func (r Request) Validate() *errs.Error {
	if r.UserID <= 0 {
		return errs.New(errs.CodeUnauthorized)
	}
	if !r.Op.Valid() {
		return errs.Newf(errs.CodeInvalidParam, "未知的账本操作类型: %q", r.Op)
	}
	if !r.Amount.IsPositive() {
		return errs.New(errs.CodeInvalidParam).WithField("amount", "金额必须大于 0")
	}
	if err := validateBizType(r.BizType); err != nil {
		return err
	}
	if err := validateBizNo(r.BizNo); err != nil {
		return err
	}
	if err := validateEntryNo(r.EntryNoValue()); err != nil {
		return err
	}
	if len([]rune(r.Remark)) > remarkMaxLen {
		return errs.New(errs.CodeInvalidParam).
			WithField("remark", fmt.Sprintf("备注不能超过 %d 个字符", remarkMaxLen))
	}
	return nil
}

func validateBizType(v string) *errs.Error {
	if v == "" {
		return errs.New(errs.CodeInvalidParam).WithField("bizType", "业务类型不能为空")
	}
	if len(v) > bizTypeMaxLen || !bizTypePattern.MatchString(v) {
		return errs.New(errs.CodeInvalidParam).
			WithField("bizType", fmt.Sprintf("业务类型只能包含字母、数字、下划线和连字符，且不超过 %d 个字符", bizTypeMaxLen))
	}
	return nil
}

func validateBizNo(v string) *errs.Error {
	if v == "" {
		return errs.New(errs.CodeInvalidParam).WithField("bizNo", "业务单号不能为空")
	}
	if len(v) > bizNoMaxLen || !bizNoPattern.MatchString(v) {
		return errs.New(errs.CodeInvalidParam).
			WithField("bizNo", fmt.Sprintf("业务单号只能包含字母、数字、下划线、点和连字符，且不超过 %d 个字符", bizNoMaxLen))
	}
	return nil
}

func validateEntryNo(v string) *errs.Error {
	if len(v) > entryNoMaxLen || !entryNoPattern.MatchString(v) {
		return errs.New(errs.CodeInvalidParam).
			WithField("entryNo", fmt.Sprintf("幂等键只能包含字母、数字、下划线、点、连字符和冒号，且不超过 %d 个字符", entryNoMaxLen))
	}
	return nil
}
