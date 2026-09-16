package server

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/cangerx/c-ssl/server/internal/config"
	"github.com/cangerx/c-ssl/server/internal/testutil"
	"github.com/cangerx/c-ssl/server/internal/upstream/payment"
)

// 本文件走真实 HTTP 验证充值订单与支付回调的接线。
//
// 域内单测覆盖了业务规则，但覆盖不到这三件事，只有把请求真的打进去才能验证：
//
//   - 路由挂对了没有。尤其是回调接口：它**不能**套鉴权中间件（调用方是渠道
//     服务器而不是用户），但用户侧的两个接口**必须**套上。两者都在同一批路由里，
//     套错一个不会有编译期报错。
//   - 回调必须拿到**原始字节**才能验签。中间件、解码器或任何一次
//     「顺手解析一下」都会破坏签名输入，而这类改动看起来完全无害。
//   - DTO 字段名与契约一致没有。

const rechargeTestSecret = "recharge-http-test-secret-0123456789"

func newRechargeTestServer(t *testing.T) (*httptest.Server, *sql.DB, *payment.MockChannel) {
	t.Helper()

	db := testutil.OpenTestDB(t)

	ch, err := payment.NewMockChannel(rechargeTestSecret, "http://localhost:8080")
	if err != nil {
		t.Fatalf("构造 Mock 支付渠道失败: %v", err)
	}

	cfg := &config.Config{
		AppEnv:               "test",
		AppPort:              "0",
		AppBaseURL:           "http://localhost:8080",
		JWTSecret:            "recharge-integration-test-secret",
		JWTAccessTTL:         15 * time.Minute,
		JWTRefreshTTL:        time.Hour,
		PaymentProvider:      payment.NameMock,
		PaymentWebhookSecret: rechargeTestSecret,
	}

	srv := httptest.NewServer(NewRouter(Deps{
		Config:             cfg,
		DB:                 db,
		Version:            "test",
		PaymentChannels:    []payment.Channel{ch},
		MockPaymentChannel: ch,
	}))
	t.Cleanup(srv.Close)

	return srv, db, ch
}

// doRaw 发送原始字节并返回状态码与响应体。
//
// 回调接口需要它：用 json.Marshal 重新编码会改变字节，签名就对不上了。
// 测试必须像真实渠道那样，签什么字节就发什么字节。
func (c *apiClient) doRaw(method, path string, body []byte, headers map[string]string) (int, []byte) {
	c.t.Helper()

	req, err := http.NewRequest(method, c.base+"/api/v1"+path, bytes.NewReader(body))
	if err != nil {
		c.t.Fatalf("构造请求失败: %v", err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
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
	return resp.StatusCode, raw
}

// createOrder 走 HTTP 创建一张充值订单，返回订单号。
func (c *apiClient) createOrder(t *testing.T, amount int64) string {
	t.Helper()
	data := asMap(t, c.mustOK("POST", "/recharge/orders", map[string]any{"amount": amount}))
	orderNo, _ := data["orderNo"].(string)
	if orderNo == "" {
		t.Fatalf("创建订单响应缺少 orderNo: %v", data)
	}
	return orderNo
}

// availableBalance 走 HTTP 查询可用余额。
func (c *apiClient) availableBalance(t *testing.T) float64 {
	t.Helper()
	return number(t, asMap(t, c.mustOK("GET", "/wallet", nil)), "availableBalance")
}

// ── 鉴权 ──────────────────────────────────────────

// TestRechargeOrderEndpointsRequireAuth 验证用户侧接口都要求登录。
//
// 这条是本文件存在的首要理由之一：忘记套鉴权中间件不会有任何编译期报错，
// 后果是任何人都能创建充值订单、看到别人的订单列表。
func TestRechargeOrderEndpointsRequireAuth(t *testing.T) {
	srv, _, _ := newRechargeTestServer(t)
	client := &apiClient{t: t, base: srv.URL} // 刻意不带令牌

	cases := []struct {
		method string
		path   string
		body   any
	}{
		{"POST", "/recharge/orders", map[string]any{"amount": 10000}},
		{"GET", "/recharge/orders", nil},
		{"POST", "/payments/mock/notify", map[string]any{"orderNo": "RC1"}},
	}

	for _, tc := range cases {
		status, payload := client.do(tc.method, tc.path, tc.body)
		if status != http.StatusUnauthorized {
			t.Errorf("未带令牌访问 %s %s 应返回 401，实际 %d", tc.method, tc.path, status)
		}
		if code, _ := payload["code"].(float64); code != 1001 {
			t.Errorf("%s %s 的业务码应为 1001，实际 %v", tc.method, tc.path, payload["code"])
		}
	}
}

// TestWebhookDoesNotRequireAuth 验证回调接口不套鉴权。
//
// 与上一条相反的方向：回调的调用方是渠道服务器，它没有用户的令牌。
// 若把回调也套上鉴权中间件，所有真实回调都会 401——
// 表现为「用户付了钱但余额永远不到账」，而且渠道会一直重试。
// 身份凭证是签名，由适配器验证。
func TestWebhookDoesNotRequireAuth(t *testing.T) {
	srv, _, ch := newRechargeTestServer(t)
	client := &apiClient{t: t, base: srv.URL} // 刻意不带令牌

	raw, signature, err := ch.EncodeNotification(&payment.Notification{
		ChannelTradeNo: "TRADE-NOAUTH",
		OrderNo:        "RC00000000000000ZZZZZZ",
		Amount:         10000,
		Status:         payment.StatusSuccess,
	})
	if err != nil {
		t.Fatalf("编码回调报文失败: %v", err)
	}

	status, body := client.doRaw("POST", "/payments/webhook/mock", raw,
		map[string]string{"X-Webhook-Signature": signature})

	// 订单不存在，所以业务上会失败；但绝不是 401——
	// 401 意味着请求在鉴权环节就被挡住了，那样真实回调永远进不来。
	if status == http.StatusUnauthorized {
		t.Fatalf("回调接口不应要求令牌，实际返回 401: %s", body)
	}
	if status != http.StatusNotFound {
		t.Errorf("订单不存在应返回 404，实际 %d: %s", status, body)
	}
}

// ── 完整流程 ──────────────────────────────────────

// TestRechargeOrderLifecycleOverHTTP 走完「下单 → 支付 → 到账 → 查询」。
//
// 中间刻意插了一次「支付前查余额」，把「创建订单不动钱」这条性质也钉住：
// 若创建即入账，用户不付款就能白拿余额。
func TestRechargeOrderLifecycleOverHTTP(t *testing.T) {
	srv, db, ch := newRechargeTestServer(t)
	client, _ := registerUser(t, srv, db)

	orderNo := client.createOrder(t, 10000)

	if got := client.availableBalance(t); got != 0 {
		t.Fatalf("支付前余额应为 0，实际 %v", got)
	}

	// 渠道投递回调（走真实 webhook，含验签）
	raw, signature, err := ch.EncodeNotification(&payment.Notification{
		ChannelTradeNo: "TRADE-LIFECYCLE",
		OrderNo:        orderNo,
		Amount:         10000,
		Status:         payment.StatusSuccess,
		PaidAt:         time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("编码回调报文失败: %v", err)
	}

	status, body := client.doRaw("POST", "/payments/webhook/mock", raw,
		map[string]string{"X-Webhook-Signature": signature})
	if status != http.StatusOK {
		t.Fatalf("回调应返回 200，实际 %d: %s", status, body)
	}

	if got := client.availableBalance(t); got != 10000 {
		t.Errorf("支付后余额应为 10000，实际 %v", got)
	}

	// 订单状态与账本
	items := asMap(t, client.mustOK("GET", "/recharge/orders", nil))["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("应有 1 张订单，实际 %d 张", len(items))
	}
	order := items[0].(map[string]any)
	if order["status"] != "paid" {
		t.Errorf("订单状态应为 paid，实际 %v", order["status"])
	}
	if order["paidAt"] == nil {
		t.Error("已支付订单应有 paidAt")
	}

	entries := asMap(t, client.mustOK("GET", "/wallet/ledger", nil))["items"].([]any)
	if len(entries) != 1 {
		t.Fatalf("账本应有 1 条流水，实际 %d 条", len(entries))
	}
	entry := entries[0].(map[string]any)
	if entry["op"] != "recharge" {
		t.Errorf("流水类型应为 recharge，实际 %v", entry["op"])
	}
	if entry["bizNo"] != orderNo {
		t.Errorf("流水的业务单号应指向充值单 %s，实际 %v", orderNo, entry["bizNo"])
	}

}

// TestWebhookIsIdempotentOverHTTP 是验收标准第 2 条的 HTTP 级验证。
//
// 渠道重试时会原样重发同一份报文，因此这里也原样重发同一份字节与签名。
func TestWebhookIsIdempotentOverHTTP(t *testing.T) {
	srv, db, ch := newRechargeTestServer(t)
	client, _ := registerUser(t, srv, db)

	orderNo := client.createOrder(t, 10000)

	raw, signature, err := ch.EncodeNotification(&payment.Notification{
		ChannelTradeNo: "TRADE-IDEMPOTENT",
		OrderNo:        orderNo,
		Amount:         10000,
		Status:         payment.StatusSuccess,
		PaidAt:         time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("编码回调报文失败: %v", err)
	}

	for i := range 3 {
		status, body := client.doRaw("POST", "/payments/webhook/mock", raw,
			map[string]string{"X-Webhook-Signature": signature})
		if status != http.StatusOK {
			t.Fatalf("第 %d 次回调应返回 200（重放要返回成功让渠道停止重试），实际 %d: %s",
				i+1, status, body)
		}
	}

	if got := client.availableBalance(t); got != 10000 {
		t.Errorf("重复回调后余额应仍为 10000，实际 %v", got)
	}
	entries := asMap(t, client.mustOK("GET", "/wallet/ledger", nil))["items"].([]any)
	if len(entries) != 1 {
		t.Errorf("账本应只有 1 条流水，实际 %d 条", len(entries))
	}
}

// ── 验收标准第 7 条：验签失败不修改订单状态 ────────

// TestWebhookRejectsBadSignatureOverHTTP 是验收标准第 7 条的 HTTP 级验证。
//
// 断言的是「订单状态与余额都没变」，而不只是「返回了 401」——
// 只断言状态码的话，一个「先入账再返回 401」的实现同样能通过。
func TestWebhookRejectsBadSignatureOverHTTP(t *testing.T) {
	srv, db, ch := newRechargeTestServer(t)
	client, _ := registerUser(t, srv, db)

	orderNo := client.createOrder(t, 10000)

	raw, goodSignature, err := ch.EncodeNotification(&payment.Notification{
		ChannelTradeNo: "TRADE-BADSIG",
		OrderNo:        orderNo,
		Amount:         10000,
		Status:         payment.StatusSuccess,
	})
	if err != nil {
		t.Fatalf("编码回调报文失败: %v", err)
	}

	cases := map[string]string{
		"缺少签名头": "",
		"签名被篡改": goodSignature[:len(goodSignature)-2] + "AB",
	}
	for name, signature := range cases {
		t.Run(name, func(t *testing.T) {
			headers := map[string]string{}
			if signature != "" {
				headers["X-Webhook-Signature"] = signature
			}

			status, body := client.doRaw("POST", "/payments/webhook/mock", raw, headers)
			if status != http.StatusUnauthorized {
				t.Fatalf("验签失败应返回 401，实际 %d: %s", status, body)
			}

			var payload map[string]any
			if err := json.Unmarshal(body, &payload); err != nil {
				t.Fatalf("响应不是合法 JSON: %s", body)
			}
			if code, _ := payload["code"].(float64); code != 1001 {
				t.Errorf("业务码应为 1001，实际 %v", payload["code"])
			}

			// 订单与余额都必须原封不动
			items := asMap(t, client.mustOK("GET", "/recharge/orders", nil))["items"].([]any)
			if got := items[0].(map[string]any)["status"]; got != "pending" {
				t.Errorf("验签失败不应改变订单状态，实际 %s", got)
			}
			if got := client.availableBalance(t); got != 0 {
				t.Errorf("验签失败不应改变余额，实际 %v", got)
			}
		})
	}
}

// TestWebhookRejectsUnknownChannel 验证未注册的渠道返回 404 而不是 500。
func TestWebhookRejectsUnknownChannel(t *testing.T) {
	srv, db, _ := newRechargeTestServer(t)
	client, _ := registerUser(t, srv, db)

	status, body := client.doRaw("POST", "/payments/webhook/no-such-channel",
		[]byte(`{}`), map[string]string{"X-Webhook-Signature": "whatever"})
	if status != http.StatusNotFound {
		t.Errorf("未注册渠道应返回 404，实际 %d: %s", status, body)
	}
}

// ── 创建订单的参数校验 ────────────────────────────

func TestCreateRechargeOrderRejectsBadAmount(t *testing.T) {
	srv, db, _ := newRechargeTestServer(t)
	client, _ := registerUser(t, srv, db)

	for _, amount := range []int64{0, -100, 99, 10_000_001} {
		status, payload := client.do("POST", "/recharge/orders", map[string]any{"amount": amount})
		if status != http.StatusBadRequest {
			t.Errorf("金额 %d 应返回 400，实际 %d：%v", amount, status, payload)
			continue
		}
		if code, _ := payload["code"].(float64); code != 1000 {
			t.Errorf("金额 %d 的业务码应为 1000，实际 %v", amount, payload["code"])
		}
		data, _ := payload["data"].(map[string]any)
		fields, _ := data["fields"].(map[string]any)
		if _, ok := fields["amount"]; !ok {
			t.Errorf("金额 %d 的错误应带 amount 字段详情，实际 %v", amount, payload["data"])
		}
	}
}

func TestCreateRechargeOrderRejectsUnknownChannel(t *testing.T) {
	srv, db, _ := newRechargeTestServer(t)
	client, _ := registerUser(t, srv, db)

	status, payload := client.do("POST", "/recharge/orders",
		map[string]any{"amount": 10000, "channel": "no-such-channel"})
	if status != http.StatusBadRequest {
		t.Errorf("未知渠道应返回 400，实际 %d：%v", status, payload)
	}
}

// ── 开发环境的模拟回调 ────────────────────────────

// TestMockNotifyCreditsWallet 验证模拟回调走的是完整路径。
//
// 它自己签名、自己验签，因此既能验证正常入账，
// 也保证开发环境的手工验证与真实回调不会分叉。
func TestMockNotifyCreditsWallet(t *testing.T) {
	srv, db, _ := newRechargeTestServer(t)
	client, _ := registerUser(t, srv, db)

	orderNo := client.createOrder(t, 20000)

	client.mustOK("POST", "/payments/mock/notify", map[string]any{
		"orderNo": orderNo, "status": "success",
	})

	if got := client.availableBalance(t); got != 20000 {
		t.Errorf("余额应为 20000，实际 %v", got)
	}
}

// TestMockNotifyIsIdempotent 验证显式传入同一交易号时只入账一次。
//
// 这是「充值回调重复提交不会重复入账」这条验收标准最容易被人工复现的形式，
// 也是开发时最可能被误判成 bug 的行为（第二次调用返回成功但余额没变）。
func TestMockNotifyIsIdempotent(t *testing.T) {
	srv, db, _ := newRechargeTestServer(t)
	client, _ := registerUser(t, srv, db)

	orderNo := client.createOrder(t, 30000)

	body := map[string]any{
		"orderNo": orderNo, "status": "success", "channelTradeNo": "MOCK-TRADE-FIXED",
	}
	for i := range 3 {
		client.mustOK("POST", "/payments/mock/notify", body)
		if got := client.availableBalance(t); got != 30000 {
			t.Fatalf("第 %d 次模拟回调后余额应仍为 30000，实际 %v", i+1, got)
		}
	}
}

// TestMockNotifyOnlyForOwnOrders 验证模拟回调不能给别人充值。
//
// 这个接口只在开发环境注册，但「开发辅助接口成为越权入口」是最典型的一类
// 环境泄漏：一旦它在某个环境里被启用，任何人都能给任意订单付款。
func TestMockNotifyOnlyForOwnOrders(t *testing.T) {
	srv, db, _ := newRechargeTestServer(t)

	victim, _ := registerUser(t, srv, db)
	orderNo := victim.createOrder(t, 10000)

	intruder, _ := registerUser(t, srv, db)
	status, payload := intruder.do("POST", "/payments/mock/notify",
		map[string]any{"orderNo": orderNo, "status": "success"})

	if status != http.StatusNotFound {
		t.Errorf("越权模拟回调应返回 404（不暴露订单是否存在），实际 %d：%v", status, payload)
	}
	if got := intruder.availableBalance(t); got != 0 {
		t.Errorf("越权调用不应给调用者充值，实际 %v", got)
	}
	if got := victim.availableBalance(t); got != 0 {
		t.Errorf("越权调用不应给订单所有者充值，实际 %v", got)
	}
}

// TestMockNotifyIsNotRegisteredWithoutMockChannel 验证未启用 Mock 渠道时
// 模拟回调接口根本不存在。
//
// 断言 404 而不是「返回错误」：路由没注册与路由注册了但拒绝，
// 是两件不同的事。前者在攻击面上不存在，后者只是被挡住了。
func TestMockNotifyIsNotRegisteredWithoutMockChannel(t *testing.T) {
	srv, db := newWalletTestServer(t) // 不注入支付渠道
	client, _ := registerUser(t, srv, db)

	status, payload := client.do("POST", "/payments/mock/notify",
		map[string]any{"orderNo": "RC1", "status": "success"})
	if status != http.StatusNotFound {
		t.Errorf("未启用 Mock 渠道时该接口应返回 404，实际 %d：%v", status, payload)
	}
}

// TestMockNotifyRejectsBadStatus 验证只接受契约里列出的两种支付结果。
func TestMockNotifyRejectsBadStatus(t *testing.T) {
	srv, db, _ := newRechargeTestServer(t)
	client, _ := registerUser(t, srv, db)

	orderNo := client.createOrder(t, 10000)

	status, payload := client.do("POST", "/payments/mock/notify",
		map[string]any{"orderNo": orderNo, "status": "refunded"})
	if status != http.StatusBadRequest {
		t.Errorf("未知支付结果应返回 400，实际 %d：%v", status, payload)
	}
	if got := client.availableBalance(t); got != 0 {
		t.Errorf("余额不应变化，实际 %v", got)
	}
}

// ── 列表与分页 ────────────────────────────────────

func TestRechargeOrderListIsNewestFirstOverHTTP(t *testing.T) {
	srv, db, _ := newRechargeTestServer(t)
	client, _ := registerUser(t, srv, db)

	first := client.createOrder(t, 1000)
	second := client.createOrder(t, 2000)
	third := client.createOrder(t, 3000)

	items := asMap(t, client.mustOK("GET", "/recharge/orders", nil))["items"].([]any)
	if len(items) != 3 {
		t.Fatalf("应有 3 张订单，实际 %d 张", len(items))
	}

	want := []string{third, second, first}
	for i, order := range items {
		if got := order.(map[string]any)["orderNo"]; got != want[i] {
			t.Errorf("第 %d 张订单应为 %v，实际 %v", i, want[i], got)
		}
	}
}

func TestRechargeOrderListPaginationOverHTTP(t *testing.T) {
	srv, db, _ := newRechargeTestServer(t)
	client, _ := registerUser(t, srv, db)

	const total = 5
	for range total {
		client.createOrder(t, 1000)
	}

	seen := make(map[string]bool)
	cursor := 0
	for {
		path := "/recharge/orders?limit=2"
		if cursor != 0 {
			path += "&cursor=" + strconv.Itoa(cursor)
		}
		data := asMap(t, client.mustOK("GET", path, nil))
		items := data["items"].([]any)
		for _, item := range items {
			orderNo := item.(map[string]any)["orderNo"].(string)
			if seen[orderNo] {
				t.Fatalf("翻页出现重复订单: %s", orderNo)
			}
			seen[orderNo] = true
		}

		next, ok := data["nextCursor"]
		if !ok || next == nil {
			break
		}
		cursor = int(next.(float64))
	}

	if len(seen) != total {
		t.Errorf("翻页共应看到 %d 张订单，实际 %d 张", total, len(seen))
	}
}

func TestRechargeOrderListRejectsBadParams(t *testing.T) {
	srv, db, _ := newRechargeTestServer(t)
	client, _ := registerUser(t, srv, db)

	for _, path := range []string{
		"/recharge/orders?limit=abc",
		"/recharge/orders?limit=0",
		"/recharge/orders?limit=-1",
		"/recharge/orders?cursor=abc",
		"/recharge/orders?cursor=-1",
	} {
		status, payload := client.do("GET", path, nil)
		if status != http.StatusBadRequest {
			t.Errorf("%s 应返回 400，实际 %d：%v", path, status, payload)
		}
	}
}

// TestRechargeOrderListLimitIsCappedNotRejected 验证超上限被截断而不是报错。
//
// 与钱包账本接口保持一致：客户端传大值只是想要更多数据，
// 没必要让它失败，但服务端必须守住资源上限。
func TestRechargeOrderListLimitIsCappedNotRejected(t *testing.T) {
	srv, db, _ := newRechargeTestServer(t)
	client, _ := registerUser(t, srv, db)

	client.createOrder(t, 1000)

	status, payload := client.do("GET", "/recharge/orders?limit=99999", nil)
	if status != http.StatusOK {
		t.Fatalf("超上限的 limit 应被截断而不是报错，实际 %d：%v", status, payload)
	}
	if code, _ := payload["code"].(float64); code != 0 {
		t.Errorf("业务码应为 0，实际 %v", payload["code"])
	}
}

// ── DTO 与契约一致性 ──────────────────────────────

// TestRechargeOrderDTOMatchesContract 验证下发的字段与契约严格一致。
//
// 两个方向都要查：
//   - 契约要求的字段必须在，否则前端拿到 undefined；
//   - 内部字段不能出现。渠道交易号这类标识一旦下发，就成了一个
//     外部可以引用、平台却无法校验的东西。
func TestRechargeOrderDTOMatchesContract(t *testing.T) {
	srv, db, _ := newRechargeTestServer(t)
	client, _ := registerUser(t, srv, db)

	client.createOrder(t, 10000)

	items := asMap(t, client.mustOK("GET", "/recharge/orders", nil))["items"].([]any)
	order := items[0].(map[string]any)

	for _, key := range []string{
		"orderNo", "amount", "status", "channel", "payUrl", "paidAt", "expiresAt", "createdAt",
	} {
		if _, ok := order[key]; !ok {
			t.Errorf("契约要求的字段 %s 缺失", key)
		}
	}

	for _, key := range []string{"id", "userId", "channelOrderNo", "channelTradeNo", "updatedAt"} {
		if _, ok := order[key]; ok {
			t.Errorf("内部字段 %s 不应下发", key)
		}
	}

	if amount := number(t, order, "amount"); amount != 10000 {
		t.Errorf("金额应为整数分 10000，实际 %v", amount)
	}
	if status := order["status"].(string); status != "pending" {
		t.Errorf("新订单状态应为 pending，实际 %s", status)
	}
}
