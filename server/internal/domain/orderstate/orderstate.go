// Package orderstate 定义证书订单状态机。
//
// 状态取值与 docs/06 第 4.4 节一致。所有状态变更必须经过 CanTransitionTo 校验，
// 不允许在业务代码里直接赋值。
package orderstate

import "fmt"

// State 是订单状态。
type State string

const (
	// PendingPayment 已创建订单，等待支付或扣款。
	PendingPayment State = "pending_payment"
	// Paid 已支付，尚未提交上游。
	Paid State = "paid"
	// Submitting 正在向上游提交下单请求。
	Submitting State = "submitting"
	// WaitingDcv 上游已受理，等待域名验证。
	WaitingDcv State = "waiting_dcv"
	// Issuing 域名已验证，证书签发中。
	Issuing State = "issuing"
	// Issued 证书已签发，可下载。
	Issued State = "issued"
	// Cancelled 已取消。
	Cancelled State = "cancelled"
	// Failed 失败，需人工介入或补偿。
	Failed State = "failed"
)

// transitions 定义合法状态迁移。
//
// 注意几个刻意的约束：
//   - Issued 是终态，不能回退；重签是产生新的订单记录，而不是改这条。
//   - Cancelled 与 Failed 是终态。
//   - 只允许 WaitingDcv → Issuing → Issued 这一条签发路径，
//     不允许从 Paid 直接跳到 Issued，避免绕过 DCV 验证。
var transitions = map[State][]State{
	PendingPayment: {Paid, Cancelled, Failed},
	Paid:           {Submitting, Cancelled, Failed},
	Submitting:     {WaitingDcv, Cancelled, Failed},
	WaitingDcv:     {Issuing, Cancelled, Failed},
	Issuing:        {Issued, Failed},
	Issued:         {},
	Cancelled:      {},
	Failed:         {Cancelled},
}

// All 返回全部合法状态，用于校验和测试。
func All() []State {
	return []State{
		PendingPayment, Paid, Submitting, WaitingDcv,
		Issuing, Issued, Cancelled, Failed,
	}
}

// Valid 判断是否为已定义的状态。
func (s State) Valid() bool {
	_, ok := transitions[s]
	return ok
}

// IsTerminal 判断是否为终态。
func (s State) IsTerminal() bool {
	next, ok := transitions[s]
	return ok && len(next) == 0
}

// CanTransitionTo 判断能否迁移到 next。
func (s State) CanTransitionTo(next State) bool {
	for _, allowed := range transitions[s] {
		if allowed == next {
			return true
		}
	}
	return false
}

// Next 返回当前状态的全部合法后继。
func (s State) Next() []State {
	out := make([]State, len(transitions[s]))
	copy(out, transitions[s])
	return out
}

// Transition 执行状态迁移，非法迁移返回错误。
func (s State) Transition(next State) (State, error) {
	if !s.Valid() {
		return s, fmt.Errorf("未知的订单状态: %q", s)
	}
	if !next.Valid() {
		return s, fmt.Errorf("未知的目标状态: %q", next)
	}
	if !s.CanTransitionTo(next) {
		return s, fmt.Errorf("订单状态不允许从 %s 迁移到 %s", s, next)
	}
	return next, nil
}
