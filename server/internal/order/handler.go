package order

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/cangerx/c-ssl/server/internal/domain/errs"
	"github.com/cangerx/c-ssl/server/internal/domain/orderstate"
	"github.com/cangerx/c-ssl/server/internal/product/rules"
	"github.com/cangerx/c-ssl/server/internal/server/httpx"
	"github.com/cangerx/c-ssl/server/internal/upstream/foxssl"
)

// signatureHeader 是上游回调的签名请求头。
//
// 与 recharge 域用同一个头名、同一套算法：上游只有一个，
// 两个域各立一套会让「同一份报文在两个接口上验签结果不同」成为可能。
const signatureHeader = "X-Webhook-Signature"

// maxWebhookBody 是回调报文的读取上限。
//
// 回调接口不套鉴权，是任何人都能打的公开端点。没有上限时，
// 一个声明了超大 Content-Length 的请求就能把内存吃光。
const maxWebhookBody = 256 * 1024

// MockUpstream 是模拟上游的开发控制接口。
//
// 刻意用独立的窄接口，而不是把方法加到 foxssl.Client 上：真实 CA 没有
// 「立刻把订单标成已签发」这种能力，写进 Client 会逼着真实适配器实现
// 一个它根本做不到的方法，只能留个返回错误的空实现——那种实现比没有更糟，
// 因为它让「接口被完整实现」这句话失去意义。
//
// 非空即表示启用了模拟上游，开发辅助路由据此注册。判断环境的事交给
// 配置层（生产环境不允许 FOXSSL_PROVIDER=mock，见 config.validate）。
type MockUpstream interface {
	IssueCertificate(upstreamOrderNo string) error
}

// Handler 处理证书订单、域名验证、证书下载与上游回调的 HTTP 请求。
type Handler struct {
	svc *Service
	// auth 由 router 注入，避免本包反向依赖 middleware 的具体实现。
	auth       func(http.Handler) http.Handler
	trustProxy bool
	// mock 非空时注册模拟上游的开发辅助接口。
	mock MockUpstream
}

// NewHandler 构造处理器。
//
// mock 为 nil 表示使用真实上游，此时开发辅助接口不注册。
func NewHandler(
	svc *Service, auth func(http.Handler) http.Handler, trustProxy bool,
	mock MockUpstream,
) *Handler {
	return &Handler{svc: svc, auth: auth, trustProxy: trustProxy, mock: mock}
}

// Routes 注册订单域路由。挂载点已带 /api/v1 前缀。
func (h *Handler) Routes(r chi.Router) {
	// 订单与证书是用户私有数据，整组套上鉴权中间件。
	r.Group(func(r chi.Router) {
		r.Use(h.auth)

		r.Post("/orders", h.create)
		r.Get("/orders", h.list)
		r.Get("/orders/{orderNo}", h.get)
		r.Post("/orders/{orderNo}/cancel", h.cancel)
		r.Post("/orders/{orderNo}/reissue", h.reissue)

		r.Get("/orders/{orderNo}/domains", h.listDomains)
		r.Post("/orders/{orderNo}/domains/verify", h.verifyDomains)
		r.Post("/orders/{orderNo}/domains/resend-email", h.resendEmail)
		r.Post("/orders/{orderNo}/domains/regenerate-token", h.regenerateToken)

		r.Get("/orders/{orderNo}/certificate", h.certificate)

		// 开发辅助：把模拟上游的订单推进到已签发。
		//
		// 只有装了模拟上游时才注册，与支付域的 /payments/mock/notify 同一套判断。
		// 它同样套鉴权、同样只允许操作自己的订单——一个能推进任意订单的
		// 开发接口，在开发环境里就是「免费签发任意证书」的入口。
		if h.mock != nil {
			r.Post("/orders/{orderNo}/mock/issue", h.mockIssue)
		}
	})

	// 上游回调不套鉴权：调用方是 FoxSSL 服务器，不是登录用户。
	// 唯一的身份凭证是签名，验签在 Service 里、读取任何数据之前完成。
	r.Post("/webhooks/foxssl", h.webhook)
}

// ── DTO ──────────────────────────────────────────
//
// 字段与 openapi/components/schemas/{order,dcv,certificate}.yaml 严格对应。
// 金额一律用「分」的整数。
//
// 两处刻意不下发：
//
//   - CostPrice（上游成本价）。那是平台付给上游的钱，属于采购成本，
//     透给用户等于把定价体系的底牌摊开。
//   - 上游的四个原始状态（订单/证书/准备/重签状态）。取值含义随上游
//     版本变化，直接暴露会引起误判；它们只进运营后台。

type contactDTO struct {
	Name  string `json:"name"`
	Email string `json:"email"`
	Phone string `json:"phone"`
	Title string `json:"title"`
}

type organizationDTO struct {
	Name           string `json:"name"`
	RegistrationNo string `json:"registrationNo"`
	Country        string `json:"country"`
	Province       string `json:"province"`
	City           string `json:"city"`
	Address        string `json:"address"`
	PostalCode     string `json:"postalCode"`
	Phone          string `json:"phone"`
}

type orderSummaryDTO struct {
	OrderNo        string   `json:"orderNo"`
	ProductID      int64    `json:"productId"`
	ProductName    string   `json:"productName"`
	Brand          string   `json:"brand"`
	ValidationType string   `json:"validationType"`
	Years          int      `json:"years"`
	KeyAlgorithm   string   `json:"keyAlgorithm"`
	Domains        []string `json:"domains"`
	Amount         int64    `json:"amount"`
	Status         string   `json:"status"`
	CertID         *string  `json:"certId"`
	FailureReason  *string  `json:"failureReason"`
	CreatedAt      string   `json:"createdAt"`
	UpdatedAt      string   `json:"updatedAt"`
}

// orderDetailDTO 在列表项的基础上补充联系人、企业信息与上游编号。
//
// 内嵌 orderSummaryDTO 而不是复制字段：两处的字段集必须保持一致，
// 复制一份意味着以后改列表字段时很容易漏掉详情，而契约里两者是 allOf 关系。
type orderDetailDTO struct {
	orderSummaryDTO
	UpstreamOrderNo *string          `json:"upstreamOrderNo"`
	Contact         contactDTO       `json:"contact"`
	Organization    *organizationDTO `json:"organization"`
	IssuedAt        *string          `json:"issuedAt"`
	ExpiresAt       *string          `json:"expiresAt"`
}

type orderListDTO struct {
	Items      []orderSummaryDTO `json:"items"`
	NextCursor *int64            `json:"nextCursor"`
}

type domainDTO struct {
	Domain           string   `json:"domain"`
	Status           string   `json:"status"`
	Method           *string  `json:"method"`
	AvailableMethods []string `json:"availableMethods"`
	DNSRecordType    *string  `json:"dnsRecordType"`
	DNSRecordName    *string  `json:"dnsRecordName"`
	DNSRecordValue   *string  `json:"dnsRecordValue"`
	FilePath         *string  `json:"filePath"`
	FileContent      *string  `json:"fileContent"`
	EmailAddresses   []string `json:"emailAddresses"`
	VerifiedAt       *string  `json:"verifiedAt"`
	ExpiresAt        *string  `json:"expiresAt"`
}

type domainListDTO struct {
	Items []domainDTO `json:"items"`
}

type certificateDTO struct {
	OrderNo      string   `json:"orderNo"`
	CertID       string   `json:"certId"`
	Status       string   `json:"status"`
	CommonName   string   `json:"commonName"`
	Domains      []string `json:"domains"`
	KeyAlgorithm string   `json:"keyAlgorithm"`
	SerialNumber *string  `json:"serialNumber"`
	IssuedAt     *string  `json:"issuedAt"`
	ExpiresAt    *string  `json:"expiresAt"`
	Certificate  string   `json:"certificate"`
	CABundle     *string  `json:"caBundle"`
}

// mockIssueDTO 是推进模拟上游之后的应答。
//
// 只回上游订单号。调用方（冒烟脚本）需要它来构造回调报文，
// 而回调必须由调用方自己签名投递——本地状态只能由回调驱动。
type mockIssueDTO struct {
	UpstreamOrderNo string `json:"upstreamOrderNo"`
}

// ── 请求体 ────────────────────────────────────────

type createOrderRequest struct {
	ProductID    int64            `json:"productId"`
	Years        int              `json:"years"`
	KeyAlgorithm string           `json:"keyAlgorithm"`
	Domains      []string         `json:"domains"`
	CSR          string           `json:"csr"`
	Contact      contactDTO       `json:"contact"`
	Organization *organizationDTO `json:"organization"`
}

type verifyDomainsRequest struct {
	Domains []string `json:"domains"`
	Method  string   `json:"method"`
}

type resendEmailRequest struct {
	Domains []string `json:"domains"`
}

// reasonRequest 是取消与重签共用的请求体。契约里两者都是 required: false。
type reasonRequest struct {
	Reason string `json:"reason"`
}

// ── 转换 ──────────────────────────────────────────

func toSummaryDTO(o Order, domains []string) orderSummaryDTO {
	if domains == nil {
		// 契约里 domains 是必填数组。nil 会被编码成 null，
		// 而前端拿到 null 再 .map() 就是一个白屏。
		domains = []string{}
	}
	return orderSummaryDTO{
		OrderNo:        o.OrderNo,
		ProductID:      o.ProductID,
		ProductName:    o.ProductName,
		Brand:          o.Brand,
		ValidationType: string(o.ValidationType),
		Years:          o.Years,
		KeyAlgorithm:   string(o.KeyAlgorithm),
		Domains:        domains,
		Amount:         o.Amount.Cents(),
		Status:         string(o.Status),
		CertID:         emptyToNil(o.CertID),
		FailureReason:  emptyToNil(o.FailureReason),
		CreatedAt:      o.CreatedAt.Format(time.RFC3339),
		UpdatedAt:      o.UpdatedAt.Format(time.RFC3339),
	}
}

func toDetailDTO(o Order) orderDetailDTO {
	dto := orderDetailDTO{
		orderSummaryDTO: toSummaryDTO(o, o.DomainNames()),
		UpstreamOrderNo: emptyToNil(o.UpstreamOrderNo),
		IssuedAt:        formatTimePtr(o.IssuedAt),
		ExpiresAt:       formatTimePtr(o.ExpiresAt),
	}
	if o.Contact != nil {
		dto.Contact = contactDTO{
			Name:  o.Contact.Name,
			Email: o.Contact.Email,
			Phone: o.Contact.Phone,
			Title: o.Contact.Title,
		}
	}
	if o.Organization != nil {
		dto.Organization = &organizationDTO{
			Name:           o.Organization.Name,
			RegistrationNo: o.Organization.RegistrationNo,
			Country:        o.Organization.Country,
			Province:       o.Organization.Province,
			City:           o.Organization.City,
			Address:        o.Organization.Address,
			PostalCode:     o.Organization.PostalCode,
			Phone:          o.Organization.Phone,
		}
	}
	return dto
}

func toDomainDTO(d Domain) domainDTO {
	// 用 Service 填好的 AllowedMethods，**不要**在这里用 AvailableMethods 重新推导。
	//
	// 这个字段必须与服务端接受什么完全一致，而「接受什么」由两个判据共同决定：
	// 材料侧逐域名推导 + 产品侧按整张证书声明（见 Service.withAllowedMethods）。
	// 在这里只取材料侧，界面就会列出产品并不支持的方式——用户选了、被 400 拒掉，
	// 却看不出是自己哪里配错了。
	methods := d.AllowedMethods
	out := domainDTO{
		Domain:           d.Domain,
		Status:           string(d.Status),
		Method:           emptyToNil(string(d.Method)),
		AvailableMethods: make([]string, 0, len(methods)),
		DNSRecordType:    emptyToNil(d.Record.Type),
		DNSRecordName:    emptyToNil(d.Record.Name),
		DNSRecordValue:   emptyToNil(d.Record.Value),
		FilePath:         emptyToNil(d.File.Path),
		FileContent:      emptyToNil(d.File.Content),
		VerifiedAt:       formatTimePtr(d.VerifiedAt),
		ExpiresAt:        formatTimePtr(d.ExpiresAt),
	}
	for _, m := range methods {
		out.AvailableMethods = append(out.AvailableMethods, string(m))
	}
	// 空数组与 null 对前端是两个分支，而契约把它声明成 nullable 数组。
	// 这里统一给 null：它表达的是「这个域名没有邮件验证方式」，
	// 而 [] 会被读成「有邮件验证但一个地址都没有」。
	if len(d.Emails) > 0 {
		out.EmailAddresses = d.Emails
	}
	return out
}

func toDomainListDTO(domains []Domain) domainListDTO {
	dto := domainListDTO{Items: make([]domainDTO, 0, len(domains))}
	for _, d := range domains {
		dto.Items = append(dto.Items, toDomainDTO(d))
	}
	return dto
}

func toCertificateDTO(c *Certificate) certificateDTO {
	domains := c.Domains
	if domains == nil {
		domains = []string{}
	}
	return certificateDTO{
		OrderNo:      c.OrderNo,
		CertID:       c.CertID,
		Status:       c.Status,
		CommonName:   c.CommonName,
		Domains:      domains,
		KeyAlgorithm: string(c.KeyAlgorithm),
		SerialNumber: emptyToNil(c.SerialNumber),
		IssuedAt:     formatTimePtr(c.IssuedAt),
		ExpiresAt:    formatTimePtr(c.ExpiresAt),
		Certificate:  c.Certificate,
		CABundle:     emptyToNil(c.CABundle),
	}
}

// emptyToNil 把空串转成 nil，让可空字段在 JSON 里是 null 而不是 ""。
//
// 两者对前端不是一回事："" 会被渲染成一个空白的输入框，
// 而 null 才会走到「暂无」的分支。
func emptyToNil(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func formatTimePtr(t *time.Time) *string {
	if t == nil {
		return nil
	}
	formatted := t.Format(time.RFC3339)
	return &formatted
}

// ── 用户侧处理函数 ────────────────────────────────

func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	var body createOrderRequest
	if err := httpx.DecodeJSON(w, r, &body); err != nil {
		httpx.Fail(w, r, err)
		return
	}

	in := CreateInput{
		UserID:       httpx.MustUserID(r.Context()),
		ProductID:    body.ProductID,
		Years:        body.Years,
		KeyAlgorithm: rules.KeyAlgorithm(body.KeyAlgorithm),
		Domains:      body.Domains,
		CSR:          body.CSR,
		Contact: Contact{
			Name:  body.Contact.Name,
			Email: body.Contact.Email,
			Phone: body.Contact.Phone,
			Title: body.Contact.Title,
		},
	}
	if body.Organization != nil {
		in.Organization = &Organization{
			Name:           body.Organization.Name,
			RegistrationNo: body.Organization.RegistrationNo,
			Country:        body.Organization.Country,
			Province:       body.Organization.Province,
			City:           body.Organization.City,
			Address:        body.Organization.Address,
			PostalCode:     body.Organization.PostalCode,
			Phone:          body.Organization.Phone,
		}
	}

	o, err := h.svc.Create(r.Context(), in)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.OK(w, r, toDetailDTO(*o))
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()

	filter := Filter{Limit: DefaultLimit}
	if raw := query.Get("limit"); raw != "" {
		limit, err := strconv.Atoi(raw)
		if err != nil || limit <= 0 {
			httpx.Fail(w, r, errs.New(errs.CodeInvalidParam).
				WithField("limit", "limit 必须是正整数"))
			return
		}
		// 超出上限直接截断而不是报错，与钱包账本、充值订单接口保持一致
		if limit > MaxLimit {
			limit = MaxLimit
		}
		filter.Limit = limit
	}
	if raw := query.Get("cursor"); raw != "" {
		cursor, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || cursor < 0 {
			httpx.Fail(w, r, errs.New(errs.CodeInvalidParam).
				WithField("cursor", "cursor 必须是非负整数"))
			return
		}
		filter.Cursor = cursor
	}
	if raw := query.Get("status"); raw != "" {
		// 状态取值不在白名单时交给 Filter.Validate 报错，
		// 校验规则只写一处，避免 handler 与模型两套判断慢慢分叉。
		filter.Status = orderstate.State(raw)
	}

	page, err := h.svc.List(r.Context(), httpx.MustUserID(r.Context()), filter)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	dto := orderListDTO{Items: make([]orderSummaryDTO, 0, len(page.Items))}
	for _, o := range page.Items {
		dto.Items = append(dto.Items, toSummaryDTO(o, page.DomainNames[o.OrderNo]))
	}
	if page.NextCursor > 0 {
		dto.NextCursor = &page.NextCursor
	}
	httpx.OK(w, r, dto)
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	o, err := h.svc.Get(r.Context(), httpx.MustUserID(r.Context()), orderNoParam(r))
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	if o.Contact == nil {
		// 联系人行与订单行在同一个事务里写入，缺失只可能是数据被外部改动过。
		// 直接报 500 而不是下发一个 contact: null——契约里它是必填对象，
		// 前端会毫无防备地读 order.contact.email。
		httpx.Fail(w, r, errs.New(errs.CodeInternal).WithCause(
			errors.New("订单缺少联系人记录，数据可能被外部改动")))
		return
	}
	httpx.OK(w, r, toDetailDTO(*o))
}

func (h *Handler) cancel(w http.ResponseWriter, r *http.Request) {
	var body reasonRequest
	if err := decodeOptional(w, r, &body); err != nil {
		httpx.Fail(w, r, err)
		return
	}

	o, err := h.svc.Cancel(r.Context(), httpx.MustUserID(r.Context()),
		orderNoParam(r), body.Reason)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.OK(w, r, toDetailDTO(*o))
}

func (h *Handler) reissue(w http.ResponseWriter, r *http.Request) {
	var body reasonRequest
	if err := decodeOptional(w, r, &body); err != nil {
		httpx.Fail(w, r, err)
		return
	}

	o, err := h.svc.Reissue(r.Context(), httpx.MustUserID(r.Context()),
		orderNoParam(r), body.Reason)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.OK(w, r, toDetailDTO(*o))
}

func (h *Handler) listDomains(w http.ResponseWriter, r *http.Request) {
	domains, err := h.svc.ListDomains(r.Context(), httpx.MustUserID(r.Context()),
		orderNoParam(r))
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.OK(w, r, toDomainListDTO(domains))
}

func (h *Handler) verifyDomains(w http.ResponseWriter, r *http.Request) {
	var body verifyDomainsRequest
	if err := httpx.DecodeJSON(w, r, &body); err != nil {
		httpx.Fail(w, r, err)
		return
	}

	domains, err := h.svc.VerifyDomains(r.Context(), httpx.MustUserID(r.Context()),
		orderNoParam(r), body.Domains, rules.DcvMethod(body.Method))
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.OK(w, r, toDomainListDTO(domains))
}

func (h *Handler) resendEmail(w http.ResponseWriter, r *http.Request) {
	var body resendEmailRequest
	if err := httpx.DecodeJSON(w, r, &body); err != nil {
		httpx.Fail(w, r, err)
		return
	}

	domains, err := h.svc.ResendDcvEmail(r.Context(), httpx.MustUserID(r.Context()),
		orderNoParam(r), body.Domains)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.OK(w, r, toDomainListDTO(domains))
}

func (h *Handler) regenerateToken(w http.ResponseWriter, r *http.Request) {
	domains, err := h.svc.RegenerateDcvToken(r.Context(), httpx.MustUserID(r.Context()),
		orderNoParam(r))
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.OK(w, r, toDomainListDTO(domains))
}

func (h *Handler) certificate(w http.ResponseWriter, r *http.Request) {
	cert, err := h.svc.Certificate(r.Context(), httpx.MustUserID(r.Context()),
		orderNoParam(r))
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.OK(w, r, toCertificateDTO(cert))
}

// mockIssue 把模拟上游的订单推进到已签发。
//
// **只动上游，不碰本地订单。** 本地状态仍由上游回调驱动：如果这里顺手把
// 订单改成 issued，冒烟脚本就会绕开回调路径，而回调恰恰是最需要端到端
// 验证的一段（验签、幂等、乱序、资金释放）。
func (h *Handler) mockIssue(w http.ResponseWriter, r *http.Request) {
	// 走 Get 而不是直接读仓储：归属校验与「订单不存在返回 404」
	// 这两条规则在 Get 里已经写好，这里再写一遍迟早会分叉。
	o, err := h.svc.Get(r.Context(), httpx.MustUserID(r.Context()), orderNoParam(r))
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	if o.UpstreamOrderNo == "" {
		// 上游还没建单，没有可推进的对象。此时改上游状态只会得到一个
		// 「订单不存在」，不如直接说清楚订单卡在哪一步。
		httpx.Fail(w, r, errs.Newf(errs.CodeInvalidOrderState,
			"订单尚未提交到上游，当前状态为 %s", o.Status))
		return
	}

	if err := h.mock.IssueCertificate(o.UpstreamOrderNo); err != nil {
		httpx.Fail(w, r, errs.Newf(errs.CodeUpstream, "推进模拟上游失败").WithCause(err))
		return
	}
	httpx.OK(w, r, mockIssueDTO{UpstreamOrderNo: o.UpstreamOrderNo})
}

// ── 上游回调 ──────────────────────────────────────

func (h *Handler) webhook(w http.ResponseWriter, r *http.Request) {
	// 必须先读原始字节：签名是对原始字节做的，
	// 先反序列化再重新序列化会改变字节，验签必然失败。
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxWebhookBody))
	if err != nil {
		httpx.Fail(w, r, errs.Newf(errs.CodeInvalidParam, "读取回调报文失败"))
		return
	}

	if err := h.svc.HandleWebhook(
		r.Context(), raw, r.Header.Get(signatureHeader),
	); err != nil {
		h.logRejection(r, err)
		httpx.Fail(w, r, err)
		return
	}

	h.writeAck(w, r)
}

// logRejection 记录被拒绝的回调。
//
// 验签失败刻意不落库：攻击者可以用伪造签名的报文把任意事件塞进事件表，
// 把真实事件挤出幂等窗口。代价是这类事件只留在日志里，
// 所以来源信息必须记全。
func (h *Handler) logRejection(r *http.Request, err error) {
	if !errors.Is(err, foxssl.ErrInvalidSignature) {
		return
	}
	slog.WarnContext(r.Context(), "FoxSSL 回调验签失败",
		"remote_addr", httpx.ClientIP(r, h.trustProxy),
		"signature", r.Header.Get(signatureHeader),
		"error", err,
	)
}

// writeAck 按上游约定的格式应答。
//
// 不用本服务的统一信封：上游只认它文档里的 {"status":"success"}，
// 包一层 code/message 会被判成失败并触发无限重推。
// 应答格式属于上游的领域知识，因此由适配器给出。
func (h *Handler) writeAck(w http.ResponseWriter, r *http.Request) {
	ack := h.svc.Ack()
	w.Header().Set("Content-Type", ack.ContentType)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(ack.Body)
}

// ── 辅助 ──────────────────────────────────────────

// orderNoParam 取出路径里的订单号。
func orderNoParam(r *http.Request) string {
	return chi.URLParam(r, "orderNo")
}

// decodeOptional 解析可选的请求体。
//
// 契约里取消与重签的请求体是 required: false，客户端可能不带 body 直接 POST。
// 直接调 DecodeJSON 会把「没带 body」判成参数错误，而那是一个完全合法的请求。
//
// 先整体读出来再判断，而不是看 ContentLength：分块传输（chunked）时
// ContentLength 是 -1，按它判断会把带 body 的分块请求当成空的。
func decodeOptional(w http.ResponseWriter, r *http.Request, dst any) *errs.Error {
	if r.Body == nil {
		return nil
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, httpx.MaxBodyBytes))
	if err != nil {
		return errs.New(errs.CodeInvalidParam).WithField("body", "请求体过大")
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		return errs.New(errs.CodeInvalidParam).WithField("body", "JSON 格式不正确")
	}
	return nil
}
