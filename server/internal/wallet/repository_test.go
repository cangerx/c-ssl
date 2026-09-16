package wallet

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/cangerx/c-ssl/server/internal/domain/errs"
	"github.com/cangerx/c-ssl/server/internal/domain/money"
	"github.com/cangerx/c-ssl/server/internal/testutil"
)

// 本文件的测试全部打真实数据库。
//
// 钱包域的验收标准——并发不超扣、余额与账本一致、重复调用不重复入账——
// 依赖行锁、唯一索引与事务回滚的真实行为，用 mock 验证不了：
// mock 只能证明「代码按我设想的方式调用了接口」，而设想本身可能才是错的。

func newTestService(t *testing.T) (*Service, *sql.DB) {
	t.Helper()
	db := testutil.OpenTestDB(t)
	return NewService(NewRepository(db)), db
}

// ── 断言辅助 ──────────────────────────────────────

func mustGetAccount(t *testing.T, svc *Service, userID int64) *Account {
	t.Helper()
	account, err := svc.Get(context.Background(), userID)
	if err != nil {
		t.Fatalf("查询钱包账户失败: %v", err)
	}
	return account
}

func assertBalances(t *testing.T, svc *Service, userID int64, available, frozen money.Amount) {
	t.Helper()
	account := mustGetAccount(t, svc, userID)
	if account.AvailableBalance != available || account.FrozenBalance != frozen {
		t.Errorf("余额不符：期望可用 %d / 冻结 %d，实际可用 %d / 冻结 %d",
			available, frozen, account.AvailableBalance, account.FrozenBalance)
	}
}

// assertConsistent 校验「账户余额 == 账本累加值」这条不变量。
func assertConsistent(t *testing.T, svc *Service, userID int64) *ReconcileResult {
	t.Helper()
	result, err := svc.Reconcile(context.Background(), userID)
	if err != nil {
		t.Fatalf("对账失败: %v", err)
	}
	if !result.Consistent() {
		available, frozen := result.Difference()
		t.Fatalf("账户余额与账本累加值不一致：可用差 %d，冻结差 %d（账户 %d/%d，账本 %d/%d，共 %d 条流水）",
			available, frozen,
			result.AccountAvailable, result.AccountFrozen,
			result.LedgerAvailable, result.LedgerFrozen, result.EntryCount)
	}
	return result
}

func countEntries(t *testing.T, db *sql.DB, userID int64) int {
	t.Helper()
	var n int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM wallet_ledger WHERE user_id = ?`, userID).Scan(&n); err != nil {
		t.Fatalf("统计账本流水失败: %v", err)
	}
	return n
}

func countAccounts(t *testing.T, db *sql.DB, userID int64) int {
	t.Helper()
	var n int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM wallet_accounts WHERE user_id = ?`, userID).Scan(&n); err != nil {
		t.Fatalf("统计钱包账户失败: %v", err)
	}
	return n
}

// ── 基本流程 ──────────────────────────────────────

func TestRechargeAndConsume(t *testing.T) {
	svc, db := newTestService(t)
	ctx := context.Background()
	userID := testutil.CreateTestUser(t, db)

	// 从未有过资金往来的用户，余额为 0 是正确状态而不是错误
	assertBalances(t, svc, userID, 0, 0)

	result, err := svc.Recharge(ctx, ChangeInput{
		UserID: userID, Amount: 10000, BizType: "recharge_order", BizNo: "R20260916000001",
	})
	if err != nil {
		t.Fatalf("充值失败: %v", err)
	}
	if result.Duplicated {
		t.Error("首次充值不应被判定为重复")
	}
	if result.Entry.AvailableDelta != 10000 || result.Entry.FrozenDelta != 0 {
		t.Errorf("充值应只增加可用余额，实际可用 %d 冻结 %d",
			result.Entry.AvailableDelta, result.Entry.FrozenDelta)
	}
	assertBalances(t, svc, userID, 10000, 0)

	if _, err := svc.Consume(ctx, ChangeInput{
		UserID: userID, Amount: 3000, BizType: "cert_order", BizNo: "O20260916000001",
	}); err != nil {
		t.Fatalf("扣款失败: %v", err)
	}
	assertBalances(t, svc, userID, 7000, 0)

	reconcile := assertConsistent(t, svc, userID)
	if reconcile.EntryCount != 2 {
		t.Errorf("应有 2 条流水，实际 %d 条", reconcile.EntryCount)
	}
}

// 冻结 → 结算 → 再冻结 → 解冻 的完整生命周期。
// 这是 Phase 2 下单流程会走的路径，在这里先钉死每一环的余额变化。
func TestFreezeSettleUnfreezeLifecycle(t *testing.T) {
	svc, db := newTestService(t)
	ctx := context.Background()
	userID := testutil.CreateTestUser(t, db)

	mustRecharge(t, svc, userID, 10000)

	// 冻结：可用 → 冻结，总额不变
	if _, err := svc.Freeze(ctx, ChangeInput{
		UserID: userID, Amount: 3000, BizType: "cert_order", BizNo: "O1",
	}); err != nil {
		t.Fatalf("冻结失败: %v", err)
	}
	assertBalances(t, svc, userID, 7000, 3000)

	// 结算：只扣冻结，可用不变
	if _, err := svc.Settle(ctx, ChangeInput{
		UserID: userID, Amount: 3000, BizType: "cert_order", BizNo: "O1",
	}); err != nil {
		t.Fatalf("结算失败: %v", err)
	}
	assertBalances(t, svc, userID, 7000, 0)

	// 再冻结一次，然后解冻：钱要回到可用余额，而不是消失
	if _, err := svc.Freeze(ctx, ChangeInput{
		UserID: userID, Amount: 3000, BizType: "cert_order", BizNo: "O2",
	}); err != nil {
		t.Fatalf("冻结失败: %v", err)
	}
	assertBalances(t, svc, userID, 4000, 3000)

	if _, err := svc.Unfreeze(ctx, ChangeInput{
		UserID: userID, Amount: 3000, BizType: "cert_order", BizNo: "O2",
	}); err != nil {
		t.Fatalf("解冻失败: %v", err)
	}
	assertBalances(t, svc, userID, 7000, 0)

	// 全程总额守恒：只有结算那一步真正减少了总资产
	reconcile := assertConsistent(t, svc, userID)
	if reconcile.AccountAvailable+reconcile.AccountFrozen != 7000 {
		t.Errorf("结算后总资产应为 7000，实际 %d",
			reconcile.AccountAvailable+reconcile.AccountFrozen)
	}
}

func TestRefundIncreasesAvailable(t *testing.T) {
	svc, db := newTestService(t)
	ctx := context.Background()
	userID := testutil.CreateTestUser(t, db)

	mustRecharge(t, svc, userID, 10000)
	if _, err := svc.Consume(ctx, ChangeInput{
		UserID: userID, Amount: 3000, BizType: "cert_order", BizNo: "O1",
	}); err != nil {
		t.Fatalf("扣款失败: %v", err)
	}
	if _, err := svc.Refund(ctx, ChangeInput{
		UserID: userID, Amount: 3000, BizType: "cert_order", BizNo: "O1", Remark: "订单取消退款",
	}); err != nil {
		t.Fatalf("退款失败: %v", err)
	}
	assertBalances(t, svc, userID, 10000, 0)
	assertConsistent(t, svc, userID)
}

// ── 余额不足 ──────────────────────────────────────

// 失败的扣款不能留下任何痕迹：既不能改余额，也不能写流水。
// 这条断言实际是在验证事务确实回滚了。
func TestInsufficientBalanceLeavesNoTrace(t *testing.T) {
	svc, db := newTestService(t)
	ctx := context.Background()
	userID := testutil.CreateTestUser(t, db)

	cases := []struct {
		name string
		op   func(context.Context, ChangeInput) (*Result, error)
		want error
	}{
		{"可用余额不足时扣款", svc.Consume, ErrInsufficientAvailable},
		{"可用余额不足时冻结", svc.Freeze, ErrInsufficientAvailable},
		{"没有冻结余额时结算", svc.Settle, ErrInsufficientFrozen},
		{"没有冻结余额时解冻", svc.Unfreeze, ErrInsufficientFrozen},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := c.op(ctx, ChangeInput{
				UserID: userID, Amount: 100, BizType: "cert_order", BizNo: "O-fail",
			})
			if !errors.Is(err, c.want) {
				t.Fatalf("应返回 %v，实际 %v", c.want, err)
			}
			// 对外是 2000 余额不足，不是 5000 内部错误
			if e := errs.From(err); e.Code != errs.CodeInsufficientBalance {
				t.Errorf("业务码应为 %d，实际 %d", errs.CodeInsufficientBalance, e.Code)
			}
		})
	}

	if n := countEntries(t, db, userID); n != 0 {
		t.Errorf("失败的扣款不应写入流水，实际有 %d 条", n)
	}
	assertBalances(t, svc, userID, 0, 0)
}

// 冻结余额不能挪作他用：冻结 3000 后，可用只有 7000，
// 想再花 8000 必须被拒绝，否则同一笔钱会被花两次。
func TestFrozenBalanceCannotBeSpentAgain(t *testing.T) {
	svc, db := newTestService(t)
	ctx := context.Background()
	userID := testutil.CreateTestUser(t, db)

	mustRecharge(t, svc, userID, 10000)
	if _, err := svc.Freeze(ctx, ChangeInput{
		UserID: userID, Amount: 3000, BizType: "cert_order", BizNo: "O1",
	}); err != nil {
		t.Fatalf("冻结失败: %v", err)
	}

	_, err := svc.Freeze(ctx, ChangeInput{
		UserID: userID, Amount: 8000, BizType: "cert_order", BizNo: "O2",
	})
	if !errors.Is(err, ErrInsufficientAvailable) {
		t.Fatalf("超出可用余额的冻结应被拒绝，实际 %v", err)
	}
	assertBalances(t, svc, userID, 7000, 3000)
	assertConsistent(t, svc, userID)
}

// ── 幂等 ──────────────────────────────────────────

func TestDuplicateRequestDoesNotDoubleApply(t *testing.T) {
	svc, db := newTestService(t)
	ctx := context.Background()
	userID := testutil.CreateTestUser(t, db)

	in := ChangeInput{
		UserID: userID, Amount: 1000,
		BizType: "recharge_order", BizNo: "R20260916000001",
	}

	first, err := svc.Recharge(ctx, in)
	if err != nil {
		t.Fatalf("首次充值失败: %v", err)
	}
	if first.Duplicated {
		t.Error("首次充值不应被判定为重复")
	}

	// 连做三次重放
	for i := range 3 {
		again, err := svc.Recharge(ctx, in)
		if err != nil {
			t.Fatalf("第 %d 次重放不应报错: %v", i+1, err)
		}
		if !again.Duplicated {
			t.Errorf("第 %d 次重放应被判定为重复", i+1)
		}
		// 重放必须返回首次的结果，而不是重新计算
		if again.Entry.ID != first.Entry.ID {
			t.Errorf("重放应返回首次的流水号 %d，实际 %d", first.Entry.ID, again.Entry.ID)
		}
	}

	assertBalances(t, svc, userID, 1000, 0)
	if n := countEntries(t, db, userID); n != 1 {
		t.Errorf("重复调用不应产生新流水，实际有 %d 条", n)
	}

	account := mustGetAccount(t, svc, userID)
	if account.Version != 1 {
		t.Errorf("重复调用不应改动余额版本，实际 version=%d（应为 1）", account.Version)
	}
}

// 显式指定幂等键时，同一单号的分次退款等场景才能各自入账。
func TestExplicitEntryNoAllowsRepeatedSameOp(t *testing.T) {
	svc, db := newTestService(t)
	ctx := context.Background()
	userID := testutil.CreateTestUser(t, db)

	mustRecharge(t, svc, userID, 10000)

	base := ChangeInput{UserID: userID, Amount: 2000, BizType: "cert_order", BizNo: "O1"}
	for i := 1; i <= 2; i++ {
		in := base
		in.EntryNo = fmt.Sprintf("cert_order:O1:refund:%d", i)
		result, err := svc.Refund(ctx, in)
		if err != nil {
			t.Fatalf("第 %d 次退款失败: %v", i, err)
		}
		if result.Duplicated {
			t.Errorf("第 %d 次退款使用了不同的幂等键，不应判定为重复", i)
		}
	}

	assertBalances(t, svc, userID, 14000, 0)
	if n := countEntries(t, db, userID); n != 3 {
		t.Errorf("应有 3 条流水，实际 %d 条", n)
	}
}

// 幂等键被内容不同的请求复用必须报错。
// 静默返回上一次的结果会把「两笔不同订单用了同一个单号」这类 bug 藏到对账时才暴露。
func TestReusedEntryNoWithDifferentPayloadIsRejected(t *testing.T) {
	svc, db := newTestService(t)
	ctx := context.Background()
	userID := testutil.CreateTestUser(t, db)

	if _, err := svc.Recharge(ctx, ChangeInput{
		UserID: userID, Amount: 1000, BizType: "recharge_order", BizNo: "R1", EntryNo: "shared-key",
	}); err != nil {
		t.Fatalf("首次充值失败: %v", err)
	}

	_, err := svc.Recharge(ctx, ChangeInput{
		UserID: userID, Amount: 2000, BizType: "recharge_order", BizNo: "R2", EntryNo: "shared-key",
	})
	if !errors.Is(err, ErrIdempotencyKeyReused) {
		t.Fatalf("复用幂等键应报错，实际 %v", err)
	}
	e := errs.From(err)
	if e.Code != errs.CodeInvalidParam {
		t.Errorf("业务码应为 %d，实际 %d", errs.CodeInvalidParam, e.Code)
	}
	if _, ok := e.Fields["entryNo"]; !ok {
		t.Errorf("应带有 entryNo 字段级提示，实际 %v", e.Fields)
	}

	assertBalances(t, svc, userID, 1000, 0)
	if n := countEntries(t, db, userID); n != 1 {
		t.Errorf("被拒绝的请求不应产生流水，实际有 %d 条", n)
	}
}

// ── 并发 ──────────────────────────────────────────

// 本域最关键的一条测试：并发扣款不能超扣。
//
// 30 个协程各扣 400 分，账户只有 10000 分，因此恰好 25 次应该成功、5 次失败。
// 如果没有 SELECT ... FOR UPDATE 的行锁，多个事务会同时读到同一个旧余额，
// 各自认为够扣，最终余额变成负数或者成功次数超过 25。
func TestConcurrentConsumeNeverOverdraws(t *testing.T) {
	svc, db := newTestService(t)
	ctx := context.Background()
	userID := testutil.CreateTestUser(t, db)

	const (
		depositAmount = money.Amount(10000)
		perConsume    = money.Amount(400)
		workers       = 30
		wantSucceeded = 25 // 10000 / 400 向下取整
	)

	mustRecharge(t, svc, userID, depositAmount)

	var (
		wg        sync.WaitGroup
		mu        sync.Mutex
		succeeded int
		rejected  int
		other     []error
	)

	start := make(chan struct{})
	for i := range workers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start // 尽量让所有协程同时发起，放大竞态

			_, err := svc.Consume(ctx, ChangeInput{
				UserID:  userID,
				Amount:  perConsume,
				BizType: "cert_order",
				BizNo:   fmt.Sprintf("O%03d", i),
			})

			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				succeeded++
			case errors.Is(err, ErrInsufficientAvailable):
				rejected++
			default:
				other = append(other, err)
			}
		}(i)
	}
	close(start)
	wg.Wait()

	for _, err := range other {
		t.Errorf("出现了预期之外的错误: %v", err)
	}
	if succeeded != wantSucceeded {
		t.Errorf("应有 %d 次扣款成功，实际 %d 次（并发下超扣或少扣都说明行锁有问题）",
			wantSucceeded, succeeded)
	}
	if rejected != workers-wantSucceeded {
		t.Errorf("应有 %d 次因余额不足被拒，实际 %d 次", workers-wantSucceeded, rejected)
	}

	// 余额必须恰好为 0，既不能为负，也不应剩下该扣没扣的钱
	assertBalances(t, svc, userID, 0, 0)

	// 每次成功的扣款恰好对应一条流水，一次不多一次不少
	reconcile := assertConsistent(t, svc, userID)
	wantEntries := int64(wantSucceeded) + 1 // 扣款 + 1 次充值
	if reconcile.EntryCount != wantEntries {
		t.Errorf("应有 %d 条流水，实际 %d 条", wantEntries, reconcile.EntryCount)
	}
	if reconcile.AccountVersion != wantEntries {
		t.Errorf("余额版本应为 %d，实际 %d（每笔成功入账恰好 +1）",
			wantEntries, reconcile.AccountVersion)
	}
}

// 并发重放同一个幂等键：只能入账一次。
// 这里的竞态在于「查不到 → 都去插」，靠的是 entry_no 的唯一索引兜底。
func TestConcurrentDuplicateRequestsApplyOnce(t *testing.T) {
	svc, db := newTestService(t)
	ctx := context.Background()
	userID := testutil.CreateTestUser(t, db)

	const workers = 12
	in := ChangeInput{
		UserID: userID, Amount: 5000,
		BizType: "recharge_order", BizNo: "R-concurrent", EntryNo: "same-key",
	}

	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		first    *Result
		failures []error
	)

	start := make(chan struct{})
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			result, err := svc.Recharge(ctx, in)

			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				failures = append(failures, err)
				return
			}
			if first == nil {
				first = result
			}
		}()
	}
	close(start)
	wg.Wait()

	for _, err := range failures {
		t.Errorf("并发重放不应报错: %v", err)
	}
	if first == nil {
		t.Fatal("没有任何一次调用成功")
	}

	assertBalances(t, svc, userID, 5000, 0)
	if n := countEntries(t, db, userID); n != 1 {
		t.Errorf("并发重放只能入账一次，实际有 %d 条流水", n)
	}
}

// 并发首次入账走的是另一条代码路径：账户还不存在时，
// SELECT ... FOR UPDATE 会在唯一索引的间隙上加间隙锁，两个事务的间隙锁互不冲突，
// 但随后的 INSERT 需要插入意向锁、与对方的间隙锁互斥——于是死锁。
//
// 这条测试验证重试机制确实把死锁兜住了：任何一个并发请求随机失败都是不可接受的，
// 用户第一次充值时看到的应该是「成功」，而不是偶发的服务端错误。
func TestConcurrentFirstOperationsOnNewAccount(t *testing.T) {
	svc, db := newTestService(t)
	ctx := context.Background()
	userID := testutil.CreateTestUser(t, db)

	const (
		workers = 12
		amount  = money.Amount(100)
	)

	// 前提：账户此刻还不存在，否则测的就不是这条路径了
	if n := countAccounts(t, db, userID); n != 0 {
		t.Fatalf("前置条件不成立：新用户不应预建账户，实际有 %d 个", n)
	}

	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		failures []error
	)

	start := make(chan struct{})
	for i := range workers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start

			_, err := svc.Recharge(ctx, ChangeInput{
				UserID:  userID,
				Amount:  amount,
				BizType: "recharge_order",
				BizNo:   fmt.Sprintf("R-first-%03d", i),
			})

			if err != nil {
				mu.Lock()
				failures = append(failures, err)
				mu.Unlock()
			}
		}(i)
	}
	close(start)
	wg.Wait()

	for _, err := range failures {
		t.Errorf("并发首次入账不应失败（死锁应由重试兜住）: %v", err)
	}

	if n := countAccounts(t, db, userID); n != 1 {
		t.Errorf("并发创建后应恰好有 1 个账户，实际 %d 个", n)
	}
	assertBalances(t, svc, userID, amount*workers, 0)

	reconcile := assertConsistent(t, svc, userID)
	if reconcile.EntryCount != workers {
		t.Errorf("应有 %d 条流水，实际 %d 条", workers, reconcile.EntryCount)
	}
}

// ── 账本不可变 ────────────────────────────────────
// 账本只追加。本测试做的是行为兜底：先记下已有流水，再做更多操作，
// 然后逐字段比对——历史条目必须一字不差。
//
// 库层没有触发器强制（创建触发器需要 SUPER 权限，迁移必须在任何环境都能跑），
// 因此这条不变量靠 Repository 不提供任何更新/删除方法 + 这个测试共同守住。
func TestLedgerNeverRewritesHistory(t *testing.T) {
	svc, db := newTestService(t)
	ctx := context.Background()
	userID := testutil.CreateTestUser(t, db)

	mustRecharge(t, svc, userID, 10000)
	if _, err := svc.Freeze(ctx, ChangeInput{
		UserID: userID, Amount: 3000, BizType: "cert_order", BizNo: "O1",
	}); err != nil {
		t.Fatalf("冻结失败: %v", err)
	}
	if _, err := svc.Settle(ctx, ChangeInput{
		UserID: userID, Amount: 3000, BizType: "cert_order", BizNo: "O1",
	}); err != nil {
		t.Fatalf("结算失败: %v", err)
	}

	before := allEntries(t, svc, userID)
	if len(before) != 3 {
		t.Fatalf("应有 3 条流水，实际 %d 条", len(before))
	}

	// 继续做更多操作
	if _, err := svc.Refund(ctx, ChangeInput{
		UserID: userID, Amount: 500, BizType: "cert_order", BizNo: "O1",
	}); err != nil {
		t.Fatalf("退款失败: %v", err)
	}
	if _, err := svc.Consume(ctx, ChangeInput{
		UserID: userID, Amount: 200, BizType: "cert_order", BizNo: "O2",
	}); err != nil {
		t.Fatalf("扣款失败: %v", err)
	}

	after := allEntries(t, svc, userID)
	if len(after) != len(before)+2 {
		t.Fatalf("流水数应从 %d 增至 %d，实际 %d", len(before), len(before)+2, len(after))
	}

	for _, old := range before {
		current, ok := after[old.ID]
		if !ok {
			t.Errorf("流水 %d 消失了——账本只追加，不应有删除", old.ID)
			continue
		}
		if got, want := entryFingerprint(current), entryFingerprint(old); got != want {
			t.Errorf("流水 %d 被改写了：\n  原值 %s\n  现值 %s", old.ID, want, got)
		}
	}
}

// ── 对账 ──────────────────────────────────────────

// 一个永远返回「一致」的对账检查是没有价值的。
// 这里绕过应用直接改库，验证它确实能发现漂移。
func TestReconcileDetectsTampering(t *testing.T) {
	svc, db := newTestService(t)
	ctx := context.Background()
	userID := testutil.CreateTestUser(t, db)

	mustRecharge(t, svc, userID, 10000)
	assertConsistent(t, svc, userID)

	// 模拟有人绕过账本直接改余额
	if _, err := db.Exec(
		`UPDATE wallet_accounts SET available_balance = available_balance + 5000 WHERE user_id = ?`,
		userID); err != nil {
		t.Fatalf("构造脏数据失败: %v", err)
	}

	result, err := svc.Reconcile(ctx, userID)
	if err != nil {
		t.Fatalf("对账失败: %v", err)
	}
	if result.Consistent() {
		t.Fatal("账户被直接改动后，对账必须报不一致")
	}
	available, frozen := result.Difference()
	if available != 5000 || frozen != 0 {
		t.Errorf("差额应为可用 5000 / 冻结 0，实际可用 %d / 冻结 %d", available, frozen)
	}
}

// ── 账户生命周期 ──────────────────────────────────

// 账户按需创建：注册流程不必知道钱包的存在，历史用户缺失账户时也能自愈。
// 但读接口不能顺手建账户——那会让一次读请求变成写操作。
func TestAccountIsCreatedOnDemandNotOnRead(t *testing.T) {
	svc, db := newTestService(t)
	userID := testutil.CreateTestUser(t, db)

	if n := countAccounts(t, db, userID); n != 0 {
		t.Fatalf("新用户不应预建钱包账户，实际有 %d 个", n)
	}

	// 读余额不建账户
	assertBalances(t, svc, userID, 0, 0)
	if n := countAccounts(t, db, userID); n != 0 {
		t.Errorf("查询余额不应创建账户，实际有 %d 个", n)
	}

	// 没有账户时对账也应一致（两侧都是 0）
	result := assertConsistent(t, svc, userID)
	if result.AccountExists {
		t.Error("账户尚未创建，AccountExists 应为 false")
	}

	// 第一次资金变动时才建
	mustRecharge(t, svc, userID, 100)
	if n := countAccounts(t, db, userID); n != 1 {
		t.Errorf("首次资金变动后应恰好有 1 个账户，实际 %d 个", n)
	}
}

// ── 账本分页 ──────────────────────────────────────

func TestListEntriesPagination(t *testing.T) {
	svc, db := newTestService(t)
	ctx := context.Background()
	userID := testutil.CreateTestUser(t, db)

	const total = 7
	for i := range total {
		if _, err := svc.Recharge(ctx, ChangeInput{
			UserID: userID, Amount: 100, BizType: "recharge_order",
			BizNo: fmt.Sprintf("R%03d", i),
		}); err != nil {
			t.Fatalf("第 %d 次充值失败: %v", i, err)
		}
	}

	seen := make(map[int64]bool)
	cursor := int64(0)
	pages := 0

	for {
		page, err := svc.ListEntries(ctx, userID, EntryFilter{Cursor: cursor, Limit: 3})
		if err != nil {
			t.Fatalf("查询账本失败: %v", err)
		}
		pages++
		if pages > total {
			t.Fatal("分页没有终止，可能游标没有推进")
		}

		for _, entry := range page.Items {
			if seen[entry.ID] {
				t.Errorf("流水 %d 在分页中出现了两次", entry.ID)
			}
			seen[entry.ID] = true
		}
		if page.NextCursor == 0 {
			break
		}
		cursor = page.NextCursor
	}

	if len(seen) != total {
		t.Errorf("分页应覆盖全部 %d 条流水，实际只取到 %d 条", total, len(seen))
	}
	if pages != 3 { // 3 + 3 + 1
		t.Errorf("7 条流水按每页 3 条应分 3 页，实际 %d 页", pages)
	}
}

func TestListEntriesIsNewestFirst(t *testing.T) {
	svc, db := newTestService(t)
	ctx := context.Background()
	userID := testutil.CreateTestUser(t, db)

	for i := range 3 {
		if _, err := svc.Recharge(ctx, ChangeInput{
			UserID: userID, Amount: 100, BizType: "recharge_order",
			BizNo: fmt.Sprintf("R%03d", i),
		}); err != nil {
			t.Fatalf("第 %d 次充值失败: %v", i, err)
		}
	}

	page, err := svc.ListEntries(ctx, userID, EntryFilter{Limit: 10})
	if err != nil {
		t.Fatalf("查询账本失败: %v", err)
	}
	if len(page.Items) != 3 {
		t.Fatalf("应有 3 条流水，实际 %d 条", len(page.Items))
	}
	for i := 1; i < len(page.Items); i++ {
		if page.Items[i-1].ID <= page.Items[i].ID {
			t.Errorf("流水应按号倒序，实际 %d 出现在 %d 之前",
				page.Items[i-1].ID, page.Items[i].ID)
		}
	}
	// 不足一页时不应给出游标，否则前端会一直请求下去
	if page.NextCursor != 0 {
		t.Errorf("不足一页时 NextCursor 应为 0，实际 %d", page.NextCursor)
	}
}

// 每笔流水都带记账后的余额快照，最后一条的快照必须与账户当前余额一致。
// 快照是排查问题时唯一的独立参照，写错了等于把排查工具本身弄坏。
func TestEntrySnapshotsMatchAccount(t *testing.T) {
	svc, db := newTestService(t)
	ctx := context.Background()
	userID := testutil.CreateTestUser(t, db)

	mustRecharge(t, svc, userID, 10000)
	if _, err := svc.Freeze(ctx, ChangeInput{
		UserID: userID, Amount: 3000, BizType: "cert_order", BizNo: "O1",
	}); err != nil {
		t.Fatalf("冻结失败: %v", err)
	}
	if _, err := svc.Settle(ctx, ChangeInput{
		UserID: userID, Amount: 1000, BizType: "cert_order", BizNo: "O1",
	}); err != nil {
		t.Fatalf("结算失败: %v", err)
	}

	page, err := svc.ListEntries(ctx, userID, EntryFilter{Limit: 10})
	if err != nil {
		t.Fatalf("查询账本失败: %v", err)
	}
	latest := page.Items[0]

	account := mustGetAccount(t, svc, userID)
	if latest.AvailableAfter != account.AvailableBalance || latest.FrozenAfter != account.FrozenBalance {
		t.Errorf("最新流水的快照（可用 %d / 冻结 %d）与账户余额（可用 %d / 冻结 %d）不一致",
			latest.AvailableAfter, latest.FrozenAfter,
			account.AvailableBalance, account.FrozenBalance)
	}
}

// ── 工具 ──────────────────────────────────────────

func mustRecharge(t *testing.T, svc *Service, userID int64, amount money.Amount) {
	t.Helper()
	_, err := svc.Recharge(context.Background(), ChangeInput{
		UserID: userID, Amount: amount,
		BizType: "recharge_order", BizNo: fmt.Sprintf("R-init-%d", amount),
	})
	if err != nil {
		t.Fatalf("充值 %d 分失败: %v", amount, err)
	}
}

// allEntries 取回某用户的全部流水，按流水号索引。
func allEntries(t *testing.T, svc *Service, userID int64) map[int64]Entry {
	t.Helper()
	page, err := svc.ListEntries(context.Background(), userID, EntryFilter{Limit: MaxEntryLimit})
	if err != nil {
		t.Fatalf("查询账本失败: %v", err)
	}
	if page.NextCursor != 0 {
		t.Fatalf("流水超过单页上限 %d 条，测试需要先分页", MaxEntryLimit)
	}

	out := make(map[int64]Entry, len(page.Items))
	for _, entry := range page.Items {
		out[entry.ID] = entry
	}
	return out
}

// entryFingerprint 把一条流水压成可比较的字符串。
// 用时间字符串而不是 time.Time 直接比较：Go 的 == 会带上时区与单调时钟，
// 从数据库读回来的值不满足这个相等性。
func entryFingerprint(e Entry) string {
	return strings.Join([]string{
		fmt.Sprint(e.ID),
		fmt.Sprint(e.AccountID),
		fmt.Sprint(e.UserID),
		e.EntryNo,
		string(e.Op),
		e.BizType,
		e.BizNo,
		fmt.Sprint(e.AvailableDelta),
		fmt.Sprint(e.FrozenDelta),
		fmt.Sprint(e.AvailableAfter),
		fmt.Sprint(e.FrozenAfter),
		e.Remark,
		e.CreatedAt.Format("2006-01-02T15:04:05Z07:00"),
	}, "|")
}
