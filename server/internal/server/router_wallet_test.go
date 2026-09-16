package server

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/cangerx/c-ssl/server/internal/config"
	"github.com/cangerx/c-ssl/server/internal/domain/money"
	"github.com/cangerx/c-ssl/server/internal/testutil"
	"github.com/cangerx/c-ssl/server/internal/wallet"
)

// 本文件走真实 HTTP 验证钱包接口的接线。
//
// 钱包域的单测覆盖了业务规则，但覆盖不到「路由挂对了没有」「鉴权中间件套上了没有」
// 「DTO 字段名与契约一致没有」——这三件事只有把请求真的打进去才能验证。
// 忘记给一组路由套 auth 是这类改动里最典型的失误，而它的后果是余额裸奔。
//
// Redis 传 nil：ratelimit.New(nil) 会放行所有请求，
// 这样本测试只依赖 MySQL，不必为了跑一条路由断言再起一个 Redis。

func newWalletTestServer(t *testing.T) (*httptest.Server, *sql.DB) {
	t.Helper()

	db := testutil.OpenTestDB(t)
	cfg := &config.Config{
		AppEnv:        "test",
		AppPort:       "0",
		JWTSecret:     "wallet-integration-test-secret",
		JWTAccessTTL:  15 * time.Minute,
		JWTRefreshTTL: time.Hour,
	}

	srv := httptest.NewServer(NewRouter(Deps{Config: cfg, DB: db, Version: "test"}))
	t.Cleanup(srv.Close)
	return srv, db
}

// ── HTTP 客户端 ───────────────────────────────────

type apiClient struct {
	t     *testing.T
	base  string
	token string
}

func (c *apiClient) do(method, path string, body any) (int, map[string]any) {
	c.t.Helper()

	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			c.t.Fatalf("序列化请求体失败: %v", err)
		}
		reader = bytes.NewReader(raw)
	}

	req, err := http.NewRequest(method, c.base+"/api/v1"+path, reader)
	if err != nil {
		c.t.Fatalf("构造请求失败: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatalf("请求 %s %s 失败: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		c.t.Fatalf("读取响应失败: %v", err)
	}

	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		c.t.Fatalf("响应不是合法 JSON: %s", raw)
	}
	return resp.StatusCode, payload
}

func (c *apiClient) mustOK(method, path string, body any) map[string]any {
	c.t.Helper()
	status, payload := c.do(method, path, body)
	if status != http.StatusOK {
		c.t.Fatalf("%s %s 应返回 200，实际 %d：%v", method, path, status, payload)
	}
	if payload["code"].(float64) != 0 {
		c.t.Fatalf("%s %s 的 code 应为 0，实际 %v", method, path, payload["code"])
	}
	return payload
}

// registerUser 走注册接口建一个真实用户，返回客户端（已带令牌）与用户 ID。
func registerUser(t *testing.T, srv *httptest.Server, db *sql.DB) (*apiClient, int64) {
	t.Helper()

	client := &apiClient{t: t, base: srv.URL}
	email := fmt.Sprintf("wallet-http-%d@example.test", time.Now().UnixNano())

	payload := client.mustOK("POST", "/auth/register", map[string]any{
		"email":    email,
		"password": "correct-horse-1",
		"nickname": "钱包测试",
	})

	data, ok := payload["data"].(map[string]any)
	if !ok {
		t.Fatalf("注册响应缺少 data: %v", payload)
	}
	client.token, _ = data["accessToken"].(string)
	if client.token == "" {
		t.Fatal("注册响应没有 accessToken")
	}

	me := client.mustOK("GET", "/me", nil)
	userID := int64(me["data"].(map[string]any)["id"].(float64))

	// 用户由接口创建，清理得自己注册
	t.Cleanup(func() { testutil.CleanupUser(t, db, userID) })
	return client, userID
}

func asMap(t *testing.T, payload map[string]any) map[string]any {
	t.Helper()
	data, ok := payload["data"].(map[string]any)
	if !ok {
		t.Fatalf("响应缺少 data 对象: %v", payload)
	}
	return data
}

func number(t *testing.T, m map[string]any, key string) float64 {
	t.Helper()
	v, ok := m[key].(float64)
	if !ok {
		t.Fatalf("字段 %s 应为数值，实际 %T（%v）", key, m[key], m[key])
	}
	return v
}

// ── 测试 ──────────────────────────────────────────

// 钱包是用户私有数据，两个接口都必须要求登录。
// 这条断言是本文件存在的首要理由：忘记套鉴权中间件不会有任何编译期报错。
func TestWalletRequiresAuth(t *testing.T) {
	srv, _ := newWalletTestServer(t)
	client := &apiClient{t: t, base: srv.URL} // 刻意不带令牌

	for _, path := range []string{"/wallet", "/wallet/ledger"} {
		status, payload := client.do("GET", path, nil)
		if status != http.StatusUnauthorized {
			t.Errorf("未带令牌访问 %s 应返回 401，实际 %d", path, status)
		}
		if code := payload["code"].(float64); code != 1001 {
			t.Errorf("%s 的业务码应为 1001，实际 %v", path, code)
		}
	}
}

func TestWalletEmptyAccountOverHTTP(t *testing.T) {
	srv, db := newWalletTestServer(t)
	client, _ := registerUser(t, srv, db)

	account := asMap(t, client.mustOK("GET", "/wallet", nil))

	// 契约要求三个字段齐全，缺一个前端就拿到 undefined
	for _, key := range []string{"availableBalance", "frozenBalance", "totalBalance"} {
		if _, ok := account[key]; !ok {
			t.Errorf("响应缺少契约字段 %s", key)
		}
	}
	if len(account) != 3 {
		t.Errorf("响应字段应恰好为 3 个，实际 %d 个：%v", len(account), account)
	}

	for _, key := range []string{"availableBalance", "frozenBalance", "totalBalance"} {
		if got := number(t, account, key); got != 0 {
			t.Errorf("新用户 %s 应为 0，实际 %v", key, got)
		}
	}

	// 账本为空时 items 必须是 []，nextCursor 必须是 null
	ledger := asMap(t, client.mustOK("GET", "/wallet/ledger", nil))
	items, ok := ledger["items"].([]any)
	if !ok {
		t.Fatalf("items 应为数组，实际 %T（%v）", ledger["items"], ledger["items"])
	}
	if len(items) != 0 {
		t.Errorf("新用户账本应为空，实际 %d 条", len(items))
	}
	if ledger["nextCursor"] != nil {
		t.Errorf("无下一页时 nextCursor 应为 null，实际 %v", ledger["nextCursor"])
	}
}

// 通过真实的钱包服务写入数据，再走 HTTP 读出来，
// 验证整条链路（路由 → 鉴权 → 处理器 → 服务 → 仓储 → 数据库）是通的。
func TestWalletBalanceAndLedgerOverHTTP(t *testing.T) {
	srv, db := newWalletTestServer(t)
	client, userID := registerUser(t, srv, db)

	svc := wallet.NewService(wallet.NewRepository(db))
	ctx := t.Context()

	steps := []struct {
		name string
		run  func() error
	}{
		{"充值 100 元", func() error {
			_, err := svc.Recharge(ctx, wallet.ChangeInput{
				UserID: userID, Amount: 10000, BizType: "recharge_order", BizNo: "R-http-1",
			})
			return err
		}},
		{"下单冻结 30 元", func() error {
			_, err := svc.Freeze(ctx, wallet.ChangeInput{
				UserID: userID, Amount: 3000, BizType: "cert_order", BizNo: "O-http-1",
			})
			return err
		}},
		{"支付完成结算", func() error {
			_, err := svc.Settle(ctx, wallet.ChangeInput{
				UserID: userID, Amount: 3000, BizType: "cert_order", BizNo: "O-http-1",
			})
			return err
		}},
	}
	for _, step := range steps {
		if err := step.run(); err != nil {
			t.Fatalf("%s 失败: %v", step.name, err)
		}
	}

	// ── 余额 ──
	account := asMap(t, client.mustOK("GET", "/wallet", nil))
	if got := number(t, account, "availableBalance"); got != 7000 {
		t.Errorf("可用余额应为 7000 分，实际 %v", got)
	}
	if got := number(t, account, "frozenBalance"); got != 0 {
		t.Errorf("冻结余额应为 0 分，实际 %v", got)
	}
	if got := number(t, account, "totalBalance"); got != 7000 {
		t.Errorf("总余额应为 7000 分，实际 %v", got)
	}

	// ── 账本 ──
	ledger := asMap(t, client.mustOK("GET", "/wallet/ledger", nil))
	items, _ := ledger["items"].([]any)
	if len(items) != 3 {
		t.Fatalf("账本应有 3 条流水，实际 %d 条", len(items))
	}

	wantOps := []string{"settle", "freeze", "recharge"} // 倒序，最新的在最前
	for i, raw := range items {
		entry := raw.(map[string]any)

		if got := entry["op"]; got != wantOps[i] {
			t.Errorf("第 %d 条流水的 op 应为 %s，实际 %v", i+1, wantOps[i], got)
		}
		// 幂等键属于服务端实现细节，不应下发
		if _, ok := entry["entryNo"]; ok {
			t.Errorf("第 %d 条流水不应下发 entryNo", i+1)
		}
		for _, key := range []string{
			"id", "op", "bizType", "bizNo", "availableDelta", "frozenDelta",
			"availableAfter", "frozenAfter", "remark", "createdAt",
		} {
			if _, ok := entry[key]; !ok {
				t.Errorf("第 %d 条流水缺少契约字段 %s", i+1, key)
			}
		}
		if _, err := time.Parse(time.RFC3339, entry["createdAt"].(string)); err != nil {
			t.Errorf("第 %d 条流水的 createdAt 不是 RFC 3339：%v", i+1, entry["createdAt"])
		}
	}

	// 冻结那条要能看出「可用减少、冻结增加」这个方向
	freeze := items[1].(map[string]any)
	if number(t, freeze, "availableDelta") != -3000 || number(t, freeze, "frozenDelta") != 3000 {
		t.Errorf("冻结流水方向不正确：可用 %v，冻结 %v",
			freeze["availableDelta"], freeze["frozenDelta"])
	}

	// 最后一条（最早的充值）之后的余额快照
	recharge := items[2].(map[string]any)
	if number(t, recharge, "availableAfter") != 10000 {
		t.Errorf("充值后的可用余额快照应为 10000，实际 %v", recharge["availableAfter"])
	}
}

func TestWalletLedgerPaginationOverHTTP(t *testing.T) {
	srv, db := newWalletTestServer(t)
	client, userID := registerUser(t, srv, db)

	svc := wallet.NewService(wallet.NewRepository(db))
	for i := range 3 {
		if _, err := svc.Recharge(t.Context(), wallet.ChangeInput{
			UserID: userID, Amount: money.Amount(100),
			BizType: "recharge_order", BizNo: fmt.Sprintf("R-page-%d", i),
		}); err != nil {
			t.Fatalf("第 %d 次充值失败: %v", i, err)
		}
	}

	// 第一页
	page1 := asMap(t, client.mustOK("GET", "/wallet/ledger?limit=2", nil))
	items1, _ := page1["items"].([]any)
	if len(items1) != 2 {
		t.Fatalf("第一页应有 2 条，实际 %d 条", len(items1))
	}
	cursor, ok := page1["nextCursor"].(float64)
	if !ok {
		t.Fatalf("还有下一页时 nextCursor 应为数值，实际 %v", page1["nextCursor"])
	}

	// 第二页：按游标继续
	page2 := asMap(t, client.mustOK("GET", fmt.Sprintf("/wallet/ledger?limit=2&cursor=%d", int64(cursor)), nil))
	items2, _ := page2["items"].([]any)
	if len(items2) != 1 {
		t.Fatalf("第二页应有 1 条，实际 %d 条", len(items2))
	}
	if page2["nextCursor"] != nil {
		t.Errorf("末页 nextCursor 应为 null，实际 %v", page2["nextCursor"])
	}

	// 两页之间不能重复，也不能漏
	seen := map[float64]bool{}
	for _, raw := range append(items1, items2...) {
		id := number(t, raw.(map[string]any), "id")
		if seen[id] {
			t.Errorf("流水 %v 在两页中重复出现", id)
		}
		seen[id] = true
	}
	if len(seen) != 3 {
		t.Errorf("两页合计应覆盖 3 条流水，实际 %d 条", len(seen))
	}
}

func TestWalletLedgerRejectsBadParams(t *testing.T) {
	srv, db := newWalletTestServer(t)
	client, _ := registerUser(t, srv, db)

	cases := []struct {
		query string
		field string
	}{
		{"limit=abc", "limit"},
		{"limit=0", "limit"},
		{"limit=-5", "limit"},
		{"cursor=-1", "cursor"},
		{"cursor=abc", "cursor"},
	}

	for _, c := range cases {
		status, payload := client.do("GET", "/wallet/ledger?"+c.query, nil)
		if status != http.StatusBadRequest {
			t.Errorf("%s 应返回 400，实际 %d", c.query, status)
			continue
		}
		if code := payload["code"].(float64); code != 1000 {
			t.Errorf("%s 的业务码应为 1000，实际 %v", c.query, code)
		}
		fields := payload["data"].(map[string]any)["fields"].(map[string]any)
		if _, ok := fields[c.field]; !ok {
			t.Errorf("%s 应返回字段级提示 %s，实际 %v", c.query, c.field, fields)
		}
	}
}

// limit 超出上限应被截断为上限值，而不是报错：
// 客户端传大值只是想要更多数据，没必要让它失败，但服务端必须守住资源上限。
func TestWalletLedgerLimitIsCappedNotRejected(t *testing.T) {
	srv, db := newWalletTestServer(t)
	client, userID := registerUser(t, srv, db)

	svc := wallet.NewService(wallet.NewRepository(db))
	if _, err := svc.Recharge(t.Context(), wallet.ChangeInput{
		UserID: userID, Amount: 100, BizType: "recharge_order", BizNo: "R-cap",
	}); err != nil {
		t.Fatalf("充值失败: %v", err)
	}

	payload := client.mustOK("GET", "/wallet/ledger?limit=100000", nil)
	items, _ := asMap(t, payload)["items"].([]any)
	if len(items) != 1 {
		t.Errorf("超限的 limit 应被截断而不是拒绝，实际返回 %d 条", len(items))
	}
}
