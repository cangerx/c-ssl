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

// CleanupUser 清理测试用户及其资金、订单数据。
//
// 导出是为了让「用户由被测代码创建」的测试也能清理干净
// （例如走 HTTP 注册接口建用户的路由级测试）。
//
// 删除顺序是被外键定死的，不能随意调整：
//
//	certificates / order_orgs / order_contacts / order_domains
//	→ certificate_orders → payment_transactions → recharge_orders
//	→ wallet_ledger → wallet_accounts → user_sessions → users
//
// 订单的六张表对外键都是 RESTRICT：订单与证书是财务与法律凭证，
// 不该因为一次误删而消失。子表必须在父表之前删，否则会得到外键错误。
//
// webhook_events 单独处理：它**没有 user_id**，也不对订单建外键
// （上游可能推送平台不认识的订单号，那些事件也必须记下来）。
// 因此只能先取出该用户的上游订单号，再按上游订单号反查删除——
// 这一步必须在删掉 certificate_orders 之前做，否则单号就找不到了。
//
// 这里直接删除账本流水与事件记录，仅因为这是测试库、且目的是不留下垃圾数据。
// 生产环境的账本是只追加、永不删除的，这条规则由 wallet.Repository 的 API 面保证
// （它不提供任何更新或删除账本的方法）。
func CleanupUser(t *testing.T, db *sql.DB, userID int64) {
	t.Helper()

	// 必须在删除订单之前取，删完就查不到了
	upstreamNos := upstreamOrderNos(t, db, userID)

	statements := []string{
		`DELETE FROM certificates WHERE order_no IN
		   (SELECT order_no FROM certificate_orders WHERE user_id = ?)`,
		`DELETE FROM order_orgs WHERE order_no IN
		   (SELECT order_no FROM certificate_orders WHERE user_id = ?)`,
		`DELETE FROM order_contacts WHERE order_no IN
		   (SELECT order_no FROM certificate_orders WHERE user_id = ?)`,
		`DELETE FROM order_domains WHERE order_no IN
		   (SELECT order_no FROM certificate_orders WHERE user_id = ?)`,
		`DELETE FROM certificate_orders WHERE user_id = ?`,
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

	for _, no := range upstreamNos {
		if _, err := db.Exec(
			`DELETE FROM webhook_events WHERE upstream_order_no = ?`, no); err != nil {
			t.Logf("清理上游事件失败（%s）: %v", no, err)
		}
	}
}

// upstreamOrderNos 取出某个用户全部订单的上游订单号。
//
// 清理上游事件用。查询失败时返回 nil 并记日志：清理不彻底只会在
// 测试库里留下垃圾，不该让一个本来通过的测试变红。
func upstreamOrderNos(t *testing.T, db *sql.DB, userID int64) []string {
	t.Helper()

	rows, err := db.Query(
		`SELECT upstream_order_no FROM certificate_orders
		 WHERE user_id = ? AND upstream_order_no IS NOT NULL`, userID)
	if err != nil {
		t.Logf("查询待清理的上游订单号失败: %v", err)
		return nil
	}
	defer func() { _ = rows.Close() }()

	var out []string
	for rows.Next() {
		var no string
		if err := rows.Scan(&no); err != nil {
			t.Logf("读取上游订单号失败: %v", err)
			return out
		}
		out = append(out, no)
	}
	if err := rows.Err(); err != nil {
		t.Logf("遍历上游订单号失败: %v", err)
	}
	return out
}
