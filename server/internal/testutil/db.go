// Package testutil 提供测试基础设施。
//
// 单独成包是为了让「需要真实数据库的测试」有一处统一的连接与清理逻辑，
// 而不是每个域包各写一遍。
//
// 为什么这些测试必须打真实数据库：钱包域的验收标准是「并发扣款不超扣」
// 「余额与账本累加值始终一致」「重复调用不重复入账」，三条都依赖
// 行锁、唯一索引、事务隔离级别的真实行为。用 mock 或内存库验证不了这些，
// 只能验证「代码按我设想的方式调用了接口」——而设想本身可能才是错的。
package testutil

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	// 驱动注册在 init 里，必须匿名导入
	_ "github.com/go-sql-driver/mysql"

	"github.com/cangerx/c-ssl/server/internal/config"
)

// pingTimeout 是测试库连通性探测的超时。
// 刻意很短：连不上就应该立刻跳过，而不是让每个测试都卡几秒。
const pingTimeout = 3 * time.Second

// OpenTestDB 连接测试库（MYSQL_TEST_DSN，指向独立的 c_ssl_test 库）。
//
// 连不上时调用 t.Skip 而不是 t.Fatal：本机没起 MySQL 的开发者不该因此看到一片红。
// 代价是依赖库没起来时测试会静默跳过，所以 CI 必须先做一次硬连通性检查
// （见 .github/workflows/ci.yml 的「校验测试库连通性」步骤）。
func OpenTestDB(t *testing.T) *sql.DB {
	t.Helper()

	// .env 在仓库根目录，而测试进程的工作目录是各自的包目录，需要向上查找
	if _, err := config.LoadDotEnv(); err != nil {
		t.Fatalf("加载 .env 失败: %v", err)
	}

	dsn := os.Getenv("MYSQL_TEST_DSN")
	if dsn == "" {
		t.Skip("未配置 MYSQL_TEST_DSN，跳过需要数据库的测试")
	}

	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), pingTimeout)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		t.Skipf("测试库不可用（%v），跳过需要数据库的测试", err)
	}
	return db
}

// CreateTestUser 插入一个测试用户，返回其 ID，并注册清理。
//
// 邮箱带上纳秒时间戳与序号，保证并发运行的测试之间不会撞唯一索引。
func CreateTestUser(t *testing.T, db *sql.DB) int64 {
	t.Helper()

	email := fmt.Sprintf("wallet-test-%d@example.test", time.Now().UnixNano())
	result, err := db.Exec(
		`INSERT INTO users (email, password_hash, nickname) VALUES (?, ?, ?)`,
		email, "$argon2id$v=19$m=65536,t=3,p=4$test$test", "测试用户")
	if err != nil {
		t.Fatalf("创建测试用户失败: %v", err)
	}

	id, err := result.LastInsertId()
	if err != nil {
		t.Fatalf("获取测试用户 ID 失败: %v", err)
	}
	t.Cleanup(func() { CleanupUser(t, db, id) })
	return id
}

// CleanupUser 清理测试用户及其资金数据。
//
// 导出是为了让「用户由被测代码创建」的测试也能清理干净
// （例如走 HTTP 注册接口建用户的路由级测试）。
//
// 删除顺序是被外键定死的，不能随意调整：
//
//	payment_transactions → recharge_orders → wallet_ledger → wallet_accounts
//	→ user_sessions → users
//
// payment_transactions 对 recharge_orders 是 ON DELETE RESTRICT，
// 而 recharge_orders 与 wallet_ledger 的父行也都不允许在有子行时被删。
// 顺序错了会得到外键错误，清理不干净会污染后续测试。
//
// 这里直接删除账本流水，仅因为这是测试库、且目的是不留下垃圾数据。
// 生产环境的账本只追加、永不删除，这条规则由 wallet.Repository 的 API 面保证
// （它不提供任何更新或删除账本的方法）。
func CleanupUser(t *testing.T, db *sql.DB, userID int64) {
	t.Helper()

	statements := []string{
		`DELETE FROM payment_transactions WHERE user_id = ?`,
		`DELETE FROM recharge_orders WHERE user_id = ?`,
		`DELETE FROM wallet_ledger WHERE user_id = ?`,
		`DELETE FROM wallet_accounts WHERE user_id = ?`,
		`DELETE FROM user_sessions WHERE user_id = ?`,
		`DELETE FROM users WHERE id = ?`,
	}
	for _, stmt := range statements {
		if _, err := db.Exec(stmt, userID); err != nil {
			// 清理失败不应让测试结论失真，但必须留下痕迹
			t.Logf("清理测试数据失败（%s）: %v", stmt, err)
		}
	}
}
