package foxssl

// 本文件是真实 FoxSSL 上游的**报文定义**：路径、请求体、响应体。
//
// 事实来源是上游官方的 Postman 文档（2021-08 至 2025-02 的历次变更说明）。
// 报文层与传输层分开成两个文件，是因为它们的失效方式完全不同：
// 传输层的 bug 是「偶发失败」，报文层的 bug 是「每次调用都失败」——
// 后者一旦写错，联调时会看到一片「字段缺失」，而错误信息里
// 不会告诉你正确字段名是什么。
//
// ── 读这份文档时踩到的六个坑 ────────────────────────
//
// 这些都不是「可能出错」，而是**照直觉写就一定会错**的地方：
//
//  1. **响应是 HTTP 200 + 业务码信封。** 不是靠 HTTP 状态区分成败。
//     上游对「余额不足」「参数错误」这类业务失败也返回 HTTP 200，
//     真正的结果在报文体的 code 里。只看 HTTP 状态会把每一次业务
//     失败都当成成功，然后拿着 data 里的 null 往下走。
//     分类逻辑在 http.go 的 classifyBusiness。
//
//  2. **鉴权头是自定义的 apiKey。** 不是 Authorization: Bearer。
//     文档里每个接口的请求头都写着 `apiKey: <your apiKey>`，且不带前缀。
//
//  3. **certID 与 orderNo 是两回事，且 certID 会超出 float64 精度。**
//     文档示例的 certID 是 142932802604630016，比 2^53 还大。
//     用 float64 解析会变成 142932802604630016 → 142932802604630020，
//     一个只在证书编号上出现的静默错误。所有 ID 字段一律 int64。
//
//  4. **域名列表接口的 dnsNames 有两种形态**：常规域名是数组，
//     IP 是裸字符串。见 wireDnsNames。
//
//  5. **请求与响应用的验证方式取值不是同一套。** 请求要小写
//     （file / dns / email / dns_txt / dns_cname），响应返回的是大写
//     常量名（HTTP_CSR_HASH / CNAME_CSR_HASH / EMAIL / DNS_TXT /
//     DNS_CNAME）。把响应里的取值直接回填到请求里，上游会回
//     6004「dcvMethod 不支持」。
//
//  6. **重生成 dcvToken 与更新验证方式共用一个路径**
//     （/certificates/dcv），只有 HTTP 方法不同：POST 是重生成，
//     PUT 是更新。写错方法不会报错，会静默地做另一件事。

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ── 路径 ──────────────────────────────────────────

// 上游接口路径。含参数的用 fmt 拼，参数一律经 urlPathEscape 转义。
const (
	pathBalance       = "/finance/balance"
	pathCreateOrder   = "/certificates/id/%d"
	pathListDomains   = "/certificates/domains/%s"
	pathVerifyDomains = "/certificates/verifyDomains/%s"
	pathOrderStatus   = "/certificates/status/%s"
	pathDownloadCert  = "/certificates/download/%s"
	pathReissue       = "/certificates/reissue"
	// pathRegenerateToken 与「更新验证方式」共用同一个路径，只有 HTTP
	// 方法不同（这里是 POST，更新验证方式是 PUT）。上游文档把两者列为
	// 两个接口，所以本适配器只实现平台用得上的这一个；
	// 更新验证方式的请求体格式记在 docs/07 备查。
	pathRegenerateToken = "/certificates/dcv"
	pathResendDcvEmail  = "/certificates/reSendDcvEmail/%s"
	pathCancelOrder     = "/certificates/cancel/%s"
	pathFindOrders      = "/certificates/orders"
)

// ── 统一信封 ──────────────────────────────────────

// 上游业务码。成功只有一个值，失败按区间分段：
// 6000-6016 下单、6100-6104 取消、6200 下载、6300-6306 改验证方式、
// 6400-6401 重生成 token、6500-6501 删域名、6600-6601 域名列表、
// 6702-6703 重发邮件、6800-6801 订单状态、6900 下载、7000-7005 联系人、
// 7100-7114 企业信息、9000-9002 生成 CSR、10000-10001 支付、
// 11000-11002 证书日志。
const (
	codeSuccess     = 200
	codeServerError = 500
)

// 与平台语义直接对应的业务码。其余码没有常量，按区间判定，见 classifyBusiness。
const (
	// codeOrderNoInvalid 订单号错误。对应「上游没有这个订单」。
	codeOrderNoInvalid = 6010
	// codeBalanceNotEnough 账户余额不足。这是**平台自己的**问题，
	// 不是用户的问题：平台预存余额不够，重试一百次也还是不够。
	codeBalanceNotEnough = 6000
	// codeOrderCreating 订单生成中。上游正在处理这次请求，
	// 可能已经建单，属于「结果未知」，见 classifyBusiness。
	codeOrderCreating = 6801
)

// envelope 是上游所有接口的统一响应外壳。
//
// data 保持 RawMessage：各接口的 data 形态完全不同（对象、null、
// 数组），在这里统一解析成 map 只会把类型信息丢掉，
// 让每个调用点重新断言一遍。
type envelope struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

// ── 余额 ──────────────────────────────────────────

type wireBalance struct {
	// Balance 是可用余额，单位分。
	Balance int64 `json:"balance"`
	// ToBeInvoicedAmount 是待开票金额，单位分。平台不用，留作对账参考。
	ToBeInvoicedAmount int64 `json:"toBeInvoicedAmount"`
}

// ── 下订单 ────────────────────────────────────────

// wireCreateOrderRequest 是下订单请求体。
//
// **这里没有任何商户侧标识。** 上游不接受商户订单号、不接受幂等键，
// orderNo 由上游生成后返回。这条事实推翻了适配层原本的假设
// （见 client.go 的包注释），并直接决定了「超时后能不能重试」的答案：
// 不能，除非先反查确认上游没建单。
type wireCreateOrderRequest struct {
	// Year 是年限。免费证书不传；其余品牌有各自的取值范围。
	Year int `json:"year,omitempty"`
	// DcvMethod 是验证方式，取值见 toWireDcvMethod，**必须小写**。
	DcvMethod string `json:"dcvMethod"`
	CSR       string `json:"csr"`
	// DomainNames 是**逗号连接**的域名字符串，且**不含主域名**——
	// 主域名由上游从 CSR 里取。只有一个域名时整个字段不传。
	DomainNames string `json:"domainNames,omitempty"`
	// ContactInfo 是联系人，SSL 证书必填。
	ContactInfo wireContactInfo `json:"contactInfo"`
	// NotifyURL 是回调地址。不传则上游不推送，平台只能靠轮询。
	NotifyURL string `json:"notifyUrl,omitempty"`
	// OrgInfo 是企业信息，OV / EV 必填。
	OrgInfo *wireOrgInfo `json:"orgInfo,omitempty"`
}

type wireContactInfo struct {
	LastName  string `json:"lastname"`
	FirstName string `json:"firstname"`
	Position  string `json:"position"`
	Email     string `json:"email"`
	Telephone string `json:"telephone"`
}

type wireOrgInfo struct {
	OrgName      string `json:"orgName"`
	CreditCode   string `json:"creditCode"`
	Country      string `json:"country"`
	Province     string `json:"province"`
	Locality     string `json:"locality"`
	Address      string `json:"address"`
	PostalCode   string `json:"postalCode"`
	Telephone    string `json:"telephone"`
	JoiCountry   string `json:"joiCountry"`
	JoiProvince  string `json:"joiProvince"`
	JoiLocality  string `json:"joiLocality"`
	RegistryAddr string `json:"registryAddr"`
	// DateOfIncorporation 格式必须是 yyyy-mm-dd。上游对格式错误返回
	// 7112，所以平台侧应当在落库前就校验，而不是等上游报错。
	DateOfIncorporation string `json:"dateOfIncorporation"`
}

// wireCreateOrderData 是下订单的响应数据。
//
// 注意它**只有三个字段**：没有订单状态，也没有创建时间。
// 上游在下单这一刻还没有状态可给（订单刚创建，还没走到支付）。
type wireCreateOrderData struct {
	// CertID 是证书编号。**必须 int64**：文档示例值 142932802604630016
	// 超出 float64 的精确整数范围（2^53），用 float64 会静默丢精度。
	CertID int64 `json:"certID"`
	// Cost 是订单金额，单位分。这是平台付给上游的钱。
	Cost int64 `json:"cost"`
	// OrderNo 是上游订单号，平台据此查询、对账、反查。
	OrderNo string `json:"orderNo"`
}

// ── 域名列表 ──────────────────────────────────────

// wireDomainsData 是域名列表的响应数据。
type wireDomainsData struct {
	DomainList []wireDomain `json:"domainList"`
}

// wireDomain 是上游返回的一条域名材料。
//
// 一组「相同根域名」的域名共用一个条目，因此 dnsNames 是数组；
// 每种验证方式的材料都在同一个条目里返回，不管当前用的是哪一种。
type wireDomain struct {
	DnsNames wireDnsNames `json:"dnsNames"`
	// DomainID 只有 digicert 产品线（geotrust / rapidssl / digicert /
	// securesite / thawte / securesitechina / geotrustchina）有，其余为 0。
	DomainID int64 `json:"domainID"`
	// Email 是邮件验证的收件地址。文档给的是单个值，
	// 这里仍按可能的多值解析，见 wireEmailAddresses。
	Email wireEmailAddresses `json:"email"`
	// Status 是域名验证状态码：2001 未验证、2002 已验证。
	Status string `json:"status"`
	// DcvMethod 是**响应侧**的验证方式常量名（HTTP_CSR_HASH 等），
	// 与请求侧的取值不是同一套，见本文件开头第 5 条。
	DcvMethod string `json:"dcvMethod"`
	// FileDcvPath 是文件验证路径，**含 {FQDN} 占位符**，需要调用方
	// 逐域名替换。文档明确：现在每个域名本身都要放文件，
	// 不再是「只在顶级域名放一次」。
	FileDcvPath string `json:"fileDcvPath"`
	// RecordType / HostRecord / HashValue 是 DNS 验证需要的三件套。
	RecordType string `json:"recordType"`
	HostRecord string `json:"hostRecord"`
	HashValue  string `json:"hashValue"`
	// UniqueValue 是 sectigo / positivessl 类证书重签后返回的唯一值。
	UniqueValue string `json:"uniqueValue"`
}

// wireDnsNames 解析上游的域名集合。
//
// **这个字段有两种形态**：常规域名返回数组
// （["1.test.com","2.test.com"]），而 IP 返回**裸字符串**
// （"140.12.56.8"，文档示例如此，且说明里写着「ip单独」）。
//
// 用 []string 直接解析的话，IP 订单会在解码这一步整体失败。而上游
// 返回的是 HTTP 200 + code 200，所以订单域看到的错误是「报文格式错误」，
// 排查方向会被引到「上游改了格式」上，而真实原因是「这一单是 IP 证书」。
type wireDnsNames []string

func (n *wireDnsNames) UnmarshalJSON(raw []byte) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		*n = nil
		return nil
	}
	if trimmed[0] == '[' {
		var list []string
		if err := json.Unmarshal(trimmed, &list); err != nil {
			return fmt.Errorf("解析 dnsNames 数组失败: %w", err)
		}
		*n = list
		return nil
	}
	// 裸字符串（IP）。空串按「没有域名」处理，而不是「有一个空域名」——
	// 后者会在订单里多出一条 domain 为空字符串的记录。
	var single string
	if err := json.Unmarshal(trimmed, &single); err != nil {
		return fmt.Errorf("解析 dnsNames 字符串失败: %w", err)
	}
	if strings.TrimSpace(single) == "" {
		*n = nil
		return nil
	}
	*n = []string{single}
	return nil
}

// wireEmailAddresses 解析邮件验证的收件地址。
//
// 文档给的 `email` 是单个地址的字符串。仍然接受逗号 / 分号分隔的多值：
// 单值里不会出现这两种分隔符，所以多拆一次是纯防御，
// 而漏拆的后果是把 "a@x.com,b@x.com" 当成一个非法邮箱展示给用户。
type wireEmailAddresses []string

func (a *wireEmailAddresses) UnmarshalJSON(raw []byte) error {
	var single string
	if err := json.Unmarshal(raw, &single); err != nil {
		// 上游若改成数组也能吃下
		var list []string
		if err2 := json.Unmarshal(raw, &list); err2 != nil {
			return fmt.Errorf("解析 email 失败: %w", err)
		}
		*a = list
		return nil
	}
	out := make([]string, 0, 1)
	for _, part := range strings.FieldsFunc(single, func(r rune) bool {
		return r == ',' || r == ';'
	}) {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	if len(out) == 0 {
		*a = nil
		return nil
	}
	*a = out
	return nil
}

// ── 订单状态 ──────────────────────────────────────

type wireOrderStatusData struct {
	Status wireStatusFields `json:"status"`
}

// wireStatusFields 是上游订单状态接口返回的七个字段。
//
// 平台侧只认 orderStatus 与 certStatus 两个（见 order.upstreamStateMap）；
// 其余四个是**上游内部阶段**，平台不参与判断，但要如实透传，
// 否则运营在排查时无法还原上游的真实情况。
type wireStatusFields struct {
	CertPrepareStatus  string `json:"certPrepareStatus"`
	CertStatus         string `json:"certStatus"`
	DcvStatus          string `json:"dcvStatus"`
	EvValidationStatus string `json:"evValidationStatus"`
	// IsReSignOrder 是「是否重签订单」（Y/N），不是重签状态。
	// 为 Y 时其余字段表示的都是重签后的订单状态。
	IsReSignOrder      string `json:"isReSignOrder"`
	OrderStatus        string `json:"orderStatus"`
	OvValidationStatus string `json:"ovValidationStatus"`
}

// ── 下载证书 ──────────────────────────────────────

type wireCertificateData struct {
	CertInfo wireCertificateInfo `json:"certInfo"`
}

// wireCertificateInfo 是证书信息。回调里的签发事件用的是同一套字段。
type wireCertificateInfo struct {
	SerialNumber string `json:"serialNumber"`
	// CertContent 是服务器证书 PEM（原文，未做 base64）。
	CertContent string `json:"certContent"`
	// MidCertContent 是中间证书链 PEM。
	MidCertContent string `json:"midCertContent"`
	// NotBefore / NotAfter 是**毫秒**时间戳。上游所有时间戳都是毫秒，
	// 当成秒会让证书有效期落在 1970 年附近。
	NotBefore int64 `json:"notBefore"`
	NotAfter  int64 `json:"notAfter"`

	CommonName  string   `json:"commonName"`
	DomainNames []string `json:"domainNames"`

	SHA1   string `json:"sha1"`
	SHA256 string `json:"sha256"`

	IssuerCommonName string `json:"issuerCommonName"`
	IssuerCountry    string `json:"issuerCountry"`
	IssuerOrg        string `json:"issuerOrg"`

	SignatureAlgo string `json:"signatureAlgo"`
	// Encryption 是 RSA / ECDSA。为 RSA 时 KeyLength 有值，
	// 为 ECDSA 时 KeyCurve 有值，两者成对出现。
	Encryption string `json:"encryption"`
	KeyLength  int    `json:"keyLength"`
	KeyCurve   string `json:"keyCurve"`
}

// ── 重签 ──────────────────────────────────────────

// wireReissueRequest 是重签请求体。
//
// csr 与 dcvMethod 都是 **required**：上游不接受「沿用原 CSR」这种省略，
// 平台必须把 CSR 原文重新提交一次。
type wireReissueRequest struct {
	CSR         string `json:"csr"`
	DcvMethod   string `json:"dcvMethod"`
	DomainNames string `json:"domainNames,omitempty"`
	OrderNo     string `json:"orderNo"`
}

type wireReissueData struct {
	CertID int64 `json:"certID"`
	// PriceDiff 是重签差价，单位分。可能为 0（同规格重签）。
	PriceDiff int64 `json:"priceDiff"`
}

// ── 重新生成 dcvToken ─────────────────────────────

// wireRegenerateTokenRequest 是重生成 dcvToken 的请求体。
//
// **它按单个域名操作**，而平台的用例是「整单重生成」。
// 适配器因此要逐个域名调用，见 HTTPClient.RegenerateDcvToken。
//
// 顺带记一笔：上游还有「更新域名验证方式」，路径与这里**完全相同**
// （/certificates/dcv），只有 HTTP 方法不同（PUT，这里是 POST）。
// 平台当前没有这个用例，所以不实现；请求体格式记在 docs/07 备查。
type wireRegenerateTokenRequest struct {
	OrderNo string `json:"orderNo"`
	Domain  string `json:"domain"`
}

type wireRegenerateTokenData struct {
	DcvToken string `json:"dcvToken"`
}

// ── 订单筛选（反查） ──────────────────────────────

type wireFindOrdersData struct {
	CurrentPage int                 `json:"currentPage"`
	ReturnCount int                 `json:"returnCount"`
	TotalOrders int                 `json:"totalOrders"`
	TotalPages  int                 `json:"totalPages"`
	Orders      []wireFilteredOrder `json:"orders"`
}

type wireFilteredOrder struct {
	OrderNo     string `json:"orderNo"`
	ProductNo   string `json:"productNo"`
	CommonName  string `json:"commonName"`
	OrderStatus string `json:"orderStatus"`
	OVStatus    string `json:"ovValidationStatus"`
	EVStatus    string `json:"evValidationStatus"`
	DcvStatus   string `json:"dcvStatus"`
	Prepare     string `json:"certPrepareStatus"`
	CertStatus  string `json:"certStatus"`
	// SubmitDateStamp / PayDateStamp 是毫秒时间戳。未支付订单的
	// PayDateStamp 为 0。
	SubmitDateStamp int64 `json:"submitDateStamp"`
	PayDateStamp    int64 `json:"payDateStamp"`
}

// ── 验证方式取值映射 ──────────────────────────────

// 请求侧的验证方式取值。**必须小写**：上游在「更新域名验证方式」
// 接口的说明里明确写了这一点，而 dcvMethod 的大小写错误会返回
// 6004「dcvMethod 不支持」——一个看起来像「产品不支持」的错误。
//
// 上游还有第五个取值 dns（响应侧叫 CNAME_CSR_HASH）。它没有被映射，
// 因为平台侧没有对应取值，见 toWireDcvMethod 的说明。
const (
	wireDcvFile     = "file"
	wireDcvEmail    = "email"
	wireDcvDNSTxt   = "dns_txt"
	wireDcvDNSCname = "dns_cname"
)

// toWireDcvMethod 把平台的验证方式翻译成上游的请求取值。
//
// 平台把文件验证分成 http_file 与 https_file（区别只是用户把验证文件
// 放在 http 还是 https 站点下），而上游只有一个 file，所以两个平台值
// 映射到同一个上游值。这个合并是不可逆的——反向翻译只能回到其中一个，
// 所以反向映射（响应侧常量名 → 平台取值）不在这里做，见 order 域的
// mapUpstreamDcvMethod。
//
// 上游的 dns（CNAME_CSR_HASH）平台侧**没有对应取值**：平台的 dns_txt 与
// dns_cname 分别对应上游的 DNS_TXT 与 DNS_CNAME，而后者只对 certum 品牌
// 开放。非 certum 品牌要提交 dns 时，当前产品配置无法表达——
// 这是已知缺口，见 docs/07。
func toWireDcvMethod(method string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(method)) {
	case "http_file", "https_file":
		return wireDcvFile, true
	case "dns_txt":
		return wireDcvDNSTxt, true
	case "dns_cname":
		return wireDcvDNSCname, true
	case "email":
		return wireDcvEmail, true
	default:
		return "", false
	}
}

// ── 小工具 ────────────────────────────────────────

// millisToTime 把上游的毫秒时间戳转成时间。0 返回零值。
//
// 上游用 0 表示「没有这个时间」（例如未支付订单的 payDateStamp），
// 转成 time.Time 就是零值，调用方用 IsZero 判断即可。
func millisToTime(ms int64) time.Time {
	if ms <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms).UTC()
}

// int64ToString 把上游的数字 ID 转成平台侧的字符串形式。
//
// 平台侧一律用字符串存上游 ID（见 Client 接口的 CertID），
// 因为它在两个系统里都可能超出 JavaScript 的安全整数范围。
func int64ToString(v int64) string {
	if v == 0 {
		return ""
	}
	return strconv.FormatInt(v, 10)
}

// joinDomainNames 拼出上游要的 domainNames 参数。
//
// 上游的约定是：**不含主域名**，其余用逗号连接，没有子域名时整个字段不传。
// 平台的约定是：Domains[0] 是主域名。所以这里要丢掉第一个。
//
// 丢掉主域名而不是全部传：主域名由上游从 CSR 里取，重复传会导致
// 上游报 6002（参数类型或参数名有误）——它把主域名当成重复的 SAN。
func joinDomainNames(domains []string) string {
	if len(domains) <= 1 {
		return ""
	}
	rest := make([]string, 0, len(domains)-1)
	for _, d := range domains[1:] {
		if trimmed := strings.TrimSpace(d); trimmed != "" {
			rest = append(rest, trimmed)
		}
	}
	return strings.Join(rest, ",")
}
