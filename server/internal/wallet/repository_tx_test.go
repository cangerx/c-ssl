package wallet

import (
	"context"
	"errors"
	"testing"

	"github.com/cangerx/c-ssl/server/internal/platform/tx"
	"github.com/cangerx/c-ssl/server/internal/testutil"
)

// 本文件验证钱包能被跨域事务「包裹」。
//
// 场景来自支付回调：改充值单状态、给钱包加款、写账本必须原子完成。
// 钱包域自己开事务就做不到——事务边界必须由发起方（payment 域）持有。
//
// 被验证的保护点是 Repository 的所有读写都经过 tx.Of(ctx, r.db) 取执行器，
// 而不是直接用 r.db。下面每个测试都是「把保护去掉就会失败」的形式：
// 若 apply 直接用 r.db，充值会独立提交，外层回滚就抹不掉它，
// TestApplyRollsBackWithCallerTransaction 立刻会红。

func TestApplyRollsBackWithCallerTransaction(t *testing.T) {
	svc, db := newTestService(t)
	manager := tx.NewManager(db)
	userID := testutil.CreateTestUser(t, db)
	ctx := context.Background()

	// 模拟「加款成功但后续步骤失败」：整个事务必须一起回滚。
	stepErr := errors.New("模拟后续步骤失败")
	err := manager.Run(ctx, func(ctx context.Context) error {
		if _, err := svc.Recharge(ctx, ChangeInput{
			UserID: userID, Amount: 10000,
			BizType: "recharge_order", BizNo: "R-rollback",
		}); err != nil {
			return err
		}
		return stepErr
	})
	if !errors.Is(err, stepErr) {
		t.Fatalf("外层错误应被原样返回，实际: %v", err)
	}

	assertBalances(t, svc, userID, 0, 0)
	if n := countEntries(t, db, userID); n != 0 {
		t.Errorf("外层事务回滚后不该留下流水，实际 %d 条", n)
	}
	if n := countAccounts(t, db, userID); n != 0 {
		t.Errorf("外层事务回滚后不该留下账户，实际 %d 个", n)
	}
}

func TestApplyCommitsWithCallerTransaction(t *testing.T) {
	svc, db := newTestService(t)
	manager := tx.NewManager(db)
	userID := testutil.CreateTestUser(t, db)
	ctx := context.Background()

	err := manager.Run(ctx, func(ctx context.Context) error {
		_, err := svc.Recharge(ctx, ChangeInput{
			UserID: userID, Amount: 10000,
			BizType: "recharge_order", BizNo: "R-commit",
		})
		return err
	})
	if err != nil {
		t.Fatalf("外层事务提交失败: %v", err)
	}

	assertBalances(t, svc, userID, 10000, 0)
	if n := countEntries(t, db, userID); n != 1 {
		t.Errorf("期望 1 条流水，实际 %d 条", n)
	}
	assertConsistent(t, svc, userID)
}

// TestReadsInsideTransactionSeeUncommittedChanges 验证读路径也走事务。
//
// 这条容易漏：写路径用了事务、读路径直接用连接池，症状是
// 「加款后立刻对账，报告说账户里没钱」——而数据其实是对的，
// 只是读的是事务外的旧快照。
func TestReadsInsideTransactionSeeUncommittedChanges(t *testing.T) {
	svc, db := newTestService(t)
	manager := tx.NewManager(db)
	userID := testutil.CreateTestUser(t, db)
	ctx := context.Background()

	err := manager.Run(ctx, func(ctx context.Context) error {
		if _, err := svc.Recharge(ctx, ChangeInput{
			UserID: userID, Amount: 10000,
			BizType: "recharge_order", BizNo: "R-isolation",
		}); err != nil {
			return err
		}

		// 事务内读：看得到自己刚写的余额与流水
		inside, err := svc.Get(ctx, userID)
		if err != nil {
			return err
		}
		if inside.AvailableBalance != 10000 {
			t.Errorf("事务内应看到未提交的余额 10000，实际 %d", inside.AvailableBalance)
		}

		page, err := svc.ListEntries(ctx, userID, EntryFilter{})
		if err != nil {
			return err
		}
		if len(page.Items) != 1 {
			t.Errorf("事务内应看到未提交的 1 条流水，实际 %d 条", len(page.Items))
		}

		// 事务外读（连接池的另一条连接）：看不到未提交的数据。
		// 这里刻意用 context.Background()，模拟一个与事务无关的并发读者。
		outside, err := svc.Get(context.Background(), userID)
		if err != nil {
			return err
		}
		if outside.AvailableBalance != 0 {
			t.Errorf("事务外不该看到未提交的余额，实际 %d", outside.AvailableBalance)
		}

		outsidePage, err := svc.ListEntries(context.Background(), userID, EntryFilter{})
		if err != nil {
			return err
		}
		if len(outsidePage.Items) != 0 {
			t.Errorf("事务外不该看到未提交的流水，实际 %d 条", len(outsidePage.Items))
		}

		return nil
	})
	if err != nil {
		t.Fatalf("事务执行失败: %v", err)
	}

	assertBalances(t, svc, userID, 10000, 0)
}

// TestNestedTransactionRunReusesOuter 验证内层 Run 不会自己提交。
//
// 这是跨域调用链上最容易出半提交的地方：内层如果开自己的事务并提交，
// 外层随后失败回滚，钱就已经落了库，而订单状态是失败的。
func TestNestedTransactionRunReusesOuter(t *testing.T) {
	svc, db := newTestService(t)
	manager := tx.NewManager(db)
	userID := testutil.CreateTestUser(t, db)
	ctx := context.Background()

	innerErr := errors.New("外层失败")
	innerRan := false

	err := manager.Run(ctx, func(ctx context.Context) error {
		// 内层 Run 应当复用外层事务，而不是自己提交一个
		if err := manager.Run(ctx, func(ctx context.Context) error {
			_, err := svc.Recharge(ctx, ChangeInput{
				UserID: userID, Amount: 10000,
				BizType: "recharge_order", BizNo: "R-nested",
			})
			return err
		}); err != nil {
			return err
		}
		innerRan = true
		return innerErr
	})
	if !errors.Is(err, innerErr) {
		t.Fatalf("外层错误应被原样返回，实际: %v", err)
	}
	if !innerRan {
		t.Fatal("内层 Run 应当成功返回——它不该替外层决定提交")
	}

	// 内层返回了 nil，但它的写入必须随外层回滚一起消失
	assertBalances(t, svc, userID, 0, 0)
	if n := countEntries(t, db, userID); n != 0 {
		t.Errorf("外层回滚后不该留下流水，实际 %d 条", n)
	}
}

// TestDuplicateInsideTransactionDoesNotCommitEarly 验证幂等重放在事务内也不提前提交。
//
// 旧实现的重放分支里有一句显式的 tx.Commit()，注释写的是「提交只是为了释放行锁」。
// 那句在独立调用时无害，被跨域事务包裹时却是致命的：它会把外层的
// 事务替调用方提交掉，外层后续失败再也回滚不了，订单状态与资金状态就此分叉。
//
// 现在 apply 拿到的是 tx.Executor 接口而不是 *sql.Tx，接口上没有 Commit，
// 这类回归在类型层面就写不出来了。这条测试是行为层的兜底。
//
// 注意测试必须在重放之前先做一笔真实变更，否则「提前提交」没有可观测的后果。
func TestDuplicateInsideTransactionDoesNotCommitEarly(t *testing.T) {
	svc, db := newTestService(t)
	manager := tx.NewManager(db)
	userID := testutil.CreateTestUser(t, db)
	ctx := context.Background()

	in := ChangeInput{
		UserID: userID, Amount: 10000,
		BizType: "recharge_order", BizNo: "R-replay",
	}
	// 先造一笔，让账户存在，并让下面的入参成为一次真正的重放
	if _, err := svc.Recharge(context.Background(), in); err != nil {
		t.Fatalf("首次充值失败: %v", err)
	}

	stepErr := errors.New("模拟后续步骤失败")
	err := manager.Run(ctx, func(ctx context.Context) error {
		// 事务内的第一笔真实变更：重放分支若提前提交，这笔变更会被一并提交，
		// 外层回滚再也抹不掉它——余额会停在 6000 而不是 10000。
		if _, err := svc.Consume(ctx, ChangeInput{
			UserID: userID, Amount: 4000,
			BizType: "order", BizNo: "O-early-commit",
		}); err != nil {
			return err
		}

		// 与首次调用完全相同的入参 → 走重放分支
		result, err := svc.Recharge(ctx, in)
		if err != nil {
			return err
		}
		if !result.Duplicated {
			t.Errorf("期望命中重放分支，实际 Duplicated=false")
		}
		return stepErr
	})
	if !errors.Is(err, stepErr) {
		t.Fatalf("外层错误应被原样返回，实际: %v", err)
	}

	// 事务回滚后：首次充值的 10000 应原封不动，扣款与重放都不留痕迹
	assertBalances(t, svc, userID, 10000, 0)
	if n := countEntries(t, db, userID); n != 1 {
		t.Errorf("回滚后应只剩首次充值的 1 条流水，实际 %d 条", n)
	}
	assertConsistent(t, svc, userID)
}

// TestApplyOutsideTransactionStillWorks 是上面几条的对照：
// 没有外层事务时，Apply 必须自己开事务并提交，否则所有现有调用方都会失效。
func TestApplyOutsideTransactionStillWorks(t *testing.T) {
	svc, db := newTestService(t)
	userID := testutil.CreateTestUser(t, db)

	if tx.InTransaction(context.Background()) {
		t.Fatal("干净的 context 里不该有事务")
	}

	mustRecharge(t, svc, userID, 10000)

	assertBalances(t, svc, userID, 10000, 0)
	if n := countEntries(t, db, userID); n != 1 {
		t.Errorf("期望 1 条流水，实际 %d 条", n)
	}
	assertConsistent(t, svc, userID)
}
