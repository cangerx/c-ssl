package wallet

import (
	"strings"
	"testing"

	"github.com/cangerx/c-ssl/server/internal/domain/errs"
	"github.com/cangerx/c-ssl/server/internal/domain/money"
)

// Deltas 是本域最核心的一处逻辑：余额怎么变完全由它决定。
// 逐操作断言，任何一条被改动都会立刻暴露。
func TestOpDeltas(t *testing.T) {
	const amount = money.Amount(1000)

	cases := []struct {
		op              Op
		wantAvailable   money.Amount
		wantFrozen      money.Amount
		wantTotalChange money.Amount
	}{
		// 充值、退款：可用增加，总额增加
		{OpRecharge, 1000, 0, 1000},
		{OpRefund, 1000, 0, 1000},
		// 直接扣款：可用减少，总额减少
		{OpConsume, -1000, 0, -1000},
		// 冻结与解冻：总额不变，只是可用与冻结之间挪动
		{OpFreeze, -1000, 1000, 0},
		{OpUnfreeze, 1000, -1000, 0},
		// 结算：只动冻结，总额减少
		{OpSettle, 0, -1000, -1000},
	}

	for _, c := range cases {
		t.Run(string(c.op), func(t *testing.T) {
			available, frozen, err := c.op.Deltas(amount)
			if err != nil {
				t.Fatalf("计算变动量失败: %v", err)
			}
			if available != c.wantAvailable {
				t.Errorf("可用余额变动应为 %d，实际 %d", c.wantAvailable, available)
			}
			if frozen != c.wantFrozen {
				t.Errorf("冻结余额变动应为 %d，实际 %d", c.wantFrozen, frozen)
			}
			if total := available + frozen; total != c.wantTotalChange {
				t.Errorf("总额变动应为 %d，实际 %d", c.wantTotalChange, total)
			}
		})
	}
}

// 冻结是最能说明「为什么必须分别记录两侧变动」的例子：
// 只记一个带正负的金额无法表达「可用减少、冻结增加、总额不变」。
func TestFreezeDoesNotChangeTotal(t *testing.T) {
	account := Account{AvailableBalance: 5000, FrozenBalance: 0}

	available, frozen, err := OpFreeze.Deltas(3000)
	if err != nil {
		t.Fatalf("计算变动量失败: %v", err)
	}

	before := account.TotalBalance()
	account.AvailableBalance += available
	account.FrozenBalance += frozen
	if account.TotalBalance() != before {
		t.Errorf("冻结后总额应不变，前 %d 后 %d", before, account.TotalBalance())
	}
	if account.AvailableBalance != 2000 || account.FrozenBalance != 3000 {
		t.Errorf("冻结后余额不正确：可用 %d，冻结 %d", account.AvailableBalance, account.FrozenBalance)
	}
}

func TestOpDeltasRejectsUnknownOp(t *testing.T) {
	if _, _, err := Op("transfer").Deltas(100); err == nil {
		t.Error("未定义的操作类型应返回错误")
	}
	if Op("transfer").Valid() {
		t.Error("未定义的操作类型不应判定为合法")
	}
}

func TestAllOpsAreValid(t *testing.T) {
	for _, op := range AllOps() {
		if !op.Valid() {
			t.Errorf("%s 应判定为合法操作", op)
		}
		if _, _, err := op.Deltas(100); err != nil {
			t.Errorf("%s 应能计算变动量: %v", op, err)
		}
	}
}

// ── 幂等键 ────────────────────────────────────────

func TestBuildEntryNo(t *testing.T) {
	got := BuildEntryNo("cert_order", "O20260916000001", OpFreeze)
	want := "cert_order:O20260916000001:freeze"
	if got != want {
		t.Errorf("幂等键应为 %q，实际 %q", want, got)
	}
}

// 同一单号的冻结与结算必须得到不同的幂等键，
// 否则结算会被当成冻结的重放而静默跳过。
func TestEntryNoDiffersPerOp(t *testing.T) {
	freeze := BuildEntryNo("cert_order", "O1", OpFreeze)
	settle := BuildEntryNo("cert_order", "O1", OpSettle)
	if freeze == settle {
		t.Fatal("同一单号的冻结与结算不应共用幂等键")
	}
}

func TestEntryNoOverride(t *testing.T) {
	req := Request{BizType: "cert_order", BizNo: "O1", Op: OpRefund, EntryNo: "manual:R1"}
	if req.EntryNoValue() != "manual:R1" {
		t.Errorf("显式指定的幂等键应优先生效，实际 %q", req.EntryNoValue())
	}

	req.EntryNo = ""
	if req.EntryNoValue() != "cert_order:O1:refund" {
		t.Errorf("未指定时应由业务字段推导，实际 %q", req.EntryNoValue())
	}
}

// ── 入参校验 ──────────────────────────────────────

func validRequest() Request {
	return Request{
		UserID:  1,
		Op:      OpRecharge,
		Amount:  1000,
		BizType: "recharge_order",
		BizNo:   "R20260916000001",
	}
}

func TestRequestValidateAcceptsValidInput(t *testing.T) {
	if err := validRequest().Validate(); err != nil {
		t.Fatalf("合法请求不应报错: %v", err)
	}
}

func TestRequestValidateRejectsBadInput(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Request)
		field  string
	}{
		{"用户未登录", func(r *Request) { r.UserID = 0 }, ""},
		{"操作类型未知", func(r *Request) { r.Op = "transfer" }, ""},
		{"金额为零", func(r *Request) { r.Amount = 0 }, "amount"},
		{"金额为负", func(r *Request) { r.Amount = -1 }, "amount"},
		{"业务类型为空", func(r *Request) { r.BizType = "" }, "bizType"},
		{"业务类型含冒号", func(r *Request) { r.BizType = "cert:order" }, "bizType"},
		{"业务类型含空格", func(r *Request) { r.BizType = "cert order" }, "bizType"},
		{"业务类型过长", func(r *Request) { r.BizType = strings.Repeat("a", bizTypeMaxLen+1) }, "bizType"},
		{"业务单号为空", func(r *Request) { r.BizNo = "" }, "bizNo"},
		{"业务单号含斜杠", func(r *Request) { r.BizNo = "O/1" }, "bizNo"},
		{"业务单号过长", func(r *Request) { r.BizNo = strings.Repeat("a", bizNoMaxLen+1) }, "bizNo"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := validRequest()
			c.mutate(&req)

			err := req.Validate()
			if err == nil {
				t.Fatal("非法请求应被拒绝")
			}
			if err.Code != errs.CodeInvalidParam && err.Code != errs.CodeUnauthorized {
				t.Errorf("错误码应为 1000 或 1001，实际 %d", err.Code)
			}
			if c.field != "" {
				if _, ok := err.Fields[c.field]; !ok {
					t.Errorf("应带有字段级提示 %q，实际 %v", c.field, err.Fields)
				}
			}
		})
	}
}

// 备注按字符数而非字节数限制，否则中文备注会被过早截断。
func TestRequestValidateRemarkByRune(t *testing.T) {
	req := validRequest()

	// 255 个汉字是 765 字节，按字节判定会误报
	req.Remark = strings.Repeat("好", remarkMaxLen)
	if err := req.Validate(); err != nil {
		t.Errorf("恰好 %d 个汉字的备注应通过: %v", remarkMaxLen, err)
	}

	req.Remark = strings.Repeat("好", remarkMaxLen+1)
	if err := req.Validate(); err == nil {
		t.Errorf("超过 %d 个字符的备注应被拒绝", remarkMaxLen)
	}
}

// ── 对账结果 ──────────────────────────────────────

func TestReconcileResultConsistent(t *testing.T) {
	ok := ReconcileResult{
		AccountAvailable: 7000, AccountFrozen: 3000,
		LedgerAvailable: 7000, LedgerFrozen: 3000,
	}
	if !ok.Consistent() {
		t.Error("两侧相等时应判定为一致")
	}
	if a, f := ok.Difference(); a != 0 || f != 0 {
		t.Errorf("一致时差额应为 0，实际 可用 %d 冻结 %d", a, f)
	}

	bad := ReconcileResult{
		AccountAvailable: 7000, AccountFrozen: 3000,
		LedgerAvailable: 6900, LedgerFrozen: 3000,
	}
	if bad.Consistent() {
		t.Error("两侧不等时应判定为不一致")
	}
	if a, _ := bad.Difference(); a != 100 {
		t.Errorf("可用余额差额应为 100，实际 %d", a)
	}
}

// 账户不存在时两侧都是 0，属于一致状态——新用户余额为 0 是正常的，不是异常。
func TestReconcileResultEmptyAccountIsConsistent(t *testing.T) {
	var r ReconcileResult
	if !r.Consistent() {
		t.Error("账户不存在时两侧均为 0，应判定为一致")
	}
}

func TestAccountTotalBalance(t *testing.T) {
	a := Account{AvailableBalance: 12345, FrozenBalance: 6789}
	if got := a.TotalBalance(); got != 19134 {
		t.Errorf("总余额应为 19134，实际 %d", got)
	}
}
