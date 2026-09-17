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
//  1. **CreateOrder 不幂等，重试前必须反查。**
//
//     这一条原本写的是反过来的：以为上游按商户订单号去重，所以
//     「超时后重试不会产生第二张证书」。上游文档推翻了它——下单接口
//     的请求体里**没有任何商户侧标识**（只有年限、验证方式、CSR、
//     域名、联系人、企业信息、回调地址），orderNo 由上游生成后返回。
//
//     也就是说「用同一个商户订单号重试」这条路根本不存在：重复提交
//     就是真的再买一张证书，平台被扣两次钱，而且每次都是真的。
//
//     所以结果未知时的流程是：**先 FindOrder 反查**（按常用名称 +
//     下单前后的时间窗口），确认上游没有匹配的单，才允许重试。
//     这条流程还没有实现（补偿轮询整条链路是空的），在那之前
//     真实上游的 CreateOrder 一旦结果未知，订单只能停在 submitting
//     等人工介入——这是安全的，不会花错钱。
//
//  2. 上游状态一律以**原文**返回，本包不做翻译。
//     上游新增状态值时不应该让本服务出错，映射成平台状态是订单域的职责。
//     原文包括：订单/证书状态码（"1002"、"3004"）、域名验证状态码
//     （"2001"、"2002"）、响应侧的验证方式常量名（HTTP_CSR_HASH 等）。
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
	// MerchantOrderNo 是平台订单号。
	//
	// **它不是上游侧的幂等键。** 上游的下单接口不接受任何商户标识，
	// 所以这个值不会被发送出去，只用于日志与本地关联。
	// 保留字段而不是删掉：出问题时最需要的信息就是「这次调用属于哪张
	// 平台订单」，而调用点手上有的是订单号，不是上游订单号。
	MerchantOrderNo string
	// UpstreamProductID 是上游的产品编号，与平台产品 ID 不是一回事。
	// 它是请求路径的一部分（/certificates/id/:pNo）。
	UpstreamProductID int
	Years             int
	KeyAlgorithm      string
	// DcvMethod 是下单时要指定的域名验证方式。
	//
	// **上游把它列为必填**，而平台把「选哪种方式」放在域名验证阶段，
	// 两者错位。所以调用方在下单时先给一个初始值（取产品声明的第一个
	// 可用方式），用户提交验证时再按实际选择走。取值是平台侧的
	// rules.DcvMethod。
	DcvMethod string
	// Domains 第一个是主域名。上游只接收主域名之外的 SAN，
	// 主域名由它从 CSR 里取。
	Domains []string
	// CSR 是证书签名请求。为空表示由上游代生成，此时私钥在上游手里。
	CSR     string
	Contact Contact
	// Organization 只有 OV / EV 需要。上游对缺企业信息的 OV / EV
	// 返回 7100，所以这里为空时上游会明确拒绝，不会静默降级成 DV。
	Organization *Organization
	// NotifyURL 是上游推送事件的地址。
	//
	// 不传则上游不推送任何事件，平台只能靠轮询拿状态。
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
	// CSR **必须提供**。上游把重签的 csr 列为必填，不接受「沿用原证书」
	// 这种省略——所以调用方要把它存的 CSR 原文重新提交一次。
	CSR string
	// DcvMethod 是重签后的验证方式，上游必填。取值是平台侧的 rules.DcvMethod。
	DcvMethod string
	// Domains 是重签后的域名列表，第一个是主域名。
	Domains []string
	// Reason 是平台侧记录的重签原因，只用于本地审计。
	// 上游没有对应参数，不会被发送。
	Reason string
}

// FindOrderRequest 是反查上游订单的条件。
//
// 反查是 CreateOrder 不幂等的必要配套：结果未知时不能直接重试，
// 得先看看上游到底建没建单（见包注释的约定 1）。
type FindOrderRequest struct {
	// CommonName 是常用名称（域名或公司名）。空表示不按它过滤。
	CommonName string
	// CreatedAfter / CreatedBefore 限定订单创建时间窗口，零值表示不限。
	//
	// **窗口要卡得足够窄。** 上游不提供按商户标识查询，反查只能靠
	// 常用名称 + 时间，所以窗口太宽会把同一域名的历史订单一起捞回来，
	// 反而被误判成「已经建过单了」而放弃重试——订单就永远卡住了。
	CreatedAfter  time.Time
	CreatedBefore time.Time
}

// OrderSummary 是反查结果里的一条上游订单。
type OrderSummary struct {
	UpstreamOrderNo string
	ProductID       string
	CommonName      string
	OrderStatus     string
	CertStatus      string
	PrepareStatus   string
	CreatedAt       time.Time
	// PaidAt 是上游记录的支付时间，未支付时为零值。
	//
	// 它是判断「上游到底建没建单」最直接的一个字段：有了支付时间，
	// 说明上游不但建了单，还扣了平台的预存余额。
	PaidAt time.Time
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
	// **不幂等：重复调用会在上游产生第二张证书并扣第二次钱。**
	// 上游的下单接口没有任何商户侧标识，平台无法让它幂等。
	// 结果未知时不得直接重试，必须先 FindOrder 反查。
	CreateOrder(ctx context.Context, req CreateOrderRequest) (*CreateOrderResponse, error)

	// OrderStatus 查询订单状态。
	OrderStatus(ctx context.Context, upstreamOrderNo string) (*OrderStatus, error)

	// ListDomains 查询域名的验证材料。
	ListDomains(ctx context.Context, upstreamOrderNo string) (*DomainsResponse, error)

	// VerifyDomains 通知上游去实际校验这些域名。
	//
	// 真实上游这个接口的请求体在文档里缺失，适配器因此不实现它
	// （调用返回 ErrNotSupported），见 methods.go 的说明。
	VerifyDomains(ctx context.Context, req VerifyDomainsRequest) error

	// ResendDcvEmail 重发域名验证邮件。
	//
	// domains 是平台侧的用法（只重发关心的那几个）。真实上游按**整单**
	// 重发、不接受域名参数，所以适配器会忽略这个参数——多发给几个域名
	// 是无害的，而报错会让这个平台侧用法彻底不可用。
	ResendDcvEmail(ctx context.Context, upstreamOrderNo string, domains []string) error

	// RegenerateDcvToken 重新生成验证 token，返回更新后的材料。
	//
	// 调用后旧 token 立即失效。
	//
	// 平台按**整单**操作，真实上游按**单个域名**操作（要传 domain）。
	// 适配器负责这个翻译：逐个域名调用，最后重新拉一次材料。
	RegenerateDcvToken(ctx context.Context, upstreamOrderNo string) (*DomainsResponse, error)

	// DownloadCertificate 下载已签发的证书。
	DownloadCertificate(ctx context.Context, upstreamOrderNo string) (*Certificate, error)

	// Reissue 申请重签。
	Reissue(ctx context.Context, req ReissueRequest) error

	// CancelOrder 取消订单。
	CancelOrder(ctx context.Context, upstreamOrderNo string) error

	// FindOrder 按常用名称与创建时间窗口反查上游订单。
	//
	// 它是 CreateOrder 不幂等的必要配套：结果未知时先用它确认上游
	// 有没有建单，再决定是否重试。没有它，真实上游的补偿重试就只能
	// 靠人工——而人工看着一堆「submitting」的订单是没法判断的。
	FindOrder(ctx context.Context, req FindOrderRequest) ([]OrderSummary, error)

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
	//
	// 真实上游用业务码 6010 表达这件事（它所有接口都返回 HTTP 200），
	// 所以 HTTP 404 不映射到这里——那个 404 只可能来自网关。
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
	// ErrPlatformBalance 表示平台在上游的预存余额不足。
	//
	// 与 ErrUnauthorized 同类：这是平台侧的问题，不是用户的问题，
	// 运维需要据此告警。**刻意不包 ErrUnavailable**：重试不会让余额
	// 变多，而把它当成「结果未知」会让订单一直冻着等补偿，
	// 补偿用的还是那个不够的余额。
	ErrPlatformBalance = errors.New("上游账户余额不足")
	// ErrNotSupported 表示上游不支持该操作，重试没有意义。
	ErrNotSupported = errors.New("上游不支持该操作")
	// ErrInvalidSignature 表示回调签名校验失败。
	ErrInvalidSignature = errors.New("上游回调签名校验失败")
	// ErrMalformedPayload 表示报文格式与预期不符。
	//
	// 两个方向共用：出站时是「上游改了响应格式」，入站时是
	// 「验签通过但回调报文解析不了」。
	//
	// 与 ErrInvalidSignature 分开：后者意味着「有人在伪造」，
	// 前者意味着「上游改了报文格式」，两者的处置方式完全不同。
	ErrMalformedPayload = errors.New("上游报文格式错误")
)
