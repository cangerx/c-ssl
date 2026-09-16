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
	// 从待支付出发应能到达除 Failed 外的全部状态，确保状态机没有孤立节点
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
		if s == Failed {
			continue
		}
		if !reached[s] {
			t.Errorf("状态 %s 从待支付不可达", s)
		}
	}
}
