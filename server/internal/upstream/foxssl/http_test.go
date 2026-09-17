package foxssl

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// ── 测试脚手架 ────────────────────────────────────

// callLog 记录假上游收到的每一次请求。
//
// 记请求头而不是只记次数：脱敏、鉴权头这类断言要看的是「发出去的到底是什么」。
type callLog struct {
	mu      sync.Mutex
	headers []http.Header
	bodies  [][]byte
	paths   []string
}

func (l *callLog) record(r *http.Request, body []byte) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.headers = append(l.headers, r.Header.Clone())
	l.bodies = append(l.bodies, body)
	l.paths = append(l.paths, r.URL.Path)
}

func (l *callLog) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.headers)
}

func (l *callLog) first() (http.Header, []byte) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.headers) == 0 {
		return nil, nil
	}
	return l.headers[0], l.bodies[0]
}

// newHTTPTestClient 起一个假上游，返回指向它的客户端与请求记录。
//
// 退避与随机数都替换成确定实现：不替换的话，一条「重试了两次」的用例
// 要真的等 600ms，几十条用例累计起来慢到没人愿意跑，最后就是整个测试
// 文件被跳过。
func newHTTPTestClient(
	t *testing.T, opts HTTPOptions, handler http.HandlerFunc,
) (*HTTPClient, *callLog) {
	t.Helper()

	log := &callLog{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		log.record(r, body)
		handler(w, r)
	}))
	t.Cleanup(srv.Close)

	opts.BaseURL = srv.URL
	if opts.APIKey == "" {
		opts.APIKey = testAPIKey
	}
	if opts.WebhookSecret == "" {
		opts.WebhookSecret = testWebhookSecret
	}
	if opts.Timeout == 0 {
		opts.Timeout = 2 * time.Second
	}
	// 显式给 2 次重试，让「重试到上限共 3 次」这类断言有确定的基准。
	// 负数留给「不重试」的用例，所以只在零值时填。
	if opts.MaxRetries == 0 {
		opts.MaxRetries = 2
	}
	opts.Sleep = func(context.Context, time.Duration) error { return nil }
	opts.Rand = func(int64) int64 { return 0 }

	c, err := NewHTTPClient(opts)
	if err != nil {
		t.Fatalf("构造客户端失败: %v", err)
	}
	return c, log
}

// alwaysStatus 返回一个固定状态码与响应体的假上游。
func alwaysStatus(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}

// ── 构造与配置 ────────────────────────────────────

// TestNewHTTPClientRejectsMissingCredentials 验证凭证缺失时拒绝构造。
//
// 与 Mock 上游同一个理由：「忘记配置」静默变成「用一个空凭证调用上游」时，
// 上游返回的错误信息通常与凭证无关，排查方向会跑偏很远。
func TestNewHTTPClientRejectsMissingCredentials(t *testing.T) {
	cases := []struct {
		name string
		opts HTTPOptions
	}{
		{"地址为空", HTTPOptions{APIKey: "k", WebhookSecret: "s"}},
		{"地址没有协议前缀", HTTPOptions{BaseURL: "api.example.com", APIKey: "k", WebhookSecret: "s"}},
		{"API Key 为空", HTTPOptions{BaseURL: "https://api.example.com", WebhookSecret: "s"}},
		{"API Key 只有空白", HTTPOptions{BaseURL: "https://api.example.com", APIKey: "  ", WebhookSecret: "s"}},
		{"回调密钥为空", HTTPOptions{BaseURL: "https://api.example.com", APIKey: "k"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := NewHTTPClient(c.opts); err == nil {
				t.Fatal("应当拒绝构造")
			}
		})
	}
}

// TestHTTPClientDefaults 验证零值配置被填成默认值而不是变成「无超时」。
//
// 无超时的 HTTP 客户端是生产事故的常见来源：一个挂住的上游会把
// 调用方的连接和 goroutine 一起吃掉，而且不会有任何报错。
func TestHTTPClientDefaults(t *testing.T) {
	c, err := NewHTTPClient(HTTPOptions{
		BaseURL:       "https://api.example.com/",
		APIKey:        "k",
		WebhookSecret: "s",
	})
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}
	if c.timeout != defaultTimeout {
		t.Errorf("超时应取默认值 %v，实际 %v", defaultTimeout, c.timeout)
	}
	if c.maxBodyBytes != defaultMaxBodyBytes {
		t.Errorf("响应体上限应取默认值 %d，实际 %d", defaultMaxBodyBytes, c.maxBodyBytes)
	}
	// 重试次数的零值必须取默认值。若零值表示「不重试」，一个只填了
	// 地址与凭证的配置会静默失去全部重试能力，而上游偶发 503 时
	// 表现只是「时不时失败一下」，看起来像上游的问题。
	if c.maxRetries != defaultMaxRetries {
		t.Errorf("重试次数应取默认值 %d，实际 %d", defaultMaxRetries, c.maxRetries)
	}
	// 尾斜杠必须去掉，否则会拼出 //certificates 这种路径，
	// 有些网关会因此返回 404，而排查方向会跑到「路径写错了」上。
	if c.baseURL != "https://api.example.com" {
		t.Errorf("根地址应去掉尾斜杠，实际 %q", c.baseURL)
	}
}

// TestAuthorizeHeader 验证凭证注入。
//
// 头名与格式是**待文档确认**的默认约定，这条测试钉住的是「改起来只有一处」：
// 覆盖 HTTPOptions 就能改，不需要动 do 里的任何逻辑。
func TestAuthorizeHeader(t *testing.T) {
	c, log := newHTTPTestClient(t, HTTPOptions{}, alwaysStatus(200, `{}`))
	if _, err := c.do(context.Background(), opOrderStatus, http.MethodGet, "/x", nil); err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	header, _ := log.first()
	if got := header.Get("Authorization"); got != "Bearer "+testAPIKey {
		t.Errorf("默认应为 Bearer 前缀，实际 %q", got)
	}

	c2, log2 := newHTTPTestClient(t, HTTPOptions{AuthHeader: "X-Api-Key", AuthScheme: "-"},
		alwaysStatus(200, `{}`))
	if _, err := c2.do(context.Background(), opOrderStatus, http.MethodGet, "/x", nil); err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	header2, _ := log2.first()
	if got := header2.Get("X-Api-Key"); got != testAPIKey {
		t.Errorf("裸放模式下应原样传凭证，实际 %q", got)
	}
}

// ── 错误分类：适配器最要紧的一组断言 ────────────────

// TestClassifyHTTPStatus 验证状态码到哨兵错误的映射。
//
// 订单域用 errors.Is(err, ErrUnavailable) 区分「上游明确拒绝」与
// 「结果未知」，这个判据决定了钱是退给用户还是继续冻着
// （见 order.isUnknownResult）。所以这里逐条钉住每个状态码落在哪一边。
func TestClassifyHTTPStatus(t *testing.T) {
	client, err := NewHTTPClient(HTTPOptions{
		BaseURL: "https://api.example.com", APIKey: "k", WebhookSecret: "s",
	})
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}

	cases := []struct {
		status int
		// unknown 为 true 表示订单域会判定「结果未知」并保持冻结。
		unknown bool
		// sentinel 非空时要求 errors.Is 命中。
		sentinel error
		reason   string
	}{
		{200, false, nil, ""}, // 不走 classify，仅作对照
		{400, false, nil, "参数不合法：上游在业务逻辑里明确拒绝，没建单"},
		{422, false, nil, "同上，校验失败"},
		{401, false, ErrUnauthorized, "鉴权在业务逻辑之前，确定没建单"},
		{403, false, ErrUnauthorized, "同上"},
		{404, false, ErrOrderNotFound, "上游不认这个订单号，重试无用"},
		{409, true, nil, "语义待文档确认，保守方向是当成未知"},
		{429, true, nil, "限流，请求可能已被处理"},
		{500, true, nil, "上游内部错误，可能已经执行了"},
		{502, true, nil, "网关错误，可能已经执行了"},
		{503, true, nil, "不可用，可能已经执行了"},
	}
	for _, tc := range cases {
		if tc.status == 200 {
			continue
		}
		t.Run(fmt.Sprintf("HTTP_%d", tc.status), func(t *testing.T) {
			err := client.classify("TestOp", tc.status, []byte(`{"msg":"x"}`))
			if err == nil {
				t.Fatal("非 2xx 应当返回错误")
			}
			gotUnknown := errors.Is(err, ErrUnavailable)
			if gotUnknown != tc.unknown {
				t.Errorf("HTTP %d 的「结果未知」判定应为 %v，实际 %v（%s）\n错误：%v",
					tc.status, tc.unknown, gotUnknown, tc.reason, err)
			}
			if tc.sentinel != nil && !errors.Is(err, tc.sentinel) {
				t.Errorf("HTTP %d 应命中 %v，实际 %v", tc.status, tc.sentinel, err)
			}
			// 「结果未知」与「上游拒绝凭证」必须互斥：
			// 前者让订单冻着等补偿，后者必须让订单解冻。
			if tc.sentinel == ErrUnauthorized && gotUnknown {
				t.Error("凭证问题不能同时被当成结果未知，否则每一张订单都会冻着等一个坏凭证")
			}
		})
	}
}

// TestCredentialNeverLeaksIntoError 验证上游回显凭证时不会泄露到错误信息。
//
// 上游偶尔会把收到的凭证原样回显在错误里（"invalid api_key: xxx"），
// 而错误信息会进日志、日志会被采集和转发。一次回显就是一次泄露。
func TestCredentialNeverLeaksIntoError(t *testing.T) {
	echo := `{"error":"invalid api_key: ` + testAPIKey + `","secret":"` + testWebhookSecret + `"}`
	c, _ := newHTTPTestClient(t, HTTPOptions{}, alwaysStatus(401, echo))

	_, err := c.do(context.Background(), opBalance, http.MethodGet, "/x", nil)
	if err == nil {
		t.Fatal("401 应当返回错误")
	}
	msg := err.Error()
	if strings.Contains(msg, testAPIKey) {
		t.Errorf("错误信息泄露了 API Key：%s", msg)
	}
	if strings.Contains(msg, testWebhookSecret) {
		t.Errorf("错误信息泄露了回调密钥：%s", msg)
	}
	// 反过来：抹掉之后仍然要留下可排查的线索，不能整个吃掉。
	if !strings.Contains(msg, "invalid api_key") {
		t.Errorf("脱敏不应把上游的错误描述一起吃掉：%s", msg)
	}
}

// TestSummariseTruncatesAndFlattens 验证响应体摘要不会把日志淹掉。
func TestSummariseTruncatesAndFlattens(t *testing.T) {
	c, err := NewHTTPClient(HTTPOptions{
		BaseURL: "https://api.example.com", APIKey: "k", WebhookSecret: "s",
	})
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}

	huge := "<html>\n  <body>" + strings.Repeat("x", 4096) + "</body>\n</html>"
	got := c.summarise([]byte(huge))
	if len(got) > 600 {
		t.Errorf("摘要应被截断，实际长度 %d", len(got))
	}
	if strings.Contains(got, "\n") {
		t.Error("摘要应压成一行")
	}
	if !strings.Contains(got, "…") {
		t.Error("被截断的摘要应有省略标记，便于判断内容不完整")
	}
}

// TestSummariseIgnoresShortSecrets 验证过短的凭证不参与替换。
//
// 拿一个两三字符的串去做全量替换会把无关文本一起改掉：一个值为 "k"
// 的密钥能把响应体里的 msg 抹成 m***g，错误信息直接失去可读性。
// 而「日志看起来怪怪的」比泄露更难被发现——没人会为此报障。
func TestSummariseIgnoresShortSecrets(t *testing.T) {
	c, err := NewHTTPClient(HTTPOptions{
		BaseURL: "https://api.example.com", APIKey: "k", WebhookSecret: "s",
	})
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}
	got := c.summarise([]byte(`{"msg":"产品不支持","code":"KEY_MISMATCH"}`))
	if strings.Contains(got, "***") {
		t.Errorf("短凭证不应参与替换，否则会误伤无关文本，实际 %q", got)
	}
	if !strings.Contains(got, "产品不支持") {
		t.Errorf("响应体内容应完整保留，实际 %q", got)
	}
}

// ── 重试策略：按副作用而不是按「失败是否暂时」 ──────

// TestWriteOpsAreNotRetriedOnTimeout 验证有副作用的操作不会因为超时而重试。
//
// 一次超时看起来只是「网络不好，重试一下」，但请求可能已经到达上游并
// 执行完了。对 VerifyDomains 来说重试会消耗掉第二次验证机会，
// 对 RegenerateDcvToken 来说会让刚刚生效的新 token 又失效一次。
func TestWriteOpsAreNotRetriedOnTimeout(t *testing.T) {
	slow := func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(150 * time.Millisecond)
		w.WriteHeader(200)
	}
	c, log := newHTTPTestClient(t, HTTPOptions{Timeout: 30 * time.Millisecond}, slow)

	_, err := c.do(context.Background(), opVerifyDomains, http.MethodPost, "/x", map[string]any{"a": 1})
	if err == nil {
		t.Fatal("超时应当返回错误")
	}
	if !errors.Is(err, ErrUnavailable) {
		t.Errorf("超时属于结果未知，应命中 ErrUnavailable，实际 %v", err)
	}
	if got := log.count(); got != 1 {
		t.Errorf("有副作用的操作在超时后不应重试，实际发出 %d 次请求", got)
	}
}

// TestReadOpsAreRetriedOnTimeout 验证只读操作在超时后会重试。
//
// 与上一条互为对照：如果两条都写「不重试」，上一条就没有鉴别力。
func TestReadOpsAreRetriedOnTimeout(t *testing.T) {
	slow := func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(150 * time.Millisecond)
		w.WriteHeader(200)
	}
	c, log := newHTTPTestClient(t, HTTPOptions{Timeout: 30 * time.Millisecond}, slow)

	_, err := c.do(context.Background(), opOrderStatus, http.MethodGet, "/x", nil)
	if err == nil {
		t.Fatal("超时应当返回错误")
	}
	// MaxRetries=2，加上首次共 3 次。
	if got := log.count(); got != 3 {
		t.Errorf("只读操作应重试到上限（共 3 次），实际 %d 次", got)
	}
}

// TestCreateOrderIsRetried 验证下单会重试。
//
// 依据是包注释里的约定 1：上游按 MerchantOrderNo 去重，重复提交返回
// 同一个上游订单。少了这条性质，重试就真的会下第二单。
func TestCreateOrderIsRetried(t *testing.T) {
	c, log := newHTTPTestClient(t, HTTPOptions{}, alwaysStatus(503, `{}`))

	if _, err := c.do(context.Background(), opCreateOrder, http.MethodPost, "/x", map[string]any{}); err == nil {
		t.Fatal("503 应当返回错误")
	}
	if got := log.count(); got != 3 {
		t.Errorf("下单应重试到上限（共 3 次），实际 %d 次", got)
	}
}

// TestWriteOpsRetriedOnlyWhenRequestNotSent 验证「请求没发出去」时可重试。
//
// 连接都没建立起来时，请求字节没有出去，重试不会让上游重复执行一次
// 操作——所以连有副作用的操作也能安全重试。这个区分只有传输层做得出来。
func TestWriteOpsRetriedOnlyWhenRequestNotSent(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	dialFailure := &http.Client{
		Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			mu.Lock()
			calls++
			mu.Unlock()
			return nil, &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
		}),
	}
	c, err := NewHTTPClient(HTTPOptions{
		BaseURL: "https://api.example.com", APIKey: "k", WebhookSecret: "s",
		HTTPClient: dialFailure,
		Sleep:      func(context.Context, time.Duration) error { return nil },
		Rand:       func(int64) int64 { return 0 },
		MaxRetries: 2,
	})
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}

	_, err = c.do(context.Background(), opCancelOrder, http.MethodPost, "/x", nil)
	if err == nil {
		t.Fatal("拨号失败应当返回错误")
	}
	if !errors.Is(err, errRequestNotSent) {
		t.Errorf("应标记为「请求未发出」，实际 %v", err)
	}
	if !errors.Is(err, ErrUnavailable) {
		t.Errorf("同时仍是「结果未知」——调用方可能重试，实际 %v", err)
	}
	if calls != 3 {
		t.Errorf("请求未发出时应重试到上限（共 3 次），实际 %d 次", calls)
	}

	// 对照：同样是写操作，若失败发生在请求发出之后，就不能重试。
	mu.Lock()
	calls = 0
	mu.Unlock()
	afterSend := &http.Client{
		Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			mu.Lock()
			calls++
			mu.Unlock()
			return nil, &net.OpError{Op: "read", Net: "tcp", Err: errors.New("connection reset")}
		}),
	}
	c2, err := NewHTTPClient(HTTPOptions{
		BaseURL: "https://api.example.com", APIKey: "k", WebhookSecret: "s",
		HTTPClient: afterSend,
		Sleep:      func(context.Context, time.Duration) error { return nil },
		Rand:       func(int64) int64 { return 0 },
		MaxRetries: 2,
	})
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}
	if _, err := c2.do(context.Background(), opCancelOrder, http.MethodPost, "/x", nil); err == nil {
		t.Fatal("读响应失败应当返回错误")
	}
	if calls != 1 {
		t.Errorf("请求已发出后的失败不应重试，实际 %d 次", calls)
	}
}

// roundTripFunc 让测试能注入自定义传输。
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// TestNoRetryAfterContextDone 验证调用方的 context 结束后不再重试。
//
// 重试必然立刻失败，只是把同一个错误重复一遍、多打三条日志。
// 但错误本身仍要归入「结果未知」——请求可能已经到达上游，
// 所以订单域会保持冻结而不是解冻。
func TestNoRetryAfterContextDone(t *testing.T) {
	slow := func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.WriteHeader(200)
	}
	c, log := newHTTPTestClient(t, HTTPOptions{Timeout: 500 * time.Millisecond}, slow)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()

	_, err := c.do(ctx, opOrderStatus, http.MethodGet, "/x", nil)
	if err == nil {
		t.Fatal("context 到期应当返回错误")
	}
	if !errors.Is(err, ErrUnavailable) {
		t.Errorf("context 到期时结果未知，应命中 ErrUnavailable，实际 %v", err)
	}
	if got := log.count(); got != 1 {
		t.Errorf("context 已结束时不应重试，实际发出 %d 次请求", got)
	}
}

// TestNegativeMaxRetriesDisablesRetry 验证负数表示一次都不重试。
//
// 留出这个取值是为了让调用点能自己接管失败处置（例如将来要在调用侧做
// 全局限流时，重试必须由限流器统一排期，不能被适配器悄悄做掉）。
func TestNegativeMaxRetriesDisablesRetry(t *testing.T) {
	c, log := newHTTPTestClient(t, HTTPOptions{MaxRetries: -1}, alwaysStatus(503, `{}`))

	if _, err := c.do(context.Background(), opOrderStatus, http.MethodGet, "/x", nil); err == nil {
		t.Fatal("503 应当返回错误")
	}
	if got := log.count(); got != 1 {
		t.Errorf("负数应表示不重试，实际发出 %d 次请求", got)
	}
}

// TestBusinessRejectionIsNotRetried 验证业务性拒绝不重试。
//
// 重试一百次也还是拒绝，只会把日志和上游的限流额度一起打满。
func TestBusinessRejectionIsNotRetried(t *testing.T) {
	c, log := newHTTPTestClient(t, HTTPOptions{}, alwaysStatus(400, `{"msg":"产品不支持"}`))

	if _, err := c.do(context.Background(), opCreateOrder, http.MethodPost, "/x", nil); err == nil {
		t.Fatal("400 应当返回错误")
	}
	if got := log.count(); got != 1 {
		t.Errorf("业务性拒绝不应重试，实际发出 %d 次请求", got)
	}
}

// ── 响应处理 ──────────────────────────────────────

// TestOversizedResponseRejected 验证超长响应被拒绝而不是被截断使用。
//
// 截断后的 JSON 必然解析失败，会得到「报文格式错误」，把排查方向引到
// 「上游改了格式」上，而真实原因通常是中间网关插了一个 HTML 错误页。
func TestOversizedResponseRejected(t *testing.T) {
	c, _ := newHTTPTestClient(t, HTTPOptions{MaxBodyBytes: 128},
		alwaysStatus(200, strings.Repeat("x", 4096)))

	_, err := c.do(context.Background(), opListDomains, http.MethodGet, "/x", nil)
	if err == nil {
		t.Fatal("超长响应应当报错")
	}
	if !errors.Is(err, ErrUnavailable) {
		t.Errorf("无法解释的应答按「结果未知」处理，应命中 ErrUnavailable，实际 %v", err)
	}
	if !strings.Contains(err.Error(), "上限") {
		t.Errorf("错误信息应指出是响应体超限，实际 %v", err)
	}
}

// TestExactLimitResponseAccepted 验证刚好等于上限的响应不会被误判。
//
// 读体时多读一个字节才能区分「刚好等于上限」与「超限」；
// 少读一个字节会让正常响应在边界上随机失败。
func TestExactLimitResponseAccepted(t *testing.T) {
	const size = 128
	c, _ := newHTTPTestClient(t, HTTPOptions{MaxBodyBytes: size},
		alwaysStatus(200, strings.Repeat("x", size)))

	raw, err := c.do(context.Background(), opListDomains, http.MethodGet, "/x", nil)
	if err != nil {
		t.Fatalf("刚好等于上限的响应应被接受，实际 %v", err)
	}
	if len(raw) != size {
		t.Errorf("响应体应完整返回，实际 %d 字节", len(raw))
	}
}

// TestParseRetryAfter 验证上游要求的等待时长被解析且有上限。
func TestParseRetryAfter(t *testing.T) {
	cases := []struct {
		raw  string
		want time.Duration
	}{
		{"", 0},
		{"3", 3 * time.Second},
		{"0", 0},
		{"-5", 0},
		{"abc", 0},
		// HTTP-date 形式不支持，返回 0 让退避算法自己决定。
		{"Wed, 21 Oct 2026 07:28:00 GMT", 0},
		// 上限 30 秒：被上游指挥着睡 10 分钟会把调用方的上下文拖死。
		{"600", 30 * time.Second},
	}
	for _, c := range cases {
		h := http.Header{}
		if c.raw != "" {
			h.Set("Retry-After", c.raw)
		}
		if got := parseRetryAfter(h); got != c.want {
			t.Errorf("Retry-After=%q 应得到 %v，实际 %v", c.raw, c.want, got)
		}
	}
}

// TestBackoffGrowsWithJitter 验证退避时长递增且有抖动。
//
// 抖动是必须的：上游出故障时所有实例会在同一时刻收到同一批失败，
// 不加抖动它们会同时重试，形成第二波峰值，把刚恢复的上游再打趴一次。
func TestBackoffGrowsWithJitter(t *testing.T) {
	noJitter := func(int64) int64 { return 0 }
	first := backoff(0, noJitter)
	second := backoff(1, noJitter)
	third := backoff(2, noJitter)
	if first >= second || second >= third {
		t.Errorf("退避应递增，实际 %v / %v / %v", first, second, third)
	}
	// 抖动上限：同一档位取到的最大等待不超过基准值。
	full := backoff(0, func(n int64) int64 { return n - 1 })
	if full >= first*2 {
		t.Errorf("抖动不应让等待翻倍，实际 %v（基准 %v）", full, first)
	}
	// 长重试不会无限增长。
	if got := backoff(20, noJitter); got > 3*time.Second {
		t.Errorf("退避应有上限 3s，实际 %v", got)
	}
}

// TestDoReturnsErrorOnInvalidBody 验证请求体编码失败被识别为本地 bug。
//
// 编码失败说明请求结构写错了，既不该重试（重试还是失败），
// 也不能算「结果未知」（请求根本没构造出来）。
func TestDoReturnsErrorOnInvalidBody(t *testing.T) {
	c, log := newHTTPTestClient(t, HTTPOptions{}, alwaysStatus(200, `{}`))

	// channel 无法被 JSON 编码。
	_, err := c.do(context.Background(), opCreateOrder, http.MethodPost, "/x", map[string]any{"ch": make(chan int)})
	if err == nil {
		t.Fatal("不可编码的请求体应当报错")
	}
	if errors.Is(err, ErrUnavailable) {
		t.Errorf("本地编码失败不是「结果未知」，实际 %v", err)
	}
	if got := log.count(); got != 0 {
		t.Errorf("编码失败不应发出任何请求，实际发出 %d 次", got)
	}
}
