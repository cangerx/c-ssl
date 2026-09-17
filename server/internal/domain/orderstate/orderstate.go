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

// Path 返回从 s 迁移到 target 的一条合法路径，不含起点、含终点。
//
// 已经处于 target 时返回空切片；不存在合法路径时返回 nil。
//
// **为什么需要它，而不是逐级 Transition。** 上游推送事件时不保证把每一个
// 中间状态都推一遍：本地还停在 waiting_dcv，下一条事件直接就是「已签发」
// 是正常情况。此时若只做单步校验，waiting_dcv → issued 会被判为非法，
// 事件被丢掉，订单永远卡在等待验证。
//
// 用 BFS 而不是「按固定顺序依次尝试」：状态的合法后继不只有一条路
// （failed 可以转到 cancelled，pending_payment 可以直接转 failed），
// 写死顺序会在图变化时静默失效。
//
// 反向的用途同样重要：**不可达即视为乱序或非法**。
// issued 是终态，没有后继，所以「已签发之后又收到等待验证」这种
// 迟到的旧事件会得到 nil，调用方据此跳过而不是把状态改回去。
func (s State) Path(target State) []State {
	if !s.Valid() || !target.Valid() {
		return nil
	}
	if s == target {
		return []State{}
	}

	// prev 记录每个状态是从哪个状态走过来的，用于回溯路径。
	prev := map[State]State{s: ""}
	queue := []State{s}

	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, next := range transitions[cur] {
			if _, seen := prev[next]; seen {
				continue
			}
			prev[next] = cur
			if next == target {
				return buildPath(prev, s, target)
			}
			queue = append(queue, next)
		}
	}
	return nil
}

// buildPath 从 prev 回溯出 s → target 的路径，不含起点、含终点。
func buildPath(prev map[State]State, from, to State) []State {
	var reversed []State
	for cur := to; cur != from; cur = prev[cur] {
		reversed = append(reversed, cur)
	}
	// 反转
	for i, j := 0, len(reversed)-1; i < j; i, j = i+1, j-1 {
		reversed[i], reversed[j] = reversed[j], reversed[i]
	}
	return reversed
}

// Reachable 判断是否存在从 s 到 target 的合法迁移路径。
func (s State) Reachable(target State) bool {
	return s == target || s.Path(target) != nil
}
