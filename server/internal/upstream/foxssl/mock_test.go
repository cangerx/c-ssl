package foxssl

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cangerx/c-ssl/server/internal/domain/money"
)

const (
	testAPIKey        = "foxssl-test-api-key-0123456789"
	testWebhookSecret = "foxssl-test-webhook-secret-0123456789"
)

func newTestClient(t *testing.T) *MockClient {
	t.Helper()
	c, err := NewMockClient(testAPIKey, testWebhookSecret, "http://localhost:8080")
	if err != nil {
		t.Fatalf("构造 Mock 上游失败: %v", err)
	}
	return c
}

func sampleRequest(orderNo string, domains ...string) CreateOrderRequest {
	if len(domains) == 0 {
		domains = []string{"example.com"}
	}
	return CreateOrderRequest{
		MerchantOrderNo:   orderNo,
		UpstreamProductID: 1001,
		Years:             1,
		KeyAlgorithm:      "rsa",
		Domains:           domains,
		CSR:               "-----BEGIN CERTIFICATE REQUEST-----\nMIIB\n-----END CERTIFICATE REQUEST-----\n",
		Contact:           Contact{Name: "张三", Email: "admin@example.com", Phone: "+86.13800138000"},
		NotifyURL:         "http://localhost:8080/api/v1/webhooks/foxssl",
	}
}

// ── 构造校验 ──────────────────────────────────────

// TestNewMockClientRequiresSecrets 验证空密钥直接拒绝构造。
//
// 允许空密钥意味着「忘记配置」会静默变成「用一个空密钥验签」，
// 而空密钥的 HMAC 是任何人都能算的，验签就成了一道装饰。
func TestNewMockClientRequiresSecrets(t *testing.T) {
	cases := []struct {
		name   string
		apiKey string
		secret string
	}{
		{"API Key 为空", "", testWebhookSecret},
		{"Webhook 密钥为空", testAPIKey, ""},
		{"两者都为空", "", ""},
		{"API Key 只有空白", "   ", testWebhookSecret},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewMockClient(tc.apiKey, tc.secret, ""); err == nil {
				t.Fatal("空密钥应当构造失败，实际成功了")
			}
		})
	}
}

// ── 下单幂等 ──────────────────────────────────────

// TestCreateOrderIsIdempotentByMerchantOrderNo 是 Mock 存在的首要理由。
//
// 平台在「上游调用超时」时拿不到「到底成没成功」的答案，只能重试。
// 如果上游不按商户订单号去重，重试就会真的下第二单——
// 用户被签两张证书、平台被扣两次钱。这条性质必须有测试兜住，
// 否则订单域里「重试不产生第二张证书」的测试就是假的。
func TestCreateOrderIsIdempotentByMerchantOrderNo(t *testing.T) {
	c := newTestClient(t)
	ctx := context.Background()

	first, err := c.CreateOrder(ctx, sampleRequest("CS20260917120000AAAAAA"))
	if err != nil {
		t.Fatalf("首次下单失败: %v", err)
	}

	second, err := c.CreateOrder(ctx, sampleRequest("CS20260917120000AAAAAA"))
	if err != nil {
		t.Fatalf("重复下单失败: %v", err)
	}

	if first.UpstreamOrderNo != second.UpstreamOrderNo {
		t.Fatalf("同一商户订单号返回了不同的上游订单号：%s vs %s",
			first.UpstreamOrderNo, second.UpstreamOrderNo)
	}
	if first.Cost != second.Cost {
		t.Fatalf("同一商户订单号返回了不同的成本：%d vs %d", first.Cost, second.Cost)
	}
	if n := c.OrderCount(); n != 1 {
		t.Fatalf("上游应只有 1 个订单，实际 %d 个", n)
	}

	// 换一个商户订单号必须产生新订单，否则幂等就退化成「永远只认第一单」
	third, err := c.CreateOrder(ctx, sampleRequest("CS20260917120000BBBBBB"))
	if err != nil {
		t.Fatalf("不同商户订单号下单失败: %v", err)
	}
	if third.UpstreamOrderNo == first.UpstreamOrderNo {
		t.Fatal("不同商户订单号不应复用同一个上游订单")
	}
	if n := c.OrderCount(); n != 2 {
		t.Fatalf("上游应有 2 个订单，实际 %d 个", n)
	}
}

// TestConcurrentCreateOrderProducesSingleUpstreamOrder 验证并发下幂等依然成立。
//
// 单线程的幂等很容易靠「先查再插」蒙过去，而真实重试是并发发生的。
func TestConcurrentCreateOrderProducesSingleUpstreamOrder(t *testing.T) {
	c := newTestClient(t)
	ctx := context.Background()

	const workers = 12
	var wg sync.WaitGroup
	results := make([]string, workers)
	errs := make([]error, workers)
	start := make(chan struct{})

	for i := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			resp, err := c.CreateOrder(ctx, sampleRequest("CS20260917120000CCCCCC"))
			if err != nil {
				errs[i] = err
				return
			}
			results[i] = resp.UpstreamOrderNo
		}()
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("第 %d 个并发下单失败: %v", i, err)
		}
	}
	for i, no := range results {
		if no != results[0] {
			t.Fatalf("并发下单返回了不同的上游订单号：第 0 个 %s，第 %d 个 %s", results[0], i, no)
		}
	}
	if n := c.OrderCount(); n != 1 {
		t.Fatalf("并发下单应只产生 1 个上游订单，实际 %d 个", n)
	}
}

func TestCreateOrderValidatesInput(t *testing.T) {
	c := newTestClient(t)
	ctx := context.Background()

	t.Run("缺少商户订单号", func(t *testing.T) {
		req := sampleRequest("")
		if _, err := c.CreateOrder(ctx, req); err == nil {
			t.Fatal("缺少商户订单号应当报错")
		}
	})

	t.Run("缺少域名", func(t *testing.T) {
		req := sampleRequest("CS20260917120000DDDDDD")
		req.Domains = nil
		if _, err := c.CreateOrder(ctx, req); err == nil {
			t.Fatal("缺少域名应当报错")
		}
	})

	t.Run("校验失败不产生订单", func(t *testing.T) {
		if n := c.OrderCount(); n != 0 {
			t.Fatalf("校验失败不应产生订单，实际 %d 个", n)
		}
	})
}

// ── DCV 材料 ──────────────────────────────────────

// TestFileDcvPathContainsPlaceholder 是一条**守卫测试**。
//
// 它断言的不是业务行为，而是「Mock 的行为没有被人改坏」：
// Mock 必须返回含 {FQDN} 的模板，订单域那段替换逻辑才真的被测到。
//
// 如果有人为了「让测试好看」把 Mock 改成返回预替换好的路径，
// 替换逻辑就写成什么样测试都是绿的——而线上拿到的是真实上游的模板，
// 于是所有文件验证都会失败。这条用例会让那次「优化」当场变红。
func TestFileDcvPathContainsPlaceholder(t *testing.T) {
	c := newTestClient(t)

	domains, err := c.ListDomains(context.Background(), mustCreateOrder(t, c))
	if err != nil {
		t.Fatalf("查询域名失败: %v", err)
	}
	if len(domains.Domains) == 0 {
		t.Fatal("未返回任何域名")
	}

	for _, d := range domains.Domains {
		if !strings.Contains(d.FileDcvPath, PlaceholderFQDN) {
			t.Fatalf("Mock 返回的文件路径 %q 不含占位符 %s。\n"+
				"这不是 Mock 的问题——是有人把它改成了预替换。\n"+
				"改回去：订单域的替换逻辑必须被真实地测到。",
				d.FileDcvPath, PlaceholderFQDN)
		}
	}
}

// TestDcvMaterialsUseBaseDomainForWildcard 验证通配符域名的验证材料
// 配在裸域名上。
//
// DNS 里不存在 `*.example.com` 这个可以承载记录的名字，
// 记录名必须是 `_dnsauth.example.com`。
func TestDcvMaterialsUseBaseDomainForWildcard(t *testing.T) {
	c := newTestClient(t)
	orderNo := mustCreateOrder(t, c, "*.example.com", "example.com")

	domains, err := c.ListDomains(context.Background(), orderNo)
	if err != nil {
		t.Fatalf("查询域名失败: %v", err)
	}

	var wildcard *Domain
	for i := range domains.Domains {
		if domains.Domains[i].Domain == "*.example.com" {
			wildcard = &domains.Domains[i]
		}
	}
	if wildcard == nil {
		t.Fatal("未返回通配符域名")
	}
	if strings.Contains(wildcard.DnsRecordName, "*") {
		t.Fatalf("通配符域名的记录名不应含 *，实际 %q", wildcard.DnsRecordName)
	}
	if wildcard.DnsRecordName != "_dnsauth.example.com" {
		t.Fatalf("记录名应为 _dnsauth.example.com，实际 %q", wildcard.DnsRecordName)
	}
}

func TestListDomainsReturnsCopy(t *testing.T) {
	c := newTestClient(t)
	orderNo := mustCreateOrder(t, c)

	first, err := c.ListDomains(context.Background(), orderNo)
	if err != nil {
		t.Fatalf("首次查询失败: %v", err)
	}
	original := first.Domains[0].DnsRecordValue
	first.Domains[0].DnsRecordValue = "被调用方改掉了"
	first.Domains[0].EmailAddresses = append(first.Domains[0].EmailAddresses, "injected@example.com")

	second, err := c.ListDomains(context.Background(), orderNo)
	if err != nil {
		t.Fatalf("二次查询失败: %v", err)
	}
	if second.Domains[0].DnsRecordValue != original {
		t.Fatalf("调用方修改返回值影响到了内部状态：期望 %q，实际 %q",
			original, second.Domains[0].DnsRecordValue)
	}
	if len(second.Domains[0].EmailAddresses) != 3 {
		t.Fatalf("调用方追加的邮箱影响到了内部状态，实际 %d 个", len(second.Domains[0].EmailAddresses))
	}
}

// TestRegenerateDcvTokenInvalidatesOldToken 验证重新生成会换掉旧值。
//
// 如果 token 是「由域名派生」的确定性值，这条测试永远通过——
// 而用户重新生成后拿到的还是旧 token，配了新记录的反而验证不过。
func TestRegenerateDcvTokenInvalidatesOldToken(t *testing.T) {
	c := newTestClient(t)
	ctx := context.Background()
	orderNo := mustCreateOrder(t, c)

	before, err := c.ListDomains(ctx, orderNo)
	if err != nil {
		t.Fatalf("查询域名失败: %v", err)
	}
	oldValue := before.Domains[0].DnsRecordValue
	oldPath := before.Domains[0].FileDcvPath

	after, err := c.RegenerateDcvToken(ctx, orderNo)
	if err != nil {
		t.Fatalf("重新生成 token 失败: %v", err)
	}
	if after.Domains[0].DnsRecordValue == oldValue {
		t.Fatalf("重新生成后 DNS 记录值没有变化，仍是 %q", oldValue)
	}
	if after.Domains[0].FileDcvPath == oldPath {
		t.Fatalf("重新生成后文件路径没有变化，仍是 %q", oldPath)
	}
}

func TestResendDcvEmailRequiresEmailMethod(t *testing.T) {
	c := newTestClient(t)
	ctx := context.Background()
	orderNo := mustCreateOrder(t, c)

	// 尚未选择邮件验证时应当拒绝
	err := c.ResendDcvEmail(ctx, orderNo, []string{"example.com"})
	if !errors.Is(err, ErrNotSupported) {
		t.Fatalf("未使用邮件验证时应返回 ErrNotSupported，实际 %v", err)
	}

	// 选定邮件验证后应当通过
	if err := c.VerifyDomains(ctx, VerifyDomainsRequest{
		UpstreamOrderNo: orderNo,
		Domains:         []string{"example.com"},
		Method:          "email",
	}); err != nil {
		t.Fatalf("提交邮件验证失败: %v", err)
	}
	if err := c.ResendDcvEmail(ctx, orderNo, []string{"example.com"}); err != nil {
		t.Fatalf("重发验证邮件失败: %v", err)
	}
}

func TestVerifyDomainsRejectsForeignDomain(t *testing.T) {
	c := newTestClient(t)
	orderNo := mustCreateOrder(t, c)

	err := c.VerifyDomains(context.Background(), VerifyDomainsRequest{
		UpstreamOrderNo: orderNo,
		Domains:         []string{"other.com"},
		Method:          "dns_txt",
	})
	if err == nil {
		t.Fatal("不属于订单的域名应当被拒绝")
	}
}

// ── 状态推进 ──────────────────────────────────────

func TestIssueCertificateAndDownload(t *testing.T) {
	c := newTestClient(t)
	ctx := context.Background()
	orderNo := mustCreateOrder(t, c)

	t.Run("未签发时下载失败", func(t *testing.T) {
		if _, err := c.DownloadCertificate(ctx, orderNo); err == nil {
			t.Fatal("未签发的订单不应能下载证书")
		}
	})

	if err := c.IssueCertificate(orderNo); err != nil {
		t.Fatalf("签发失败: %v", err)
	}

	status, err := c.OrderStatus(ctx, orderNo)
	if err != nil {
		t.Fatalf("查询状态失败: %v", err)
	}
	if status.OrderStatus != MockStatusIssued {
		t.Fatalf("订单状态应为 %s，实际 %s", MockStatusIssued, status.OrderStatus)
	}
	if status.CertStatus != MockStatusIssued {
		t.Fatalf("证书状态应为 %s，实际 %s", MockStatusIssued, status.CertStatus)
	}
	if status.CertID == "" {
		t.Fatal("签发后应有证书编号")
	}

	cert, err := c.DownloadCertificate(ctx, orderNo)
	if err != nil {
		t.Fatalf("下载证书失败: %v", err)
	}
	if !strings.Contains(cert.Certificate, "BEGIN CERTIFICATE") {
		t.Fatalf("证书内容不是 PEM 格式：%q", cert.Certificate)
	}
	if cert.CommonName != "example.com" {
		t.Fatalf("主域名应为 example.com，实际 %q", cert.CommonName)
	}
	if !cert.ExpiresAt.After(cert.IssuedAt) {
		t.Fatal("到期时间应晚于签发时间")
	}
}

func TestReissueProducesNewCertID(t *testing.T) {
	c := newTestClient(t)
	ctx := context.Background()
	orderNo := mustCreateOrder(t, c)

	// 未签发时不能重签
	if err := c.Reissue(ctx, ReissueRequest{UpstreamOrderNo: orderNo}); !errors.Is(err, ErrNotSupported) {
		t.Fatalf("未签发时应返回 ErrNotSupported，实际 %v", err)
	}

	if err := c.IssueCertificate(orderNo); err != nil {
		t.Fatalf("签发失败: %v", err)
	}
	before, err := c.DownloadCertificate(ctx, orderNo)
	if err != nil {
		t.Fatalf("下载证书失败: %v", err)
	}

	if err := c.Reissue(ctx, ReissueRequest{UpstreamOrderNo: orderNo, Reason: "私钥泄漏"}); err != nil {
		t.Fatalf("重签失败: %v", err)
	}
	after, err := c.DownloadCertificate(ctx, orderNo)
	if err != nil {
		t.Fatalf("重签后下载证书失败: %v", err)
	}
	if after.CertID == before.CertID {
		t.Fatalf("重签后证书编号不应相同，都是 %s", before.CertID)
	}
	if after.SerialNumber == before.SerialNumber {
		t.Fatal("重签后序列号不应相同")
	}
}

func TestCancelOrderRejectsIssued(t *testing.T) {
	c := newTestClient(t)
	ctx := context.Background()
	orderNo := mustCreateOrder(t, c)

	if err := c.CancelOrder(ctx, orderNo); err != nil {
		t.Fatalf("未签发的订单应可取消: %v", err)
	}
	status, err := c.OrderStatus(ctx, orderNo)
	if err != nil {
		t.Fatalf("查询状态失败: %v", err)
	}
	if status.OrderStatus != MockStatusCancelled {
		t.Fatalf("状态应为 %s，实际 %s", MockStatusCancelled, status.OrderStatus)
	}

	// 已签发的订单不能取消
	issued := mustCreateOrder(t, c, "issued.example.com")
	if err := c.IssueCertificate(issued); err != nil {
		t.Fatalf("签发失败: %v", err)
	}
	if err := c.CancelOrder(ctx, issued); !errors.Is(err, ErrNotSupported) {
		t.Fatalf("已签发订单取消应返回 ErrNotSupported，实际 %v", err)
	}
}

func TestOrderNotFound(t *testing.T) {
	c := newTestClient(t)
	ctx := context.Background()

	cases := map[string]func() error{
		"OrderStatus":         func() error { _, err := c.OrderStatus(ctx, "FX-NOPE"); return err },
		"ListDomains":         func() error { _, err := c.ListDomains(ctx, "FX-NOPE"); return err },
		"DownloadCertificate": func() error { _, err := c.DownloadCertificate(ctx, "FX-NOPE"); return err },
		"CancelOrder":         func() error { return c.CancelOrder(ctx, "FX-NOPE") },
		"SetOrderStatus":      func() error { return c.SetOrderStatus("FX-NOPE", "issued") },
	}
	for name, fn := range cases {
		t.Run(name, func(t *testing.T) {
			if err := fn(); !errors.Is(err, ErrOrderNotFound) {
				t.Fatalf("应返回 ErrOrderNotFound，实际 %v", err)
			}
		})
	}
}

// ── 失败注入 ──────────────────────────────────────

// TestFailOnceIsOneShot 验证失败注入只生效一次。
//
// 持续失败只能测出「一直失败」，测不出「重试后成功」——
// 而补偿机制的价值恰恰在后者。
func TestFailOnceIsOneShot(t *testing.T) {
	c := newTestClient(t)
	ctx := context.Background()

	c.FailOnce(OpCreateOrder, ErrUnavailable)
	if _, err := c.CreateOrder(ctx, sampleRequest("CS20260917120000EEEEEE")); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("首次应返回注入的错误，实际 %v", err)
	}
	resp, err := c.CreateOrder(ctx, sampleRequest("CS20260917120000EEEEEE"))
	if err != nil {
		t.Fatalf("第二次应成功，实际 %v", err)
	}
	if resp.UpstreamOrderNo == "" {
		t.Fatal("第二次应返回上游订单号")
	}
	if n := c.OrderCount(); n != 1 {
		t.Fatalf("应只产生 1 个上游订单，实际 %d 个", n)
	}
}

func TestFailOncePerOperation(t *testing.T) {
	c := newTestClient(t)
	orderNo := mustCreateOrder(t, c)

	ops := map[string]func() error{
		OpBalance:     func() error { _, err := c.Balance(context.Background()); return err },
		OpOrderStatus: func() error { _, err := c.OrderStatus(context.Background(), orderNo); return err },
		OpListDomains: func() error { _, err := c.ListDomains(context.Background(), orderNo); return err },
		OpVerifyDomains: func() error {
			return c.VerifyDomains(context.Background(), VerifyDomainsRequest{UpstreamOrderNo: orderNo, Domains: []string{"example.com"}, Method: "dns_txt"})
		},
		OpRegenerateDcvToken:  func() error { _, err := c.RegenerateDcvToken(context.Background(), orderNo); return err },
		OpDownloadCertificate: func() error { _, err := c.DownloadCertificate(context.Background(), orderNo); return err },
		OpCancelOrder:         func() error { return c.CancelOrder(context.Background(), orderNo) },
	}
	sentinel := errors.New("注入的故障")
	for op, fn := range ops {
		t.Run(op, func(t *testing.T) {
			c.FailOnce(op, sentinel)
			if err := fn(); !errors.Is(err, sentinel) {
				t.Fatalf("应返回注入的错误，实际 %v", err)
			}
			if err := fn(); errors.Is(err, sentinel) {
				t.Fatal("失败注入应只生效一次")
			}
		})
	}
}

func TestSetCost(t *testing.T) {
	c := newTestClient(t)
	c.SetCost(money.Amount(8888))

	resp, err := c.CreateOrder(context.Background(), sampleRequest("CS20260917120000FFFFFF"))
	if err != nil {
		t.Fatalf("下单失败: %v", err)
	}
	if resp.Cost != money.Amount(8888) {
		t.Fatalf("成本应为 8888，实际 %d", resp.Cost)
	}
}

// ── 回调验签与解析 ────────────────────────────────

func sampleNotification() *Notification {
	return &Notification{
		EventType:       "certificate_issued",
		UpstreamOrderNo: "FX-20260917-0001",
		Status:          MockStatusIssued,
		CertID:          "CERT-FX-20260917-0001",
		OccurredAt:      time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC),
	}
}

func TestSignAndParseNotificationRoundTrip(t *testing.T) {
	c := newTestClient(t)

	n := sampleNotification()
	raw, err := EncodeNotification(n)
	if err != nil {
		t.Fatalf("编码报文失败: %v", err)
	}
	got, err := c.ParseNotification(raw, Sign(testWebhookSecret, raw))
	if err != nil {
		t.Fatalf("验签解析失败: %v", err)
	}

	if got.EventType != n.EventType {
		t.Fatalf("事件类型不符：期望 %q，实际 %q", n.EventType, got.EventType)
	}
	if got.UpstreamOrderNo != n.UpstreamOrderNo {
		t.Fatalf("订单号不符：期望 %q，实际 %q", n.UpstreamOrderNo, got.UpstreamOrderNo)
	}
	if got.Status != n.Status {
		t.Fatalf("状态不符：期望 %q，实际 %q", n.Status, got.Status)
	}
	if got.CertID != n.CertID {
		t.Fatalf("证书编号不符：期望 %q，实际 %q", n.CertID, got.CertID)
	}
	if !got.OccurredAt.Equal(n.OccurredAt) {
		t.Fatalf("事件时间不符：期望 %v，实际 %v", n.OccurredAt, got.OccurredAt)
	}
	if string(got.Raw) != string(raw) {
		t.Fatal("Raw 应与原始字节一致")
	}
}

func TestParseNotificationWithDomains(t *testing.T) {
	c := newTestClient(t)

	n := sampleNotification()
	n.EventType = "dcv_status"
	n.Domains = []DomainStatus{
		{Domain: "example.com", Status: "verified", Method: "dns_txt"},
		{Domain: "www.example.com", Status: "verifying", Method: "dns_txt"},
	}
	raw, err := EncodeNotification(n)
	if err != nil {
		t.Fatalf("编码报文失败: %v", err)
	}
	got, err := c.ParseNotification(raw, Sign(testWebhookSecret, raw))
	if err != nil {
		t.Fatalf("验签解析失败: %v", err)
	}
	if len(got.Domains) != 2 {
		t.Fatalf("应解析出 2 个域名，实际 %d 个", len(got.Domains))
	}
	if got.Domains[0].Domain != "example.com" || got.Domains[0].Status != "verified" {
		t.Fatalf("首个域名解析错误：%+v", got.Domains[0])
	}
}

// TestParseNotificationRejectsTamperedBytes 逐字节篡改，每次都必须被拒绝。
//
// 只测「改一个字符」是不够的：签名比较若用 == 而不是 hmac.Equal，
// 篡改位置不同会有不同的耗时特征。这里把每个字节都翻一遍，
// 确认没有哪个位置能蒙混过关。
func TestParseNotificationRejectsTamperedBytes(t *testing.T) {
	c := newTestClient(t)

	n := sampleNotification()
	raw, err := EncodeNotification(n)
	if err != nil {
		t.Fatalf("编码报文失败: %v", err)
	}
	sig := Sign(testWebhookSecret, raw)

	for i := range raw {
		tampered := append([]byte(nil), raw...)
		tampered[i] ^= 0x01
		if _, err := c.ParseNotification(tampered, sig); !errors.Is(err, ErrInvalidSignature) {
			t.Fatalf("第 %d 个字节被篡改后应验签失败，实际 %v（字节 %q）", i, err, tampered[i])
		}
	}
}

func TestParseNotificationRejectsBadSignature(t *testing.T) {
	c := newTestClient(t)
	raw, err := EncodeNotification(sampleNotification())
	if err != nil {
		t.Fatalf("编码报文失败: %v", err)
	}

	cases := map[string]string{
		"空签名":     "",
		"截断签名":    Sign(testWebhookSecret, raw)[:20],
		"多一个字符":   Sign(testWebhookSecret, raw) + "A",
		"完全错误的签名": "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=",
		"用别的密钥签的": Sign("another-secret-0123456789", raw),
	}
	for name, sig := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := c.ParseNotification(raw, sig); !errors.Is(err, ErrInvalidSignature) {
				t.Fatalf("应返回 ErrInvalidSignature，实际 %v", err)
			}
		})
	}
}

// TestParseNotificationRejectsMalformedPayload 验证验签通过但报文损坏时
// 返回的是 ErrMalformedPayload 而不是 ErrInvalidSignature。
//
// 两者必须分开：前者意味着「上游改了报文格式」，后者意味着「有人在伪造」，
// 处置方式完全不同——一个是通知上游，一个是拉安全告警。
func TestParseNotificationRejectsMalformedPayload(t *testing.T) {
	c := newTestClient(t)

	cases := map[string]string{
		"不是 JSON":     `not json at all`,
		"空对象":         `{}`,
		"缺少 orderNo":  `{"event":"x","status":"issued"}`,
		"缺少 status":   `{"event":"x","orderNo":"FX-1"}`,
		"orderNo 为空白": `{"event":"x","orderNo":"   ","status":"issued"}`,
		"被截断的 JSON":   `{"event":"x","orderNo":"FX-1"`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			raw := []byte(body)
			_, err := c.ParseNotification(raw, Sign(testWebhookSecret, raw))
			if !errors.Is(err, ErrMalformedPayload) {
				t.Fatalf("应返回 ErrMalformedPayload，实际 %v", err)
			}
		})
	}
}

func TestAckFormat(t *testing.T) {
	c := newTestClient(t)
	ack := c.Ack()

	if ack.ContentType != "application/json" {
		t.Fatalf("Content-Type 应为 application/json，实际 %q", ack.ContentType)
	}
	// 上游只认它自己文档里的格式，包一层 code/message 会被判成失败并触发重推
	if string(ack.Body) != `{"status":"success"}` {
		t.Fatalf("应答体应为 {\"status\":\"success\"}，实际 %q", ack.Body)
	}
}

func TestName(t *testing.T) {
	c := newTestClient(t)
	if c.Name() != NameMock {
		t.Fatalf("上游标识应为 %q，实际 %q", NameMock, c.Name())
	}
}

// ── 辅助 ──────────────────────────────────────────

// mustCreateOrder 创建一张订单并返回上游订单号。
func mustCreateOrder(t *testing.T, c *MockClient, domains ...string) string {
	t.Helper()
	orderNo := fmt.Sprintf("CS20260917120000%s", randomToken()[:6])
	resp, err := c.CreateOrder(context.Background(), sampleRequest(orderNo, domains...))
	if err != nil {
		t.Fatalf("创建订单失败: %v", err)
	}
	return resp.UpstreamOrderNo
}
