package payment

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/cangerx/c-ssl/server/internal/domain/money"
)

// MockChannel 是开发环境使用的模拟支付渠道。
//
// 它把「用户在支付页完成支付」这一步压缩成本地生成一个支付地址，
// 其余环节（报文格式、HMAC-SHA256 签名、回调处理、幂等）与真实渠道完全一致。
// 这样 Phase 1 能验证资金状态机，切到真实渠道时只需换一个 Channel 实现。
type MockChannel struct {
	secret  []byte
	baseURL string
}

// NewMockChannel 构造 Mock 渠道。
//
// 密钥为空直接报错：HMAC 用空密钥时任何人都能算出同样的签名，
// 验签会退化成一道没有任何作用的装饰。
func NewMockChannel(secret, baseURL string) (*MockChannel, error) {
	if secret == "" {
		return nil, fmt.Errorf("mock 支付渠道密钥未配置（PAYMENT_WEBHOOK_SECRET）")
	}
	if baseURL == "" {
		return nil, fmt.Errorf("mock 支付渠道缺少站点地址（APP_BASE_URL）")
	}
	return &MockChannel{
		secret:  []byte(secret),
		baseURL: strings.TrimRight(baseURL, "/"),
	}, nil
}

// Name 返回渠道标识。
func (c *MockChannel) Name() string { return NameMock }

// CreatePayment 在本地生成支付凭据，不访问网络。
//
// req.NotifyURL 被忽略：Mock 渠道不会从外部发起网络回调，
// 开发环境的回调由 POST /payments/mock/notify 在进程内模拟。
// 该字段保留在 CreateRequest 里，因为真实渠道都要求传回调地址。
func (c *MockChannel) CreatePayment(_ context.Context, req CreateRequest) (*Payment, error) {
	if req.OrderNo == "" {
		return nil, fmt.Errorf("创建 Mock 支付失败: 充值单号为空")
	}
	if !req.Amount.IsPositive() {
		return nil, fmt.Errorf("创建 Mock 支付失败: 金额必须为正")
	}

	query := url.Values{}
	query.Set("orderNo", req.OrderNo)
	query.Set("amount", fmt.Sprint(req.Amount.Cents()))

	return &Payment{
		ChannelOrderNo: channelOrderPrefix + req.OrderNo,
		PayURL:         c.baseURL + "/mock-pay?" + query.Encode(),
	}, nil
}

const channelOrderPrefix = "MOCK-ORDER-"

// mockNotification 是 Mock 渠道回调报文的线上格式。
//
// 字段名与 openapi/components/schemas/payment.yaml 的 PaymentNotification 一致。
// 真实渠道的字段名各不相同（微信是 out_trade_no / transaction_id），
// 那些差异由各自的适配器消化，不渗透到这里。
type mockNotification struct {
	ChannelTradeNo string `json:"channelTradeNo"`
	ChannelOrderNo string `json:"channelOrderNo"`
	OrderNo        string `json:"orderNo"`
	Amount         int64  `json:"amount"`
	Status         string `json:"status"`
	PaidAt         string `json:"paidAt,omitempty"`
}

// ParseNotification 验签并解析回调报文。
func (c *MockChannel) ParseNotification(raw []byte, signature string) (*Notification, error) {
	// 先验签再解析。顺序不能反：验签只需要字节，是更廉价的一步；
	// 而解析会把不可信输入喂给 JSON 解码器，能不做就不做。
	if err := c.verify(raw, signature); err != nil {
		return nil, err
	}

	var payload mockNotification
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrMalformedPayload, err)
	}

	// 逐字段校验。验签只证明报文来自持有密钥的一方，不证明它内容完整——
	// 渠道发来缺字段的报文时要在适配器里拦住，
	// 否则零值会一路流进资金逻辑，变成「金额 0 元、单号空串」的加款请求。
	switch {
	case payload.ChannelTradeNo == "":
		return nil, fmt.Errorf("%w: 缺少 channelTradeNo", ErrMalformedPayload)
	case payload.OrderNo == "":
		return nil, fmt.Errorf("%w: 缺少 orderNo", ErrMalformedPayload)
	case payload.Amount <= 0:
		return nil, fmt.Errorf("%w: amount 必须为正整数（单位分）", ErrMalformedPayload)
	case !Status(payload.Status).Valid():
		return nil, fmt.Errorf("%w: 未知的支付结果 %q", ErrMalformedPayload, payload.Status)
	}

	var paidAt time.Time
	if payload.PaidAt != "" {
		parsed, err := time.Parse(time.RFC3339, payload.PaidAt)
		if err != nil {
			return nil, fmt.Errorf("%w: paidAt 不是 RFC 3339 时间", ErrMalformedPayload)
		}
		paidAt = parsed
	}

	return &Notification{
		// Channel 取自适配器自己的名字，不取自报文。
		// 取自报文意味着签名通过后还能被报文内容改变路由，没有必要。
		Channel:        NameMock,
		ChannelTradeNo: payload.ChannelTradeNo,
		ChannelOrderNo: payload.ChannelOrderNo,
		OrderNo:        payload.OrderNo,
		Amount:         money.Amount(payload.Amount),
		Status:         Status(payload.Status),
		PaidAt:         paidAt,
		// 复制一份：raw 由调用方持有，可能是会被复用的读缓冲区。
		// 直接引用会在异步落库时读到被改写的内容。
		Raw: bytes.Clone(raw),
	}, nil
}

// Ack 返回 Mock 渠道期望的成功应答，与契约里的 PaymentWebhookAck 一致。
func (c *MockChannel) Ack() Ack {
	return Ack{
		ContentType: "application/json; charset=utf-8",
		Body:        []byte(`{"code":0,"message":"success","data":null}`),
	}
}

// Sign 计算报文签名：对原始字节做 HMAC-SHA256，再 Base64 编码。
//
// 与 docs/06 第 6 节里 FoxSSL Webhook 的方案一致，不另立一套。
func (c *MockChannel) Sign(raw []byte) string {
	mac := hmac.New(sha256.New, c.secret)
	mac.Write(raw)
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// EncodeNotification 把归一化结构编码成 Mock 渠道的报文字节并计算签名。
//
// 导出是刻意的：测试与开发环境的模拟回调都需要构造「带合法签名的报文」。
// 如果各处自己拼 JSON，报文格式一旦调整就会出现「测试全绿但真实回调解析失败」
// ——因为测试签的是它自己拼的那份字节，与适配器解析的不是同一份。
// 让构造与解析共用同一处定义，这类漂移就不可能发生。
func (c *MockChannel) EncodeNotification(n *Notification) ([]byte, string, error) {
	var paidAt string
	if !n.PaidAt.IsZero() {
		paidAt = n.PaidAt.UTC().Format(time.RFC3339)
	}

	raw, err := json.Marshal(mockNotification{
		ChannelTradeNo: n.ChannelTradeNo,
		ChannelOrderNo: n.ChannelOrderNo,
		OrderNo:        n.OrderNo,
		Amount:         n.Amount.Cents(),
		Status:         string(n.Status),
		PaidAt:         paidAt,
	})
	if err != nil {
		return nil, "", fmt.Errorf("编码 Mock 回调报文失败: %w", err)
	}
	return raw, c.Sign(raw), nil
}

// verify 校验签名。
func (c *MockChannel) verify(raw []byte, signature string) error {
	if signature == "" {
		return ErrInvalidSignature
	}
	// 用 hmac.Equal 而不是 ==：普通比较在第一个不同的字节处就返回，
	// 比较耗时随匹配前缀增长，攻击者可以据此逐字节猜出正确签名。
	if !hmac.Equal([]byte(c.Sign(raw)), []byte(signature)) {
		return ErrInvalidSignature
	}
	return nil
}

// NewTradeNo 生成一个渠道交易号。
//
// 供开发环境的模拟回调与测试使用，让「同一个交易号重复回调」
// 与「不同交易号」两种场景都能被构造出来。
func NewTradeNo(now time.Time) string {
	var buf [4]byte
	if _, err := rand.Read(buf[:]); err != nil {
		// crypto/rand 失败属于系统级异常，退回时间纳秒保证不 panic
		return fmt.Sprintf("MOCK-TRADE-%d", now.UnixNano())
	}
	return fmt.Sprintf("MOCK-TRADE-%s-%s", now.UTC().Format("20060102150405"), hex.EncodeToString(buf[:]))
}
