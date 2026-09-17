package foxssl

// 本文件钉住**入站回调**：验签方案、报文结构、应答体。
//
// 回调是外部唯一能直接推动订单状态的入口，而且那个接口不套登录鉴权——
// 验签是它唯一的身份凭证。所以这里既测「正常报文能解析」，
// 也测「伪造与残缺报文会被挡住」。

import (
	"errors"
	"strings"
	"testing"
)

// realIssuedPayload 是上游文档给的证书签发回调报文，逐字段照抄。
//
// 用文档原文而不是自己编一份：Mock 的报文格式（{event, orderNo, status,
// certId, domains[], occurredAt}）与真实上游完全不同，用它测出来的
// 「解析正确」在真实上游上会变成一片「字段缺失」。
const realIssuedPayload = `{
    "auth": {"authToken": "c55eed1e30e4f3cb36de07c9f76d50c8", "randomStr": "success"},
    "notifyInfo": {
        "orderNo": "2021011221355666",
        "certID": 142932802604630016,
        "status": "3004",
        "statusDesc": "issued",
        "serialNumber": "ebdd502ca70645e0acdbf0c9a43f6af",
        "certContent": "-----BEGIN CERTIFICATE-----\nMIIFpDCCBIygAwIBAgIQDr3VAspzZF4Kzb8MmkP2rzANBgkqhkiG9w0BAQsFADCB\n-----END CERTIFICATE-----\n",
        "midCertContent": "-----BEGIN CERTIFICATE-----\nMIIEMjCCAxqgAwIBAgIBATANBgkqhkiG9w0BAQUFADB7MQsw\n-----END CERTIFICATE-----\n",
        "notBefore": 1606262400000,
        "notAfter": 1637884799000,
        "commonName": "test.com",
        "domainNames": ["test.com"],
        "sha1": "77ddc84576d56661867c0f95a341642e9699d350",
        "sha256": "0e27a14fcb7a6014ab0500171438ce1eeaeffa7d6f2006b344d5d3e1ed2835f7",
        "issuerCommonName": "Sectigo RSA Domain Validation Secure Server CA",
        "issuerCountry": "GB",
        "issuerOrg": "Sectigo Limited",
        "signatureAlgo": "SHA256-RSA",
        "encryption": "RSA",
        "keyLength": 2048,
        "keyCurve": ""
    }
}`

// realCancelledPayload 是证书取消回调，只有四个字段。
const realCancelledPayload = `{
    "auth": {"authToken": "c55eed1e30e4f3cb36de07c9f76d50c8", "randomStr": "success"},
    "notifyInfo": {"orderNo": "202001016666", "certID": 142932802604630016,
        "status": "3005", "statusDesc": "canceld"}
}`

// newNotifyClient 构造一个用于回调测试的客户端。
//
// 只填 BaseURL 与 APIKey：真实上游的回调验签密钥就是 API Key，
// 没有第二个密钥字段（见 HTTPOptions.APIKey）。
func newNotifyClient(t *testing.T) *HTTPClient {
	t.Helper()
	c, err := NewHTTPClient(HTTPOptions{
		BaseURL: "https://api.example.com",
		APIKey:  testAPIKey,
	})
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}
	return c
}

// TestParseNotificationRealPayload 验证真实签发回调的验签与解析。
func TestParseNotificationRealPayload(t *testing.T) {
	c := newNotifyClient(t)
	raw := []byte(realIssuedPayload)

	got, err := c.ParseNotification(raw, Sign(testAPIKey, raw))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}

	if got.UpstreamOrderNo != "2021011221355666" {
		t.Errorf("orderNo 映射错了：%q", got.UpstreamOrderNo)
	}
	// 回调带的是**证书状态码**（3004），不是订单状态码（1002 一类）。
	// 两者取值区间不同，订单域的映射表要能同时认。
	if got.Status != "3004" {
		t.Errorf("status 应保持上游原文 3004，实际 %q", got.Status)
	}
	// certID 超出 float64 的精确整数范围，必须用 int64 解析。
	// 文档示例值正好是 32 的倍数（float64 也能精确表示），所以这里
	// 只断言它没被截断成科学计数法或负数。
	if got.CertID != "142932802604630016" {
		t.Errorf("certID 解析错了：%q", got.CertID)
	}
	// 原始报文字节要留档：它是事件幂等键的一部分（见 order.buildEventKey），
	// 也是事后对账时唯一能还原上游原文的东西。
	if string(got.Raw) != string(raw) {
		t.Error("Raw 应是验签通过后的原始报文字节")
	}

	// 真实回调报文里**没有**事件类型、域名验证状态、发生时间。
	// 这三个字段留空是刻意的，见 notify.go 的说明。
	if got.EventType != "" {
		t.Errorf("真实回调没有事件类型字段，不该编一个出来：%q", got.EventType)
	}
	if len(got.Domains) != 0 {
		t.Errorf("真实回调没有域名验证状态，实际 %+v", got.Domains)
	}
	if !got.OccurredAt.IsZero() {
		t.Errorf("真实回调没有发生时间，不该编一个出来：%v", got.OccurredAt)
	}
}

// TestParseNotificationCancelledPayload 验证取消回调（字段更少）也能解析。
func TestParseNotificationCancelledPayload(t *testing.T) {
	c := newNotifyClient(t)
	raw := []byte(realCancelledPayload)

	got, err := c.ParseNotification(raw, Sign(testAPIKey, raw))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if got.Status != "3005" {
		t.Errorf("status 应为 3005，实际 %q", got.Status)
	}
	if got.UpstreamOrderNo != "202001016666" {
		t.Errorf("orderNo 映射错了：%q", got.UpstreamOrderNo)
	}
}

// TestParseNotificationRejectsWrongSecret 验证签名密钥就是 API Key。
//
// 上游文档给的验签示例是 crypto.createHmac('sha256', apiKey)。
// 如果实现里换成了另一个密钥（例如一个独立的 webhook secret），
// 所有回调都会验签失败——而错误信息只会说「签名不匹配」，
// 排查方向会跑到「上游改了签名算法」上去。
func TestParseNotificationRejectsWrongSecret(t *testing.T) {
	c := newNotifyClient(t)
	raw := []byte(realIssuedPayload)

	// 用另一个足够长的密钥签，确认不是「随便什么串都能过」。
	other := Sign("a-completely-different-secret-000000", raw)
	if _, err := c.ParseNotification(raw, other); !errors.Is(err, ErrInvalidSignature) {
		t.Fatalf("换一把密钥签应当被拒，实际 %v", err)
	}
}

// TestParseNotificationRejectsTamperedBody 验证报文被改动后验签失败。
//
// 这是验签存在的全部意义：回调接口不套登录鉴权，任何人都能往它发请求。
func TestParseNotificationRejectsTamperedBody(t *testing.T) {
	c := newNotifyClient(t)
	raw := []byte(realIssuedPayload)
	sig := Sign(testAPIKey, raw)

	// 把订单号改掉——攻击者最想做的就是让平台把别人的证书
	// 认成自己的订单。
	tampered := []byte(strings.Replace(realIssuedPayload,
		"2021011221355666", "2021011221355667", 1))
	if _, err := c.ParseNotification(tampered, sig); !errors.Is(err, ErrInvalidSignature) {
		t.Fatalf("改动报文后验签应当失败，实际 %v", err)
	}
}

// TestParseNotificationRejectsMalformed 验证验签通过但字段残缺时报格式错误。
//
// 与签名失败分开：签名失败意味着「有人在伪造」，格式错误意味着
// 「上游改了报文格式」，两者的处置方式完全不同。
func TestParseNotificationRejectsMalformed(t *testing.T) {
	c := newNotifyClient(t)
	cases := map[string]string{
		"缺 orderNo":    `{"notifyInfo":{"status":"3004"}}`,
		"缺 status":     `{"notifyInfo":{"orderNo":"2021011221355666"}}`,
		"orderNo 只有空白": `{"notifyInfo":{"orderNo":"  ","status":"3004"}}`,
		"根本不是 JSON":    `not json at all`,
	}
	for name, payload := range cases {
		t.Run(name, func(t *testing.T) {
			raw := []byte(payload)
			_, err := c.ParseNotification(raw, Sign(testAPIKey, raw))
			if !errors.Is(err, ErrMalformedPayload) {
				t.Fatalf("应返回 ErrMalformedPayload，实际 %v", err)
			}
			// 不能被误判成签名失败：那会把「上游改了格式」报成
			// 「有人在伪造」，让人去查安全事件。
			if errors.Is(err, ErrInvalidSignature) {
				t.Error("格式错误不该被报成签名失败")
			}
		})
	}
}

// TestParseNotificationChecksSignatureBeforeParsing 验证先验签再解析。
//
// 顺序不可交换：先解析会让未经认证的报文进入 JSON 解码器，
// 把攻击面扩大到解码器的所有实现细节上。这里用一个「签名错且报文残缺」
// 的输入来区分两者——如果实现先解析，报出来的会是格式错误。
func TestParseNotificationChecksSignatureBeforeParsing(t *testing.T) {
	c := newNotifyClient(t)
	raw := []byte(`{"notifyInfo":{}}`)

	_, err := c.ParseNotification(raw, "wrong-signature")
	if !errors.Is(err, ErrInvalidSignature) {
		t.Fatalf("签名错且报文残缺时应报签名失败（说明先验签），实际 %v", err)
	}
}

// TestAckBody 验证应答体是上游认的那一个。
//
// 上游只在收到 HTTP 200 + {"status":"success"} 时才算「确认接收」，
// 否则按 7 个阶段重推（1 分钟内 3 次、5 分钟 5 次……直到 24 小时 1 次）。
// 返回本服务的统一响应信封会被判为失败并触发无限重推。
func TestAckBody(t *testing.T) {
	ack := newNotifyClient(t).Ack()
	if ack.ContentType != "application/json" {
		t.Errorf("Content-Type 应为 application/json，实际 %q", ack.ContentType)
	}
	if string(ack.Body) != `{"status":"success"}` {
		t.Errorf("应答体应为上游约定的那个，实际 %s", ack.Body)
	}
}

// TestSignatureHeaderName 验证签名头名与文档一致。
//
// 上游发的是 X-Webhook-Signature。取错头名的表现是「所有回调都验签失败」，
// 而原因看起来像密钥不对。
func TestSignatureHeaderName(t *testing.T) {
	if got := SignatureHeader(); got != "X-Webhook-Signature" {
		t.Errorf("签名头名应为 X-Webhook-Signature，实际 %q", got)
	}
}
