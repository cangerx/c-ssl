// Package foxssl 是 FoxSSL 上游的适配层。
//
// 业务代码只依赖本包的 Client 接口，不直接发 HTTP。这与支付渠道适配层
// （internal/upstream/payment）是同一个思路：接入真实上游时新增一个实现，
// 订单域一行都不用改。
//
// 为什么先做 Mock：真实 FoxSSL 凭证要等商务开通，而订单域的资金流程
// （冻结、结算、解冻、补偿）与上游可用性无关，可以先用 Mock 跑通并验收。
//
// ── 两条必须守住的约定 ──────────────────────────────
//
//  1. CreateOrder 以 MerchantOrderNo 为幂等键。上游按商户订单号去重，
//     重复提交返回同一个上游订单。这是「网络超时后重试不会产生第二张证书」
//     的唯一保证——平台拿不到「上一次到底成没成功」的答案时只能重试，
//     如果上游不去重，重试就会真的下一单，用户被签两张证书、平台被扣两次钱。
//
//  2. 上游状态一律以**原文**返回，本包不做翻译。
//     上游新增状态值时不应该让本服务出错，映射成平台状态是订单域的职责。
package foxssl

import (
	"context"
	"errors"
	"time"

	"github.com/cangerx/c-ssl/server/internal/domain/money"
)

// 已注册的上游标识。
const (
	// NameMock 是开发环境的模拟上游。
	NameMock = "mock"
	// NameHTTP 是真实上游。
	//
	// 取值与配置里 FOXSSL_PROVIDER 的对应关系待确认，见 http.go 末尾的
	// 待办清单——配置校验目前只放行 mock。
	NameHTTP = "foxssl"
)

// ── 出站请求与响应 ────────────────────────────────

// Balance 是上游账户余额。
type Balance struct {
	// Amount 是可用余额，单位分。
	Amount money.Amount
	// Currency 是币种，通常为 CNY。
	Currency string
}

// Contact 是证书联系人。
type Contact struct {
	Name  string
	Email string
	Phone string
	Title string
}

// Organization 是企业主体信息，只有 OV / EV 需要。
type Organization struct {
	Name           string
	RegistrationNo string
	Country        string
	Province       string
	City           string
	Address        string
	PostalCode     string
	Phone          string
}

// CreateOrderRequest 是在上游创建订单所需的信息。
type CreateOrderRequest struct {
	// MerchantOrderNo 是平台订单号，同时是上游侧的幂等键。
	//
	// 必须是平台的 order_no 而不是随机值：重试时要能构造出同一个值，
	// 否则上游去重无从谈起。
	MerchantOrderNo string
	// UpstreamProductID 是上游的产品编号，与平台产品 ID 不是一回事。
	UpstreamProductID int
	Years             int
	KeyAlgorithm      string
	// Domains 第一个是主域名。
	Domains []string
	// CSR 是证书签名请求。为空表示由上游代生成，此时私钥在上游手里。
	CSR          string
	Contact      Contact
	Organization *Organization
	// NotifyURL 是上游推送事件的地址。
	NotifyURL string
}

// CreateOrderResponse 是上游下单的结果。
type CreateOrderResponse struct {
	// UpstreamOrderNo 是上游订单号，平台据此查询与对账。
	UpstreamOrderNo string
	// CertID 是上游证书编号，下单时可能还是空的，签发后才有。
	CertID string
	// Status 是上游订单状态原文。
	Status string
	// Cost 是上游成本价，单位分。
	//
	// 这是**平台付给上游的钱**，不是用户应付的钱。用户扣款依据的是零售价，
	// 两者不做联动；Cost 只用于运营对账与毛利分析。
	Cost money.Amount
	// CreatedAt 是上游记录的创建时间。
	CreatedAt time.Time
}

// OrderStatus 是上游订单的当前状态。
//
// 四个状态字段原样对应上游文档，平台不做合并——上游的
// 「准备状态」「重签状态」与「订单状态」是三条独立的线，
// 合并成一个字段会让运营无法还原上游的真实情况。
type OrderStatus struct {
	UpstreamOrderNo   string
	OrderStatus       string
	CertStatus        string
	PrepareStatus     string
	ReissueStatus     string
	CertID            string
	CommonName        string
	Domains           []DomainStatus
	ExpiresAt         time.Time
	FailureReasonText string
}

// DomainStatus 是单个域名在上游的验证状态。
type DomainStatus struct {
	Domain string
	// Status 是上游的域名验证状态原文。
	Status string
	// Method 是上游记录的验证方式。
	Method string
}

// Domain 是上游返回的域名验证材料。
//
// **FileDcvPath 可能是含 {FQDN} 的模板**，调用方必须逐域名替换后才能展示。
// 本包不做替换：替换需要知道每个域名，而本包只知道上游返回了什么。
// 替换发生在订单域，见 internal/order 的 dcv.go。
type Domain struct {
	Domain string
	Status string
	Method string
	// DnsRecordType 是 TXT 或 CNAME。
	DnsRecordType string
	// DnsRecordName 是记录主机名，由上游指定，可能不是裸域名。
	DnsRecordName string
	// DnsRecordValue 是记录值。
	DnsRecordValue string
	// FileDcvPath 是文件验证路径，**可能含 {FQDN} 占位符**。
	FileDcvPath string
	// FileContent 是文件验证需要放置的内容。
	FileContent string
	// EmailAddresses 是邮件验证可用的收件地址。
	EmailAddresses []string
	// VerifiedAt 是验证通过时间，零值表示尚未通过。
	VerifiedAt time.Time
	// ExpiresAt 是验证失效时间，零值表示上游未给出。
	ExpiresAt time.Time
}

// DomainsResponse 是域名列表查询的结果。
type DomainsResponse struct {
	UpstreamOrderNo string
	Domains         []Domain
}

// VerifyDomainsRequest 是提交域名验证的入参。
type VerifyDomainsRequest struct {
	UpstreamOrderNo string
	Domains         []string
	// Method 是验证方式，取值由上游定义。
	Method string
}

// ReissueRequest 是重签入参。
type ReissueRequest struct {
	UpstreamOrderNo string
	// CSR 为空表示沿用原证书的 CSR。
	CSR    string
	Reason string
}

// Certificate 是已签发的证书。
type Certificate struct {
	CertID       string
	Status       string
	CommonName   string
	Domains      []string
	KeyAlgorithm string
	SerialNumber string
	// Certificate 是服务器证书 PEM。
	Certificate string
	// CABundle 是 CA 根证书链 PEM，上游未提供时为空。
	CABundle string
	IssuedAt time.Time
	// ExpiresAt 是证书到期时间。
	ExpiresAt time.Time
}

// ── 入站：上游事件回调 ────────────────────────────

// Notification 是上游回调报文归一化之后的结果。
type Notification struct {
	// EventType 是事件类型原文。
	EventType string
	// UpstreamOrderNo 是上游订单号。报文里没有平台订单号，
	// 平台据此反查本地订单。
	UpstreamOrderNo string
	// Status 是上游状态原文。
	Status string
	// CertID 是证书编号，签发类事件才有。
	CertID string
	// Domains 是域名验证状态，验证类事件才有。
	Domains []DomainStatus
	// OccurredAt 是事件在上游产生的时间，可能为零值。
	// 上游不保证投递顺序，调用方据此判断事件是否已过期。
	OccurredAt time.Time
	// Raw 是验签通过后的原始报文字节，用于落库留档。
	Raw []byte
}

// Ack 是上游期望的成功应答。
//
// 单独建模而不是统一用本服务的响应信封：上游只认它自己文档里的
// {"status":"success"}，返回统一信封会被判为失败并触发无限重推。
type Ack struct {
	ContentType string
	Body        []byte
}

// ── 接口 ──────────────────────────────────────────

// Client 是上游适配器。
//
// 出站方法与入站方法刻意放在同一个接口上，理由与支付渠道一致：
// 调用方注入的是一个「上游」而不是半个上游。需要拆分时再拆。
type Client interface {
	// Name 返回上游标识，必须与配置里使用的取值一致。
	Name() string

	// Balance 查询上游账户余额。
	Balance(ctx context.Context) (*Balance, error)

	// CreateOrder 在上游创建订单。
	//
	// 必须幂等：同一个 MerchantOrderNo 重复调用返回同一个上游订单，
	// 不得在上游产生第二张证书。
	CreateOrder(ctx context.Context, req CreateOrderRequest) (*CreateOrderResponse, error)

	// OrderStatus 查询订单状态。
	OrderStatus(ctx context.Context, upstreamOrderNo string) (*OrderStatus, error)

	// ListDomains 查询域名的验证材料。
	ListDomains(ctx context.Context, upstreamOrderNo string) (*DomainsResponse, error)

	// VerifyDomains 通知上游去实际校验这些域名。
	VerifyDomains(ctx context.Context, req VerifyDomainsRequest) error

	// ResendDcvEmail 重发域名验证邮件。
	ResendDcvEmail(ctx context.Context, upstreamOrderNo string, domains []string) error

	// RegenerateDcvToken 重新生成验证 token，返回更新后的材料。
	//
	// 调用后旧 token 立即失效。
	RegenerateDcvToken(ctx context.Context, upstreamOrderNo string) (*DomainsResponse, error)

	// DownloadCertificate 下载已签发的证书。
	DownloadCertificate(ctx context.Context, upstreamOrderNo string) (*Certificate, error)

	// Reissue 申请重签。
	Reissue(ctx context.Context, req ReissueRequest) error

	// CancelOrder 取消订单。
	CancelOrder(ctx context.Context, upstreamOrderNo string) error

	// ParseNotification 验签并解析上游回调。
	//
	// 验签与解析刻意合并在一个方法里：拆成两个的话，调用方漏掉验签
	// 不会有任何编译错误，而后果是任何人都能伪造回调把订单推进到已签发。
	// 合成一个方法后，「不验签就解析」这条路径根本不存在。
	//
	// 签名必须是上游提供的原始字节的签名。先反序列化再重新序列化会改变字节，
	// 导致验签必然失败，或者被迫实现成「重新序列化后再验签」——那等于没有验签。
	//
	// 失败时返回 ErrInvalidSignature 或 ErrMalformedPayload。
	ParseNotification(raw []byte, signature string) (*Notification, error)

	// Ack 返回上游期望的成功应答体。
	Ack() Ack
}

// 适配器可能返回的哨兵错误。订单域据此映射业务错误码，
// 不需要知道是哪个上游在报错。
var (
	// ErrOrderNotFound 表示上游没有这个订单。
	//
	// 与「上游不可用」必须分开：前者说明我们记了一个上游不认的订单号，
	// 是本地数据出了问题；后者只是暂时性故障，重试即可。
	ErrOrderNotFound = errors.New("上游订单不存在")
	// ErrUnavailable 表示上游暂时不可用，值得重试。
	ErrUnavailable = errors.New("上游服务暂时不可用")
	// ErrUnauthorized 表示上游拒绝了我们的凭证。
	//
	// **刻意不包 ErrUnavailable。** 鉴权发生在业务逻辑之前，上游确定
	// 没有建单，因此订单域可以放心解冻；把它归入「结果未知」会让
	// 每一张订单都冻着等补偿，而补偿用的还是同一份坏凭证。
	//
	// 单独建一个哨兵是为了能被认出来：这是平台的配置问题，不是用户的
	// 问题，运维需要据此告警，而不是等用户来报「下单失败」。
	ErrUnauthorized = errors.New("上游拒绝凭证")
	// ErrNotSupported 表示上游不支持该操作，重试没有意义。
	ErrNotSupported = errors.New("上游不支持该操作")
	// ErrInvalidSignature 表示回调签名校验失败。
	ErrInvalidSignature = errors.New("上游回调签名校验失败")
	// ErrMalformedPayload 表示验签通过但报文无法解析。
	//
	// 与 ErrInvalidSignature 分开：前者意味着「有人在伪造」，
	// 后者意味着「上游改了报文格式」，两者的处置方式完全不同。
	ErrMalformedPayload = errors.New("上游回调报文格式错误")
)
