// Package order 是证书订单域。
//
// 目录结构遵循垂直切片约定：model / repository / service / handler 同包，
// DCV 材料处理在 dcv.go。
//
// ── 本域最需要想清楚的三件事 ────────────────────────
//
//  1. **用户扣多少钱。** 按零售价扣，不是按上游返回的 cost。
//     上游 cost 是平台付给 FoxSSL 的成本，用它决定用户扣款等于把采购成本
//     透传给用户；而且 cost 高于零售价时冻结额不够实扣，订单会卡在
//     「上游已下单、用户没扣够钱」的中间态。按零售价扣款则
//     冻结额恒等于结算额，这个中间态在结构上不存在。
//     cost 仍然记进订单供运营对账，不参与任何用户余额计算。
//
//  2. **上游调用不能进事务。** 网络 I/O 一次慢响应会长时间占住事务与连接。
//     因此流程被切成「事务 → 网络 → 事务」，中间态的语义必须明确，
//     否则崩溃后会不知道上游到底建没建单。
//
//  3. **重试必须安全。** 平台在上游超时时拿不到「到底成没成功」的答案，
//     只能重试。重试安全的前提是向上游传本地 order_no 做幂等键，
//     由上游去重——这一条在 internal/upstream/foxssl 里实现并有测试兜住。
package order

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/cangerx/c-ssl/server/internal/domain/errs"
	"github.com/cangerx/c-ssl/server/internal/domain/money"
	"github.com/cangerx/c-ssl/server/internal/domain/orderstate"
	"github.com/cangerx/c-ssl/server/internal/product/rules"
)

// ── 实体 ──────────────────────────────────────────

// Order 是证书订单。
type Order struct {
	ID          int64
	OrderNo     string
	UserID      int64
	ProductID   int64
	ProductName string
	Brand       string
	// ValidationType / ProductName / Brand 是下单时的产品快照。
	// 产品会改价、会下架、能力字段也会调整，而订单是历史事实。
	ValidationType rules.ValidationType
	// UpstreamProductID 是下单时发给上游的产品编号快照。
	//
	// 补偿重试必须用它而不是重新读产品：产品配置后来改了的话，
	// 用新编号重试会得到一个与原始订单不同的结果。
	UpstreamProductID int
	Years             int
	KeyAlgorithm      rules.KeyAlgorithm
	// Amount 是订单金额（分），等于冻结并实扣的零售价。免费证书为 0。
	Amount money.Amount
	// CostPrice 是上游成本价（分），仅用于运营对账与毛利分析。
	// **禁止出现在任何面向用户的 DTO 里。**
	CostPrice money.Amount
	Status    orderstate.State

	UpstreamOrderNo string
	CertID          string
	// 上游的四个原始状态，原样保存。它们只进运营后台——
	// 取值含义随上游版本变化，直接暴露给用户会引起误判。
	UpstreamOrderStatus   string
	UpstreamCertStatus    string
	UpstreamPrepareStatus string
	UpstreamReissueStatus string

	CSR           string
	FailureReason string
	CancelReason  string

	SubmittedAt *time.Time
	IssuedAt    *time.Time
	ExpiresAt   *time.Time
	CreatedAt   time.Time
	UpdatedAt   time.Time

	// 关联数据。列表查询不加载这些字段。
	Domains      []Domain
	Contact      *Contact
	Organization *Organization
}

// Domains 的域名列表，第一个是主域名。
func (o Order) DomainNames() []string {
	out := make([]string, 0, len(o.Domains))
	for _, d := range o.Domains {
		out = append(out, d.Domain)
	}
	return out
}

// PrimaryDomain 返回主域名，没有域名时返回空串。
func (o Order) PrimaryDomain() string {
	for _, d := range o.Domains {
		if d.Primary {
			return d.Domain
		}
	}
	if len(o.Domains) > 0 {
		return o.Domains[0].Domain
	}
	return ""
}

// IsFree 判断是否免费订单。免费订单不产生任何资金动作。
func (o Order) IsFree() bool { return o.Amount.IsZero() }

// DomainStatus 是单个域名的验证状态。
type DomainStatus string

const (
	// DomainPending 尚未提交验证。
	DomainPending DomainStatus = "pending"
	// DomainVerifying 已提交，等待上游校验。
	DomainVerifying DomainStatus = "verifying"
	// DomainVerified 已验证通过。
	DomainVerified DomainStatus = "verified"
	// DomainFailed 验证失败。
	DomainFailed DomainStatus = "failed"
	// DomainExpired 验证已过期，需要重新验证。
	DomainExpired DomainStatus = "expired"
)

// Valid 判断是否为已定义的状态。
func (s DomainStatus) Valid() bool {
	switch s {
	case DomainPending, DomainVerifying, DomainVerified, DomainFailed, DomainExpired:
		return true
	}
	return false
}

// Domain 是订单里的一个域名及其验证材料。
type Domain struct {
	ID         int64
	OrderNo    string
	Domain     string
	Wildcard   bool
	Primary    bool
	Status     DomainStatus
	Method     rules.DcvMethod
	Record     DNSRecord
	File       FileChallenge
	Emails     []string
	VerifiedAt *time.Time
	ExpiresAt  *time.Time
	CreatedAt  time.Time
	UpdatedAt  time.Time

	// AllowedMethods 是这张订单当前允许提交的验证方式，由 Service.ListDomains
	// 计算后填入。**不落库**：它依赖产品配置，而产品配置随时可能被运营改动，
	// 存下来就会与产品页不一致。
	//
	// 它必须与「服务端接受什么」完全一致，因此不能在 handler 里另行推导——
	// 界面上列出的方式用户选了却被拒，是这个域最让人费解的故障。
	AllowedMethods []rules.DcvMethod
}

// DNSRecord 是 DNS 验证需要的记录。
type DNSRecord struct {
	Type  string // TXT / CNAME
	Name  string
	Value string
}

// FileChallenge 是文件验证需要的材料。
//
// Path 必须是**替换过 {FQDN} 之后**的最终路径。上游返回的是模板，
// 模板不允许流到这一层之外。
type FileChallenge struct {
	Path    string
	Content string
}

// Contact 是证书联系人。
type Contact struct {
	Name  string
	Email string
	Phone string
	Title string
}

// Organization 是企业主体信息，只有 OV / EV 有。
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

// Certificate 是已签发的证书。
//
// **平台不保存私钥。** 下单时提交的 CSR 对应的私钥始终留在生成它的地方。
type Certificate struct {
	ID           int64
	OrderNo      string
	CertID       string
	Status       string
	CommonName   string
	Domains      []string
	KeyAlgorithm rules.KeyAlgorithm
	SerialNumber string
	Certificate  string
	CABundle     string
	IssuedAt     *time.Time
	ExpiresAt    *time.Time
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// ── 入参与出参 ────────────────────────────────────

// CreateInput 是下单入参。
type CreateInput struct {
	UserID       int64
	ProductID    int64
	Years        int
	KeyAlgorithm rules.KeyAlgorithm
	Domains      []string
	CSR          string
	Contact      Contact
	Organization *Organization
}

// Filter 是订单列表查询条件。
type Filter struct {
	// Cursor 为上一页最后一条的 ID，0 表示从头开始。
	// 用 ID 而非 offset：订单持续新增，offset 分页会漏记录或重复。
	Cursor int64
	Limit  int
	// Status 为空表示不筛选。
	Status orderstate.State
}

// Page 是一页订单。
type Page struct {
	Items []Order
	// NextCursor 为 0 表示没有更多数据。
	NextCursor int64
	// DomainNames 是每个订单的域名名列表，键为订单号。
	//
	// 刻意不塞进 Order.Domains：那里装的是带验证材料的完整域名行，
	// 而列表查询只取了域名本身。混进去会让「Order.Domains 已加载」
	// 这个前提变得不可靠——后续有人在列表项上读 d.File.Path 会静默拿到空串，
	// 而不是像 nil 那样至少暴露「没加载」。
	DomainNames map[string][]string
}

// WebhookEvent 是一条上游事件记录。
type WebhookEvent struct {
	ID              int64
	Provider        string
	EventKey        string
	EventType       string
	UpstreamOrderNo string
	UpstreamStatus  string
	PayloadHash     string
	Payload         string
	OccurredAt      *time.Time
	ReceivedAt      time.Time
	ProcessStatus   string
	ProcessError    string
	Attempts        int
	ProcessedAt     *time.Time
}

// 事件处理状态。
const (
	EventPending = "pending"
	EventDone    = "done"
	EventFailed  = "failed"
)

// ── 常量 ──────────────────────────────────────────

const (
	// DefaultLimit 是订单分页的默认条数。
	DefaultLimit = 20
	// MaxLimit 是订单分页的最大条数，防止客户端拉全表。
	MaxLimit = 100

	// MaxDomains 是单个订单允许的域名数上限。
	MaxDomains = 100
	// CSRMaxLen 是 CSR 的长度上限。真实 CSR 约 1-2KB，
	// 留足余量同时挡住把整个文件塞进来的情况。
	CSRMaxLen = 8192
	// ReasonMaxLen 是取消/重签原因的字符数上限。
	ReasonMaxLen = 255
	// MaxRegenerateToken 是同一订单允许重新生成 token 的次数。
	// 它是为「token 泄漏」准备的，正常流程不该用到；
	// 不限次数的话，反复重生成会让已经配好的记录永远失效。
	MaxRegenerateToken = 3

	// providerFoxSSL 是上游标识，与 foxssl.NameMock 一致。
	// 这里写字面量而不是导入 foxssl 包会形成环：
	// foxssl 不依赖 order，但让 order 去依赖上游包会把适配层
	// 变成业务的一部分，测试时也无法替换。由配置层保证一致。
	providerFoxSSL = "foxssl"

	// bizTypeOrder 是账本流水的业务类型前缀。
	bizTypeOrder = "certificate_order"

	// entryNoMaxLen 等字段长度与数据库列宽对齐，见 000006 迁移。
	domainMaxLen  = 253
	payloadMaxLen = 8 * 1024
)

// ── 错误 ──────────────────────────────────────────

// 领域哨兵错误。服务层据此映射业务错误码。
var (
	// ErrOrderNotFound 订单不存在，或不属于当前用户。
	//
	// 两种情况共用同一个错误是刻意的：返回 403 等于确认「这张单存在」，
	// 可以被用来枚举别人的订单号。
	ErrOrderNotFound = errors.New("订单不存在")

	// ErrProductNotFound 产品不存在或已下架。
	ErrProductNotFound = errors.New("产品不存在或已下架")

	// ErrOrderNotCancellable 订单当前状态不允许取消。
	ErrOrderNotCancellable = errors.New("订单当前状态不允许取消")
	// ErrCancelNotSupported 产品不支持取消。
	ErrCancelNotSupported = errors.New("该产品不支持取消")

	// ErrStateUnreachable 目标状态从当前状态不可达。
	//
	// 既覆盖「非法迁移」，也覆盖「状态回退」——上游不保证投递顺序，
	// 一条迟到的旧事件会得到这个错误，调用方据此跳过而不是把状态改回去。
	ErrStateUnreachable = errors.New("订单状态不可达")

	// ErrOrderNotReissuable 订单当前状态不允许重签。
	ErrOrderNotReissuable = errors.New("订单当前状态不允许重签")
	// ErrReissueNotSupported 产品不支持重签。
	ErrReissueNotSupported = errors.New("该产品不支持重签")

	// ErrCertificateNotReady 证书尚未签发，不能下载。
	ErrCertificateNotReady = errors.New("证书尚未签发")

	// ErrDomainNotInOrder 域名不属于该订单。
	ErrDomainNotInOrder = errors.New("域名不属于该订单")
	// ErrMethodNotAvailable 所选验证方式对这些域名不可用。
	ErrMethodNotAvailable = errors.New("所选验证方式不可用")
	// ErrOrderNotVerifiable 订单当前状态不允许操作域名验证。
	ErrOrderNotVerifiable = errors.New("订单当前状态不允许操作域名验证")

	// ErrRegenerateLimitExceeded 重新生成 token 的次数已达上限。
	ErrRegenerateLimitExceeded = errors.New("重新生成验证 token 的次数已达上限")

	// ErrUpstreamUnknown 上游调用结果未知（超时、连接中断等）。
	//
	// **与「上游明确拒绝」必须分开。** 明确拒绝时可以放心解冻并置失败；
	// 结果未知时上游可能已经建了单，此时解冻等于平台白付一张证书的钱。
	// 这种情况下订单停在 submitting，由补偿流程按同一个商户订单号重试
	// ——上游幂等保证重试不会产生第二张证书。
	ErrUpstreamUnknown = errors.New("上游调用结果未知")

	// ErrEventUnknownOrder 上游事件指向的订单在平台不存在。
	//
	// 事件仍然要落库：丢了就再也查不到上游到底推过什么。
	ErrEventUnknownOrder = errors.New("事件指向的订单在平台不存在")
)

// ── 校验 ──────────────────────────────────────────

// Validate 校验下单入参。产品规则相关的校验在服务层做，
// 因为那需要先读出产品。
func (in *CreateInput) Validate() *errs.Error {
	if in.UserID <= 0 {
		return errs.New(errs.CodeUnauthorized)
	}
	if in.ProductID <= 0 {
		return errs.New(errs.CodeInvalidParam).WithField("productId", "产品 ID 无效")
	}
	if in.Years <= 0 {
		return errs.New(errs.CodeInvalidParam).WithField("years", "年限必须大于 0")
	}
	if !in.KeyAlgorithm.Valid() {
		return errs.New(errs.CodeInvalidParam).
			WithField("keyAlgorithm", fmt.Sprintf("未知的密钥算法: %q", in.KeyAlgorithm))
	}
	if len(in.Domains) == 0 {
		return errs.New(errs.CodeInvalidParam).WithField("domains", "至少需要一个域名")
	}
	if len(in.Domains) > MaxDomains {
		return errs.New(errs.CodeInvalidParam).
			WithField("domains", fmt.Sprintf("域名数量不能超过 %d 个", MaxDomains))
	}
	if len([]rune(in.CSR)) > CSRMaxLen {
		return errs.New(errs.CodeInvalidParam).
			WithField("csr", fmt.Sprintf("CSR 长度不能超过 %d 个字符", CSRMaxLen))
	}

	seen := make(map[string]struct{}, len(in.Domains))
	for i, raw := range in.Domains {
		d := strings.TrimSpace(strings.ToLower(raw))
		if err := validateDomain(d); err != nil {
			return errs.New(errs.CodeInvalidParam).
				WithField("domains", fmt.Sprintf("第 %d 个域名无效：%s", i+1, err))
		}
		if _, dup := seen[d]; dup {
			return errs.New(errs.CodeInvalidParam).
				WithField("domains", fmt.Sprintf("域名 %s 重复提交", d))
		}
		seen[d] = struct{}{}
	}

	if err := in.Contact.Validate(); err != nil {
		return err
	}
	if in.Organization != nil {
		if err := in.Organization.Validate(); err != nil {
			return err
		}
	}
	return nil
}

// Normalize 清理入参：域名统一小写去空白、CSR 去首尾空白。
func (in *CreateInput) Normalize() {
	for i, d := range in.Domains {
		in.Domains[i] = strings.TrimSpace(strings.ToLower(d))
	}
	in.CSR = strings.TrimSpace(in.CSR)
	in.Contact.Name = strings.TrimSpace(in.Contact.Name)
	in.Contact.Email = strings.TrimSpace(strings.ToLower(in.Contact.Email))
	in.Contact.Phone = strings.TrimSpace(in.Contact.Phone)
	in.Contact.Title = strings.TrimSpace(in.Contact.Title)
	if in.Organization != nil {
		org := in.Organization
		org.Name = strings.TrimSpace(org.Name)
		org.RegistrationNo = strings.TrimSpace(org.RegistrationNo)
		org.Country = strings.ToUpper(strings.TrimSpace(org.Country))
		org.Province = strings.TrimSpace(org.Province)
		org.City = strings.TrimSpace(org.City)
		org.Address = strings.TrimSpace(org.Address)
		org.PostalCode = strings.TrimSpace(org.PostalCode)
		org.Phone = strings.TrimSpace(org.Phone)
	}
}

// Validate 校验联系人。
func (c Contact) Validate() *errs.Error {
	if strings.TrimSpace(c.Name) == "" {
		return errs.New(errs.CodeInvalidParam).WithField("contact.name", "联系人姓名不能为空")
	}
	if len([]rune(c.Name)) > 64 {
		return errs.New(errs.CodeInvalidParam).WithField("contact.name", "联系人姓名不能超过 64 个字符")
	}
	if !looksLikeEmail(c.Email) {
		return errs.New(errs.CodeInvalidParam).WithField("contact.email", "联系人邮箱格式不正确")
	}
	if len(c.Email) > 128 {
		return errs.New(errs.CodeInvalidParam).WithField("contact.email", "联系人邮箱不能超过 128 个字符")
	}
	if strings.TrimSpace(c.Phone) == "" {
		return errs.New(errs.CodeInvalidParam).WithField("contact.phone", "联系电话不能为空")
	}
	if len(c.Phone) > 32 {
		return errs.New(errs.CodeInvalidParam).WithField("contact.phone", "联系电话不能超过 32 个字符")
	}
	if len([]rune(c.Title)) > 64 {
		return errs.New(errs.CodeInvalidParam).WithField("contact.title", "职位不能超过 64 个字符")
	}
	return nil
}

// Validate 校验企业主体信息。
//
// 字段要求偏严是有意的：这些信息会由 CA 人工核验，填错只会导致
// 审核驳回，让用户白等几天。在提交前拦住比事后解释便宜得多。
func (o Organization) Validate() *errs.Error {
	if strings.TrimSpace(o.Name) == "" {
		return errs.New(errs.CodeInvalidParam).WithField("organization.name", "企业名称不能为空")
	}
	if len([]rune(o.Name)) > 128 {
		return errs.New(errs.CodeInvalidParam).WithField("organization.name", "企业名称不能超过 128 个字符")
	}
	if strings.TrimSpace(o.RegistrationNo) == "" {
		return errs.New(errs.CodeInvalidParam).
			WithField("organization.registrationNo", "统一社会信用代码不能为空")
	}
	if len(o.RegistrationNo) > 64 {
		return errs.New(errs.CodeInvalidParam).
			WithField("organization.registrationNo", "注册号不能超过 64 个字符")
	}
	if len(o.Country) != 2 {
		return errs.New(errs.CodeInvalidParam).
			WithField("organization.country", "国家代码必须是 ISO 3166-1 的两位字母")
	}
	for _, r := range o.Country {
		if r < 'A' || r > 'Z' {
			return errs.New(errs.CodeInvalidParam).
				WithField("organization.country", "国家代码必须是两位大写字母")
		}
	}
	if strings.TrimSpace(o.City) == "" {
		return errs.New(errs.CodeInvalidParam).WithField("organization.city", "城市不能为空")
	}
	if strings.TrimSpace(o.Address) == "" {
		return errs.New(errs.CodeInvalidParam).WithField("organization.address", "注册地址不能为空")
	}
	if len([]rune(o.Address)) > 255 {
		return errs.New(errs.CodeInvalidParam).
			WithField("organization.address", "注册地址不能超过 255 个字符")
	}
	if strings.TrimSpace(o.PostalCode) == "" {
		return errs.New(errs.CodeInvalidParam).WithField("organization.postalCode", "邮编不能为空")
	}
	if strings.TrimSpace(o.Phone) == "" {
		return errs.New(errs.CodeInvalidParam).WithField("organization.phone", "企业联系电话不能为空")
	}
	return nil
}

// Validate 校验分页参数，并把越界的 limit 截断到合法范围。
func (f *Filter) Validate() *errs.Error {
	if f.Cursor < 0 {
		return errs.New(errs.CodeInvalidParam).WithField("cursor", "游标不能为负数")
	}
	if f.Limit <= 0 {
		f.Limit = DefaultLimit
	}
	if f.Limit > MaxLimit {
		// 截断而不是报错：与钱包账本、充值订单接口保持一致，
		// 前端传大值时拿到 100 条比拿到 400 更有用。
		f.Limit = MaxLimit
	}
	if f.Status != "" && !f.Status.Valid() {
		return errs.New(errs.CodeInvalidParam).
			WithField("status", fmt.Sprintf("未知的订单状态: %q", f.Status))
	}
	return nil
}

// ── 内部校验辅助 ──────────────────────────────────

// validateDomain 校验单个域名的形态。
//
// 这里只做「明显不是域名」的拦截，不做严格的 DNS 语法校验：
// 过严的正则会拒掉合法域名（国际化域名、下划线等），
// 而真正的把关在上游——上游会告诉你它认不认这个域名。
func validateDomain(d string) error {
	if d == "" {
		return errors.New("域名不能为空")
	}
	if len(d) > domainMaxLen {
		return fmt.Errorf("域名长度超过 %d", domainMaxLen)
	}
	body := strings.TrimPrefix(d, "*.")
	if body == "" {
		return errors.New("通配符后缺少域名")
	}
	if strings.Contains(body, "*") {
		return errors.New("通配符只能出现在最前面")
	}
	if !strings.Contains(body, ".") {
		return errors.New("域名必须包含至少一个点")
	}
	if strings.HasPrefix(body, ".") || strings.HasSuffix(body, ".") {
		return errors.New("域名不能以点开头或结尾")
	}
	if strings.Contains(body, "..") {
		return errors.New("域名不能包含连续的点")
	}
	for _, r := range body {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9':
		case r == '-' || r == '.' || r == '_':
		default:
			return fmt.Errorf("域名包含非法字符 %q", r)
		}
	}
	return nil
}

// looksLikeEmail 做一个宽松的邮箱格式检查。
//
// 不用严格的正则：RFC 5322 允许的形态多到正则写不对，
// 而真正重要的是「CA 能通过它联系到人」，那只有发信才知道。
func looksLikeEmail(s string) bool {
	at := strings.IndexByte(s, '@')
	if at <= 0 || at != strings.LastIndexByte(s, '@') {
		return false
	}
	local, domainPart := s[:at], s[at+1:]
	if local == "" || domainPart == "" {
		return false
	}
	if !strings.Contains(domainPart, ".") {
		return false
	}
	if strings.HasPrefix(domainPart, ".") || strings.HasSuffix(domainPart, ".") {
		return false
	}
	return !strings.ContainsAny(s, " \t\r\n")
}
