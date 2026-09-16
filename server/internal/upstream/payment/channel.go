// Package payment 是支付渠道适配层。
//
// 业务代码只依赖 Channel 接口，不关心具体渠道的报文格式与签名算法。
// 这与 FoxSSL 适配层（internal/upstream/foxssl）是同一个思路：
// 接入真实渠道时新增一个实现，业务代码一行都不用改。
//
// 为什么先做 Mock：真实支付渠道需要企业资质与审核，Phase 1 用 Mock 把
// 资金状态机跑通，验收通过后再切换生产凭证（见 docs/06 第 12 节）。
package payment

import (
	"context"
	"errors"
	"time"

	"github.com/cangerx/c-ssl/server/internal/domain/money"
)

// 已注册的渠道标识。
const (
	// NameMock 是开发环境的模拟渠道。
	NameMock = "mock"
)

// Status 是渠道回执的支付结果。
type Status string

const (
	// StatusSuccess 支付成功，平台应给钱包加款。
	StatusSuccess Status = "success"
	// StatusFailed 支付失败，平台只改订单状态，不动资金。
	StatusFailed Status = "failed"
)

// Valid 判断是否为已定义的支付结果。
func (s Status) Valid() bool {
	return s == StatusSuccess || s == StatusFailed
}

// Notification 是各渠道回调报文归一化之后的结果。
//
// 归一化放在适配器里做：渠道报文的字段名、金额单位、时间格式各不相同，
// 若让业务代码去分辨「这个渠道的金额是元还是分」，那部分差异就会
// 渗透到资金逻辑里——而资金逻辑最不该有分支。
type Notification struct {
	// Channel 是渠道标识，由适配器填自己的名字，不取自报文。
	// 取自报文意味着签名通过后还能被报文内容改变路由，没有必要。
	Channel string
	// ChannelTradeNo 是渠道侧对这次支付的唯一标识，幂等键的一半。
	ChannelTradeNo string
	// ChannelOrderNo 是渠道侧订单号。
	ChannelOrderNo string
	// OrderNo 是平台充值单号。
	OrderNo string
	// Amount 是渠道实收金额，单位分。业务层会与订单金额逐分比对。
	Amount money.Amount
	// Status 是支付结果。
	Status Status
	// PaidAt 是渠道记录的支付时间，可能为零值。
	PaidAt time.Time
	// Raw 是验签通过后的原始报文字节，用于落库留档与争议排查。
	Raw []byte
}

// CreateRequest 是在渠道侧发起一次支付所需的信息。
type CreateRequest struct {
	// OrderNo 是平台充值单号，渠道以它作为商户订单号。
	OrderNo string
	// Amount 是充值金额，单位分。
	Amount money.Amount
	// Subject 是展示给用户的商品标题。
	Subject string
	// NotifyURL 是渠道回调本服务的地址。
	NotifyURL string
	// ExpireAt 是订单过期时间，渠道应在此之后拒绝支付。
	ExpireAt time.Time
}

// Payment 是渠道返回的支付凭据。
type Payment struct {
	// ChannelOrderNo 是渠道侧订单号，落库后用于对账与查询。
	ChannelOrderNo string
	// PayURL 是用户完成支付的跳转地址。
	PayURL string
}

// Ack 是渠道期望的成功应答。
//
// 单独建模而不是统一用本服务的响应信封：真实渠道各有各的成功标识
// （微信支付要 {"code":"SUCCESS"}，支付宝要纯文本 success），
// 返回统一信封会被渠道判为失败并触发无限重试。
// 应答格式属于渠道的领域知识，因此由适配器给出。
type Ack struct {
	ContentType string
	Body        []byte
}

// Channel 是支付渠道适配器。
type Channel interface {
	// Name 返回渠道标识，必须与配置里使用的取值一致。
	Name() string

	// CreatePayment 在渠道侧发起一次支付，返回支付地址。
	CreatePayment(ctx context.Context, req CreateRequest) (*Payment, error)

	// ParseNotification 验签并解析渠道回调。
	//
	// 验签与解析刻意合并在一个方法里：拆成两个的话，调用方漏掉验签
	// 不会有任何编译错误，而后果是任何人都能伪造回调给钱包充值。
	// 合成一个方法后，「不验签就解析」这条路径根本不存在。
	//
	// 签名必须是渠道提供的原始字节的签名。先反序列化再重新序列化会改变字节
	// （键顺序、空白、数字格式），导致验签必然失败，或者被迫实现成
	// 「重新序列化后再验签」——那就等于没有验签。
	//
	// 失败时返回 ErrInvalidSignature 或 ErrMalformedPayload。
	ParseNotification(raw []byte, signature string) (*Notification, error)

	// Ack 返回渠道期望的成功应答体。
	Ack() Ack
}

// 适配器可能返回的哨兵错误。业务层据此映射业务错误码，
// 不需要知道是哪个渠道在报错。
var (
	// ErrInvalidSignature 表示回调签名校验失败。
	ErrInvalidSignature = errors.New("回调签名校验失败")
	// ErrMalformedPayload 表示验签通过但报文无法解析。
	//
	// 与 ErrInvalidSignature 分开：前者意味着「有人在伪造」，
	// 后者意味着「渠道改了报文格式」，两者的处置方式完全不同。
	ErrMalformedPayload = errors.New("回调报文格式错误")
	// ErrUnknownChannel 表示渠道标识未注册。
	ErrUnknownChannel = errors.New("未注册的支付渠道")
)
