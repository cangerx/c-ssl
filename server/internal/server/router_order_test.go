package server

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cangerx/c-ssl/server/internal/config"
	"github.com/cangerx/c-ssl/server/internal/testutil"
	"github.com/cangerx/c-ssl/server/internal/upstream/foxssl"
	"github.com/cangerx/c-ssl/server/internal/upstream/payment"
)

// 本文件走真实 HTTP 验证订单域、域名验证与上游回调的接线。
//
// 域内单测覆盖了业务规则，但覆盖不到这四件事，只有把请求真的打进去才能验证：
//
//   - 路由挂对了没有。尤其是 `/webhooks/foxssl`：它**不能**套鉴权中间件
//     （调用方是 FoxSSL 服务器而不是用户），而用户侧的九个接口**必须**套上。
//     两者都在同一批路由里，套错一个不会有编译期报错。
//   - 回调的应答体必须是 `{"status":"success"}`，**不能**套本服务的统一信封。
//     套上之后上游会把它判成失败并无限重推，而本地日志一切正常。
//   - 回调必须拿到**原始字节**才能验签。任何一次「顺手解析一下」都会
//     破坏签名输入，而这类改动看起来完全无害。
//   - DTO 字段名与契约一致没有，以及成本价、上游原始状态这些内部字段没被带出去。

const (
	orderHTTPSecret = "order-http-test-secret-0123456789"
	// 迁移里种下的产品：AlphaSSL DV 单域名，零售价 29800
	orderHTTPProductID = 1
	orderHTTPRetail    = 29800
)

func newOrderTestServer(t *testing.T) (*httptest.Server, *sql.DB, *foxssl.MockClient) {
	t.Helper()
	return newOrderTestServerOpt(t, true)
}

// newOrderTestServerOpt 构造测试服务器，withMockUpstream 控制是否装配模拟上游。
//
// 关掉它用来验证生产装配：开发辅助接口不能出现在没装模拟上游的进程里。
// 只靠「路由注册代码里有个 if」是保证不了的，得真的少装一次跑一遍。
func newOrderTestServerOpt(
	t *testing.T, withMockUpstream bool,
) (*httptest.Server, *sql.DB, *foxssl.MockClient) {
	t.Helper()

	db := testutil.OpenTestDB(t)

	upstream, err := foxssl.NewMockClient("test-api-key", orderHTTPSecret, "http://localhost:8080")
	if err != nil {
		t.Fatalf("构造 Mock 上游失败: %v", err)
	}

	ch, err := payment.NewMockChannel(orderHTTPSecret, "http://localhost:8080")
	if err != nil {
		t.Fatalf("构造 Mock 支付渠道失败: %v", err)
	}

	cfg := &config.Config{
		AppEnv:               "test",
		AppPort:              "0",
		AppBaseURL:           "http://localhost:8080",
		JWTSecret:            "order-integration-test-secret",
		JWTAccessTTL:         15 * time.Minute,
		JWTRefreshTTL:        time.Hour,
		PaymentProvider:      payment.NameMock,
		PaymentWebhookSecret: orderHTTPSecret,
		FoxSSLProvider:       foxssl.NameMock,
		FoxSSLWebhookSecret:  orderHTTPSecret,
	}

	deps := Deps{
		Config:             cfg,
		DB:                 db,
		Version:            "test",
		PaymentChannels:    []payment.Channel{ch},
		MockPaymentChannel: ch,
		FoxSSLClient:       upstream,
	}
	if withMockUpstream {
		deps.MockFoxSSLClient = upstream
	}

	srv := httptest.NewServer(NewRouter(deps))
	t.Cleanup(srv.Close)

	return srv, db, upstream
}

// ── HTTP 辅助 ─────────────────────────────────────

// credit 走充值接口给用户充钱，返回充值订单号。
//
// 不直接 UPDATE 余额表：余额与账本必须一致，绕过去写会让
// 「下单后余额对不对」这条断言失去意义。
func (c *apiClient) credit(t *testing.T, amount int64) {
	t.Helper()
	data := asMap(t, c.mustOK("POST", "/recharge/orders", map[string]any{"amount": amount}))
	orderNo, _ := data["orderNo"].(string)
	if orderNo == "" {
		t.Fatalf("充值下单响应缺少 orderNo: %v", data)
	}
	c.mustOK("POST", "/payments/mock/notify", map[string]any{"orderNo": orderNo})
}

// createCertOrder 走 HTTP 下一张证书订单，返回订单详情。
func (c *apiClient) createCertOrder(t *testing.T, domains []string) map[string]any {
	t.Helper()

	body := map[string]any{
		"productId":    orderHTTPProductID,
		"years":        1,
		"keyAlgorithm": "rsa",
		"domains":      domains,
		"contact": map[string]any{
			"name": "张三", "email": "admin@example.com", "phone": "+86.13800138000",
		},
	}
	return asMap(t, c.mustOK("POST", "/orders", body))
}

// postWebhook 以原始字节投递一条上游事件。
//
// 必须用原始字节：用 json.Marshal 重新编码会改变字节，签名就对不上了。
// 测试要像真实上游那样，签什么字节就发什么字节。
func (c *apiClient) postWebhook(raw []byte, signature string) (int, []byte) {
	c.t.Helper()
	headers := map[string]string{}
	if signature != "" {
		headers["X-Webhook-Signature"] = signature
	}
	return c.doRaw("POST", "/webhooks/foxssl", raw, headers)
}

// ── 鉴权 ──────────────────────────────────────────

// TestOrderEndpointsRequireAuth 验证订单域的用户侧接口都要求登录。
//
// 这条是本文件存在的首要理由之一：忘记套鉴权中间件不会有任何编译期报错，
// 后果是任何人都能下单、看到别人的订单与域名验证材料、下载别人的证书。
func TestOrderEndpointsRequireAuth(t *testing.T) {
	srv, _, _ := newOrderTestServer(t)
	client := &apiClient{t: t, base: srv.URL} // 刻意不带令牌

	cases := []struct {
		method string
		path   string
		body   any
	}{
		{"POST", "/orders", map[string]any{"productId": 1}},
		{"GET", "/orders", nil},
		{"GET", "/orders/CS20260917120000AAAAAA", nil},
		{"POST", "/orders/CS20260917120000AAAAAA/cancel", map[string]any{"reason": "x"}},
		{"POST", "/orders/CS20260917120000AAAAAA/reissue", map[string]any{"reason": "x"}},
		{"GET", "/orders/CS20260917120000AAAAAA/domains", nil},
		{"POST", "/orders/CS20260917120000AAAAAA/domains/verify",
			map[string]any{"domains": []string{"example.com"}, "method": "dns_txt"}},
		{"POST", "/orders/CS20260917120000AAAAAA/domains/resend-email",
			map[string]any{"domains": []string{"example.com"}}},
		{"POST", "/orders/CS20260917120000AAAAAA/domains/regenerate-token", nil},
		{"GET", "/orders/CS20260917120000AAAAAA/certificate", nil},
		// 开发辅助接口同样要登录：一个能推进任意订单的接口，
		// 在开发环境里就是「免费签发任意证书」的入口。
		{"POST", "/orders/CS20260917120000AAAAAA/mock/issue", nil},
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

// TestWebhookDoesNotRequireAuth 验证上游回调不套鉴权。
//
// 与上一条相反的方向：回调的调用方是 FoxSSL 服务器，它没有用户的令牌。
// 若把回调也套上鉴权中间件，所有真实回调都会 401——
// 表现为「证书签出来了但订单永远停在等待验证」，而上游会一直重推。
// 身份凭证是签名，由适配器验证。
func TestFoxSSLWebhookDoesNotRequireAuth(t *testing.T) {
	srv, db, _ := newOrderTestServer(t)
	client := &apiClient{t: t, base: srv.URL} // 刻意不带令牌
	cleanupWebhookEvents(t, db, "FX-NOPE")

	raw, err := foxssl.EncodeNotification(&foxssl.Notification{
		EventType:       "order_status",
		UpstreamOrderNo: "FX-NOPE",
		Status:          "issued",
	})
	if err != nil {
		t.Fatalf("编码事件报文失败: %v", err)
	}

	status, body := client.postWebhook(raw, foxssl.Sign(orderHTTPSecret, raw))

	// 订单不存在，事件会被记下但处理失败；但绝不是 401——
	// 401 意味着请求在鉴权环节就被挡住了，那样真实回调永远进不来。
	if status == http.StatusUnauthorized {
		t.Fatalf("回调接口不应要求令牌，实际返回 401: %s", body)
	}
	if status != http.StatusOK {
		t.Errorf("回调应返回 200，实际 %d: %s", status, body)
	}
}

// ── 完整流程 ──────────────────────────────────────

// TestOrderLifecycleOverHTTP 走完「充值 → 下单 → 取验证材料 → 提交验证
// → 上游签发 → 回调 → 下载证书」。
//
// 这是本域唯一一条覆盖全链路的测试，价值在于它把各段之间的契约钉住：
// 上一步下发的字段正好是下一步要用的输入（订单号、域名、上游订单号）。
func TestOrderLifecycleOverHTTP(t *testing.T) {
	srv, db, upstream := newOrderTestServer(t)
	client, _ := registerUser(t, srv, db)

	client.credit(t, 100_000)

	// 下单：钱被实扣（上游受理即结算），冻结归零
	order := client.createCertOrder(t, []string{"example.com"})
	orderNo, _ := order["orderNo"].(string)
	if orderNo == "" {
		t.Fatalf("下单响应缺少 orderNo: %v", order)
	}
	if got := order["status"]; got != "waiting_dcv" {
		t.Fatalf("上游受理后状态应为 waiting_dcv，实际 %v", got)
	}
	if got := number(t, order, "amount"); got != orderHTTPRetail {
		t.Errorf("订单金额应为零售价 %d，实际 %v", orderHTTPRetail, got)
	}

	wallet := asMap(t, client.mustOK("GET", "/wallet", nil))
	if got := number(t, wallet, "availableBalance"); got != 100_000-orderHTTPRetail {
		t.Errorf("可用余额应为 %d，实际 %v", 100_000-orderHTTPRetail, got)
	}
	if got := number(t, wallet, "frozenBalance"); got != 0 {
		t.Errorf("上游受理后冻结余额应为 0，实际 %v", got)
	}

	// 域名验证材料：路径必须是展开过的最终值
	domains := asMap(t, client.mustOK("GET", "/orders/"+orderNo+"/domains", nil))["items"].([]any)
	if len(domains) != 1 {
		t.Fatalf("应有 1 个域名，实际 %d 个", len(domains))
	}
	domain := domains[0].(map[string]any)
	if got, _ := domain["filePath"].(string); got == "" {
		t.Error("文件验证路径不应为空")
	} else {
		if strings.Contains(got, "{FQDN}") || strings.Contains(got, "*.") {
			t.Errorf("文件验证路径必须是展开后的最终值，实际 %q", got)
		}
	}
	methods, _ := domain["availableMethods"].([]any)
	if len(methods) == 0 {
		t.Error("可用验证方式不应为空——前端靠它渲染选项")
	}

	// 提交验证
	client.mustOK("POST", "/orders/"+orderNo+"/domains/verify", map[string]any{
		"domains": []string{"example.com"}, "method": "dns_txt",
	})

	// 上游签发后投递事件
	upstreamNo, _ := asMap(t, client.mustOK("GET", "/orders/"+orderNo, nil))["upstreamOrderNo"].(string)
	if upstreamNo == "" {
		t.Fatal("订单详情应包含 upstreamOrderNo")
	}
	if err := upstream.IssueCertificate(upstreamNo); err != nil {
		t.Fatalf("推进上游到已签发失败: %v", err)
	}

	raw, err := foxssl.EncodeNotification(&foxssl.Notification{
		EventType:       "certificate_issued",
		UpstreamOrderNo: upstreamNo,
		Status:          foxssl.MockStatusIssued,
		OccurredAt:      time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("编码事件报文失败: %v", err)
	}
	status, body := client.postWebhook(raw, foxssl.Sign(orderHTTPSecret, raw))
	if status != http.StatusOK {
		t.Fatalf("回调应返回 200，实际 %d: %s", status, body)
	}

	if got := asMap(t, client.mustOK("GET", "/orders/"+orderNo, nil))["status"]; got != "issued" {
		t.Fatalf("回调后订单状态应为 issued，实际 %v", got)
	}

	// 下载证书
	cert := asMap(t, client.mustOK("GET", "/orders/"+orderNo+"/certificate", nil))
	pem, _ := cert["certificate"].(string)
	if !strings.Contains(pem, "BEGIN CERTIFICATE") {
		t.Errorf("证书内容应是 PEM 文本，实际 %q", pem)
	}
	if got := cert["commonName"]; got != "example.com" {
		t.Errorf("证书主域名应为 example.com，实际 %v", got)
	}
}

// ── 回调的应答格式 ────────────────────────────────

// TestWebhookAckIsNotEnvelope 验证回调应答不套统一响应信封。
//
// 上游只认它自己文档里的 `{"status":"success"}`。包一层 code/message 会被
// 判成失败并触发无限重推——而本地日志一切正常，故障只在流量上体现出来。
func TestWebhookAckIsNotEnvelope(t *testing.T) {
	srv, db, _ := newOrderTestServer(t)
	client, _ := registerUser(t, srv, db)
	client.credit(t, 100_000)

	order := client.createCertOrder(t, []string{"example.com"})
	orderNo := order["orderNo"].(string)
	upstreamNo := asMap(t, client.mustOK("GET", "/orders/"+orderNo, nil))["upstreamOrderNo"].(string)

	raw, err := foxssl.EncodeNotification(&foxssl.Notification{
		EventType:       "order_status",
		UpstreamOrderNo: upstreamNo,
		Status:          foxssl.MockStatusIssuing,
		OccurredAt:      time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("编码事件报文失败: %v", err)
	}

	status, body := client.postWebhook(raw, foxssl.Sign(orderHTTPSecret, raw))
	if status != http.StatusOK {
		t.Fatalf("回调应返回 200，实际 %d: %s", status, body)
	}

	var ack map[string]any
	if err := json.Unmarshal(body, &ack); err != nil {
		t.Fatalf("应答不是合法 JSON: %s", body)
	}
	if ack["status"] != "success" {
		t.Errorf("应答应为 {\"status\":\"success\"}，实际 %s", body)
	}
	for _, key := range []string{"code", "message", "data"} {
		if _, ok := ack[key]; ok {
			t.Errorf("应答不应包含统一信封字段 %s，实际 %s", key, body)
		}
	}
}

// TestWebhookRejectsBadSignatureOverHTTP 验证验签失败返回 401 且不改动数据。
//
// 断言的是「订单状态没变」，而不只是「返回了 401」——
// 只断言状态码的话，一个「先把状态改了再返回 401」的实现同样能通过。
func TestFoxSSLWebhookRejectsBadSignatureOverHTTP(t *testing.T) {
	srv, db, _ := newOrderTestServer(t)
	client, _ := registerUser(t, srv, db)
	client.credit(t, 100_000)

	order := client.createCertOrder(t, []string{"example.com"})
	orderNo := order["orderNo"].(string)
	upstreamNo := asMap(t, client.mustOK("GET", "/orders/"+orderNo, nil))["upstreamOrderNo"].(string)

	raw, err := foxssl.EncodeNotification(&foxssl.Notification{
		EventType:       "certificate_issued",
		UpstreamOrderNo: upstreamNo,
		Status:          foxssl.MockStatusIssued,
		OccurredAt:      time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("编码事件报文失败: %v", err)
	}
	good := foxssl.Sign(orderHTTPSecret, raw)

	cases := map[string]string{
		"缺少签名头": "",
		"签名被篡改": good[:len(good)-2] + "AB",
	}
	for name, signature := range cases {
		t.Run(name, func(t *testing.T) {
			status, body := client.postWebhook(raw, signature)
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

			if got := asMap(t, client.mustOK("GET", "/orders/"+orderNo, nil))["status"]; got != "waiting_dcv" {
				t.Errorf("验签失败不应改变订单状态，实际 %v", got)
			}
		})
	}
}

// ── 越权 ──────────────────────────────────────────

// TestOrderDetailRejectsOtherUsersOrder 验证访问他人订单返回 404 而不是 403。
//
// 403 等于确认「这张单存在」，可以拿来做订单号枚举。
// 同一个 404 也用在「真的不存在」上，两种情况的响应必须完全一致。
func TestOrderDetailRejectsOtherUsersOrder(t *testing.T) {
	srv, db, _ := newOrderTestServer(t)

	victim, _ := registerUser(t, srv, db)
	victim.credit(t, 100_000)
	orderNo := victim.createCertOrder(t, []string{"example.com"})["orderNo"].(string)

	intruder, _ := registerUser(t, srv, db)

	for _, path := range []string{
		"/orders/" + orderNo,
		"/orders/" + orderNo + "/domains",
		"/orders/" + orderNo + "/certificate",
	} {
		status, payload := intruder.do("GET", path, nil)
		if status != http.StatusNotFound {
			t.Errorf("越权访问 %s 应返回 404，实际 %d：%v", path, status, payload)
		}
	}

	status, payload := intruder.do("POST", "/orders/"+orderNo+"/cancel",
		map[string]any{"reason": "越权"})
	if status != http.StatusNotFound {
		t.Errorf("越权取消应返回 404，实际 %d：%v", status, payload)
	}

	// 受害者的订单与余额都必须原封不动
	if got := asMap(t, victim.mustOK("GET", "/orders/"+orderNo, nil))["status"]; got != "waiting_dcv" {
		t.Errorf("越权操作不应改动订单，实际状态 %v", got)
	}
}

// ── 模拟上游的开发辅助接口 ────────────────────────

// TestMockIssueEndpointIsAbsentWithoutMockUpstream 验证没装模拟上游时不注册该接口。
//
// 这条断言的是生产装配：`Deps.MockFoxSSLClient` 为 nil 时（接了真实上游的环境）
// 这个接口必须不存在。它一旦存在，就是一个「让上游以为订单已签发」的入口，
// 而调用它只需要一个普通用户的令牌。
//
// **必须先建一张真订单再调。** 拿一个不存在的订单号去调，404 会同时来自
// 「路由没注册」和「订单不存在」两个完全不同的原因，于是无论路由注册与否
// 这条测试都是绿的——它就成了一个纯粹的装饰。
//
// 用真订单时两种实现的响应是不同的，测试才有鉴别力：
//   - 没注册：路由层的 404；
//   - 注册了但没装上游（把 nil 指针装进接口的经典写法）：解引用空指针，
//     被 Recover 中间件兜成 500。
func TestMockIssueEndpointIsAbsentWithoutMockUpstream(t *testing.T) {
	srv, db, _ := newOrderTestServerOpt(t, false)
	client, _ := registerUser(t, srv, db)
	client.credit(t, 100_000)

	orderNo := client.createCertOrder(t, []string{"example.com"})["orderNo"].(string)

	status, payload := client.do("POST", "/orders/"+orderNo+"/mock/issue", nil)
	if status != http.StatusNotFound {
		t.Errorf("未装配模拟上游时应返回 404（接口不存在），实际 %d：%v", status, payload)
	}
}

// TestMockIssueAdvancesOnlyUpstream 验证该接口只动上游、不动本地订单。
//
// 本地状态必须仍由回调驱动。如果这个接口顺手把订单改成 issued，
// 冒烟脚本就会绕开回调路径——而回调（验签、幂等、乱序、资金释放）
// 恰恰是最需要端到端验证的一段。
//
// 反向也断言一次：推进之后证书接口仍应拒绝，说明订单真的还停在等待验证。
func TestMockIssueAdvancesOnlyUpstream(t *testing.T) {
	srv, db, _ := newOrderTestServer(t)
	client, _ := registerUser(t, srv, db)
	client.credit(t, 100_000)

	orderNo := client.createCertOrder(t, []string{"example.com"})["orderNo"].(string)

	issued := asMap(t, client.mustOK("POST", "/orders/"+orderNo+"/mock/issue", nil))
	upstreamNo, _ := issued["upstreamOrderNo"].(string)
	if upstreamNo == "" {
		t.Fatalf("推进应答应带上游订单号，实际 %v", issued)
	}

	if got := asMap(t, client.mustOK("GET", "/orders/"+orderNo, nil))["status"]; got != "waiting_dcv" {
		t.Errorf("推进上游不应改变本地订单状态，实际 %v", got)
	}
	if status, _ := client.do("GET", "/orders/"+orderNo+"/certificate", nil); status != http.StatusConflict {
		t.Errorf("订单尚未收到签发事件，证书接口应返回 409，实际 %d", status)
	}

	// 上游侧确实已签发：此时投递回调必须能把订单推到 issued 并让证书可下载。
	raw, err := foxssl.EncodeNotification(&foxssl.Notification{
		EventType:       "certificate_issued",
		UpstreamOrderNo: upstreamNo,
		Status:          foxssl.MockStatusIssued,
		OccurredAt:      time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("编码事件报文失败: %v", err)
	}
	if status, body := client.postWebhook(raw, foxssl.Sign(orderHTTPSecret, raw)); status != http.StatusOK {
		t.Fatalf("回调应返回 200，实际 %d: %s", status, body)
	}

	if got := asMap(t, client.mustOK("GET", "/orders/"+orderNo, nil))["status"]; got != "issued" {
		t.Fatalf("回调后订单状态应为 issued，实际 %v", got)
	}
	cert := asMap(t, client.mustOK("GET", "/orders/"+orderNo+"/certificate", nil))
	if pem, _ := cert["certificate"].(string); !strings.Contains(pem, "BEGIN CERTIFICATE") {
		t.Errorf("证书内容应是 PEM 文本，实际 %q", pem)
	}
}

// TestMockIssueRejectsOtherUsersOrder 验证不能推进他人的订单。
//
// 越权推进的后果比越权查询更重：它会让上游真的去签发一张证书，
// 费用记在平台账上，而订单属于别人。
func TestMockIssueRejectsOtherUsersOrder(t *testing.T) {
	srv, db, _ := newOrderTestServer(t)

	victim, _ := registerUser(t, srv, db)
	victim.credit(t, 100_000)
	orderNo := victim.createCertOrder(t, []string{"example.com"})["orderNo"].(string)

	intruder, _ := registerUser(t, srv, db)

	status, payload := intruder.do("POST", "/orders/"+orderNo+"/mock/issue", nil)
	if status != http.StatusNotFound {
		t.Errorf("越权推进应返回 404，实际 %d：%v", status, payload)
	}
	if got := asMap(t, victim.mustOK("GET", "/orders/"+orderNo, nil))["status"]; got != "waiting_dcv" {
		t.Errorf("越权推进不应改动订单，实际状态 %v", got)
	}
}

// ── DTO 与契约一致性 ──────────────────────────────

// TestOrderDTOMatchesContract 验证下发的字段与契约严格一致。
//
// 两个方向都要查：
//   - 契约要求的字段必须在，否则前端拿到 undefined；
//   - 内部字段不能出现。成本价一旦下发，等于把采购成本摊给了用户；
//     上游原始状态则会在上游改取值含义时引起误判。
func TestOrderDTOMatchesContract(t *testing.T) {
	srv, db, _ := newOrderTestServer(t)
	client, _ := registerUser(t, srv, db)
	client.credit(t, 100_000)

	order := client.createCertOrder(t, []string{"example.com"})

	for _, key := range []string{
		"orderNo", "productId", "productName", "brand", "validationType",
		"years", "keyAlgorithm", "domains", "amount", "status",
		"certId", "failureReason", "createdAt", "updatedAt",
		"upstreamOrderNo", "contact", "organization", "issuedAt", "expiresAt",
	} {
		if _, ok := order[key]; !ok {
			t.Errorf("契约要求的字段 %s 缺失", key)
		}
	}

	for _, key := range []string{
		"id", "userId", "costPrice", "csr",
		"upstreamOrderStatus", "upstreamCertStatus",
		"upstreamPrepareStatus", "upstreamReissueStatus",
		"cancelReason", "submittedAt",
	} {
		if _, ok := order[key]; ok {
			t.Errorf("内部字段 %s 不应下发", key)
		}
	}

	// 详情里 contact 是必填对象，不能是 null——前端会直接读 contact.email
	contact, ok := order["contact"].(map[string]any)
	if !ok {
		t.Fatalf("contact 应是对象，实际 %v", order["contact"])
	}
	if contact["email"] != "admin@example.com" {
		t.Errorf("联系人邮箱应为 admin@example.com，实际 %v", contact["email"])
	}

	// 列表项刻意不含联系人：列表页用不到，而这些字段含个人信息
	items := asMap(t, client.mustOK("GET", "/orders", nil))["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("应有 1 张订单，实际 %d 张", len(items))
	}
	summary := items[0].(map[string]any)
	for _, key := range []string{"contact", "organization", "csr"} {
		if _, ok := summary[key]; ok {
			t.Errorf("列表项不应包含 %s", key)
		}
	}
	// 列表项必须有 domains，否则列表页无法展示订单覆盖了哪些域名
	domains, ok := summary["domains"].([]any)
	if !ok || len(domains) != 1 || domains[0] != "example.com" {
		t.Errorf("列表项的 domains 应为 [example.com]，实际 %v", summary["domains"])
	}
}

// TestDomainDTOMatchesContract 验证域名材料的字段与契约一致。
func TestDomainDTOMatchesContract(t *testing.T) {
	srv, db, _ := newOrderTestServer(t)
	client, _ := registerUser(t, srv, db)
	client.credit(t, 100_000)

	orderNo := client.createCertOrder(t, []string{"example.com"})["orderNo"].(string)

	items := asMap(t, client.mustOK("GET", "/orders/"+orderNo+"/domains", nil))["items"].([]any)
	domain := items[0].(map[string]any)

	for _, key := range []string{
		"domain", "status", "method", "availableMethods",
		"dnsRecordType", "dnsRecordName", "dnsRecordValue",
		"filePath", "fileContent", "emailAddresses", "verifiedAt", "expiresAt",
	} {
		if _, ok := domain[key]; !ok {
			t.Errorf("契约要求的字段 %s 缺失", key)
		}
	}
	if _, ok := domain["id"]; ok {
		t.Error("内部字段 id 不应下发")
	}

	// 尚未选择验证方式时 method 应是 null 而不是空串——
	// 空串会被前端渲染成一个空白选项
	if domain["method"] != nil {
		t.Errorf("尚未选择验证方式时 method 应为 null，实际 %v", domain["method"])
	}

	// availableMethods 必须是**按产品声明收窄过**的那一份。
	//
	// 产品 1（AlphaSSL）声明里没有 email，而上游材料给了三个收件地址——
	// 材料侧单独看是支持邮件验证的。这个组合是刻意挑的：只有产品侧把关
	// 真的生效，这里才不会出现 email；而一旦 handler 改回按材料侧推导
	// （直接用 AvailableMethods），它立刻就会冒出来。
	methods, _ := domain["availableMethods"].([]any)
	if len(methods) == 0 {
		t.Fatal("可用验证方式不该为空——前端靠它渲染选项")
	}
	// 前提：材料里确实有收件地址。没有的话，下面那条断言换成「材料里没邮件」
	// 也照样通过，测的就不是产品侧把关了。
	if domain["emailAddresses"] == nil {
		t.Fatal("前提不成立：上游材料里应有收件地址")
	}
	for _, m := range methods {
		if m == "email" {
			t.Errorf("产品未声明的 email 不该出现在 availableMethods 里，实际 %v", methods)
		}
	}
}

// ── 参数校验 ──────────────────────────────────────

// TestCreateOrderRejectsBadParams 验证下单参数校验。
//
// 产品规则不满足时也走 400，字段级原因在 data.fields 里——
// 前端要靠它把错误显示在对应的输入框上。
func TestCreateOrderRejectsBadParams(t *testing.T) {
	srv, db, _ := newOrderTestServer(t)
	client, _ := registerUser(t, srv, db)
	client.credit(t, 100_000)

	base := func() map[string]any {
		return map[string]any{
			"productId": orderHTTPProductID, "years": 1, "keyAlgorithm": "rsa",
			"domains": []string{"example.com"},
			"contact": map[string]any{
				"name": "张三", "email": "admin@example.com", "phone": "+86.13800138000",
			},
		}
	}

	cases := map[string]func(m map[string]any){
		"产品不存在":     func(m map[string]any) { m["productId"] = 999999 },
		"年限没有价格":    func(m map[string]any) { m["years"] = 5 },
		"算法不受支持":    func(m map[string]any) { m["keyAlgorithm"] = "dsa" },
		"域名为空":      func(m map[string]any) { m["domains"] = []string{} },
		"域名不是域名":    func(m map[string]any) { m["domains"] = []string{"notadomain"} },
		"域名重复":      func(m map[string]any) { m["domains"] = []string{"a.com", "a.com"} },
		"单域名产品给了两个": func(m map[string]any) { m["domains"] = []string{"a.com", "b.com"} },
		"该产品不需要企业信息": func(m map[string]any) {
			m["organization"] = map[string]any{
				"name": "某某公司", "registrationNo": "91310000MA1K35XXXX",
				"country": "CN", "province": "上海市", "city": "上海市",
				"address": "某某路 1 号", "postalCode": "200120", "phone": "+86.02150000000",
			}
		},
		"联系人邮箱非法": func(m map[string]any) {
			m["contact"].(map[string]any)["email"] = "not-an-email"
		},
		"联系人姓名为空": func(m map[string]any) {
			m["contact"].(map[string]any)["name"] = ""
		},
	}

	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			body := base()
			mutate(body)

			status, payload := client.do("POST", "/orders", body)
			if status != http.StatusBadRequest && status != http.StatusNotFound {
				t.Fatalf("应返回 4xx，实际 %d：%v", status, payload)
			}
		})
	}

	// 全部失败之后不应留下任何订单，余额也不该变
	if items := asMap(t, client.mustOK("GET", "/orders", nil))["items"].([]any); len(items) != 0 {
		t.Errorf("被拒绝的下单不应留下订单，实际 %d 张", len(items))
	}
	wallet := asMap(t, client.mustOK("GET", "/wallet", nil))
	if got := number(t, wallet, "availableBalance"); got != 100_000 {
		t.Errorf("被拒绝的下单不应改动余额，实际 %v", got)
	}
}

// TestCreateOrderRejectsInsufficientBalance 验证余额不足返回 2000。
func TestCreateOrderRejectsInsufficientBalance(t *testing.T) {
	srv, db, _ := newOrderTestServer(t)
	client, _ := registerUser(t, srv, db)
	client.credit(t, 1000)

	status, payload := client.do("POST", "/orders", map[string]any{
		"productId": orderHTTPProductID, "years": 1, "keyAlgorithm": "rsa",
		"domains": []string{"example.com"},
		"contact": map[string]any{
			"name": "张三", "email": "admin@example.com", "phone": "+86.13800138000",
		},
	})
	if status != http.StatusBadRequest {
		t.Fatalf("余额不足应返回 400，实际 %d：%v", status, payload)
	}
	if code, _ := payload["code"].(float64); code != 2000 {
		t.Errorf("余额不足的业务码应为 2000，实际 %v", payload["code"])
	}
}

// TestCancelWithoutBodyIsAccepted 验证取消与重签的请求体是可选的。
//
// 契约里两者的 requestBody 都是 required: false，客户端可能不带 body
// 直接 POST。把它判成参数错误会挡住一个完全合法的请求。
func TestCancelWithoutBodyIsAccepted(t *testing.T) {
	srv, db, _ := newOrderTestServer(t)
	client, _ := registerUser(t, srv, db)
	client.credit(t, 100_000)

	orderNo := client.createCertOrder(t, []string{"example.com"})["orderNo"].(string)

	status, payload := client.do("POST", "/orders/"+orderNo+"/cancel", nil)
	if status != http.StatusOK {
		t.Fatalf("不带 body 的取消应成功，实际 %d：%v", status, payload)
	}
	if got := asMap(t, payload)["status"]; got != "cancelled" {
		t.Errorf("取消后状态应为 cancelled，实际 %v", got)
	}

	// 已实扣的钱必须退回
	wallet := asMap(t, client.mustOK("GET", "/wallet", nil))
	if got := number(t, wallet, "availableBalance"); got != 100_000 {
		t.Errorf("取消后可用余额应完整退回 100000，实际 %v", got)
	}
}

// ── 列表与分页 ────────────────────────────────────

func TestOrderListPaginationOverHTTP(t *testing.T) {
	srv, db, _ := newOrderTestServer(t)
	client, _ := registerUser(t, srv, db)
	client.credit(t, 1_000_000)

	const total = 5
	for range total {
		client.createCertOrder(t, []string{"example.com"})
	}

	seen := make(map[string]bool)
	cursor := 0
	for {
		path := "/orders?limit=2"
		if cursor != 0 {
			path += "&cursor=" + strconv.Itoa(cursor)
		}
		data := asMap(t, client.mustOK("GET", path, nil))
		for _, item := range data["items"].([]any) {
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

// TestOrderListFiltersByStatusOverHTTP 验证按状态筛选。
//
// 筛选走的是数据库而不是内存过滤：订单会持续增长，
// 拉全表再筛的代价随用户历史订单数线性上升。
func TestOrderListFiltersByStatusOverHTTP(t *testing.T) {
	srv, db, _ := newOrderTestServer(t)
	client, _ := registerUser(t, srv, db)
	client.credit(t, 1_000_000)

	first := client.createCertOrder(t, []string{"example.com"})["orderNo"].(string)
	client.createCertOrder(t, []string{"example.com"})
	client.mustOK("POST", "/orders/"+first+"/cancel", map[string]any{"reason": "x"})

	items := asMap(t, client.mustOK("GET", "/orders?status=cancelled", nil))["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("cancelled 应有 1 张订单，实际 %d 张", len(items))
	}
	if got := items[0].(map[string]any)["orderNo"]; got != first {
		t.Errorf("筛选结果应为 %s，实际 %v", first, got)
	}

	status, payload := client.do("GET", "/orders?status=no-such-status", nil)
	if status != http.StatusBadRequest {
		t.Errorf("未知状态应返回 400，实际 %d：%v", status, payload)
	}
}

// ── 辅助 ──────────────────────────────────────────

// cleanupWebhookEvents 清掉某个上游订单号下的事件。
//
// 上游订单号不挂在任何用户下，testutil.CleanupUser 的按用户清理覆盖不到，
// 不显式清理会在测试库里累积并污染后续断言。
func cleanupWebhookEvents(t *testing.T, db *sql.DB, upstreamOrderNo string) {
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
