package foxssl

// 本文件是真实 FoxSSL 上游的 HTTP 适配器。
//
// ── 分两层，改动的风险完全不同 ─────────────────────
//
//   - **传输层**（超时、重试、响应限长、日志脱敏、错误分类）：
//     与上游报文格式无关，可以完整测试，上游改字段也不会让它失效。
//   - **报文层**（路径、请求体、响应字段名）：完全由上游文档决定。
//
// 报文层在拿到文档之前**不实现、也不猜**。猜出来的字段名会一路绿到线上，
// 然后在真实联调时全部报「字段缺失」——而那时代码已经写过一遍，
// 返工比现在慢得多。11 个方法的清单与需要文档提供的内容见文件末尾。
//
// ── 传输层里最要紧的一件事：错误分类 ────────────────
//
// 订单域靠本包的错误判断两件截然不同的事（见 order.isUnknownResult）：
//
//   - **上游明确拒绝**（订单没建）→ 解冻并置失败，钱退给用户
//   - **结果未知**（可能已经建了单）→ 保持冻结，等补偿按同一个商户订单号重试
//
// 两个方向都会花错钱：把「未知」当「拒绝」是平台白付一张证书，
// 把「拒绝」当「未知」是用户的钱永远冻着。所以本文件里每一处
// 「什么情况算未知」的判断都要有依据，不能凭感觉。

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/cangerx/c-ssl/server/internal/platform/logger"
)

// 默认值。除鉴权头外都与上游文档无关。
const (
	// defaultTimeout 是单次尝试的总超时（含连接、发送、读取）。
	//
	// 上游回调要求 8 秒内返回，但那是**入站**方向的约束；出站调用
	// 面对的是同一个上游，10 秒留出余量。上游文档没有给超时约定，
	// 真实值要按实测 P99 调，见 HTTPOptions.Timeout。
	defaultTimeout = 10 * time.Second
	// defaultMaxRetries 是自动重试的次数（不含首次）。
	defaultMaxRetries = 2
	// defaultMaxBodyBytes 是响应体上限。
	//
	// 证书 PEM 链通常几十 KB，2 MiB 留了两个数量级的余量。设上限不是为了
	// 省内存，而是防止上游或中间网关返回一个巨大的 HTML 错误页时，
	// 把整段内容读进内存、写进日志。
	defaultMaxBodyBytes = 2 << 20
)

// 鉴权头的取值。
//
// **这两个值来自上游官方文档，不是猜的。** 文档里每个接口的请求头都写着
// `apiKey: <your apiKey>`，且不带任何前缀（不是 Authorization: Bearer）。
//
// 上一版这两个默认值是 Authorization + Bearer，那是「业界最常见的约定」。
// 这个例子说明为什么默认值必须能被文档证伪：Bearer 写错时上游返回的
// 是「参数不合法」一类的业务错误，看不出是鉴权头的问题。
const (
	defaultAuthHeader = "apiKey"
	// defaultAuthScheme 为 "-" 表示凭证裸放，不加前缀。
	defaultAuthScheme = "-"
)

// HTTPOptions 是真实上游客户端的构造参数。
type HTTPOptions struct {
	// BaseURL 是上游接口根地址，不含路径。
	BaseURL string
	// APIKey 是调用上游接口的凭证，**同时是回调验签的密钥**。
	//
	// 上游文档给的验签示例是 crypto.createHmac('sha256', apiKey)——
	// 没有独立的 webhook secret。所以这里没有第二个密钥字段：
	// 多一个「看起来能用」的字段，运维就会去配它，然后所有回调
	// 因为验签失败被拒，而错误信息只会说「签名不匹配」。
	APIKey string

	// Timeout 是单次尝试的超时，零值取 defaultTimeout。
	//
	// 注意它**不是**一次调用的总超时：重试会各给一次。
	// 总时长由调用方的 context 兜住（见 attempt）。
	Timeout time.Duration
	// MaxRetries 是自动重试次数，零值取 defaultMaxRetries。
	// 负数表示不重试。
	MaxRetries int
	// MaxBodyBytes 是响应体上限，零值取 defaultMaxBodyBytes。
	MaxBodyBytes int64

	// AuthHeader 是凭证所在请求头，空取 defaultAuthHeader。
	AuthHeader string
	// AuthScheme 是凭证前缀（如 "Bearer"），空取 defaultAuthScheme。
	// 置为 "-" 表示凭证裸放，不加前缀。
	AuthScheme string

	// HTTPClient 可注入自定义传输。为空时按 Timeout 构造一个。
	HTTPClient *http.Client

	// Sleep 用于退避等待，可注入以便测试。
	Sleep func(ctx context.Context, d time.Duration) error
	// Rand 用于退避抖动，可注入以便测试确定化。
	Rand func(n int64) int64
}

// HTTPClient 是真实 FoxSSL 上游的客户端。
//
// 实现了 foxssl.Client 的全部出站方法与入站回调解析，
// 报文格式的出处与剩余缺口见文件末尾。
type HTTPClient struct {
	baseURL    string
	apiKey     string
	authHeader string
	authScheme string

	http         *http.Client
	timeout      time.Duration
	maxRetries   int
	maxBodyBytes int64

	sleep func(ctx context.Context, d time.Duration) error
	rand  func(n int64) int64
}

// NewHTTPClient 构造真实上游客户端。
//
// 凭证为空时直接报错，与 Mock 上游同一个理由：「忘记配置」静默变成
// 「用一个空凭证调用上游」时，上游返回的错误信息通常与凭证无关
// （多半是「参数不合法」），排查方向会跑偏很远。
func NewHTTPClient(opts HTTPOptions) (*HTTPClient, error) {
	base := strings.TrimRight(strings.TrimSpace(opts.BaseURL), "/")
	if base == "" {
		return nil, fmt.Errorf("上游地址未配置（FOXSSL_BASE_URL）")
	}
	if !strings.HasPrefix(base, "http://") && !strings.HasPrefix(base, "https://") {
		return nil, fmt.Errorf("上游地址必须以 http:// 或 https:// 开头，实际 %q", opts.BaseURL)
	}
	if strings.TrimSpace(opts.APIKey) == "" {
		return nil, fmt.Errorf("上游 API Key 未配置（FOXSSL_API_KEY）")
	}

	c := &HTTPClient{
		baseURL:      base,
		apiKey:       opts.APIKey,
		authHeader:   opts.AuthHeader,
		authScheme:   opts.AuthScheme,
		timeout:      opts.Timeout,
		maxRetries:   opts.MaxRetries,
		maxBodyBytes: opts.MaxBodyBytes,
		sleep:        opts.Sleep,
		rand:         opts.Rand,
	}
	if c.authHeader == "" {
		c.authHeader = defaultAuthHeader
	}
	if c.authScheme == "" {
		c.authScheme = defaultAuthScheme
	}
	if c.timeout <= 0 {
		c.timeout = defaultTimeout
	}
	// MaxRetries 的零值取默认值，负数表示「一次都不重试」。
	//
	// 不能让零值直接表示「不重试」：那样一个只填了地址与凭证的
	// HTTPOptions 会静默地失去全部重试能力，而上游偶发 503 时
	// 表现就是「时不时失败一下」，看起来像上游的问题。
	switch {
	case opts.MaxRetries > 0:
		c.maxRetries = opts.MaxRetries
	case opts.MaxRetries < 0:
		c.maxRetries = 0
	default:
		c.maxRetries = defaultMaxRetries
	}
	if c.maxBodyBytes <= 0 {
		c.maxBodyBytes = defaultMaxBodyBytes
	}
	if c.http = opts.HTTPClient; c.http == nil {
		// 不设 http.Client.Timeout：超时统一由每次尝试的 context 控制，
		// 两处都设会让「到底是哪一层超时了」变得无法判断。
		c.http = &http.Client{}
	}
	if c.sleep == nil {
		c.sleep = sleepContext
	}
	if c.rand == nil {
		c.rand = rand.Int63n
	}
	return c, nil
}

// Name 返回上游标识。
func (c *HTTPClient) Name() string { return NameHTTP }

// 上游操作名。用于日志、错误信息与重试策略查表。
const (
	opBalance             = "Balance"
	opCreateOrder         = "CreateOrder"
	opOrderStatus         = "OrderStatus"
	opListDomains         = "ListDomains"
	opVerifyDomains       = "VerifyDomains"
	opResendDcvEmail      = "ResendDcvEmail"
	opRegenerateDcvToken  = "RegenerateDcvToken"
	opDownloadCertificate = "DownloadCertificate"
	opReissue             = "Reissue"
	opCancelOrder         = "CancelOrder"
	opFindOrder           = "FindOrder"
)

// idempotentOps 是「重复执行不会产生第二次副作用」的操作。
//
// 判据不是「这个接口看起来像查询」，而是**重复调用一次会不会改变上游状态**：
//
//   - 只读操作天然满足。
//   - **CreateOrder 不满足。** 上游的下单接口没有任何商户侧标识，
//     orderNo 由上游生成，所以重复提交就是真的再买一张证书。
//     上一版把它列在这里，依据是「上游按商户订单号去重」——
//     那条假设来自接口设计的直觉，被文档推翻了。这个错误的代价是
//     平台重复下单、重复扣钱，而且每次都是真的。
//     结果未知时的正确做法是先 FindOrder 反查，见 client.go 包注释的约定 1。
//
// 不在这里的操作（下单、提交验证、重发邮件、重生成 token、重签、取消）都
// 会改变上游状态或消耗一次机会，见 safeToRetry。
var idempotentOps = map[string]bool{
	opBalance:             true,
	opOrderStatus:         true,
	opListDomains:         true,
	opDownloadCertificate: true,
	// FindOrder 是纯查询，且它正是「结果未知时该调的那个」——
	// 如果连它都不能重试，补偿路径第一步就卡住了。
	opFindOrder: true,
}

// errRequestNotSent 标记「请求字节没有发出去」的失败。
//
// 它同时属于 ErrUnavailable（值得重试），但比 ErrUnavailable 多一层信息：
// **可以证明上游没有收到这次请求**，因此连有副作用的操作也能安全重试。
//
// 这个区分只有传输层做得出来——调用方拿到的错误里看不出「是连接没建起来
// 还是应答在路上丢了」，而两者的处置完全不同。
var errRequestNotSent = errors.New("请求未发出")

// do 发一次出站请求，按需重试，返回成功应答里 **data 字段**的原始字节。
//
// 返回的是 data 而不是整个报文：信封（code / msg）的意义只在判断成败，
// 判断完之后它对调用方没有任何用处，而把它一起返回会诱使每个调用点
// 自己再解析一遍信封、再判断一次 code。
//
// 返回的错误总是包着本包的哨兵错误之一，调用方（订单域）据此判断
// 「上游明确拒绝」还是「结果未知」。
func (c *HTTPClient) do(
	ctx context.Context, op, method, path string, body any,
) ([]byte, error) {
	var payload []byte
	if body != nil {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			// 编码失败是本地 bug（请求结构写错了），重试没有意义，
			// 也不能算「结果未知」——请求根本没构造出来。
			return nil, fmt.Errorf("编码 %s 请求失败: %w", op, err)
		}
	}

	endpoint := c.baseURL + path
	for attempt := 0; ; attempt++ {
		raw, retryAfter, err := c.attempt(ctx, op, method, endpoint, payload)
		if err == nil {
			// HTTP 2xx 不等于成功：上游把业务失败也放在 HTTP 200 里，
			// 真正的结果在报文体的 code 里（见 wire.go 开头第 1 条）。
			// 所以信封解析必须在这个重试循环内——业务码 500 与 6801
			// 都属于「结果未知」，值得重试，而在循环外解析就没机会重试了。
			data, unwrapErr := c.unwrap(op, raw)
			if unwrapErr == nil {
				return data, nil
			}
			err = unwrapErr
			// 上游没给 Retry-After（业务码在报文体里，与响应头无关）。
			retryAfter = 0
		}

		// 调用方的 context 已经结束（超时或被取消）时不再重试：
		// 重试必然立刻失败，只是把同一个错误重复一遍。
		// 但错误本身仍归入「结果未知」——请求可能已经到达上游，
		// 所以订单域会保持冻结而不是解冻。
		if ctx.Err() != nil {
			return nil, err
		}
		if attempt >= c.maxRetries || !safeToRetry(op, err) {
			return nil, err
		}

		wait := backoff(attempt, c.rand)
		if retryAfter > 0 {
			wait = retryAfter
		}
		slog.WarnContext(ctx, "上游调用失败，准备重试",
			"op", op, "attempt", attempt+1, "wait", wait, "error", err)
		if sleepErr := c.sleep(ctx, wait); sleepErr != nil {
			return nil, err
		}
	}
}

// unwrap 拆开上游的统一信封，返回 data 字段。
//
// 上游所有接口都是 HTTP 200 + {code, msg, data}：业务失败（余额不足、
// 参数错误、订单不存在）也用 200 返回，只在 code 里体现。只看 HTTP
// 状态会把每一次业务失败都当成成功，然后拿着 data 里的 null 往下走——
// 表现为「下单成功了但订单号是空的」这类莫名其妙的现象。
func (c *HTTPClient) unwrap(op string, raw []byte) ([]byte, error) {
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		// 2xx 但报文体不是上游的信封格式。典型来源是中间网关插入的
		// HTML 页或反向代理的 JSON 错误。
		//
		// 归入「结果未知」而不是「明确拒绝」：我们无法判断上游到底
		// 执行没执行这次操作，保守方向是假设它执行了。
		return nil, fmt.Errorf("%s: %w: 响应不是上游信封格式: %v（%s）",
			op, ErrUnavailable, err, c.summarise(raw))
	}
	if env.Code != codeSuccess {
		return nil, c.classifyBusiness(op, env.Code, env.Msg)
	}
	return env.Data, nil
}

// classifyBusiness 把上游业务码映射成哨兵错误。
//
// 这是与 classify（HTTP 状态层）并列的第二层，也是**上游文档到位之后
// 才可能写对的一层**。两层的分工：
//
//   - classify 处理「报文都没正常回来」：网关 5xx、鉴权 401、限流 429。
//   - classifyBusiness 处理「报文正常回来了，但业务码说没成」。
//
// 判据与 classify 一致：**上游到底有没有执行这次操作。**
// 只有拿不准的那几个码归入「结果未知」，其余一律算明确拒绝。
func (c *HTTPClient) classifyBusiness(op string, code int, msg string) error {
	detail := fmt.Sprintf("code=%d msg=%s", code, strings.TrimSpace(msg))

	switch code {
	case codeServerError, codeOrderCreating:
		// 500 是上游内部错误，6801 是「订单生成中」。两者都可能发生在
		// 「上游已经开始建单」之后，我们拿不准它建没建成。
		//
		// 保守方向：按「结果未知」处理，让订单保持冻结。
		// 判成明确拒绝会让订单域解冻并置失败，而上游那张证书可能
		// 已经在签了——平台白付一笔钱。
		return fmt.Errorf("%s: %w: %s", op, ErrUnavailable, detail)

	case codeOrderNoInvalid:
		// 上游明确说没有这个订单。与「上游不可用」必须分开：
		// 前者说明我们记了一个上游不认的订单号，是本地数据出了问题，
		// 重试没有意义。
		return fmt.Errorf("%s: %w: %s", op, ErrOrderNotFound, detail)

	case codeBalanceNotEnough:
		// 平台在上游的预存余额不足。**不是用户的余额不足。**
		//
		// 与 ErrUnauthorized 同类：这是平台的配置/运营问题，运维要
		// 据此告警，而不是等用户来报「下单失败」。也刻意不归入
		// ErrUnavailable——重试不会让余额变多，而把它当成「结果未知」
		// 会让订单一直冻着等补偿，补偿用的还是那个不够的余额。
		return fmt.Errorf("%s: %w: 平台在上游的预存余额不足，需先充值: %s",
			op, ErrPlatformBalance, detail)

	default:
		// 其余业务码都是「上游在业务逻辑里明确拒绝了」：参数不合法
		// （6002/6006/6008）、产品不支持（6001/6004/6005）、年限不支持
		// （6003）、订单已取消（6100）、域名已验证（6303）、
		// 证书未签发（6200/6900）、联系人/企业信息字段错误（7000-7114）
		// 等。重试一百次还是同一个结果，而且上游确定没有执行操作。
		return fmt.Errorf("%s: 上游拒绝: %s", op, detail)
	}
}

// attempt 做一次尝试，返回原始报文与上游要求的等待时长。
func (c *HTTPClient) attempt(
	ctx context.Context, op, method, endpoint string, payload []byte,
) ([]byte, time.Duration, error) {
	// 单次尝试的超时取「配置值」与「调用方剩余预算」中较小者：
	// 用满配置超时可能直接超出调用方的 deadline，那样连一次完整尝试
	// 都跑不完，而错误信息会指向上游慢，掩盖了真正的原因。
	timeout := c.timeout
	if deadline, ok := ctx.Deadline(); ok {
		if remaining := time.Until(deadline); remaining < timeout {
			timeout = remaining
		}
	}
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var reader io.Reader
	if payload != nil {
		reader = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(reqCtx, method, endpoint, reader)
	if err != nil {
		return nil, 0, fmt.Errorf("构造 %s 请求失败: %w", op, err)
	}
	req.Header.Set("Accept", "application/json")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	c.authorize(req)

	start := time.Now()
	resp, err := c.http.Do(req)
	if err != nil {
		mapped := c.transportError(op, err)
		slog.WarnContext(ctx, "上游请求未完成",
			"op", op, "method", method, "duration_ms", time.Since(start).Milliseconds(),
			"error", mapped)
		return nil, 0, mapped
	}
	defer func() { _ = resp.Body.Close() }()

	// 读体限长。多读一个字节用来判断「是否超限」，否则刚好等于上限的
	// 响应与超限响应无法区分。
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, c.maxBodyBytes+1))
	if readErr != nil {
		// 请求已经发出去了，结果未知——不能归入「明确拒绝」。
		return nil, 0, fmt.Errorf("%s: %w: 读取响应失败: %v", op, ErrUnavailable, readErr)
	}
	if int64(len(raw)) > c.maxBodyBytes {
		// 超长响应直接报错，不截断使用：截断后的 JSON 必然解析失败，
		// 会得到「报文格式错误」，把排查方向引到「上游改了格式」上，
		// 而真实原因通常是中间网关插了一个 HTML 错误页。
		//
		// 归入「结果未知」而不是「明确拒绝」：拿到一个无法解释的应答时，
		// 保守方向是假设上游可能已经执行了这次操作。
		return nil, 0, fmt.Errorf("%s: %w: 响应体超过 %d 字节上限（可能是网关错误页）",
			op, ErrUnavailable, c.maxBodyBytes)
	}

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return raw, 0, nil
	}
	return nil, parseRetryAfter(resp.Header), c.classify(op, resp.StatusCode, raw)
}

// authorize 注入凭证。
func (c *HTTPClient) authorize(req *http.Request) {
	value := c.apiKey
	if c.authScheme != "-" {
		value = c.authScheme + " " + c.apiKey
	}
	req.Header.Set(c.authHeader, value)
}

// transportError 把传输层失败映射成哨兵错误。
//
// 判据只有一个：**请求字节有没有可能已经到达上游。**
func (c *HTTPClient) transportError(op string, err error) error {
	if requestNotSent(err) {
		return fmt.Errorf("%s: %w: %w: %v", op, ErrUnavailable, errRequestNotSent, err)
	}
	return fmt.Errorf("%s: %w: %v", op, ErrUnavailable, err)
}

// requestNotSent 判断失败是否发生在请求字节发出之前。
//
// 连接都没建立起来（DNS 解析失败、连接被拒、拨号超时）时，请求字节
// 就没有出去，因此重试不会让上游重复执行一次操作。
//
// 反过来，**读响应时超时不能判定为「没发出去」**：请求早已到达上游，
// 它可能已经执行完了，只是应答在路上丢了。这个区分是「有副作用的操作
// 能不能自动重试」的全部依据。
//
// 注意 context 被取消（含 deadline 到期）也走不到这里——那类失败
// 没有足够信息，一律按「可能已到达」处理。
func requestNotSent(err error) bool {
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return true
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return opErr.Op == "dial"
	}
	return false
}

// safeToRetry 判断某个操作在遇到某类失败时能否自动重试。
//
// **判据是「重复执行一次会不会造成第二次副作用」，不是「失败是不是暂时的」。**
// 这个区分很容易被忽略：一次超时看起来只是「网络不好，重试一下」，
// 但对 VerifyDomains 来说，请求可能已经到达上游并消耗掉一次验证机会；
// 对 RegenerateDcvToken 来说，旧 token 可能已经失效。
func safeToRetry(op string, err error) bool {
	// 业务性拒绝（参数不合法、产品不支持、订单不存在）重试一百次还是拒绝。
	if !errors.Is(err, ErrUnavailable) {
		return false
	}
	if idempotentOps[op] {
		return true
	}
	// 有副作用的操作只在能证明请求没发出去时才重试。
	return errors.Is(err, errRequestNotSent)
}

// classify 把一次失败应答映射成哨兵错误。
//
// **这是整个适配器最要紧的一个函数。** 映射错一个方向，代价都是钱：
// 把「结果未知」判成「明确拒绝」会让订单域解冻，而平台已经欠上游
// 一张证书的钱；反过来会让用户的钱永远冻着。
//
// 这是**第一层**，只处理「报文没能正常回来」的情况。上游把业务失败
// 放在 HTTP 200 的报文里，那一层在 classifyBusiness。
func (c *HTTPClient) classify(op string, status int, body []byte) error {
	detail := c.summarise(body)

	switch {
	case status == http.StatusNotFound:
		// **不再当成「上游订单不存在」。** 上游用业务码 6010 表达这件事，
		// 而它所有接口都返回 HTTP 200，所以这里的 404 只可能来自
		// 网关、反向代理或路径写错。
		//
		// 判成 ErrOrderNotFound 会让订单域解冻并置失败——如果 404 其实是
		// 网关抖动，那一单就被白白判死了。按「结果未知」处理最坏只是
		// 多冻一会儿，等人工或补偿收敛。
		return fmt.Errorf("%s: %w（HTTP 404，可能来自网关而非上游）%s",
			op, ErrUnavailable, detail)

	case status == http.StatusUnauthorized, status == http.StatusForbidden:
		// 凭证问题。**不归入 ErrUnavailable**：鉴权发生在业务逻辑之前，
		// 上游确定没有建单，订单域可以放心解冻。
		// 但必须能被一眼认出来——这是配置问题，不是用户的问题，
		// 靠错误信息区分，见 ErrUnauthorized 的说明。
		return fmt.Errorf("%s: %w（HTTP %d）%s", op, ErrUnauthorized, status, detail)

	case status == http.StatusTooManyRequests, status >= 500:
		return fmt.Errorf("%s: %w（HTTP %d）%s", op, ErrUnavailable, status, detail)

	case status == http.StatusConflict:
		// 上游文档里没有 409。保留这一支只作防御：万一某个中间层
		// 用它表达「请求冲突」，按「结果未知」处理是最保守的方向。
		return fmt.Errorf("%s: %w（HTTP 409）%s", op, ErrUnavailable, detail)

	default:
		// 其余 4xx：网关或代理在业务逻辑之外拒绝，重试没有意义。
		// 归入「明确拒绝」是安全的——4xx 意味着上游没有进业务逻辑，
		// 不会建单。
		return fmt.Errorf("%s: 上游拒绝（HTTP %d）%s", op, status, detail)
	}
}

// summarise 把响应体压成一行可读文本，用于错误信息与日志。
//
// 两件事必须做：
//
//   - **抹掉凭证。** 上游偶尔会把收到的凭证原样回显在错误信息里
//     （"invalid api_key: xxx"）。日志会被采集、会被转发，一次回显
//     就是一次泄露。这里是最后一道防线，不是「应该不会」的假设。
//   - **截断。** 一个 HTML 错误页可能有几十 KB，整段进日志会把关键
//     信息淹掉，也会让日志量失控。
func (c *HTTPClient) summarise(body []byte) string {
	const (
		maxLen = 512
		// minSecretLen 是「值得在文本里抹掉」的凭证长度下限。
		//
		// 低于这个长度的凭证不参与替换：一是短凭证本来就不该出现在生产
		// 配置里（上游给的是长串 API Key），二是拿一个两三字符的
		// 串去做全量替换会把无关文本一起改掉——一个值 "k" 的密钥能把
		// 响应体里的 msg 抹成 m***g，错误信息直接失去可读性，
		// 而那种「日志看起来怪怪的」比泄露更难被发现。
		minSecretLen = 8
	)

	text := string(bytes.ToValidUTF8(body, []byte("?")))
	// 只抹 API Key：它同时是调用凭证与回调验签密钥（见 HTTPOptions.APIKey），
	// 也是唯一会出现在上游错误信息里的凭证。
	if len(c.apiKey) >= minSecretLen {
		text = strings.ReplaceAll(text, c.apiKey, logger.Redact(c.apiKey))
	}
	text = strings.Join(strings.Fields(text), " ")
	if len(text) > maxLen {
		text = text[:maxLen] + "…"
	}
	if text == "" {
		return ""
	}
	return "body=" + text
}

// parseRetryAfter 解析上游要求的等待时长。
//
// 只认秒数形式。HTTP-date 形式需要与上游做时间对齐，差一分钟就会
// 算出负数或超长等待，而收益只有「精确一点」——不值得。
//
// 上限 30 秒：被上游指挥着睡很久会把调用方的请求上下文拖死，
// 而重试本来就是为了尽快收敛。
func parseRetryAfter(h http.Header) time.Duration {
	const maxWait = 30 * time.Second

	raw := strings.TrimSpace(h.Get("Retry-After"))
	if raw == "" {
		return 0
	}
	seconds, err := strconv.Atoi(raw)
	if err != nil || seconds <= 0 {
		return 0
	}
	wait := time.Duration(seconds) * time.Second
	if wait > maxWait {
		return maxWait
	}
	return wait
}

// backoff 计算第 attempt 次重试前的等待时长（attempt 从 0 开始）。
//
// 指数增长 + 抖动。抖动是必须的：上游出故障时，所有实例会在同一时刻
// 收到同一批失败，不加抖动的话它们会同时重试，形成第二波峰值，
// 把刚恢复的上游再打趴一次。
func backoff(attempt int, randFn func(n int64) int64) time.Duration {
	const (
		base    = 200 * time.Millisecond
		maxWait = 3 * time.Second
	)
	d := base << uint(attempt)
	if d > maxWait {
		d = maxWait
	}
	// 抖动区间取 [d/2, d)，即最多提前一半。
	half := int64(d / 2)
	return time.Duration(half + randFn(half))
}

// sleepContext 等待指定时长，context 结束时提前返回。
func sleepContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// ── 上游接口对照表 ─────────────────────────────────
//
// 事实来源是上游官方的 Postman 文档。出站方法在 methods.go，
// 入站回调在 notify.go。
//
//	平台方法              上游接口                                  状态
//	──────────────────────────────────────────────────────────────────────
//	Balance               GET  /finance/balance                     已实现
//	CreateOrder           POST /certificates/id/:pNo                已实现
//	OrderStatus           GET  /certificates/status/:orderNo        已实现
//	ListDomains           GET  /certificates/domains/:orderNo       已实现
//	VerifyDomains         PUT  /certificates/verifyDomains/:orderNo 阻塞（见下）
//	ResendDcvEmail        PUT  /certificates/reSendDcvEmail/:orderNo 已实现
//	RegenerateDcvToken    POST /certificates/dcv                    已实现
//	DownloadCertificate   GET  /certificates/download/:orderNo      已实现
//	Reissue               POST /certificates/reissue                已实现
//	CancelOrder           GET  /certificates/cancel/:orderNo        已实现
//	FindOrder             GET  /certificates/orders                 已实现
//	ParseNotification     ─   回调（X-Webhook-Signature）            已实现
//
// 上游还有两个接口平台用不到，不实现：证书日志查询
// （GET /certificates/ctLogs）、更新域名验证方式（PUT /certificates/dcv）。
// 后者的请求体格式记在 docs/07 备查。
//
// ── 还没闭合的缺口 ─────────────────────────────────
//
// 这几件事在文档到位后暴露出来，都**不能靠猜**补上：
//
//  1. **提交域名验证（VerifyDomains）的请求体在文档里是缺失的。**
//     文档给了路径与响应，但没有参数表，Postman 集合里这个条目
//     连 originalRequest 都没有。所以这个方法直接返回 ErrNotSupported，
//     见 methods.go 里的说明——发一个「看起来成功但什么都没做」的
//     请求比报错更糟。
//
//  2. **notifyUrl 还没有接线。** 订单域的 submit 传的是空串，而上游
//     只在收到 notifyUrl 时才会推送。也就是说真实上游环境下回调
//     永远不会到达，状态同步只能靠轮询（OrderStatus 在生产代码里
//     目前一次都没被调用）。这块与上游报文无关，是平台侧的接线工作。
//
//  3. **上游的 dns（响应侧 CNAME_CSR_HASH）平台侧没有对应取值。**
//     平台只有 dns_txt 与 dns_cname，而这两个只对 certum 品牌开放。
//     非 certum 品牌要提交 DNS 验证时，当前产品配置无法表达——
//     见 toWireDcvMethod 的说明。
//
//  4. **文件验证的内容字段不在域名列表接口里。** 上游只返回
//     fileDcvPath（文件名固定为 gsdv.txt）与 hashValue，没有单独的
//     文件内容字段。适配器**刻意不填** FileContent：填 hashValue 是
//     一个把握较高但未经验证的推断，猜错的后果是用户拿着错误的
//     内容去配、验证不通过且难以排查；留空的后果只是文件验证不出现在
//     可用方式里，用户改用 DNS 或邮件。联调时确认后补上。
//
//  5. **上游没有给限流约定。** 文档里没有 QPS 上限、没有 429 的说明。
//     如果联调时遇到限流，除了本文件的退避重试，调用侧还要做节流。
