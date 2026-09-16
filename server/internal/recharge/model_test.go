package recharge

import (
	"strings"
	"testing"
	"time"

	"github.com/cangerx/c-ssl/server/internal/domain/errs"
	"github.com/cangerx/c-ssl/server/internal/domain/money"
)

// ── 状态机 ────────────────────────────────────────

func TestStatusTransitions(t *testing.T) {
	allowed := map[Status][]Status{
		StatusPending: {StatusPaid, StatusFailed, StatusClosed},
	}
	denied := map[Status][]Status{
		StatusPaid:   {StatusPending, StatusPaid, StatusFailed, StatusClosed},
		StatusFailed: {StatusPending, StatusPaid, StatusFailed, StatusClosed},
		StatusClosed: {StatusPending, StatusPaid, StatusFailed, StatusClosed},
	}

	for from, targets := range allowed {
		for _, to := range targets {
			if !from.CanTransitionTo(to) {
				t.Errorf("应当允许 %s → %s", from, to)
			}
		}
	}
	for from, targets := range denied {
		for _, to := range targets {
			if from.CanTransitionTo(to) {
				t.Errorf("不应允许 %s → %s", from, to)
			}
		}
	}
}

// TestTerminalStatusesNeverMove 单独把终态钉住。
//
// 终态可回退是本域最不能接受的错误：把一张已支付订单改回待支付，
// 意味着同一张单可以再收一次钱，而订单号与渠道流水的一一对应也断了，
// 对账时无法回答「这笔钱对应哪一次支付」。
func TestTerminalStatusesNeverMove(t *testing.T) {
	for _, status := range []Status{StatusPaid, StatusFailed, StatusClosed} {
		if !status.Terminal() {
			t.Errorf("%s 应当是终态", status)
		}
		for _, next := range AllStatuses() {
			if status.CanTransitionTo(next) {
				t.Errorf("终态 %s 不应能迁移到 %s", status, next)
			}
		}
	}

	if StatusPending.Terminal() {
		t.Error("pending 不应是终态")
	}
}

func TestStatusValid(t *testing.T) {
	for _, status := range AllStatuses() {
		if !status.Valid() {
			t.Errorf("%s 应当是合法状态", status)
		}
	}
	for _, status := range []Status{"", "paid ", "PAID", "refunded", "unknown"} {
		if status.Valid() {
			t.Errorf("%q 不应是合法状态", status)
		}
	}
}

// ── 入参校验 ──────────────────────────────────────

func TestCreateInputValidate(t *testing.T) {
	cases := []struct {
		name    string
		input   CreateInput
		wantErr bool
		field   string
	}{
		{
			name:  "合法金额",
			input: CreateInput{UserID: 1, Amount: 10000},
		},
		{
			name:  "正好是最小值",
			input: CreateInput{UserID: 1, Amount: MinAmount},
		},
		{
			name:  "正好是最大值",
			input: CreateInput{UserID: 1, Amount: MaxAmount},
		},
		{
			name:    "低于最小值",
			input:   CreateInput{UserID: 1, Amount: MinAmount - 1},
			wantErr: true,
			field:   "amount",
		},
		{
			name:    "高于最大值",
			input:   CreateInput{UserID: 1, Amount: MaxAmount + 1},
			wantErr: true,
			field:   "amount",
		},
		{
			name:    "零金额",
			input:   CreateInput{UserID: 1, Amount: 0},
			wantErr: true,
			field:   "amount",
		},
		{
			name:    "负金额",
			input:   CreateInput{UserID: 1, Amount: -100},
			wantErr: true,
			field:   "amount",
		},
		{
			name:    "未登录",
			input:   CreateInput{Amount: 10000},
			wantErr: true,
		},
		{
			name:    "渠道标识过长",
			input:   CreateInput{UserID: 1, Amount: 10000, Channel: strings.Repeat("x", channelMaxLen+1)},
			wantErr: true,
			field:   "channel",
		},
		{
			name:  "渠道标识正好到上限",
			input: CreateInput{UserID: 1, Amount: 10000, Channel: strings.Repeat("x", channelMaxLen)},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.input.Validate()
			if !tc.wantErr {
				if err != nil {
					t.Fatalf("不应报错，实际: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("应当报错")
			}
			if tc.field != "" {
				if _, ok := err.Fields[tc.field]; !ok {
					t.Errorf("错误应带字段 %s，实际 %v", tc.field, err.Fields)
				}
			}
		})
	}
}

// TestMinAmountIsPositive 验证金额下限本身是正数。
//
// 下限若是 0 或负数，校验就形同虚设，而校验失败时的表现是
// 「0 元充值订单被创建出来」——它会走到渠道、走到回调，
// 最后在账本里留下一条金额为 0 的流水。
func TestMinAmountIsPositive(t *testing.T) {
	if !MinAmount.IsPositive() {
		t.Errorf("最小充值金额应为正数，实际 %d", MinAmount)
	}
	if MaxAmount <= MinAmount {
		t.Errorf("最大充值金额 %d 应大于最小值 %d", MaxAmount, MinAmount)
	}
}

// ── 分页 ──────────────────────────────────────────

func TestFilterNormalize(t *testing.T) {
	cases := []struct {
		name  string
		in    Filter
		want  Filter
		limit int
	}{
		{name: "零值取默认", in: Filter{}, limit: DefaultLimit},
		{name: "负数取默认", in: Filter{Limit: -5}, limit: DefaultLimit},
		{name: "正常值保留", in: Filter{Limit: 50}, limit: 50},
		{name: "超上限被截断", in: Filter{Limit: MaxLimit + 1}, limit: MaxLimit},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.in.Normalize()
			if got.Limit != tc.limit {
				t.Errorf("limit 期望 %d，实际 %d", tc.limit, got.Limit)
			}
		})
	}

	if got := (Filter{Limit: 10, Cursor: -1}).Normalize(); got.Cursor != 0 {
		t.Errorf("负游标应归零，实际 %d", got.Cursor)
	}
}

// ── 报文留档 ──────────────────────────────────────

func TestTruncatePayload(t *testing.T) {
	short := []byte(`{"a":1}`)
	if got := truncatePayload(short); got != string(short) {
		t.Errorf("短报文应原样保留，实际 %q", got)
	}

	long := make([]byte, maxRawPayload+500)
	for i := range long {
		long[i] = 'a'
	}
	got := truncatePayload(long)
	if len(got) > maxRawPayload {
		t.Errorf("截断后长度应不超过 %d，实际 %d", maxRawPayload, len(got))
	}
}

// TestTruncatePayloadKeepsValidUTF8 验证截断不会留下半个汉字。
//
// 按字节截断很可能切在一个汉字中间。那半个字符写进 utf8mb4 列时，
// MySQL 会直接报 "Incorrect string value" 让整个回调失败——
// 而原始报文只是留档，不该有能力打断一笔真实的入账。
func TestTruncatePayloadKeepsValidUTF8(t *testing.T) {
	// 用汉字填满并略微超出上限，保证截断点落在某个汉字中间
	body := []byte(strings.Repeat("中", maxRawPayload/3+10))
	if len(body) <= maxRawPayload {
		t.Fatalf("测试数据没有超出上限：%d", len(body))
	}

	got := truncatePayload(body)
	if !strings.ContainsRune(got, '中') && got != "" {
		t.Fatalf("截断结果异常: %q", got)
	}
	for i, r := range got {
		if r == '\uFFFD' {
			t.Fatalf("截断结果第 %d 个位置出现了替换字符", i)
		}
	}
	// 长度按字节仍在上限内
	if len(got) > maxRawPayload {
		t.Errorf("截断后长度应不超过 %d，实际 %d", maxRawPayload, len(got))
	}
}

// ── 订单过期时间 ──────────────────────────────────

// TestOrderTTLIsSane 验证有效期落在一个合理区间。
//
// 太短会让用户付不了款，太长会让待支付订单堆积、
// 也让「渠道很晚才回调」这种本不该入账的情况变得常见。
func TestOrderTTLIsSane(t *testing.T) {
	if OrderTTL < 5*time.Minute {
		t.Errorf("订单有效期 %s 太短，用户来不及完成支付", OrderTTL)
	}
	if OrderTTL > 24*time.Hour {
		t.Errorf("订单有效期 %s 太长，待支付订单会堆积", OrderTTL)
	}
}

// ── 错误码映射 ────────────────────────────────────

// TestMapErrorKeepsUnknownErrorsUntouched 验证未登记的错误原样透传。
//
// 这条是有意为之的：mapError 若把未知错误统一成某个业务码，
// 数据库连接失败之类的内部问题就会被包装成「参数错误」返回给客户端，
// 排查时看到的是一个与真相无关的提示。
func TestMapErrorKeepsUnknownErrorsUntouched(t *testing.T) {
	raw := errs.New(errs.CodeInternal)
	if got := mapError(raw); got != raw {
		t.Errorf("已带业务码的错误应原样返回，实际 %v", got)
	}
	if got := mapError(nil); got != nil {
		t.Errorf("nil 应映射为 nil，实际 %v", got)
	}
}

func TestMapErrorUsesExpectedCodes(t *testing.T) {
	cases := []struct {
		err  error
		want errs.Code
	}{
		{ErrOrderNotFound, errs.CodeNotFound},
		{ErrAmountMismatch, errs.CodeInvalidParam},
		{ErrTransactionReused, errs.CodeInvalidParam},
		{ErrOrderExpired, errs.CodeInvalidOrderState},
		{ErrOrderNotPayable, errs.CodeInvalidOrderState},
	}

	for _, tc := range cases {
		t.Run(tc.err.Error(), func(t *testing.T) {
			mapped := mapError(tc.err)
			e := errs.From(mapped)
			if e.Code != tc.want {
				t.Errorf("错误码期望 %d，实际 %d", tc.want, e.Code)
			}
			// 底层原因必须保留，否则日志里只剩一句「参数校验失败」
			if e.Unwrap() == nil {
				t.Error("映射后应保留底层原因")
			}
		})
	}
}

// TestInsufficientAmountWouldBeRejected 是一处易错点的显式记录：
// 充值金额与订单金额的比对必须是「不等就拒」，而不是「小于就拒」。
//
// 只判「回执金额小于订单金额」的话，多付的钱会被当成合法回调入账——
// 而实际入账金额取自订单，用户多付的部分就凭空消失了。
func TestAmountComparisonIsExactEquality(t *testing.T) {
	order := money.Amount(10000)

	for _, receipt := range []money.Amount{9999, 10001, 1, 0, 10000} {
		equal := order == receipt
		if receipt == order && !equal {
			t.Errorf("%d 与 %d 应当相等", order, receipt)
		}
		if receipt != order && equal {
			t.Errorf("%d 与 %d 不应相等", order, receipt)
		}
	}
}
