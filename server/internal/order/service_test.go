package order

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/cangerx/c-ssl/server/internal/domain/errs"
	"github.com/cangerx/c-ssl/server/internal/domain/money"
	"github.com/cangerx/c-ssl/server/internal/domain/orderstate"
	"github.com/cangerx/c-ssl/server/internal/product"
	"github.com/cangerx/c-ssl/server/internal/product/rules"
	"github.com/cangerx/c-ssl/server/internal/testutil"
	"github.com/cangerx/c-ssl/server/internal/upstream/foxssl"
	"github.com/cangerx/c-ssl/server/internal/wallet"
)

// 本文件走真实数据库验证订单域的资金流程。
//
// 为什么必须打真实数据库：本域要守住的几条性质全都依赖行锁、唯一索引与
// 事务隔离级别的真实行为——
//
//   - 「上游结果未知时余额仍然冻结着」需要真的有一条冻结记录留在库里；
//   - 「重试不产生第二张证书」需要上游按商户订单号幂等，而这条幂等
//     由 foxssl.MockClient 实现并自带测试；
//   - 「余额不足时订单不会留下」需要事务真的回滚。
//
// 用 mock 仓储验证不了这些，只能验证「代码按我设想的方式调用了接口」——
// 而设想本身可能才是错的。

const orderTestSecret = "order-test-webhook-secret-0123456789"

// 迁移里种下的产品，测试直接引用。
//
// 用真实种子数据而不是测试里现造一个产品：产品能力字段（是否支持取消、
// 是否要企业信息、允许多少个域名）决定了订单流程走哪条分支，
// 自己造一个就等于把这些分支的输入换成了测试作者的理解，
// 而真实种子里那些「不支持取消的免费证书」正是最容易被漏掉的边界。
const (
	productDV       int64 = 1 // AlphaSSL DV 单域名，零售价 29800，支持取消与重签
	productDVRetail       = money.Amount(29800)
	productFree     int64 = 6 // 免费 DV，零售价 0，不支持取消与重签
)

type fixture struct {
	db       *sql.DB
	svc      *Service
	repo     *Repository
	wallet   *wallet.Service
	upstream *foxssl.MockClient
	userID   int64
}

func newFixture(t *testing.T) *fixture {
	t.Helper()

	db := testutil.OpenTestDB(t)
	userID := testutil.CreateTestUser(t, db)

	upstream, err := foxssl.NewMockClient("test-api-key", orderTestSecret, "http://localhost:8080")
	if err != nil {
		t.Fatalf("构造 Mock 上游失败: %v", err)
	}

	walletSvc := wallet.NewService(wallet.NewRepository(db))
	repo := NewRepository(db)

	return &fixture{
		db:       db,
		repo:     repo,
		svc:      NewService(repo, walletSvc, product.NewService(product.NewRepository(db)), upstream),
		wallet:   walletSvc,
		upstream: upstream,
		userID:   userID,
	}
}

// credit 给测试用户充值。
//
// 走钱包域自己的接口而不是直接 UPDATE 余额表：余额表上的
// available + frozen 必须与账本累加值一致，绕过去写会让后续断言失去意义。
func (f *fixture) credit(t *testing.T, amount money.Amount) {
	t.Helper()
	if _, err := f.wallet.Recharge(context.Background(), wallet.ChangeInput{
		UserID: f.userID, Amount: amount, BizType: "test", BizNo: "seed",
	}); err != nil {
		t.Fatalf("充值测试余额失败: %v", err)
	}
}

func (f *fixture) account(t *testing.T) *wallet.Account {
	t.Helper()
	acc, err := f.wallet.Get(context.Background(), f.userID)
	if err != nil {
		t.Fatalf("查询钱包失败: %v", err)
	}
	return acc
}

// createDV 下一张普通 DV 订单。
func (f *fixture) createDV(t *testing.T, domains ...string) *Order {
	t.Helper()
	if len(domains) == 0 {
		domains = []string{"example.com"}
	}
	o, err := f.svc.Create(context.Background(), CreateInput{
		UserID:       f.userID,
		ProductID:    productDV,
		Years:        1,
		KeyAlgorithm: rules.RSA,
		Domains:      domains,
		Contact:      testContact(),
	})
	if err != nil {
		t.Fatalf("下单失败: %v", err)
	}
	return o
}

func testContact() Contact {
	return Contact{Name: "张三", Email: "admin@example.com", Phone: "+86.13800138000"}
}

// reload 从库里重新读订单，避免读到内存里的乐观副本。
func (f *fixture) reload(t *testing.T, orderNo string) *Order {
	t.Helper()
	o, err := f.repo.GetByNo(context.Background(), orderNo)
	if err != nil {
		t.Fatalf("读取订单 %s 失败: %v", orderNo, err)
	}
	return o
}

func (f *fixture) countEvents(t *testing.T, upstreamOrderNo string) int {
	t.Helper()
	var n int
	if err := f.db.QueryRow(
		`SELECT COUNT(*) FROM webhook_events WHERE upstream_order_no = ?`,
		upstreamOrderNo).Scan(&n); err != nil {
		t.Fatalf("统计上游事件失败: %v", err)
	}
	return n
}

// cleanupEvents 按上游订单号清掉事件。
//
// 平台不认识的上游订单号不挂在任何用户下，testutil.CleanupUser 的
// 按用户清理覆盖不到它。不显式清理的话，事件会在测试库里累积，
// 下一次运行时「只有一行事件」这类断言会看到上一次留下的行而失败——
// 而失败信息会指向一个与被测代码无关的原因。
//
// 删除动作同时发生在测试开始前与结束后：只清结束后的话，
// 上一次失败（或上次跑的是另一份代码）留下的行仍然会污染本次断言。
func cleanupEvents(t *testing.T, db *sql.DB, upstreamOrderNo string) {
	t.Helper()

	remove := func() {
		if _, err := db.Exec(
			`DELETE FROM webhook_events WHERE upstream_order_no = ?`,
			upstreamOrderNo); err != nil {
			t.Logf("清理上游事件失败（%s）: %v", upstreamOrderNo, err)
		}
	}
	remove()
	t.Cleanup(remove)
}

// issueUpstream 让 Mock 上游把订单推进到已签发。
//
// 事件与实际状态必须一致：上游是先签发、再通知。只发通知不推进上游的话，
// 平台收到「已签发」后会去下载证书，而上游那边这张单还没签发，
// 下载必然失败——测出来的是 Mock 的不一致，不是被测代码的问题。
func (f *fixture) issueUpstream(t *testing.T, upstreamOrderNo string) {
	t.Helper()
	if err := f.upstream.IssueCertificate(upstreamOrderNo); err != nil {
		t.Fatalf("推进上游订单到已签发失败: %v", err)
	}
}

// ── 下单：冻结与结算 ──────────────────────────────

// TestCreateOrderSettlesAtRetailPrice 验证下单成功后的资金状态。
//
// 冻结发生在下单事务里，结算发生在上游受理之后。两者都是零售价，
// 因此完成后冻结余额归零、可用余额减少恰好一张证书的钱。
func TestCreateOrderSettlesAtRetailPrice(t *testing.T) {
	f := newFixture(t)
	f.credit(t, 100_000)

	o := f.createDV(t)

	if o.Status != orderstate.WaitingDcv {
		t.Errorf("上游受理后状态应为 waiting_dcv，实际 %s", o.Status)
	}
	if o.Amount != productDVRetail {
		t.Errorf("订单金额应为零售价 %d，实际 %d", productDVRetail, o.Amount)
	}
	if o.UpstreamOrderNo == "" {
		t.Error("上游受理后应记录上游订单号")
	}

	acc := f.account(t)
	if acc.FrozenBalance != 0 {
		t.Errorf("结算后冻结余额应为 0，实际 %d", acc.FrozenBalance)
	}
	if want := money.Amount(100_000) - productDVRetail; acc.AvailableBalance != want {
		t.Errorf("可用余额应为 %d，实际 %d", want, acc.AvailableBalance)
	}
	if f.upstream.OrderCount() != 1 {
		t.Errorf("上游应只有 1 张订单，实际 %d 张", f.upstream.OrderCount())
	}
}

// TestChargeIgnoresUpstreamCost 验证扣款依据是零售价而不是上游成本价。
//
// 这是「定价依据」那条设计取舍的守卫测试。把上游 cost 调成远高于零售价的值，
// 如果实现里用了 cost，可用余额会变成负数或者报余额不足——
// 而两者都是把采购成本透传给了用户。
func TestChargeIgnoresUpstreamCost(t *testing.T) {
	f := newFixture(t)
	f.credit(t, 100_000)
	f.upstream.SetCost(productDVRetail * 10)

	o := f.createDV(t)

	if o.Amount != productDVRetail {
		t.Errorf("订单金额应取零售价 %d，实际 %d", productDVRetail, o.Amount)
	}
	if o.CostPrice != productDVRetail*10 {
		t.Errorf("成本价应被原样记录供对账，实际 %d", o.CostPrice)
	}

	acc := f.account(t)
	if want := money.Amount(100_000) - productDVRetail; acc.AvailableBalance != want {
		t.Errorf("可用余额应为 %d（只扣零售价），实际 %d", want, acc.AvailableBalance)
	}
	if acc.AvailableBalance < 0 {
		t.Error("可用余额不应为负——说明扣款用了比零售价更高的上游成本价")
	}
}

// TestInsufficientBalanceLeavesNoOrder 验证余额不足时整个下单事务回滚。
//
// 只成其一就会出现「钱冻了但没有订单」（用户凭空少一笔可用余额）
// 或者「有订单但没冻结」（用户可以反复下单而不受限）。
func TestInsufficientBalanceLeavesNoOrder(t *testing.T) {
	f := newFixture(t)
	f.credit(t, 1000) // 远低于 29800

	_, err := f.svc.Create(context.Background(), CreateInput{
		UserID:       f.userID,
		ProductID:    productDV,
		Years:        1,
		KeyAlgorithm: rules.RSA,
		Domains:      []string{"example.com"},
		Contact:      testContact(),
	})
	if err == nil {
		t.Fatal("余额不足时应下单失败")
	}

	var e *errs.Error
	if !errors.As(err, &e) || e.Code != errs.CodeInsufficientBalance {
		t.Errorf("应返回余额不足（%d），实际 %v", errs.CodeInsufficientBalance, err)
	}

	acc := f.account(t)
	if acc.AvailableBalance != 1000 || acc.FrozenBalance != 0 {
		t.Errorf("失败的下单不应改动余额，实际可用 %d 冻结 %d",
			acc.AvailableBalance, acc.FrozenBalance)
	}

	var n int
	if err := f.db.QueryRow(
		`SELECT COUNT(*) FROM certificate_orders WHERE user_id = ?`, f.userID).Scan(&n); err != nil {
		t.Fatalf("统计订单失败: %v", err)
	}
	if n != 0 {
		t.Errorf("回滚后不应留下订单，实际 %d 张", n)
	}
	if f.upstream.OrderCount() != 0 {
		t.Errorf("余额不足时不应调用上游，实际建了 %d 张单", f.upstream.OrderCount())
	}
}

// ── 上游失败的两条分支 ────────────────────────────

// TestUnknownUpstreamResultKeepsFundsFrozen 是本文件最重要的一条。
//
// 上游超时（ErrUnavailable）意味着**平台不知道上游到底建没建单**。
// 此时解冻并置失败，等于平台白付一张证书的钱——而上游那张证书还在，
// 后续对账时会发现一笔对不上的支出。
//
// 正确行为是订单停在 submitting、余额保持冻结，由补偿流程按同一个
// 商户订单号重试；上游幂等保证重试不会产生第二张证书。
func TestUnknownUpstreamResultKeepsFundsFrozen(t *testing.T) {
	f := newFixture(t)
	f.credit(t, 100_000)
	f.upstream.FailOnce(foxssl.OpCreateOrder, foxssl.ErrUnavailable)

	_, err := f.svc.Create(context.Background(), CreateInput{
		UserID:       f.userID,
		ProductID:    productDV,
		Years:        1,
		KeyAlgorithm: rules.RSA,
		Domains:      []string{"example.com"},
		Contact:      testContact(),
	})
	if err == nil {
		t.Fatal("上游超时时下单应返回错误")
	}

	var e *errs.Error
	if !errors.As(err, &e) || e.Code != errs.CodeUpstream {
		t.Errorf("上游结果未知应返回 %d（502），实际 %v", errs.CodeUpstream, err)
	}

	var orderNo, status string
	if err := f.db.QueryRow(
		`SELECT order_no, status FROM certificate_orders WHERE user_id = ?`,
		f.userID).Scan(&orderNo, &status); err != nil {
		t.Fatalf("订单应当留在库里等待补偿，查询失败: %v", err)
	}
	if status != string(orderstate.Submitting) {
		t.Errorf("订单应停在 submitting，实际 %s", status)
	}

	acc := f.account(t)
	if acc.FrozenBalance != productDVRetail {
		t.Errorf("结果未知时金额必须保持冻结，实际冻结 %d", acc.FrozenBalance)
	}
	if want := money.Amount(100_000) - productDVRetail; acc.AvailableBalance != want {
		t.Errorf("可用余额应为 %d，实际 %d", want, acc.AvailableBalance)
	}
}

// TestDeterministicUpstreamRejectionUnfreezes 是上一条的对照。
//
// 上游明确拒绝时它一定没建单，可以放心解冻并置失败。
// 与「结果未知」共用一条代码路径的话，两者必有一个是错的——
// 而错的那一个要么让平台白付钱，要么让用户的钱永远冻着。
func TestDeterministicUpstreamRejectionUnfreezes(t *testing.T) {
	f := newFixture(t)
	f.credit(t, 100_000)
	f.upstream.FailOnce(foxssl.OpCreateOrder, errors.New("该域名不被支持"))

	_, err := f.svc.Create(context.Background(), CreateInput{
		UserID:       f.userID,
		ProductID:    productDV,
		Years:        1,
		KeyAlgorithm: rules.RSA,
		Domains:      []string{"example.com"},
		Contact:      testContact(),
	})
	if err == nil {
		t.Fatal("上游拒绝时应下单失败")
	}

	var status, reason string
	if err := f.db.QueryRow(
		`SELECT status, failure_reason FROM certificate_orders WHERE user_id = ?`,
		f.userID).Scan(&status, &reason); err != nil {
		t.Fatalf("查询订单失败: %v", err)
	}
	if status != string(orderstate.Failed) {
		t.Errorf("上游明确拒绝后状态应为 failed，实际 %s", status)
	}
	if reason == "" {
		t.Error("失败订单应记录失败原因")
	}

	acc := f.account(t)
	if acc.FrozenBalance != 0 {
		t.Errorf("上游明确拒绝后应解冻，实际冻结 %d", acc.FrozenBalance)
	}
	if acc.AvailableBalance != 100_000 {
		t.Errorf("可用余额应完整退回 100000，实际 %d", acc.AvailableBalance)
	}
}

// TestRetrySubmitConvergesToSingleUpstreamOrder 验证补偿重试的安全性。
//
// 上游按商户订单号幂等，因此「超时后重试」无论上一次到底成没成功，
// 结果都收敛到同一张证书。断言 OrderCount()==1 是这条性质的直接检验：
// 去掉幂等实现，重试会真的下第二单。
func TestRetrySubmitConvergesToSingleUpstreamOrder(t *testing.T) {
	f := newFixture(t)
	f.credit(t, 100_000)
	f.upstream.FailOnce(foxssl.OpCreateOrder, foxssl.ErrUnavailable)

	_, err := f.svc.Create(context.Background(), CreateInput{
		UserID:       f.userID,
		ProductID:    productDV,
		Years:        1,
		KeyAlgorithm: rules.RSA,
		Domains:      []string{"example.com"},
		Contact:      testContact(),
	})
	if err == nil {
		t.Fatal("第一次下单应因上游超时失败")
	}

	var orderNo string
	if err := f.db.QueryRow(
		`SELECT order_no FROM certificate_orders WHERE user_id = ?`,
		f.userID).Scan(&orderNo); err != nil {
		t.Fatalf("查询订单失败: %v", err)
	}

	if err := f.svc.RetrySubmit(context.Background(), orderNo); err != nil {
		t.Fatalf("补偿重试应成功: %v", err)
	}

	if got := f.upstream.OrderCount(); got != 1 {
		t.Errorf("重试后上游应只有 1 张订单，实际 %d 张——幂等失效会产生第二张证书", got)
	}

	o := f.reload(t, orderNo)
	if o.Status != orderstate.WaitingDcv {
		t.Errorf("重试成功后状态应为 waiting_dcv，实际 %s", o.Status)
	}
	if o.UpstreamOrderNo == "" {
		t.Error("重试成功后应记录上游订单号")
	}

	acc := f.account(t)
	if acc.FrozenBalance != 0 {
		t.Errorf("重试成功后应完成结算，实际冻结 %d", acc.FrozenBalance)
	}
	if want := money.Amount(100_000) - productDVRetail; acc.AvailableBalance != want {
		t.Errorf("可用余额应为 %d，实际 %d", want, acc.AvailableBalance)
	}
}

// TestRetrySubmitRejectsOrderNotInSubmitting 验证补偿入口不会误伤别的状态。
//
// 对一张已签发的订单调用重试，会以同一个商户订单号再向上游下一次单。
// 上游幂等会挡住它，但平台不该依赖上游替自己兜底。
func TestRetrySubmitRejectsOrderNotInSubmitting(t *testing.T) {
	f := newFixture(t)
	f.credit(t, 100_000)
	o := f.createDV(t) // waiting_dcv

	err := f.svc.RetrySubmit(context.Background(), o.OrderNo)
	if err == nil {
		t.Fatal("非 submitting 状态的订单不应接受补偿重试")
	}
	var e *errs.Error
	if !errors.As(err, &e) || e.Code != errs.CodeInvalidOrderState {
		t.Errorf("应返回 %d，实际 %v", errs.CodeInvalidOrderState, err)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ledgerOps 返回用户的账本操作类型，按流水号升序。
func (f *fixture) ledgerOps(t *testing.T) []string {
	t.Helper()
	rows, err := f.db.Query(
		`SELECT op FROM wallet_ledger WHERE user_id = ? ORDER BY id ASC`, f.userID)
	if err != nil {
		t.Fatalf("查询账本失败: %v", err)
	}
	defer func() { _ = rows.Close() }()

	var out []string
	for rows.Next() {
		var op string
		if err := rows.Scan(&op); err != nil {
			t.Fatalf("读取账本行失败: %v", err)
		}
		out = append(out, op)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("遍历账本失败: %v", err)
	}
	return out
}

// TestCancelRefundsRatherThanUnfreezes 钉住「退钱动作取决于订单走到哪一步」。
//
// 订单在上游受理时就完成了结算（钱已实扣），此时冻结余额是 0。
// 取消一张已进入域名验证的订单如果走解冻，会直接报「冻结余额不足」——
// 表现为「用户永远取消不掉自己的订单」。
//
// 断言账本里出现 refund 而不只是 unfreeze：两者对可用余额的效果相同，
// 只断言余额的话，这条测试在一个「先 settle 再 unfreeze」的错误实现上
// 同样能通过（那种实现会失败，但失败原因不同）。
func TestCancelRefundsRatherThanUnfreezes(t *testing.T) {
	f := newFixture(t)
	f.credit(t, 100_000)
	o := f.createDV(t) // 已经走到 waiting_dcv，钱已实扣

	if _, err := f.svc.Cancel(context.Background(), f.userID, o.OrderNo, "不想要了"); err != nil {
		t.Fatalf("取消已进入验证阶段的订单不应失败: %v", err)
	}

	if acc := f.account(t); acc.AvailableBalance != 100_000 {
		t.Errorf("取消后可用余额应完整退回 100000，实际 %d", acc.AvailableBalance)
	}

	// 冻结也写一条流水（与解冻配对、净效果为零），因此序列是
	// 充值 → 冻结 → 结算 → 退款。这里断言的是末两步：
	// 结算之后必须跟一条**退款**，而不是解冻。
	ops := f.ledgerOps(t)
	if want := []string{"recharge", "freeze", "settle", "refund"}; !equalStrings(ops, want) {
		t.Errorf("账本流水序列应为 %v，实际 %v", want, ops)
	}
}

// TestCancelFailedOrderDoesNotRefundTwice 验证已退过钱的订单不会再退一次。
//
// failed 状态下的钱已经在上游拒绝时退回了。状态机允许 failed → cancelled，
// 于是「取消一张失败订单」是一条真实可达的路径；若沿用按状态猜的动作，
// 这里会再退一笔，用户凭空多出一张证书的钱。
func TestCancelFailedOrderDoesNotRefundTwice(t *testing.T) {
	f := newFixture(t)
	f.credit(t, 100_000)
	f.upstream.FailOnce(foxssl.OpCreateOrder, errors.New("该域名不被支持"))

	if _, err := f.svc.Create(context.Background(), CreateInput{
		UserID:       f.userID,
		ProductID:    productDV,
		Years:        1,
		KeyAlgorithm: rules.RSA,
		Domains:      []string{"example.com"},
		Contact:      testContact(),
	}); err == nil {
		t.Fatal("上游拒绝时应下单失败")
	}

	var orderNo string
	if err := f.db.QueryRow(
		`SELECT order_no FROM certificate_orders WHERE user_id = ?`,
		f.userID).Scan(&orderNo); err != nil {
		t.Fatalf("查询订单失败: %v", err)
	}

	before := f.ledgerOps(t)
	accBefore := f.account(t)

	if _, err := f.svc.Cancel(context.Background(), f.userID, orderNo, "清理掉这张废单"); err != nil {
		t.Fatalf("取消一张失败订单应被允许: %v", err)
	}

	if after := f.ledgerOps(t); len(after) != len(before) {
		t.Errorf("取消失败订单不应产生新的资金流水，之前 %v，之后 %v", before, after)
	}
	accAfter := f.account(t)
	if accAfter.AvailableBalance != accBefore.AvailableBalance ||
		accAfter.FrozenBalance != accBefore.FrozenBalance {
		t.Errorf("取消失败订单不应改动余额：之前 可用 %d 冻结 %d，之后 可用 %d 冻结 %d",
			accBefore.AvailableBalance, accBefore.FrozenBalance,
			accAfter.AvailableBalance, accAfter.FrozenBalance)
	}
}

// TestWebhookFailureRefundsSettledOrder 验证上游判定失败时会把钱退给用户。
//
// 这条路径很容易被漏掉：订单已经走到 waiting_dcv、钱已实扣，
// 此时 CA 在人工核验阶段驳回（OV/EV 常见）。若事件处理只改状态不退钱，
// 订单会显示「失败」而用户的钱留在平台，没有任何自动路径把它退回去。
func TestWebhookFailureRefundsSettledOrder(t *testing.T) {
	f := newFixture(t)
	f.credit(t, 100_000)
	o := f.createDV(t)

	// 前置条件：钱已经实扣
	if acc := f.account(t); acc.AvailableBalance != 100_000-productDVRetail {
		t.Fatalf("前置条件不成立：可用余额应为 %d，实际 %d",
			100_000-productDVRetail, acc.AvailableBalance)
	}

	raw, signature := encodeEvent(t, &foxssl.Notification{
		EventType:       "order_status",
		UpstreamOrderNo: o.UpstreamOrderNo,
		Status:          foxssl.MockStatusFailed,
		OccurredAt:      time.Now().UTC(),
	})
	if err := f.svc.HandleWebhook(context.Background(), raw, signature); err != nil {
		t.Fatalf("失败事件处理失败: %v", err)
	}

	if got := f.reload(t, o.OrderNo).Status; got != orderstate.Failed {
		t.Errorf("订单状态应为 failed，实际 %s", got)
	}
	if acc := f.account(t); acc.AvailableBalance != 100_000 {
		t.Errorf("上游判定失败后应退款，可用余额应为 100000，实际 %d", acc.AvailableBalance)
	}

	if ops := f.ledgerOps(t); ops[len(ops)-1] != "refund" {
		t.Errorf("账本最后一条应为退款，实际流水序列 %v", ops)
	}
}

// TestWebhookFailureForLostResponseLeavesFundsFrozen 覆盖「上游建了单但响应丢了」。
//
// 场景是：平台调用上游，上游建单成功，但响应在回程丢失。
// 本地此时没有任何上游订单号，订单停在 submitting、钱还冻结着。
// 之后上游推来一条失败事件——它带的是**上游**订单号，平台反查不到本地订单。
//
// 正确行为是保守的：事件记下来（线索不能丢），但不动订单、不动钱。
// 这个订单只能由补偿流程按同一个商户订单号重试来收敛，
// 而补偿重试是安全的（上游按商户订单号幂等）。
//
// 反过来做——比如按事件里的状态去「猜」一张本地订单——会让一笔
// 已经冻结的钱凭空退掉，而那张上游证书可能正在签发。
func TestWebhookFailureForLostResponseLeavesFundsFrozen(t *testing.T) {
	f := newFixture(t)
	f.credit(t, 100_000)
	ctx := context.Background()

	f.upstream.FailOnce(foxssl.OpCreateOrder, foxssl.ErrUnavailable)
	if _, err := f.svc.Create(ctx, CreateInput{
		UserID:       f.userID,
		ProductID:    productDV,
		Years:        1,
		KeyAlgorithm: rules.RSA,
		Domains:      []string{"example.com"},
		Contact:      testContact(),
	}); err == nil {
		t.Fatal("上游超时时下单应失败")
	}

	var orderNo, status string
	if err := f.db.QueryRow(
		`SELECT order_no, status FROM certificate_orders WHERE user_id = ?`,
		f.userID).Scan(&orderNo, &status); err != nil {
		t.Fatalf("查询订单失败: %v", err)
	}
	if status != string(orderstate.Submitting) {
		t.Fatalf("前置条件不成立：订单应停在 submitting，实际 %s", status)
	}

	// 模拟「上游其实建单了」：用同一个商户订单号在上游补建一张。
	// 上游按商户订单号幂等，所以这正是补偿重试会得到的结果。
	if _, err := f.upstream.CreateOrder(ctx, foxssl.CreateOrderRequest{
		MerchantOrderNo: orderNo,
		Domains:         []string{"example.com"},
	}); err != nil {
		t.Fatalf("在上游补建订单失败: %v", err)
	}
	upstreamNo := f.upstream.UpstreamOrderNo(orderNo)
	if upstreamNo == "" {
		t.Fatal("上游补建后应能查到上游订单号")
	}
	cleanupEvents(t, f.db, upstreamNo)

	raw, signature := encodeEvent(t, &foxssl.Notification{
		EventType:       "order_status",
		UpstreamOrderNo: upstreamNo,
		Status:          foxssl.MockStatusFailed,
		OccurredAt:      time.Now().UTC(),
	})
	if err := f.svc.HandleWebhook(ctx, raw, signature); err != nil {
		t.Fatalf("反查不到本地订单的事件不应把错误抛给上游: %v", err)
	}

	if n := f.countEvents(t, upstreamNo); n != 1 {
		t.Errorf("事件应被记录以便排查，实际 %d 行", n)
	}
	if got := f.reload(t, orderNo).Status; got != orderstate.Submitting {
		t.Errorf("反查不到本地订单时不应改动状态，实际 %s", got)
	}

	acc := f.account(t)
	if acc.FrozenBalance != productDVRetail {
		t.Errorf("反查不到本地订单时不应动钱，冻结额应为 %d，实际 %d",
			productDVRetail, acc.FrozenBalance)
	}
}

// ── 免费订单 ──────────────────────────────────────

// TestFreeOrderProducesNoFundMovement 验证免费证书不产生任何资金动作。
//
// 免费证书的零售价就是 0，是一条真实存在的业务路径。
// 走一遍「冻结 0 元再结算 0 元」会在账本里留下两条金额为 0 的流水，
// 把用户的账单页面塞满无意义的记录。
func TestFreeOrderProducesNoFundMovement(t *testing.T) {
	f := newFixture(t)

	o, err := f.svc.Create(context.Background(), CreateInput{
		UserID:       f.userID,
		ProductID:    productFree,
		Years:        1,
		KeyAlgorithm: rules.RSA,
		Domains:      []string{"example.com"},
		Contact:      testContact(),
	})
	if err != nil {
		t.Fatalf("免费证书应能下单: %v", err)
	}

	if o.Amount != 0 {
		t.Errorf("免费证书金额应为 0，实际 %d", o.Amount)
	}
	if o.Status != orderstate.WaitingDcv {
		t.Errorf("免费证书也应走到 waiting_dcv，实际 %s", o.Status)
	}

	acc := f.account(t)
	if acc.AvailableBalance != 0 || acc.FrozenBalance != 0 {
		t.Errorf("免费证书不应改动余额，实际可用 %d 冻结 %d",
			acc.AvailableBalance, acc.FrozenBalance)
	}

	var entries int
	if err := f.db.QueryRow(
		`SELECT COUNT(*) FROM wallet_ledger WHERE user_id = ?`, f.userID).Scan(&entries); err != nil {
		t.Fatalf("统计账本失败: %v", err)
	}
	if entries != 0 {
		t.Errorf("免费证书不应产生账本流水，实际 %d 条", entries)
	}
}

// ── 取消 ──────────────────────────────────────────

// TestCancelUnfreezesAndNotifiesUpstream 验证取消会通知上游并退回冻结金额。
//
// 顺序是刻意的：先向上游发起取消，上游确认后才解冻。
// 反过来做的话，上游仍在跑而钱已经退给用户，等于平台白送一张证书。
func TestCancelUnfreezesAndNotifiesUpstream(t *testing.T) {
	f := newFixture(t)
	f.credit(t, 100_000)
	o := f.createDV(t)

	got, err := f.svc.Cancel(context.Background(), f.userID, o.OrderNo, "用户改主意了")
	if err != nil {
		t.Fatalf("取消订单失败: %v", err)
	}
	if got.Status != orderstate.Cancelled {
		t.Errorf("取消后状态应为 cancelled，实际 %s", got.Status)
	}
	if got.CancelReason != "用户改主意了" {
		t.Errorf("应记录取消原因，实际 %q", got.CancelReason)
	}

	acc := f.account(t)
	if acc.AvailableBalance != 100_000 {
		t.Errorf("取消后可用余额应完整退回 100000，实际 %d", acc.AvailableBalance)
	}
	if acc.FrozenBalance != 0 {
		t.Errorf("取消后不应有冻结余额，实际 %d", acc.FrozenBalance)
	}

	// 上游侧也应被取消。只改本地状态的话，上游会继续签发并扣平台的成本。
	up, err := f.upstream.OrderStatus(context.Background(), o.UpstreamOrderNo)
	if err != nil {
		t.Fatalf("查询上游订单状态失败: %v", err)
	}
	if up.OrderStatus != foxssl.MockStatusCancelled {
		t.Errorf("上游订单状态应为 cancelled，实际 %s", up.OrderStatus)
	}
}

// TestCancelRejectedWhenProductDoesNotSupportIt 验证产品能力把关在服务端。
//
// 免费证书的 cancel_supported 是 0。前端不展示取消按钮只是体验，
// 绕过前端直接调接口是最容易的事。
func TestCancelRejectedWhenProductDoesNotSupportIt(t *testing.T) {
	f := newFixture(t)

	o, err := f.svc.Create(context.Background(), CreateInput{
		UserID:       f.userID,
		ProductID:    productFree,
		Years:        1,
		KeyAlgorithm: rules.RSA,
		Domains:      []string{"example.com"},
		Contact:      testContact(),
	})
	if err != nil {
		t.Fatalf("免费证书应能下单: %v", err)
	}

	_, err = f.svc.Cancel(context.Background(), f.userID, o.OrderNo, "")
	if err == nil {
		t.Fatal("不支持取消的产品应拒绝取消")
	}
	var e *errs.Error
	if !errors.As(err, &e) || e.Code != errs.CodeInvalidOrderState {
		t.Errorf("应返回 %d，实际 %v", errs.CodeInvalidOrderState, err)
	}

	if got := f.reload(t, o.OrderNo).Status; got != orderstate.WaitingDcv {
		t.Errorf("被拒绝的取消不应改动订单状态，实际 %s", got)
	}
}

// TestCancelRejectsOtherUsersOrder 验证越权取消返回「不存在」而不是「无权限」。
//
// 返回 403 等于确认「这张单存在」，可以拿来做订单号枚举。
func TestCancelRejectsOtherUsersOrder(t *testing.T) {
	f := newFixture(t)
	f.credit(t, 100_000)
	o := f.createDV(t)

	intruder := testutil.CreateTestUser(t, f.db)

	_, err := f.svc.Cancel(context.Background(), intruder, o.OrderNo, "")
	if err == nil {
		t.Fatal("不应允许取消他人的订单")
	}
	var e *errs.Error
	if !errors.As(err, &e) || e.Code != errs.CodeNotFound {
		t.Errorf("越权访问应返回 %d，实际 %v", errs.CodeNotFound, err)
	}

	if got := f.reload(t, o.OrderNo).Status; got != orderstate.WaitingDcv {
		t.Errorf("越权取消不应改动订单，实际状态 %s", got)
	}
	acc := f.account(t)
	if acc.FrozenBalance != 0 || acc.AvailableBalance != 100_000-productDVRetail {
		t.Errorf("越权取消不应改动余额，实际可用 %d 冻结 %d",
			acc.AvailableBalance, acc.FrozenBalance)
	}
}

// ── 域名验证 ──────────────────────────────────────

// TestDomainMaterialHasNoPlaceholder 是跨层的守卫测试。
//
// 从「下单 → 上游返回材料 → 落库 → 读出来」整条链路上，
// 任何一个环节把上游模板原样存下来，用户都会看到一个含花括号的路径。
func TestDomainMaterialHasNoPlaceholder(t *testing.T) {
	f := newFixture(t)
	f.credit(t, 100_000)

	// 通配符域名最容易出问题：它的替换目标与自身不同
	o, err := f.svc.Create(context.Background(), CreateInput{
		UserID:       f.userID,
		ProductID:    2, // GlobalSign DV 通配符
		Years:        1,
		KeyAlgorithm: rules.RSA,
		Domains:      []string{"*.example.com"},
		Contact:      testContact(),
	})
	if err != nil {
		t.Fatalf("下单失败: %v", err)
	}

	domains, err := f.svc.ListDomains(context.Background(), f.userID, o.OrderNo)
	if err != nil {
		t.Fatalf("查询域名材料失败: %v", err)
	}
	if len(domains) == 0 {
		t.Fatal("应至少有一个域名的验证材料")
	}

	for _, d := range domains {
		if strings.Contains(d.File.Path, "{FQDN}") {
			t.Errorf("域名 %s 的文件路径残留占位符: %q", d.Domain, d.File.Path)
		}
		if strings.Contains(d.File.Path, "*.") {
			t.Errorf("域名 %s 的文件路径残留通配符: %q", d.Domain, d.File.Path)
		}
		if d.File.Path != "" && !strings.Contains(d.File.Path, "example.com") {
			t.Errorf("域名 %s 的文件路径应指向裸域名，实际 %q", d.Domain, d.File.Path)
		}
	}
}

// TestVerifyDomainsRejectsUnavailableMethod 验证所选方式必须真的可用。
//
// 可用方式由上游返回的材料反推。Mock 只给了 TXT 记录，
// 因此 dns_cname 虽然产品配置里有、对这张订单却不可用。
// 放行的话，用户会按 CNAME 配好记录，然后在验证阶段才失败。
func TestVerifyDomainsRejectsUnavailableMethod(t *testing.T) {
	f := newFixture(t)
	f.credit(t, 100_000)
	o := f.createDV(t)

	_, err := f.svc.VerifyDomains(context.Background(), f.userID, o.OrderNo,
		[]string{"example.com"}, rules.DnsCname)
	if err == nil {
		t.Fatal("上游没给 CNAME 材料时不应接受 dns_cname")
	}
	var e *errs.Error
	if !errors.As(err, &e) || e.Code != errs.CodeInvalidParam {
		t.Errorf("应返回 %d，实际 %v", errs.CodeInvalidParam, err)
	}

	// 可用的方式必须能通过，否则上一条断言可能只是因为「什么都通不过」
	if _, err := f.svc.VerifyDomains(context.Background(), f.userID, o.OrderNo,
		[]string{"example.com"}, rules.DnsTxt); err != nil {
		t.Fatalf("dns_txt 应当可用: %v", err)
	}
}

// TestVerifyDomainsRejectsForeignDomain 验证不能对不属于本订单的域名发起验证。
func TestVerifyDomainsRejectsForeignDomain(t *testing.T) {
	f := newFixture(t)
	f.credit(t, 100_000)
	o := f.createDV(t)

	_, err := f.svc.VerifyDomains(context.Background(), f.userID, o.OrderNo,
		[]string{"attacker.example.net"}, rules.DnsTxt)
	if err == nil {
		t.Fatal("不属于本订单的域名应被拒绝")
	}
	var e *errs.Error
	if !errors.As(err, &e) || e.Code != errs.CodeInvalidParam {
		t.Errorf("应返回 %d，实际 %v", errs.CodeInvalidParam, err)
	}
}

// TestVerifyDomainsRejectsMethodNotDeclaredByProduct 验证产品声明的验证方式真的把关。
//
// 上游给每个域名都返回了邮件地址，所以「材料支持邮件验证」这一层拦不住 email；
// 产品（AlphaSSL）声明里没有 email。docs/06 第 4.3 节把这几条列为
// **后端必须校验**的规则，验收标准第 6 条也写明「产品规则在后端校验，
// 不能依赖前端绕过」。产品页写着不支持、接口却接受，产品配置就成了纯装饰。
//
// 断言落在 ListDomains 下发的 AllowedMethods 上，而不是只断言「提交被拒」——
// 后者会被材料侧的任何一条规则顺带满足，于是把产品侧判断整个删掉，
// 测试依然是绿的。下发的集合同时管住两件事：产品侧是否收窄、下发给前端的
// 是不是同一份数据。
func TestVerifyDomainsRejectsMethodNotDeclaredByProduct(t *testing.T) {
	f := newFixture(t)
	f.credit(t, 100_000)
	o := f.createDV(t)
	ctx := context.Background()

	all, err := f.repo.ListDomains(ctx, o.OrderNo)
	if err != nil {
		t.Fatalf("读取域名材料失败: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("应有 1 个域名，实际 %d 个", len(all))
	}

	// 前提一：材料侧支持 email
	if !MethodAllowed(all[0], rules.Email) {
		t.Fatalf("前提不成立：上游材料里应有邮件地址，实际 %v", all[0].Emails)
	}
	// 前提二：产品侧不支持 email
	declared, err := f.svc.declaredDcvMethods(ctx, o.ProductID, all)
	if err != nil {
		t.Fatalf("推导产品声明的验证方式失败: %v", err)
	}
	if declared == nil {
		t.Fatal("前提不成立：产品应当读得到，declared 不应为 nil")
	}
	if containsMethod(declared, rules.Email) {
		t.Fatalf("前提不成立：产品声明里不该有 email，实际 %v", declared)
	}

	// 材料侧单独看是支持 email 的。这条断言让下面那条有意义：
	// 如果材料侧本来就不支持 email，下面那条测的就不是产品侧把关了。
	if !containsMethod(IntersectMethods(all), rules.Email) {
		t.Fatalf("前提不成立：只看材料时 email 应当可用，实际 %v", IntersectMethods(all))
	}

	domains, err := f.svc.ListDomains(ctx, f.userID, o.OrderNo)
	if err != nil {
		t.Fatalf("读取域名失败: %v", err)
	}
	if containsMethod(domains[0].AllowedMethods, rules.Email) {
		t.Errorf("产品未声明的验证方式不该下发给前端，实际 %v", domains[0].AllowedMethods)
	}
	// 反过来：声明过的必须还在。否则上面那条可能只是因为「什么都收没了」。
	if !containsMethod(domains[0].AllowedMethods, rules.DnsTxt) {
		t.Errorf("产品声明过的 dns_txt 应当保留，实际 %v", domains[0].AllowedMethods)
	}

	_, err = f.svc.VerifyDomains(ctx, f.userID, o.OrderNo,
		[]string{"example.com"}, rules.Email)
	if err == nil {
		t.Fatal("产品未声明的验证方式应被拒绝")
	}
	var e *errs.Error
	if !errors.As(err, &e) || e.Code != errs.CodeInvalidParam {
		t.Fatalf("应返回 %d，实际 %v", errs.CodeInvalidParam, err)
	}
	if _, ok := e.Fields["method"]; !ok {
		t.Errorf("应带 method 字段提示，实际 %v", e.Fields)
	}
	// 拒绝理由必须指向产品。说成「这些域名不支持」会把用户引去改域名，
	// 而问题在产品上——他无论怎么配记录都不会通过。
	if !strings.Contains(e.Message, "产品") {
		t.Errorf("拒绝理由应指向产品，实际 %q", e.Message)
	}

	// 产品声明过的 dns_txt 仍要能通过，否则上面那条可能只是因为「什么都通不过」
	if _, err := f.svc.VerifyDomains(ctx, f.userID, o.OrderNo,
		[]string{"example.com"}, rules.DnsTxt); err != nil {
		t.Fatalf("dns_txt 应当可用: %v", err)
	}
}

// TestAllowedMethodsMatchWhatVerifyAccepts 验证下发的 AllowedMethods 与
// VerifyDomains 接受什么完全一致。
//
// 这是这个域最要紧的一条不变量：界面上的选项来自 AllowedMethods，
// 用户照着选却被拒，他只会以为是自己哪里配错了记录，然后一遍遍重试。
//
// 两个判据分别算在两处（材料侧逐域名推导、产品侧按整张证书声明），
// 任何一处漂移都会让这条断言失败，所以它同时守住两处。
func TestAllowedMethodsMatchWhatVerifyAccepts(t *testing.T) {
	f := newFixture(t)
	f.credit(t, 100_000)
	o := f.createDV(t)
	ctx := context.Background()

	domains, err := f.svc.ListDomains(ctx, f.userID, o.OrderNo)
	if err != nil {
		t.Fatalf("读取域名失败: %v", err)
	}
	if len(domains) != 1 {
		t.Fatalf("应有 1 个域名，实际 %d 个", len(domains))
	}
	listed := domains[0].AllowedMethods

	// 拿空集合去比空集合会让下面的循环恒真，先挡住这种情况。
	if len(listed) == 0 {
		t.Fatal("下发的可用验证方式不该为空——前端靠它渲染选项")
	}

	// 五种方式逐个真提交一次，而不是照着判据自己推一遍：
	// 用判据推的话，判据写错了断言也跟着错，等于什么都没测。
	all := []rules.DcvMethod{
		rules.DnsTxt, rules.DnsCname, rules.HTTPFile, rules.HTTPSFile, rules.Email,
	}
	for _, m := range all {
		_, err := f.svc.VerifyDomains(ctx, f.userID, o.OrderNo, []string{"example.com"}, m)
		accepted := err == nil
		if accepted != containsMethod(listed, m) {
			got := "被拒"
			if accepted {
				got = "被接受"
			}
			t.Errorf("验证方式 %s 提交后%s，但下发的列表 %v 里它是 %v",
				m, got, listed, containsMethod(listed, m))
		}
	}
}

// TestVerifyDomainsAllowsDeclaredMethodWhenProductDelisted 验证产品下架后不会卡住在途订单。
//
// 产品下架对用户端等同于不存在（products.Get 返回 404），此时平台已经不知道
// 当初卖的是什么，但用户已经付过钱、上游也备好了材料。用「读不到产品」
// 当成「什么都不支持」，会让一张已付款的订单连域名都验证不了，只能烂在那里。
//
// 所以这条路径退回到只按上游材料判断——与 Cancel / Reissue 的保守方向相反，
// 理由见 allowedDcvMethods 的说明。
func TestVerifyDomainsAllowsDeclaredMethodWhenProductDelisted(t *testing.T) {
	f := newFixture(t)
	const tempProduct int64 = 9001
	seedTempProduct(t, f.db, tempProduct)
	f.credit(t, 100_000)

	o, err := f.svc.Create(context.Background(), CreateInput{
		UserID:       f.userID,
		ProductID:    tempProduct,
		Years:        1,
		KeyAlgorithm: rules.RSA,
		Domains:      []string{"example.com"},
		Contact:      testContact(),
	})
	if err != nil {
		t.Fatalf("用临时产品下单失败: %v", err)
	}

	// 下架。用临时产品而不是把产品 1 下架：其它测试包并行跑着，
	// 改共享种子数据会让它们随机失败，而那种失败极难定位。
	if _, err := f.db.Exec(
		`UPDATE products SET status = 'off_shelf' WHERE id = ?`, tempProduct); err != nil {
		t.Fatalf("下架临时产品失败: %v", err)
	}

	if _, err := f.svc.VerifyDomains(context.Background(), f.userID, o.OrderNo,
		[]string{"example.com"}, rules.DnsTxt); err != nil {
		t.Fatalf("产品下架后仍应允许提交上游支持的验证方式，实际 %v", err)
	}
}

// seedTempProduct 造一个只属于本用例的产品，并保证每次运行都存在且处于上架状态。
//
// 用临时产品而不是把种子产品下架：其它测试包并行跑着，改共享数据会让它们
// 随机失败，而那种失败极难定位。种子产品的能力（不支持取消、不需要企业信息）
// 也决定了订单走哪条分支，不该被本用例借用。
//
// **刻意不删。** certificate_orders.product_id 是 ON DELETE RESTRICT 外键，
// 测试结束时订单还在，删产品必然失败；而为了删产品去删订单，又会把订单的
// 域名、联系人、证书行留成孤儿。留一行产品在测试库里没有副作用——
// 测试库本来就是一次性数据，而「每次重建」让重复运行不会撞主键。
func seedTempProduct(t *testing.T, db *sql.DB, id int64) {
	t.Helper()

	if _, err := db.Exec(
		`INSERT INTO products
		   (id, name, brand, validation_type, wildcard_supported, ip_supported,
		    multi_domain_supported, key_algorithms, dcv_methods,
		    reissue_supported, cancel_supported, require_organization_info,
		    upstream_product_id, status, sort_order)
		 VALUES (?, '临时产品（测试用）', 'TestCA', 'dv', 0, 0, 0,
		         'rsa', 'dns_txt,http_file', 1, 1, 0, 9901, 'active', 999)
		 ON DUPLICATE KEY UPDATE status = 'active'`,
		id); err != nil {
		t.Fatalf("插入临时产品失败: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO product_prices (product_id, years, cost_price, retail_price)
		 VALUES (?, 1, 1000, 2000)
		 ON DUPLICATE KEY UPDATE retail_price = VALUES(retail_price)`, id); err != nil {
		t.Fatalf("插入临时产品价格失败: %v", err)
	}
}

// TestRegenerateTokenRejectsVerifiedDomain 验证已验证的域名不会被重置。
//
// 重新生成 token 会换掉 DNS 记录值与文件路径，对已验证的域名来说
// 等于把验证结果作废——用户得重新配一遍。而重新生成的正当理由是
// 「token 泄漏」，此时该做的是联系客服换发，不是把已通过的域名拖下水。
func TestRegenerateTokenRejectsVerifiedDomain(t *testing.T) {
	f := newFixture(t)
	f.credit(t, 100_000)
	o := f.createDV(t)

	ctx := context.Background()
	if err := f.repo.SyncDomainStatus(ctx, o.OrderNo, "example.com", DomainVerified, nil); err != nil {
		t.Fatalf("构造已验证域名失败: %v", err)
	}

	if _, err := f.svc.RegenerateDcvToken(ctx, f.userID, o.OrderNo); err == nil {
		t.Fatal("已有域名验证通过时不应允许重新生成 token")
	}
}

// ── 上游回调 ──────────────────────────────────────

// encodeEvent 把事件编码成上游报文，并用上游的密钥签名。
//
// 签名必须由测试自己做，而且用的是与服务端验签同一个密钥——
// 用别的密钥签出来的报文会被判成伪造，那样测的就成了「验签会拦住它」，
// 而不是「验签通过之后会发生什么」。
func encodeEvent(t *testing.T, n *foxssl.Notification) ([]byte, string) {
	t.Helper()
	raw, err := foxssl.EncodeNotification(n)
	if err != nil {
		t.Fatalf("编码事件报文失败: %v", err)
	}
	return raw, foxssl.Sign(orderTestSecret, raw)
}

// issuedNotification 构造一条「已签发」事件，并附上合法的签名。
func issuedNotification(t *testing.T, upstreamOrderNo string) ([]byte, string) {
	t.Helper()
	return encodeEvent(t, &foxssl.Notification{
		EventType:       "certificate_issued",
		UpstreamOrderNo: upstreamOrderNo,
		Status:          foxssl.MockStatusIssued,
		OccurredAt:      time.Now().UTC(),
	})
}

// TestWebhookAdvancesOrderAndIsIdempotent 验证回调推进状态且重复投递只生效一次。
//
// 幂等键由 上游:事件类型:上游订单号:报文哈希 组成，因此重推同一份字节
// 会撞上同一个键。断言的是「事件表只有一行」而不是「状态没变」——
// 后者在一个「每次都重新下载证书」的实现上同样能通过。
func TestWebhookAdvancesOrderAndIsIdempotent(t *testing.T) {
	f := newFixture(t)
	f.credit(t, 100_000)
	o := f.createDV(t)
	f.issueUpstream(t, o.UpstreamOrderNo)

	raw, signature := issuedNotification(t, o.UpstreamOrderNo)

	for i := range 3 {
		if err := f.svc.HandleWebhook(context.Background(), raw, signature); err != nil {
			t.Fatalf("第 %d 次回调不应报错: %v", i+1, err)
		}
	}

	if got := f.reload(t, o.OrderNo).Status; got != orderstate.Issued {
		t.Errorf("回调后状态应为 issued，实际 %s", got)
	}
	if n := f.countEvents(t, o.UpstreamOrderNo); n != 1 {
		t.Errorf("重复投递应只留下 1 行事件，实际 %d 行", n)
	}

	// 签发后应当能把证书取下来，否则「已签发」只是一个状态字
	cert, err := f.svc.Certificate(context.Background(), f.userID, o.OrderNo)
	if err != nil {
		t.Fatalf("已签发订单应能取到证书: %v", err)
	}
	if !strings.Contains(cert.Certificate, "BEGIN CERTIFICATE") {
		t.Errorf("证书内容应是 PEM 文本，实际 %q", cert.Certificate)
	}
}

// TestWebhookRejectsTamperedSignatureWithoutWriting 验证验签失败时一行都不写。
//
// 不是「验签失败后回滚」，而是根本没有任何东西需要回滚。
// 落库的代价很具体：攻击者可以用伪造报文占满事件表，
// 把真实事件挤出幂等窗口，让一笔真实入账被当成重放丢掉。
func TestWebhookRejectsTamperedSignatureWithoutWriting(t *testing.T) {
	f := newFixture(t)
	f.credit(t, 100_000)
	o := f.createDV(t)

	raw, good := issuedNotification(t, o.UpstreamOrderNo)

	cases := map[string]string{
		"缺少签名":     "",
		"签名被篡改":    good[:len(good)-2] + "AB",
		"签名换了一把密钥": foxssl.Sign("another-secret-entirely-0000000000", raw),
	}
	for name, signature := range cases {
		t.Run(name, func(t *testing.T) {
			err := f.svc.HandleWebhook(context.Background(), raw, signature)
			if err == nil {
				t.Fatal("验签失败时应返回错误")
			}
			if !errors.Is(err, foxssl.ErrInvalidSignature) {
				t.Errorf("应返回 ErrInvalidSignature，实际 %v", err)
			}
			if n := f.countEvents(t, o.UpstreamOrderNo); n != 0 {
				t.Errorf("验签失败不应写入任何事件，实际 %d 行", n)
			}
			if got := f.reload(t, o.OrderNo).Status; got != orderstate.WaitingDcv {
				t.Errorf("验签失败不应改动订单状态，实际 %s", got)
			}
		})
	}
}

// TestWebhookDoesNotRegressIssuedOrder 验证乱序投递不会把已签发的订单改回去。
//
// 上游不保证投递顺序，一条迟到的「等待验证」事件完全可能晚于「已签发」到达。
// 状态机对回退返回 nil，服务层据此跳过——但事件本身仍要记下来。
func TestWebhookDoesNotRegressIssuedOrder(t *testing.T) {
	f := newFixture(t)
	f.credit(t, 100_000)
	o := f.createDV(t)
	ctx := context.Background()
	f.issueUpstream(t, o.UpstreamOrderNo)

	raw, signature := issuedNotification(t, o.UpstreamOrderNo)
	if err := f.svc.HandleWebhook(ctx, raw, signature); err != nil {
		t.Fatalf("签发事件处理失败: %v", err)
	}
	if got := f.reload(t, o.OrderNo).Status; got != orderstate.Issued {
		t.Fatalf("前置条件不成立：状态应为 issued，实际 %s", got)
	}

	// 一条迟到的「回到等待验证」事件
	late, lateSig := encodeEvent(t, &foxssl.Notification{
		EventType:       "order_status",
		UpstreamOrderNo: o.UpstreamOrderNo,
		Status:          foxssl.MockStatusWaitingDcv,
		OccurredAt:      time.Now().Add(-time.Hour).UTC(),
	})
	if err := f.svc.HandleWebhook(ctx, late, lateSig); err != nil {
		t.Fatalf("迟到事件不应导致报错: %v", err)
	}

	if got := f.reload(t, o.OrderNo).Status; got != orderstate.Issued {
		t.Errorf("已签发的订单不应被迟到事件改回，实际 %s", got)
	}
	if n := f.countEvents(t, o.UpstreamOrderNo); n != 2 {
		t.Errorf("迟到事件仍应被记录，实际事件表有 %d 行", n)
	}
}

// TestWebhookRecordsEventForUnknownOrder 验证上游推了平台不认识的订单号时，
// 事件仍然落库、接口仍然返回成功。
//
// 两个方向都要对：不落库就再也查不到上游到底推过什么（排查线索直接丢掉）；
// 返回错误会让上游无限重推一个永远处理不了的事件。
func TestWebhookRecordsEventForUnknownOrder(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	const unknown = "FX-19700101-999999"
	cleanupEvents(t, f.db, unknown)

	raw, signature := encodeEvent(t, &foxssl.Notification{
		EventType:       "certificate_issued",
		UpstreamOrderNo: unknown,
		Status:          foxssl.MockStatusIssued,
		OccurredAt:      time.Now().UTC(),
	})

	if err := f.svc.HandleWebhook(ctx, raw, signature); err != nil {
		t.Fatalf("处理不了的事件不应把错误抛给上游: %v", err)
	}

	if n := f.countEvents(t, unknown); n != 1 {
		t.Fatalf("事件应被记录，实际 %d 行", n)
	}

	var status, processErr string
	if err := f.db.QueryRow(
		`SELECT process_status, process_error FROM webhook_events WHERE upstream_order_no = ?`,
		unknown).Scan(&status, &processErr); err != nil {
		t.Fatalf("查询事件失败: %v", err)
	}
	if status != EventFailed {
		t.Errorf("处理失败的事件应标记为 %s，实际 %s", EventFailed, status)
	}
	if processErr == "" {
		t.Error("处理失败的事件应留下原因，否则后台任务无从判断该重试还是该人工介入")
	}
}

// TestWebhookUnknownStatusDoesNotGuess 验证未识别的上游状态只记录、不改状态。
//
// 猜一个状态比不动更危险：猜成 issued 会让未签发的订单可下载，
// 而用户拿到的证书在浏览器里不被信任。
func TestWebhookUnknownStatusDoesNotGuess(t *testing.T) {
	f := newFixture(t)
	f.credit(t, 100_000)
	o := f.createDV(t)

	raw, signature := encodeEvent(t, &foxssl.Notification{
		EventType:       "order_status",
		UpstreamOrderNo: o.UpstreamOrderNo,
		Status:          "some_brand_new_status",
		OccurredAt:      time.Now().UTC(),
	})

	if err := f.svc.HandleWebhook(context.Background(), raw, signature); err != nil {
		t.Fatalf("未识别的状态不应导致报错: %v", err)
	}

	got := f.reload(t, o.OrderNo)
	if got.Status != orderstate.WaitingDcv {
		t.Errorf("未识别的上游状态不应改动本地状态，实际 %s", got.Status)
	}
	if got.UpstreamOrderStatus != "some_brand_new_status" {
		t.Errorf("原始状态应被原样记录供排查，实际 %q", got.UpstreamOrderStatus)
	}
}
