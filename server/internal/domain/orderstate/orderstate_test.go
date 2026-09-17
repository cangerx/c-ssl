package orderstate

import "testing"

func TestCanTransitionTo(t *testing.T) {
	tests := []struct {
		name string
		from State
		to   State
		want bool
	}{
		{"待支付到已支付", PendingPayment, Paid, true},
		{"待支付可取消", PendingPayment, Cancelled, true},
		{"已支付到提交中", Paid, Submitting, true},
		{"提交中到待验证", Submitting, WaitingDcv, true},
		{"待验证到签发中", WaitingDcv, Issuing, true},
		{"签发中到已签发", Issuing, Issued, true},

		// 关键约束：不允许从已支付直接跳到已签发，否则等于绕过 DCV 验证
		{"已支付不能直接到已签发", Paid, Issued, false},
		{"待验证不能直接到已签发", WaitingDcv, Issued, false},

		// 状态不可回退
		{"已支付不能回到待支付", Paid, PendingPayment, false},
		{"待验证不能回到提交中", WaitingDcv, Submitting, false},

		// 终态
		{"已签发不能到任何状态", Issued, Cancelled, false},
		{"已取消不能到任何状态", Cancelled, Failed, false},
		{"失败可转取消", Failed, Cancelled, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.from.CanTransitionTo(tt.to); got != tt.want {
				t.Errorf("%s.CanTransitionTo(%s) = %v，期望 %v", tt.from, tt.to, got, tt.want)
			}
		})
	}
}

func TestTerminal(t *testing.T) {
	terminal := []State{Issued, Cancelled}
	nonTerminal := []State{PendingPayment, Paid, Submitting, WaitingDcv, Issuing, Failed}

	for _, s := range terminal {
		if !s.IsTerminal() {
			t.Errorf("%s 应为终态", s)
		}
	}
	for _, s := range nonTerminal {
		if s.IsTerminal() {
			t.Errorf("%s 不应为终态", s)
		}
	}
}

func TestTransition(t *testing.T) {
	got, err := PendingPayment.Transition(Paid)
	if err != nil {
		t.Fatalf("合法迁移不应报错: %v", err)
	}
	if got != Paid {
		t.Errorf("迁移结果 = %s，期望 %s", got, Paid)
	}

	if _, err := Paid.Transition(Issued); err == nil {
		t.Error("非法迁移应报错")
	}
	if _, err := State("bogus").Transition(Paid); err == nil {
		t.Error("未知源状态应报错")
	}
	if _, err := Paid.Transition(State("bogus")); err == nil {
		t.Error("未知目标状态应报错")
	}
}

func TestValid(t *testing.T) {
	for _, s := range All() {
		if !s.Valid() {
			t.Errorf("%s 应被识别为合法状态", s)
		}
	}
	if State("").Valid() {
		t.Error("空状态不应合法")
	}
	if State("done").Valid() {
		t.Error("未定义状态不应合法")
	}
}

func TestAllStatesAreReachable(t *testing.T) {
	// 从待支付出发应能到达全部状态（含 Failed：待支付可以直接失败），
	// 确保状态机没有孤立节点
	reached := map[State]bool{PendingPayment: true}
	queue := []State{PendingPayment}

	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, next := range cur.Next() {
			if !reached[next] {
				reached[next] = true
				queue = append(queue, next)
			}
		}
	}

	for _, s := range All() {
		if !reached[s] {
			t.Errorf("状态 %s 从待支付不可达", s)
		}
	}
}

// TestPathFillsSkippedStates 验证跨级推进时会补齐中间的每一步。
//
// 上游推送事件时不保证把每个中间状态都推一遍：本地还停在 waiting_dcv，
// 下一条事件直接就是「已签发」是正常情况。若只做单步校验，
// waiting_dcv → issued 会被判为非法，事件被丢掉，订单永远卡在等待验证。
func TestPathFillsSkippedStates(t *testing.T) {
	cases := []struct {
		from   State
		to     State
		expect []State
	}{
		{
			// 上游一次跳到签发：必须补上 issuing
			WaitingDcv, Issued,
			[]State{Issuing, Issued},
		},
		{
			// 本地刚建单，上游直接说等待验证：补上 paid、submitting
			PendingPayment, WaitingDcv,
			[]State{Paid, Submitting, WaitingDcv},
		},
		{
			// 单步
			PendingPayment, Paid,
			[]State{Paid},
		},
		{
			// 待支付直接失败
			PendingPayment, Failed,
			[]State{Failed},
		},
	}

	for _, tc := range cases {
		got := tc.from.Path(tc.to)
		if len(got) != len(tc.expect) {
			t.Errorf("%s → %s：期望路径 %v，实际 %v", tc.from, tc.to, tc.expect, got)
			continue
		}
		for i := range got {
			if got[i] != tc.expect[i] {
				t.Errorf("%s → %s：期望路径 %v，实际 %v", tc.from, tc.to, tc.expect, got)
				break
			}
		}
	}
}

// TestPathReturnsEmptyWhenAlreadyThere 验证已在目标状态时返回空切片而不是 nil。
//
// 空切片与 nil 必须可区分：前者表示「已经在目标状态，无事可做」，
// 后者表示「不可达」。调用方若把两者当成同一件事，
// 就会把乱序的旧事件也当成幂等重复而放行。
func TestPathReturnsEmptyWhenAlreadyThere(t *testing.T) {
	for _, s := range All() {
		got := s.Path(s)
		if got == nil {
			t.Errorf("%s → %s：应返回空切片而不是 nil", s, s)
			continue
		}
		if len(got) != 0 {
			t.Errorf("%s → %s：应返回空切片，实际 %v", s, s, got)
		}
	}
}

// TestPathReturnsNilForRegressions 验证回退路径不可达。
//
// 这是乱序投递的保护：上游不保证投递顺序，一条迟到的「等待验证」
// 事件可能在上报「已签发」之后才到达。此时必须得到 nil 并跳过，
// 而不是把已经签发的订单改回等待验证。
func TestPathReturnsNilForRegressions(t *testing.T) {
	cases := []struct {
		from State
		to   State
		why  string
	}{
		{Issued, WaitingDcv, "已签发的订单不能被旧事件改回等待验证"},
		{Issued, Issuing, "已签发的订单不能回到签发中"},
		{Issued, Failed, "已签发是终态，不能因为一条迟到的事件变成失败"},
		{Issued, Cancelled, "已签发不能取消"},
		{Issued, PendingPayment, "任何状态都不能回到待支付"},
		{Cancelled, Issuing, "已取消不能回到签发中"},
		{Cancelled, Issued, "已取消不能变成已签发"},
		{WaitingDcv, PendingPayment, "不能回退到待支付"},
		{WaitingDcv, Paid, "不能回退到已支付"},
		{Issuing, WaitingDcv, "签发中不能回退到等待验证"},
	}

	for _, tc := range cases {
		if got := tc.from.Path(tc.to); got != nil {
			t.Errorf("%s → %s 应当不可达（%s），实际路径 %v", tc.from, tc.to, tc.why, got)
		}
		if tc.from.Reachable(tc.to) {
			t.Errorf("%s → %s 的 Reachable 应为 false", tc.from, tc.to)
		}
	}
}

func TestPathRejectsUnknownStates(t *testing.T) {
	if got := State("nope").Path(Issued); got != nil {
		t.Errorf("未知起点应返回 nil，实际 %v", got)
	}
	if got := Issued.Path(State("nope")); got != nil {
		t.Errorf("未知终点应返回 nil，实际 %v", got)
	}
}

// TestPathIsAlwaysWalkable 验证 Path 返回的每一步都是合法的单步迁移。
//
// 路径本身合法不代表每一步都合法——中间多一步或少一步都能让
// 调用方在写入数据库时撞上状态机校验。
func TestPathIsAlwaysWalkable(t *testing.T) {
	for _, from := range All() {
		for _, to := range All() {
			path := from.Path(to)
			if path == nil {
				continue
			}
			cur := from
			for _, step := range path {
				if !cur.CanTransitionTo(step) {
					t.Errorf("%s → %s 的路径 %v 中 %s → %s 不是合法单步迁移",
						from, to, path, cur, step)
					break
				}
				cur = step
			}
			if cur != to {
				t.Errorf("%s → %s 的路径 %v 终点是 %s", from, to, path, cur)
			}
		}
	}
}
