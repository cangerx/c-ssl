package recharge

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/cangerx/c-ssl/server/internal/domain/errs"
	"github.com/cangerx/c-ssl/server/internal/domain/money"
	"github.com/cangerx/c-ssl/server/internal/upstream/payment"
)

// Status 是充值订单状态。
//
// 只与 wallet 的 Op 类似，是本域自己的概念，不放进共享内核：
// 充值单与证书订单是两套完全不同的状态机，共用一个类型只会让
// 「这个状态在这个域里意味着什么」变得含糊。
type Status string

const (
	// StatusPending 已创建，等待用户支付。
	StatusPending Status = "pending"
	// StatusPaid 支付成功，钱包已加款。
	StatusPaid Status = "paid"
	// StatusFailed 渠道明确返回支付失败。
	StatusFailed Status = "failed"
	// StatusClosed 超时未支付，已关闭。
	StatusClosed Status = "closed"
)

// transitions 定义合法状态迁移。
//
// Paid / Failed / Closed 都是终态。用户重新发起支付会得到一张新订单，
// 而不是把旧订单改回 Pending——否则订单号与渠道流水就失去了一一对应，
// 对账时无法回答「这笔钱对应哪一次支付」。
var transitions = map[Status][]Status{
	StatusPending: {StatusPaid, StatusFailed, StatusClosed},
	StatusPaid:    {},
	StatusFailed:  {},
	StatusClosed:  {},
}

// AllStatuses 返回全部合法状态，用于校验和测试。
func AllStatuses() []Status {
	return []Status{StatusPending, StatusPaid, StatusFailed, StatusClosed}
}

// Valid 判断是否为已定义的状态。
func (s Status) Valid() bool {
	_, ok := transitions[s]
	return ok
}

// Terminal 判断是否为终态。
func (s Status) Terminal() bool {
	next, ok := transitions[s]
	return ok && len(next) == 0
}

// CanTransitionTo 判断能否迁移到目标状态。
func (s Status) CanTransitionTo(next Status) bool {
	for _, allowed := range transitions[s] {
		if allowed == next {
			return true
		}
	}
	return false
}

// Order 是一张充值订单。
//
// ChannelTradeNo 只记录成功那笔支付的交易号，不加唯一约束——
// 唯一索引建在 payment_transactions 的 (channel, channel_trade_no) 上。
// 原因见 000005 迁移的说明：同一张订单可能先有一次失败支付、再有一次成功支付。
type Order struct {
	ID             int64
	OrderNo        string
	UserID         int64
	Amount         money.Amount
	Status         Status
	Channel        string
	ChannelOrderNo string
	ChannelTradeNo string
	PayURL         string
	PaidAt         *time.Time
	ExpiresAt      time.Time
	Remark         string
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// 充值金额与分页的硬约束。
//
// 上下限写死在代码里而不是配置：它们是资金安全的护栏，
// 不该因为一次配置误改就让「0.01 元充值 100 万次」或「一次充 1 亿元」成为可能。
// 将来要做成运营可调时，也必须带审批与审计（见 docs/06 第 8.4 节的人工调账流程）。
const (
	// MinAmount 是最小充值金额，100 分（1 元）。
	MinAmount money.Amount = 100
	// MaxAmount 是单笔最大充值金额，10000000 分（10 万元）。
	MaxAmount money.Amount = 10_000_000

	// DefaultLimit 是订单列表的默认每页条数。
	DefaultLimit = 20
	// MaxLimit 是订单列表的每页条数上限。
	MaxLimit = 100

	// OrderTTL 是充值订单的有效期。
	//
	// 30 分钟足够完成一次支付，又不至于让待支付订单堆积。
	// 过期后的回调不会入账：渠道的支付结果不可信地晚到时，
	// 自动入账会把一笔已经对不上的钱塞进账本。
	OrderTTL = 30 * time.Minute
)

// bizTypeRechargeOrder 是充值加款写进账本时使用的业务类型。
//
// 取值必须匹配 wallet 的 bizType 字符集（字母数字下划线连字符），
// 否则加款会被参数校验拒绝。
const bizTypeRechargeOrder = "recharge_order"

// CreateInput 是创建充值订单的入参。
type CreateInput struct {
	UserID int64
	Amount money.Amount
	// Channel 留空时使用服务端配置的默认渠道。
	Channel string
}

// Validate 校验创建入参。
func (in CreateInput) Validate() *errs.Error {
	if in.UserID <= 0 {
		return errs.New(errs.CodeUnauthorized)
	}
	switch {
	case in.Amount < MinAmount:
		return errs.New(errs.CodeInvalidParam).
			WithField("amount", fmt.Sprintf("单笔充值不能少于 %d 分", MinAmount.Cents()))
	case in.Amount > MaxAmount:
		return errs.New(errs.CodeInvalidParam).
			WithField("amount", fmt.Sprintf("单笔充值不能超过 %d 分", MaxAmount.Cents()))
	}
	if len(in.Channel) > channelMaxLen {
		return errs.New(errs.CodeInvalidParam).WithField("channel", "渠道标识过长")
	}
	return nil
}

// channelMaxLen 与 recharge_orders.channel 的列宽一致。
const channelMaxLen = 16

// Filter 是订单列表的查询条件。
type Filter struct {
	// Cursor 为 0 表示从最新一条开始。
	Cursor int64
	Limit  int
}

// Normalize 把分页参数收敛到合法区间。
func (f Filter) Normalize() Filter {
	if f.Limit <= 0 {
		f.Limit = DefaultLimit
	}
	if f.Limit > MaxLimit {
		f.Limit = MaxLimit
	}
	if f.Cursor < 0 {
		f.Cursor = 0
	}
	return f
}

// Page 是一页充值订单。
type Page struct {
	Items []Order
	// NextCursor 为 0 表示没有下一页。
	NextCursor int64
}

// Transaction 是一条支付渠道流水。
type Transaction struct {
	ID             int64
	Channel        string
	ChannelTradeNo string
	OrderNo        string
	UserID         int64
	Amount         money.Amount
	Status         payment.Status
	PaidAt         *time.Time
	RawPayload     string
	CreatedAt      time.Time
}

// 本域的哨兵错误。Service 会把它们映射成业务错误码。
var (
	// ErrOrderNotFound 表示充值单不存在。
	ErrOrderNotFound = errors.New("充值订单不存在")
	// ErrAmountMismatch 表示渠道回执金额与订单金额不一致。
	ErrAmountMismatch = errors.New("回调金额与订单金额不一致")
	// ErrOrderNotPayable 表示订单当前状态不接受这次回调。
	ErrOrderNotPayable = errors.New("订单当前状态不接受该回调")
	// ErrOrderExpired 表示订单已过期。
	ErrOrderExpired = errors.New("充值订单已过期")
	// ErrTransactionReused 表示同一个渠道交易号被用在内容不同的请求上。
	ErrTransactionReused = errors.New("渠道交易号已被内容不同的请求使用")
)

// maxRawPayload 是落库的渠道原始报文长度上限。
//
// 截断而不是拒绝：原始报文只用于排查与争议，缺一截不影响资金正确性，
// 但让一个异常大的报文把请求打失败，反而是可用性问题。
const maxRawPayload = 8 * 1024

// truncatePayload 按字节截断原始报文。
//
// 按字节而不是按 rune：这里只是留档，截断处出现半个 UTF-8 字符不影响用途，
// 而按 rune 截断需要在每次回调上多遍历一遍全部字节。
func truncatePayload(raw []byte) string {
	if len(raw) <= maxRawPayload {
		return string(raw)
	}
	return strings.ToValidUTF8(string(raw[:maxRawPayload]), "")
}
