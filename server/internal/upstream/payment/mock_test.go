package payment

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/cangerx/c-ssl/server/internal/domain/money"
)

const testSecret = "test-payment-webhook-secret"

func newTestChannel(t *testing.T) *MockChannel {
	t.Helper()
	ch, err := NewMockChannel(testSecret, "http://localhost:8080")
	if err != nil {
		t.Fatalf("构造 Mock 渠道失败: %v", err)
	}
	return ch
}

func testNotification() *Notification {
	return &Notification{
		Channel:        NameMock,
		ChannelTradeNo: "MOCK-TRADE-0001",
		ChannelOrderNo: "MOCK-ORDER-RC0001",
		OrderNo:        "RC20260916191234A7K3M9",
		Amount:         10000,
		Status:         StatusSuccess,
		PaidAt:         time.Date(2026, 9, 16, 11, 12, 34, 0, time.UTC),
	}
}

// TestSignatureRoundTrip 验证自己签的报文能通过自己的验签。
//
// 这条看起来是废话，但它覆盖的是一类真实故障：构造报文与解析报文
// 各写一份格式定义，两边漂移之后「测试全绿但真实回调解析失败」。
// 本项目让两者共用 EncodeNotification / ParseNotification，这里就是那个保证。
func TestSignatureRoundTrip(t *testing.T) {
	ch := newTestChannel(t)
	want := testNotification()

	raw, signature, err := ch.EncodeNotification(want)
	if err != nil {
		t.Fatalf("编码回调报文失败: %v", err)
	}

	got, err := ch.ParseNotification(raw, signature)
	if err != nil {
		t.Fatalf("解析回调报文失败: %v", err)
	}

	if got.Channel != NameMock {
		t.Errorf("渠道标识不符：期望 %s，实际 %s", NameMock, got.Channel)
	}
	if got.ChannelTradeNo != want.ChannelTradeNo {
		t.Errorf("渠道交易号不符：期望 %s，实际 %s", want.ChannelTradeNo, got.ChannelTradeNo)
	}
	if got.OrderNo != want.OrderNo {
		t.Errorf("充值单号不符：期望 %s，实际 %s", want.OrderNo, got.OrderNo)
	}
	if got.Amount != want.Amount {
		t.Errorf("金额不符：期望 %d，实际 %d", want.Amount, got.Amount)
	}
	if got.Status != want.Status {
		t.Errorf("支付结果不符：期望 %s，实际 %s", want.Status, got.Status)
	}
	if !got.PaidAt.Equal(want.PaidAt) {
		t.Errorf("支付时间不符：期望 %s，实际 %s", want.PaidAt, got.PaidAt)
	}
}

// TestParseRejectsTamperedBody 验证改动报文任何一个字节都会验签失败。
//
// 逐个位置改而不是只改一处：只改一处的话，如果实现恰好用了
// 「只对某个字段签名」这种错误做法，测试也可能通过。
func TestParseRejectsTamperedBody(t *testing.T) {
	ch := newTestChannel(t)
	raw, signature, err := ch.EncodeNotification(testNotification())
	if err != nil {
		t.Fatalf("编码回调报文失败: %v", err)
	}

	for i := range raw {
		tampered := make([]byte, len(raw))
		copy(tampered, raw)
		// 改成一个一定不同的字节，且保持仍是合法 JSON 字符
		if tampered[i] == 'x' {
			tampered[i] = 'y'
		} else {
			tampered[i] = 'x'
		}

		if _, err := ch.ParseNotification(tampered, signature); !errors.Is(err, ErrInvalidSignature) {
			t.Fatalf("第 %d 字节被篡改后仍通过验签（错误为 %v），签名没有覆盖整个报文", i, err)
		}
	}
}

func TestParseRejectsBadSignature(t *testing.T) {
	ch := newTestChannel(t)
	raw, signature, err := ch.EncodeNotification(testNotification())
	if err != nil {
		t.Fatalf("编码回调报文失败: %v", err)
	}

	cases := map[string]string{
		"空签名":     "",
		"签名截断":    signature[:len(signature)-2],
		"签名多一位":   signature + "A",
		"完全不同的签名": "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=",
	}
	for name, bad := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ch.ParseNotification(raw, bad); !errors.Is(err, ErrInvalidSignature) {
				t.Errorf("期望 ErrInvalidSignature，实际 %v", err)
			}
		})
	}
}

// TestParseRejectsAnotherChannelsSignature 验证用别的密钥签的报文通不过。
//
// 这条是「验签真的在起作用」的正面证据：签名算法、长度、编码都正确，
// 只有密钥不同，仍然必须被拒绝。
func TestParseRejectsAnotherChannelsSignature(t *testing.T) {
	ch := newTestChannel(t)
	attacker, err := NewMockChannel("another-secret-entirely", "http://localhost:8080")
	if err != nil {
		t.Fatalf("构造对照渠道失败: %v", err)
	}

	raw, forgedSignature, err := attacker.EncodeNotification(testNotification())
	if err != nil {
		t.Fatalf("编码伪造报文失败: %v", err)
	}

	if _, err := ch.ParseNotification(raw, forgedSignature); !errors.Is(err, ErrInvalidSignature) {
		t.Errorf("用别的密钥签名的报文被接受了，错误为 %v", err)
	}
}

// TestParseRejectsMalformedPayload 验证验签通过但报文不完整时被拒绝。
//
// 验签只证明报文来自持有密钥的一方，不证明它内容完整。
// 这些报文都是「合法签名 + 缺字段」，必须与「有人在伪造」区分开：
// 前者是 ErrMalformedPayload（渠道改了格式），后者是 ErrInvalidSignature。
func TestParseRejectsMalformedPayload(t *testing.T) {
	ch := newTestChannel(t)

	cases := map[string]string{
		"缺少渠道交易号": `{"orderNo":"RC1","amount":100,"status":"success"}`,
		"缺少充值单号":  `{"channelTradeNo":"T1","amount":100,"status":"success"}`,
		"金额为零":    `{"channelTradeNo":"T1","orderNo":"RC1","amount":0,"status":"success"}`,
		"金额为负":    `{"channelTradeNo":"T1","orderNo":"RC1","amount":-100,"status":"success"}`,
		"未知支付结果":  `{"channelTradeNo":"T1","orderNo":"RC1","amount":100,"status":"refunded"}`,
		"支付结果为空":  `{"channelTradeNo":"T1","orderNo":"RC1","amount":100}`,
		"时间格式错误":  `{"channelTradeNo":"T1","orderNo":"RC1","amount":100,"status":"success","paidAt":"昨天"}`,
		"不是 JSON": `not json at all`,
	}

	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			raw := []byte(body)
			_, err := ch.ParseNotification(raw, ch.Sign(raw))
			if !errors.Is(err, ErrMalformedPayload) {
				t.Errorf("期望 ErrMalformedPayload，实际 %v", err)
			}
		})
	}
}

// TestCreatePaymentBuildsPayURL 验证支付地址带上单号与金额。
//
// 金额也放进地址：支付页要展示用户付多少钱，缺了它开发时只能靠猜。
func TestCreatePaymentBuildsPayURL(t *testing.T) {
	ch := newTestChannel(t)

	created, err := ch.CreatePayment(context.Background(), CreateRequest{
		OrderNo: "RC20260916191234A7K3M9",
		Amount:  10000,
	})
	if err != nil {
		t.Fatalf("创建支付失败: %v", err)
	}

	if !strings.Contains(created.PayURL, "orderNo=RC20260916191234A7K3M9") {
		t.Errorf("支付地址缺少单号: %s", created.PayURL)
	}
	if !strings.Contains(created.PayURL, "amount=10000") {
		t.Errorf("支付地址缺少金额: %s", created.PayURL)
	}
	if created.ChannelOrderNo == "" {
		t.Error("渠道订单号不应为空")
	}
}

func TestCreatePaymentRejectsBadInput(t *testing.T) {
	ch := newTestChannel(t)

	if _, err := ch.CreatePayment(context.Background(), CreateRequest{Amount: 100}); err == nil {
		t.Error("单号为空时应报错")
	}
	if _, err := ch.CreatePayment(context.Background(), CreateRequest{OrderNo: "RC1"}); err == nil {
		t.Error("金额为零时应报错")
	}
}

// TestNewMockChannelRejectsEmptySecret 验证空密钥在构造时就被拦住。
//
// 空密钥下 HMAC 的签名是公开可算的，验签会退化成一道装饰。
// 让它在构造时失败，比让服务带着一个形同虚设的验签跑起来安全得多。
func TestNewMockChannelRejectsEmptySecret(t *testing.T) {
	if _, err := NewMockChannel("", "http://localhost:8080"); err == nil {
		t.Error("空密钥应被拒绝")
	}
	if _, err := NewMockChannel(testSecret, ""); err == nil {
		t.Error("缺少站点地址应被拒绝")
	}
}

// TestAckIsUnifiedEnvelope 验证应答体是契约里的统一信封。
//
// 应答格式由渠道决定而不是本服务的偏好，所以这里断言的是
// 「Mock 渠道选了这个格式」——换成真实渠道时这条测试会跟着改，
// 那正是它应该发生的事。
func TestAckIsUnifiedEnvelope(t *testing.T) {
	ack := newTestChannel(t).Ack()

	if ack.ContentType != "application/json; charset=utf-8" {
		t.Errorf("Content-Type 不符: %s", ack.ContentType)
	}

	var envelope struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(ack.Body, &envelope); err != nil {
		t.Fatalf("应答体不是合法 JSON: %v", err)
	}
	if envelope.Code != 0 {
		t.Errorf("应答 code 应为 0，实际 %d", envelope.Code)
	}
	if envelope.Message != "success" {
		t.Errorf("应答 message 应为 success，实际 %s", envelope.Message)
	}
}

// TestAmountIsCents 验证报文里的金额单位是分，不是元。
//
// 单位搞错是最容易发生、也最难发现的一类资金 bug：10000 分与 10000 元
// 相差 100 倍，而两者都是合法数字。这里用「金额原样往返」把单位钉住。
func TestAmountIsCents(t *testing.T) {
	ch := newTestChannel(t)

	n := testNotification()
	n.Amount = money.Amount(12345) // 123.45 元

	raw, signature, err := ch.EncodeNotification(n)
	if err != nil {
		t.Fatalf("编码失败: %v", err)
	}

	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("报文不是合法 JSON: %v", err)
	}
	if got := payload["amount"]; got != float64(12345) {
		t.Errorf("报文里的金额应为 12345（分），实际 %v", got)
	}

	parsed, err := ch.ParseNotification(raw, signature)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if parsed.Amount != 12345 {
		t.Errorf("解析后的金额应为 12345 分，实际 %d", parsed.Amount)
	}
}

// TestParseCopiesRawPayload 验证解析结果里的原始报文是副本。
//
// 直接引用调用方的缓冲区，会在异步落库时读到被改写的内容。
func TestParseCopiesRawPayload(t *testing.T) {
	ch := newTestChannel(t)
	raw, signature, err := ch.EncodeNotification(testNotification())
	if err != nil {
		t.Fatalf("编码失败: %v", err)
	}

	parsed, err := ch.ParseNotification(raw, signature)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(parsed.Raw) == 0 {
		t.Fatal("原始报文不应为空")
	}

	original := string(parsed.Raw)
	for i := range raw {
		raw[i] = 'x'
	}
	if string(parsed.Raw) != original {
		t.Error("原始报文被外部改动影响了，说明没有复制")
	}
}
