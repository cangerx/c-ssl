package foxssl

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/cangerx/c-ssl/server/internal/domain/money"
)

// PlaceholderFQDN 是上游文件验证路径里的域名占位符。
//
// 上游返回的是模板，不是可以直接用的路径。订单域必须逐个域名替换后
// 才能展示给用户——直接把模板交出去，用户会照着一个含 {FQDN} 的
// 地址去放验证文件，然后怎么试都不通过。
const PlaceholderFQDN = "{FQDN}"

// fileDcvPathTemplate 是 Mock 返回的文件验证路径模板。
//
// **刻意保留占位符，不做预替换。** 如果 Mock 返回已经替换好的路径，
// 订单域里那段替换逻辑就永远测不到——它写成什么样测试都是绿的，
// 而线上拿到的是真实上游的模板，于是所有文件验证都会失败。
// mock_test.go 里有一条用例专门断言这个模板确实含占位符，
// 防止有人为了「让测试好看」把它改成预替换的。
const fileDcvPathTemplate = "/.well-known/pki-validation/" + PlaceholderFQDN + "/%s.txt"

// 操作名，用于失败注入。
const (
	OpBalance             = "Balance"
	OpCreateOrder         = "CreateOrder"
	OpOrderStatus         = "OrderStatus"
	OpListDomains         = "ListDomains"
	OpVerifyDomains       = "VerifyDomains"
	OpResendDcvEmail      = "ResendDcvEmail"
	OpRegenerateDcvToken  = "RegenerateDcvToken"
	OpDownloadCertificate = "DownloadCertificate"
	OpReissue             = "Reissue"
	OpCancelOrder         = "CancelOrder"
	OpParseNotification   = "ParseNotification"
)

// 上游订单状态取值。真实上游的取值比这里多，Mock 只实现
// 平台会用到的那几个，其余状态由测试直接注入字符串。
const (
	MockStatusWaitingDcv = "waiting_dcv"
	MockStatusIssuing    = "issuing"
	MockStatusIssued     = "issued"
	MockStatusCancelled  = "cancelled"
	MockStatusFailed     = "failed"
)

// MockClient 是 FoxSSL 的模拟实现，用于开发与自动化测试。
//
// 它刻意实现了真实上游最容易被忽略的那个性质：**CreateOrder 按商户订单号幂等**。
// 少了它，订单域「重试不产生第二张证书」的测试就是假的——
// 而这条性质恰恰是资金安全的关键。
type MockClient struct {
	apiKey        string
	webhookSecret string
	baseURL       string
	clock         func() time.Time

	mu         sync.Mutex
	orders     map[string]*mockOrder // key: 上游订单号
	byMerchant map[string]string     // 商户订单号 → 上游订单号
	// seq 是订单序号。初值随机，见 NewMockClient 的说明。
	seq int64
	// cost 是 CreateOrder 返回的成本价，可被测试调整。
	cost money.Amount
	// failures 是一次性失败注入，key 是操作名。命中后即删除，
	// 这样「第一次失败、第二次成功」的重试路径可以被精确构造。
	failures map[string]error
}

type mockOrder struct {
	upstreamOrderNo string
	merchantOrderNo string
	productID       int
	years           int
	keyAlgorithm    string
	domains         []string
	csr             string
	contact         Contact
	org             *Organization

	orderStatus   string
	certStatus    string
	prepareStatus string
	reissueStatus string
	certID        string
	cost          money.Amount
	failureReason string

	domainStates map[string]*Domain
	createdAt    time.Time
	expiresAt    time.Time
	cert         *Certificate
}

// NewMockClient 构造模拟上游。
//
// apiKey 与 webhookSecret 都必须非空。允许空值意味着「忘记配置」会
// 静默变成「用一个空密钥验签」——而空密钥的 HMAC 是任何人都会算的，
// 验签就成了一道装饰。
//
// 订单序号的初值取自随机数，而不是从 1 开始。从 1 开始时，
// 两个独立进程里的 Mock 会生成完全相同的上游订单号
// （`FX-20260916-0001`），而测试包之间是并行跑的——
// 于是不同包的测试会算出同一个事件幂等键，后跑的那个看到「重复事件」
// 直接跳过，测试静默变成假绿。真实上游也不会每天从 1 开始编号。
func NewMockClient(apiKey, webhookSecret, baseURL string) (*MockClient, error) {
	if strings.TrimSpace(apiKey) == "" {
		return nil, fmt.Errorf("mock 上游的 API Key 未配置（FOXSSL_API_KEY）")
	}
	if strings.TrimSpace(webhookSecret) == "" {
		return nil, fmt.Errorf("mock 上游的 Webhook 密钥未配置（FOXSSL_WEBHOOK_SECRET）")
	}
	return &MockClient{
		apiKey:        apiKey,
		webhookSecret: webhookSecret,
		baseURL:       strings.TrimRight(baseURL, "/"),
		clock:         time.Now,
		orders:        make(map[string]*mockOrder),
		byMerchant:    make(map[string]string),
		seq:           randomSeq(),
		cost:          1000,
		failures:      make(map[string]error),
	}, nil
}

// randomSeq 生成订单序号的初值。
//
// 取不到随机数时退回 0：这只影响上游订单号的取值分布，
// 不影响任何业务行为，没必要因为一个随机数就让构造失败。
func randomSeq() int64 {
	var buf [4]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return 0
	}
	n := int64(buf[0])<<24 | int64(buf[1])<<16 | int64(buf[2])<<8 | int64(buf[3])
	return n % 1_000_000
}

// Name 返回上游标识。
func (c *MockClient) Name() string { return NameMock }

// ── 测试控制接口 ──────────────────────────────────

// FailOnce 让下一次指定操作返回 err，仅生效一次。
//
// 用「一次性」而不是「持续失败」：重试路径的验证需要「第一次失败、
// 第二次成功」，持续失败只能测到「一直失败」，测不出补偿是否真的有效。
func (c *MockClient) FailOnce(op string, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.failures[op] = err
}

// SetCost 设置 CreateOrder 返回的成本价。
func (c *MockClient) SetCost(amount money.Amount) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cost = amount
}

// SetClock 替换时钟，供测试控制时间。
func (c *MockClient) SetClock(clock func() time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.clock = clock
}

// SetOrderStatus 直接设置上游订单状态，模拟上游推进。
func (c *MockClient) SetOrderStatus(upstreamOrderNo, status string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	o, ok := c.orders[upstreamOrderNo]
	if !ok {
		return fmt.Errorf("%w: %s", ErrOrderNotFound, upstreamOrderNo)
	}
	o.orderStatus = status
	return nil
}

// SetDomainStatus 设置某个域名的上游验证状态。
func (c *MockClient) SetDomainStatus(upstreamOrderNo, domain, status string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	o, ok := c.orders[upstreamOrderNo]
	if !ok {
		return fmt.Errorf("%w: %s", ErrOrderNotFound, upstreamOrderNo)
	}
	d, ok := o.domainStates[domain]
	if !ok {
		return fmt.Errorf("域名 %s 不属于订单 %s", domain, upstreamOrderNo)
	}
	d.Status = status
	if status == "verified" {
		d.VerifiedAt = c.clock()
	}
	return nil
}

// IssueCertificate 把订单推进到已签发，并生成一张证书。
func (c *MockClient) IssueCertificate(upstreamOrderNo string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	o, ok := c.orders[upstreamOrderNo]
	if !ok {
		return fmt.Errorf("%w: %s", ErrOrderNotFound, upstreamOrderNo)
	}
	now := c.clock()
	if o.certID == "" {
		o.certID = "CERT-" + o.upstreamOrderNo
	}
	o.orderStatus = MockStatusIssued
	o.certStatus = MockStatusIssued
	o.expiresAt = now.AddDate(0, 0, 365*o.years)
	for _, d := range o.domainStates {
		d.Status = "verified"
		if d.VerifiedAt.IsZero() {
			d.VerifiedAt = now
		}
	}
	o.cert = &Certificate{
		CertID:       o.certID,
		Status:       MockStatusIssued,
		CommonName:   o.domains[0],
		Domains:      append([]string(nil), o.domains...),
		KeyAlgorithm: o.keyAlgorithm,
		SerialNumber: fmt.Sprintf("%X", sha256.Sum256([]byte(o.upstreamOrderNo)))[:32],
		Certificate:  mockCertificatePEM(o.certID, o.domains),
		CABundle:     mockCABundlePEM(),
		IssuedAt:     now,
		ExpiresAt:    now.AddDate(0, 0, 365*o.years),
	}
	return nil
}

// UpstreamOrderNo 返回某个商户订单号对应的上游订单号，不存在时返回空串。
//
// 测试用它断言「同一个本地订单号只产生了一个上游订单」。
func (c *MockClient) UpstreamOrderNo(merchantOrderNo string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.byMerchant[merchantOrderNo]
}

// OrderCount 返回上游侧的订单总数，用于断言没有产生重复订单。
func (c *MockClient) OrderCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.orders)
}

// fail 取出并清除一次性失败注入。
func (c *MockClient) fail(op string) error {
	if err, ok := c.failures[op]; ok {
		delete(c.failures, op)
		return err
	}
	return nil
}

// ── Client 实现 ───────────────────────────────────

// Balance 返回一个固定的上游余额。
func (c *MockClient) Balance(_ context.Context) (*Balance, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.fail(OpBalance); err != nil {
		return nil, err
	}
	return &Balance{Amount: 100_000_00, Currency: "CNY"}, nil
}

// CreateOrder 在上游创建订单。
//
// **幂等**：同一个 MerchantOrderNo 重复调用返回首次的结果，
// 不产生第二个上游订单。真实上游同样按商户订单号去重，
// 这是平台在网络超时后敢于重试的前提。
func (c *MockClient) CreateOrder(_ context.Context, req CreateOrderRequest) (*CreateOrderResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if err := c.fail(OpCreateOrder); err != nil {
		return nil, err
	}
	if req.MerchantOrderNo == "" {
		return nil, fmt.Errorf("%w: 缺少商户订单号", ErrMalformedPayload)
	}
	if len(req.Domains) == 0 {
		return nil, fmt.Errorf("%w: 缺少域名", ErrMalformedPayload)
	}

	// 命中幂等：返回首次结果，不新建订单。
	if no, ok := c.byMerchant[req.MerchantOrderNo]; ok {
		o := c.orders[no]
		return &CreateOrderResponse{
			UpstreamOrderNo: o.upstreamOrderNo,
			CertID:          o.certID,
			Status:          o.orderStatus,
			Cost:            o.cost,
			CreatedAt:       o.createdAt,
		}, nil
	}

	c.seq++
	now := c.clock()
	o := &mockOrder{
		upstreamOrderNo: fmt.Sprintf("FX-%s-%06d", now.Format("20060102"), c.seq),
		merchantOrderNo: req.MerchantOrderNo,
		productID:       req.UpstreamProductID,
		years:           req.Years,
		keyAlgorithm:    req.KeyAlgorithm,
		domains:         append([]string(nil), req.Domains...),
		csr:             req.CSR,
		contact:         req.Contact,
		org:             req.Organization,
		orderStatus:     MockStatusWaitingDcv,
		prepareStatus:   "prepared",
		cost:            c.cost,
		domainStates:    make(map[string]*Domain, len(req.Domains)),
		createdAt:       now,
	}
	for _, d := range req.Domains {
		o.domainStates[d] = c.newDomain(d)
	}
	c.orders[o.upstreamOrderNo] = o
	c.byMerchant[req.MerchantOrderNo] = o.upstreamOrderNo

	return &CreateOrderResponse{
		UpstreamOrderNo: o.upstreamOrderNo,
		CertID:          o.certID,
		Status:          o.orderStatus,
		Cost:            o.cost,
		CreatedAt:       o.createdAt,
	}, nil
}

// newDomain 生成一个域名的初始验证材料。
//
// 调用方必须持有 c.mu。
func (c *MockClient) newDomain(domain string) *Domain {
	base := baseDomain(domain)
	token := randomToken()
	return &Domain{
		Domain:         domain,
		Status:         "pending",
		DnsRecordType:  "TXT",
		DnsRecordName:  "_dnsauth." + base,
		DnsRecordValue: token,
		// 含 {FQDN} 占位符的模板，交给订单域去替换。
		FileDcvPath:    fmt.Sprintf(fileDcvPathTemplate, token),
		FileContent:    token,
		EmailAddresses: []string{"admin@" + base, "hostmaster@" + base, "webmaster@" + base},
	}
}

// OrderStatus 查询订单状态。
func (c *MockClient) OrderStatus(_ context.Context, upstreamOrderNo string) (*OrderStatus, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.fail(OpOrderStatus); err != nil {
		return nil, err
	}
	o, ok := c.orders[upstreamOrderNo]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrOrderNotFound, upstreamOrderNo)
	}
	out := &OrderStatus{
		UpstreamOrderNo:   o.upstreamOrderNo,
		OrderStatus:       o.orderStatus,
		CertStatus:        o.certStatus,
		PrepareStatus:     o.prepareStatus,
		ReissueStatus:     o.reissueStatus,
		CertID:            o.certID,
		CommonName:        o.domains[0],
		ExpiresAt:         o.expiresAt,
		FailureReasonText: o.failureReason,
	}
	for _, d := range o.domains {
		ds := o.domainStates[d]
		out.Domains = append(out.Domains, DomainStatus{
			Domain: ds.Domain,
			Status: ds.Status,
			Method: ds.Method,
		})
	}
	return out, nil
}

// ListDomains 返回域名的验证材料。
func (c *MockClient) ListDomains(_ context.Context, upstreamOrderNo string) (*DomainsResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.fail(OpListDomains); err != nil {
		return nil, err
	}
	o, ok := c.orders[upstreamOrderNo]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrOrderNotFound, upstreamOrderNo)
	}
	return &DomainsResponse{
		UpstreamOrderNo: o.upstreamOrderNo,
		Domains:         copyDomains(o),
	}, nil
}

// VerifyDomains 通知上游校验域名，并把状态推进到 verifying。
func (c *MockClient) VerifyDomains(_ context.Context, req VerifyDomainsRequest) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.fail(OpVerifyDomains); err != nil {
		return err
	}
	o, ok := c.orders[req.UpstreamOrderNo]
	if !ok {
		return fmt.Errorf("%w: %s", ErrOrderNotFound, req.UpstreamOrderNo)
	}
	for _, name := range req.Domains {
		d, ok := o.domainStates[name]
		if !ok {
			return fmt.Errorf("域名 %s 不属于订单 %s", name, req.UpstreamOrderNo)
		}
		d.Method = req.Method
		if d.Status != "verified" {
			d.Status = "verifying"
		}
	}
	return nil
}

// ResendDcvEmail 重发验证邮件。
func (c *MockClient) ResendDcvEmail(_ context.Context, upstreamOrderNo string, domains []string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.fail(OpResendDcvEmail); err != nil {
		return err
	}
	o, ok := c.orders[upstreamOrderNo]
	if !ok {
		return fmt.Errorf("%w: %s", ErrOrderNotFound, upstreamOrderNo)
	}
	for _, name := range domains {
		d, ok := o.domainStates[name]
		if !ok {
			return fmt.Errorf("域名 %s 不属于订单 %s", name, upstreamOrderNo)
		}
		if d.Method != "email" {
			return fmt.Errorf("%w: 域名 %s 未使用邮件验证", ErrNotSupported, name)
		}
	}
	return nil
}

// RegenerateDcvToken 重新生成验证 token。
func (c *MockClient) RegenerateDcvToken(_ context.Context, upstreamOrderNo string) (*DomainsResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.fail(OpRegenerateDcvToken); err != nil {
		return nil, err
	}
	o, ok := c.orders[upstreamOrderNo]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrOrderNotFound, upstreamOrderNo)
	}
	for _, name := range o.domains {
		o.domainStates[name] = c.newDomain(name)
	}
	return &DomainsResponse{
		UpstreamOrderNo: o.upstreamOrderNo,
		Domains:         copyDomains(o),
	}, nil
}

// DownloadCertificate 返回证书。
func (c *MockClient) DownloadCertificate(_ context.Context, upstreamOrderNo string) (*Certificate, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.fail(OpDownloadCertificate); err != nil {
		return nil, err
	}
	o, ok := c.orders[upstreamOrderNo]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrOrderNotFound, upstreamOrderNo)
	}
	if o.cert == nil {
		return nil, fmt.Errorf("%w: 订单 %s 尚未签发", ErrOrderNotFound, upstreamOrderNo)
	}
	cert := *o.cert
	return &cert, nil
}

// Reissue 申请重签，并生成一张新证书。
func (c *MockClient) Reissue(_ context.Context, req ReissueRequest) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.fail(OpReissue); err != nil {
		return err
	}
	o, ok := c.orders[req.UpstreamOrderNo]
	if !ok {
		return fmt.Errorf("%w: %s", ErrOrderNotFound, req.UpstreamOrderNo)
	}
	if o.orderStatus != MockStatusIssued {
		return fmt.Errorf("%w: 订单 %s 当前状态为 %s，不能重签", ErrNotSupported, req.UpstreamOrderNo, o.orderStatus)
	}
	if req.CSR != "" {
		o.csr = req.CSR
	}
	o.reissueStatus = "reissued"
	o.certID = "CERT-" + o.upstreamOrderNo + "-R1"
	now := c.clock()
	o.cert = &Certificate{
		CertID:       o.certID,
		Status:       MockStatusIssued,
		CommonName:   o.domains[0],
		Domains:      append([]string(nil), o.domains...),
		KeyAlgorithm: o.keyAlgorithm,
		SerialNumber: fmt.Sprintf("%X", sha256.Sum256([]byte(o.certID)))[:32],
		Certificate:  mockCertificatePEM(o.certID, o.domains),
		CABundle:     mockCABundlePEM(),
		IssuedAt:     now,
		ExpiresAt:    now.AddDate(0, 0, 365*o.years),
	}
	o.expiresAt = o.cert.ExpiresAt
	return nil
}

// CancelOrder 取消订单。
func (c *MockClient) CancelOrder(_ context.Context, upstreamOrderNo string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.fail(OpCancelOrder); err != nil {
		return err
	}
	o, ok := c.orders[upstreamOrderNo]
	if !ok {
		return fmt.Errorf("%w: %s", ErrOrderNotFound, upstreamOrderNo)
	}
	if o.orderStatus == MockStatusIssued {
		return fmt.Errorf("%w: 已签发的订单不能取消", ErrNotSupported)
	}
	o.orderStatus = MockStatusCancelled
	return nil
}

// ── 入站 ──────────────────────────────────────────

// Ack 返回上游约定的成功应答。
func (c *MockClient) Ack() Ack {
	return Ack{ContentType: "application/json", Body: []byte(`{"status":"success"}`)}
}

// mockNotification 是线上报文的格式定义。
//
// 编码与解析共用这一份定义，避免「签名时字段顺序是 A、解析时按 B 读」
// 这类只在真机联调时才会暴露的错位。
type mockNotification struct {
	Event      string             `json:"event"`
	OrderNo    string             `json:"orderNo"`
	Status     string             `json:"status"`
	CertID     string             `json:"certId,omitempty"`
	Domains    []mockDomainStatus `json:"domains,omitempty"`
	OccurredAt int64              `json:"occurredAt,omitempty"`
}

type mockDomainStatus struct {
	Domain string `json:"domain"`
	Status string `json:"status"`
	Method string `json:"method,omitempty"`
}

// EncodeNotification 把事件编码成上游报文格式，供测试与模拟投递使用。
func EncodeNotification(n *Notification) ([]byte, error) {
	msg := mockNotification{
		Event:   n.EventType,
		OrderNo: n.UpstreamOrderNo,
		Status:  n.Status,
		CertID:  n.CertID,
	}
	// 用类型转换而不是逐字段构造：两个结构体字段完全一致，
	// 将来给 DomainStatus 加字段时这里会编译失败，而不是静默丢掉新字段。
	for _, d := range n.Domains {
		msg.Domains = append(msg.Domains, mockDomainStatus(d))
	}
	if !n.OccurredAt.IsZero() {
		msg.OccurredAt = n.OccurredAt.Unix()
	}
	return json.Marshal(msg)
}

// Sign 计算报文的签名：HMAC-SHA256 后取 Base64。
func Sign(secret string, raw []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(raw)
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// ParseNotification 验签并解析上游回调。
//
// 先验签再解析，顺序不可交换：先解析会让未经认证的报文进入 JSON 解码器，
// 把攻击面扩大到解码器的所有实现细节上。
func (c *MockClient) ParseNotification(raw []byte, signature string) (*Notification, error) {
	c.mu.Lock()
	failErr := c.fail(OpParseNotification)
	secret := c.webhookSecret
	c.mu.Unlock()

	if failErr != nil {
		return nil, failErr
	}

	// hmac.Equal 而不是 ==：普通字符串比较耗时随匹配前缀增长，
	// 可以被逐字节猜测出合法签名。
	expected := Sign(secret, raw)
	if !hmac.Equal([]byte(expected), []byte(signature)) {
		return nil, ErrInvalidSignature
	}

	var msg mockNotification
	if err := json.Unmarshal(raw, &msg); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrMalformedPayload, err)
	}
	if strings.TrimSpace(msg.OrderNo) == "" {
		return nil, fmt.Errorf("%w: 缺少 orderNo", ErrMalformedPayload)
	}
	if strings.TrimSpace(msg.Status) == "" {
		return nil, fmt.Errorf("%w: 缺少 status", ErrMalformedPayload)
	}

	n := &Notification{
		EventType:       msg.Event,
		UpstreamOrderNo: msg.OrderNo,
		Status:          msg.Status,
		CertID:          msg.CertID,
		Raw:             append([]byte(nil), raw...),
	}
	if msg.OccurredAt > 0 {
		n.OccurredAt = time.Unix(msg.OccurredAt, 0).UTC()
	}
	for _, d := range msg.Domains {
		n.Domains = append(n.Domains, DomainStatus(d))
	}
	return n, nil
}

// ── 内部辅助 ──────────────────────────────────────

// baseDomain 去掉通配符前缀。
//
// 通配符域名的验证材料配在裸域名上：`*.example.com` 的 TXT 记录要加在
// `_dnsauth.example.com`，文件要放到 `example.com` 下——
// DNS 里根本不存在 `*.example.com` 这个可以承载记录的名字。
func baseDomain(domain string) string {
	return strings.TrimPrefix(strings.TrimSpace(domain), "*.")
}

func randomToken() string {
	// 用随机值而不是「域名派生」：派生值在重新生成 token 时不会变，
	// 于是「重新生成后旧 token 失效」这条性质就永远测不出来。
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		// crypto/rand 失败属于系统级异常，退回时间纳秒保证不 panic
		return strings.ToUpper(fmt.Sprintf("%016X", time.Now().UnixNano()))
	}
	return strings.ToUpper(fmt.Sprintf("%X", buf))
}

// copyDomains 深拷贝域名列表，避免调用方改到内部状态。
func copyDomains(o *mockOrder) []Domain {
	out := make([]Domain, 0, len(o.domains))
	for _, name := range o.domains {
		d := *o.domainStates[name]
		d.EmailAddresses = append([]string(nil), d.EmailAddresses...)
		out = append(out, d)
	}
	return out
}

func mockCertificatePEM(certID string, domains []string) string {
	body := base64.StdEncoding.EncodeToString([]byte("cert:" + certID + ":" + strings.Join(domains, ",")))
	return "-----BEGIN CERTIFICATE-----\n" + body + "\n-----END CERTIFICATE-----\n"
}

func mockCABundlePEM() string {
	body := base64.StdEncoding.EncodeToString([]byte("ca-bundle:foxssl-mock"))
	return "-----BEGIN CERTIFICATE-----\n" + body + "\n-----END CERTIFICATE-----\n"
}
