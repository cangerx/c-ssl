package foxssl

// 本文件钉住**报文层**：上游文档说了什么，这里就断言什么。
//
// 与 http_test.go 的分工：那里测的是「传输层在各种失败下怎么反应」，
// 与上游报文格式无关；这里测的是「字段名、路径、取值映射对不对」，
// 全部由上游文档决定。
//
// 这一层写错的代价与传输层不同：传输层的 bug 是偶发失败，报文层的 bug
// 是**每次调用都失败**，而且错误信息通常指向「参数不合法」这类
// 看不出根因的地方。所以每条断言都写清它防的是哪个具体后果。

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

// ── 下订单 ────────────────────────────────────────

// TestCreateOrderWireRequest 验证下单请求体与路径的映射。
func TestCreateOrderWireRequest(t *testing.T) {
	// certID 故意用**奇数**：文档示例值 142932802604630016 正好是 32 的
	// 倍数，用 float64 解析也能精确表示，测不出精度问题。奇数一定
	// 落在两个可表示值之间，float64 会把它舍入掉。
	const oddCertID = "142932802604630017"

	c, log := newHTTPTestClient(t, HTTPOptions{}, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		_, _ = io.WriteString(w, okEnvelope(
			`{"certID":142932802604630017,"cost":123456,"orderNo":"20201232020789"}`))
	})

	resp, err := c.CreateOrder(context.Background(), CreateOrderRequest{
		MerchantOrderNo:   "CS20260917120000A7K3M9",
		UpstreamProductID: 197,
		Years:             2,
		DcvMethod:         "dns_txt",
		Domains:           []string{"example.com", "www.example.com", "api.example.com"},
		CSR:               "-----BEGIN CERTIFICATE REQUEST-----",
		Contact:           Contact{Name: "star", Email: "a@b.com", Phone: "13800000000", Title: "IT"},
	})
	if err != nil {
		t.Fatalf("下单失败: %v", err)
	}

	method, path, _, body := log.call(0)
	if method != http.MethodPost {
		t.Errorf("下单应为 POST，实际 %s", method)
	}
	// 上游产品编号是**路径的一部分**（/certificates/id/:pNo），不是请求体字段。
	if path != "/certificates/id/197" {
		t.Errorf("路径应为 /certificates/id/197，实际 %s", path)
	}

	var sent map[string]any
	if err := json.Unmarshal(body, &sent); err != nil {
		t.Fatalf("请求体不是 JSON: %v", err)
	}

	// 主域名不进 domainNames：上游从 CSR 里取主域名，重复传会被当成
	// 重复的 SAN 而报 6002。
	if got := sent["domainNames"]; got != "www.example.com,api.example.com" {
		t.Errorf("domainNames 应为「不含主域名的逗号串」，实际 %v", got)
	}
	// 必须小写：上游在「更新验证方式」接口的说明里明确要求，而大小写
	// 错误换回的是 6004「dcvMethod 不支持」，看起来像产品不支持。
	if got := sent["dcvMethod"]; got != "dns_txt" {
		t.Errorf("dcvMethod 应为小写 dns_txt，实际 %v", got)
	}
	if got := sent["year"]; got != float64(2) {
		t.Errorf("year 应为 2，实际 %v", got)
	}
	// 平台订单号**不应**出现在请求体里。上游没有这个参数，传了也不会
	// 被当成幂等键——而「看起来传了」会让人以为有幂等保护。
	if _, ok := sent["merchantOrderNo"]; ok {
		t.Error("请求体里不该有商户订单号：上游不接受它，传了只会让人以为有幂等保护")
	}
	// 联系人字段名是上游的小写拼法（lastname / firstname）。
	contact, ok := sent["contactInfo"].(map[string]any)
	if !ok {
		t.Fatalf("缺少 contactInfo：%v", sent)
	}
	if contact["email"] != "a@b.com" || contact["telephone"] != "13800000000" {
		t.Errorf("联系人映射错了：%v", contact)
	}
	if _, ok := contact["lastname"]; !ok {
		t.Errorf("联系人字段名应为上游的 lastname，实际 %v", contact)
	}
	// OV / EV 才需要 orgInfo，这里没传企业信息，就不该出现这个字段。
	if _, ok := sent["orgInfo"]; ok {
		t.Error("未提供企业信息时不该出现 orgInfo：DV 证书带上它会报 7100 之外的多余错误")
	}

	// certID 超出 float64 的精确整数范围，必须用 int64 解析。
	if resp.CertID != oddCertID {
		t.Errorf("certID 丢了精度：期望 %s，实际 %s", oddCertID, resp.CertID)
	}
	if int64(resp.Cost) != 123456 {
		t.Errorf("cost 应为 123456 分，实际 %d", int64(resp.Cost))
	}
	if resp.UpstreamOrderNo != "20201232020789" {
		t.Errorf("orderNo 映射错了：%s", resp.UpstreamOrderNo)
	}
}

// TestCreateOrderRejectsUnmappableDcvMethod 验证平台取值翻不成上游取值时直接拒绝。
//
// 上游的 dcvMethod 取值只有五个（file / dns / email / dns_txt / dns_cname），
// 而平台的 http_file 与 https_file 都要映射成 file。遇到平台侧新增了
// 取值而这里没跟上时，**必须在本地拒绝**：发出去只会换回 6004，
// 那个错误看起来像「产品不支持这个方式」，会把人引到产品配置上去查。
func TestCreateOrderRejectsUnmappableDcvMethod(t *testing.T) {
	c, log := newHTTPTestClient(t, HTTPOptions{}, alwaysStatus(200, okEnvelope("")))

	_, err := c.CreateOrder(context.Background(), CreateOrderRequest{
		UpstreamProductID: 197,
		Domains:           []string{"example.com"},
		DcvMethod:         "dns", // 平台侧没有这个取值
	})
	if !errors.Is(err, ErrNotSupported) {
		t.Fatalf("应返回 ErrNotSupported，实际 %v", err)
	}
	if log.count() != 0 {
		t.Error("无法翻译的验证方式不该发出请求：上游会返回一个指向产品配置的错误")
	}
}

// TestCreateOrderMissingOrderNoIsUnknown 验证「成功但没给订单号」按结果未知处理。
//
// 上游说成功却没给 orderNo，平台就永远查不到这一单。判成「明确拒绝」
// 会让订单域解冻并置失败，而证书很可能已经建了——平台白付一张证书。
func TestCreateOrderMissingOrderNoIsUnknown(t *testing.T) {
	c, _ := newHTTPTestClient(t, HTTPOptions{}, alwaysStatus(200,
		okEnvelope(`{"certID":1,"cost":1,"orderNo":""}`)))

	_, err := c.CreateOrder(context.Background(), CreateOrderRequest{
		UpstreamProductID: 197, Domains: []string{"example.com"}, DcvMethod: "dns_txt",
	})
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("应归入结果未知（ErrUnavailable），实际 %v", err)
	}
}

// ── 域名列表 ──────────────────────────────────────

// TestListDomainsHandlesBothDnsNamesShapes 验证 dnsNames 的两种形态都能解析。
//
// 上游对常规域名返回**数组**，对 IP 返回**裸字符串**（文档示例
// "140.12.56.8"，说明里写着「ip 单独」）。用 []string 直接解析的话，
// IP 订单会在解码这一步整体失败——而上游返回的是 HTTP 200 + code 200，
// 所以错误表现成「报文格式错误」，把排查方向引到「上游改了格式」上，
// 真实原因却是「这一单是 IP 证书」。
func TestListDomainsHandlesBothDnsNamesShapes(t *testing.T) {
	payload := `{"domainList":[
		{"dnsNames":["1.test.com","2.test.com"],"domainID":0,"email":"admin@test.com",
		 "status":"2002","dcvMethod":"CNAME_CSR_HASH","recordType":"TXT","hostRecord":"@",
		 "hashValue":"tok1","fileDcvPath":"http://{FQDN}/.well-known/pki-validation/gsdv.txt",
		 "uniqueValue":""},
		{"dnsNames":"140.12.56.8","domainID":0,"email":"","status":"2001",
		 "dcvMethod":"HTTP_CSR_HASH","recordType":"TXT","hostRecord":"@","hashValue":"tok2",
		 "fileDcvPath":"http://{FQDN}/.well-known/pki-validation/gsdv.txt","uniqueValue":""}
	]}`
	c, _ := newHTTPTestClient(t, HTTPOptions{}, alwaysStatus(200, okEnvelope(payload)))

	got, err := c.ListDomains(context.Background(), "20201232020789")
	if err != nil {
		t.Fatalf("拉取域名失败: %v", err)
	}
	// 上游按「相同根域名」分组返回，平台是逐域名一条记录，所以要展开。
	if len(got.Domains) != 3 {
		t.Fatalf("应展开成 3 个域名，实际 %d：%+v", len(got.Domains), got.Domains)
	}
	if got.Domains[0].Domain != "1.test.com" || got.Domains[1].Domain != "2.test.com" {
		t.Errorf("数组形态的域名没展开对：%v", []string{got.Domains[0].Domain, got.Domains[1].Domain})
	}
	if got.Domains[2].Domain != "140.12.56.8" {
		t.Errorf("裸字符串形态的域名没解析出来：%q", got.Domains[2].Domain)
	}

	// 验证方式保持**上游原文**（响应侧是大写常量名），翻译成平台取值
	// 是订单域的职责，见 order.mapUpstreamDcvMethod。
	if got.Domains[0].Method != "CNAME_CSR_HASH" {
		t.Errorf("Method 应保持上游原文，实际 %q", got.Domains[0].Method)
	}
	// 分组内共享的材料要跟着展开到每个域名上。
	if got.Domains[1].DnsRecordValue != "tok1" {
		t.Errorf("同组域名应共享记录值，实际 %q", got.Domains[1].DnsRecordValue)
	}
	// 文件路径里的 {FQDN} 占位符**不能在适配层替换**：适配器不知道
	// 每个域名，替换发生在订单域。
	if !strings.Contains(got.Domains[0].FileDcvPath, PlaceholderFQDN) {
		t.Errorf("适配层不该替换 {FQDN}：%q", got.Domains[0].FileDcvPath)
	}

	if len(got.Domains[0].EmailAddresses) != 1 ||
		got.Domains[0].EmailAddresses[0] != "admin@test.com" {
		t.Errorf("邮件地址解析错了：%v", got.Domains[0].EmailAddresses)
	}
	// 空 email 不该产出一个「有一个空字符串」的列表——那会让
	// AvailableMethods 以为这个域名支持邮件验证。
	if len(got.Domains[2].EmailAddresses) != 0 {
		t.Errorf("空 email 应解析成空列表，实际 %v", got.Domains[2].EmailAddresses)
	}
}

// ── 订单状态与证书 ────────────────────────────────

// TestOrderStatusWireFields 验证订单状态接口的七个字段被如实透传。
func TestOrderStatusWireFields(t *testing.T) {
	payload := `{"status":{"certPrepareStatus":"2002","certStatus":"3004","dcvStatus":"2002",
		"evValidationStatus":"2000","isReSignOrder":"N","orderStatus":"1002",
		"ovValidationStatus":"2000"}}`
	c, _ := newHTTPTestClient(t, HTTPOptions{}, alwaysStatus(200, okEnvelope(payload)))

	got, err := c.OrderStatus(context.Background(), "20201232020789")
	if err != nil {
		t.Fatalf("查询状态失败: %v", err)
	}
	// 四个状态字段原样对应上游文档，平台不做合并。
	if got.OrderStatus != "1002" {
		t.Errorf("orderStatus 应为 1002，实际 %q", got.OrderStatus)
	}
	if got.CertStatus != "3004" {
		t.Errorf("certStatus 应为 3004，实际 %q", got.CertStatus)
	}
	if got.PrepareStatus != "2002" {
		t.Errorf("prepareStatus 应为 2002，实际 %q", got.PrepareStatus)
	}
	// 上游状态接口没有独立的「重签状态」字段，只有 isReSignOrder（Y/N）。
	// 不能把 Y/N 塞进 ReissueStatus——运营会把它读成「重签进度」。
	if got.ReissueStatus != "" {
		t.Errorf("ReissueStatus 不该被 isReSignOrder 填充，实际 %q", got.ReissueStatus)
	}
}

// TestDownloadCertificateMillisTimestamps 验证证书有效期按**毫秒**解析。
//
// 上游所有时间戳都是毫秒。当成秒会让证书有效期落在 1970 年附近——
// 一个不会报错、只会让「证书已过期」的判断全部反过来的错误。
func TestDownloadCertificateMillisTimestamps(t *testing.T) {
	payload := `{"certInfo":{"serialNumber":"ebdd50","certContent":"-----BEGIN CERTIFICATE-----",
		"midCertContent":"-----BEGIN CERTIFICATE-----","notBefore":1606262400000,
		"notAfter":1637884799000,"commonName":"test.com","domainNames":["test.com"],
		"sha1":"a","sha256":"b","issuerCommonName":"Sectigo","issuerCountry":"GB",
		"issuerOrg":"Sectigo Limited","signatureAlgo":"SHA256-RSA","encryption":"RSA",
		"keyLength":2048,"keyCurve":""}}`
	c, _ := newHTTPTestClient(t, HTTPOptions{}, alwaysStatus(200, okEnvelope(payload)))

	got, err := c.DownloadCertificate(context.Background(), "20201232020789")
	if err != nil {
		t.Fatalf("下载证书失败: %v", err)
	}
	wantIssued := time.UnixMilli(1606262400000).UTC()
	if !got.IssuedAt.Equal(wantIssued) {
		t.Errorf("签发时间应为 %v（毫秒），实际 %v", wantIssued, got.IssuedAt)
	}
	if got.ExpiresAt.Year() != 2021 {
		t.Errorf("到期时间落在 %d 年，毫秒时间戳被当成秒了", got.ExpiresAt.Year())
	}
	if got.Certificate == "" {
		t.Error("证书 PEM 不该为空")
	}
}

// TestDownloadCertificateEmptyContentIsUnknown 验证「成功但没给证书」按结果未知处理。
//
// 判成「明确拒绝」会让订单域把一张已经签发的订单当成失败去退款。
func TestDownloadCertificateEmptyContentIsUnknown(t *testing.T) {
	c, _ := newHTTPTestClient(t, HTTPOptions{}, alwaysStatus(200,
		okEnvelope(`{"certInfo":{"serialNumber":"x","certContent":""}}`)))

	if _, err := c.DownloadCertificate(context.Background(), "20201232020789"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("应归入结果未知（ErrUnavailable），实际 %v", err)
	}
}

// ── 重签与取消 ────────────────────────────────────

// TestReissueWireRequest 验证重签请求体。
//
// 上游把 csr 与 dcvMethod 都列为必填，不接受「沿用原证书的 CSR」。
func TestReissueWireRequest(t *testing.T) {
	c, log := newHTTPTestClient(t, HTTPOptions{}, alwaysStatus(200,
		okEnvelope(`{"certID":142932802604636666,"priceDiff":66666}`)))

	err := c.Reissue(context.Background(), ReissueRequest{
		UpstreamOrderNo: "20201232020789",
		CSR:             "-----BEGIN CERTIFICATE REQUEST-----",
		DcvMethod:       "http_file",
		Domains:         []string{"example.com", "www.example.com"},
		Reason:          "用户申请",
	})
	if err != nil {
		t.Fatalf("重签失败: %v", err)
	}

	method, path, _, body := log.call(0)
	if method != http.MethodPost || path != "/certificates/reissue" {
		t.Errorf("重签应打到 POST /certificates/reissue，实际 %s %s", method, path)
	}

	var sent map[string]any
	if err := json.Unmarshal(body, &sent); err != nil {
		t.Fatalf("请求体不是 JSON: %v", err)
	}
	if sent["orderNo"] != "20201232020789" {
		t.Errorf("orderNo 映射错了：%v", sent["orderNo"])
	}
	// http_file 要翻成上游的 file。
	if sent["dcvMethod"] != "file" {
		t.Errorf("dcvMethod 应翻成 file，实际 %v", sent["dcvMethod"])
	}
	if sent["domainNames"] != "www.example.com" {
		t.Errorf("domainNames 应不含主域名，实际 %v", sent["domainNames"])
	}
	// Reason 是平台侧的审计字段，上游没有这个参数。
	if _, ok := sent["reason"]; ok {
		t.Error("reason 不该发给上游：它没有这个参数")
	}
}

// TestReissueRequiresCSR 验证缺少 CSR 时在本地拒绝。
//
// 上游的 csr 是必填，不传会换回 6006「csr 不合法」——那个错误会让人
// 去查 CSR 格式，而真实原因是「根本没传」。
func TestReissueRequiresCSR(t *testing.T) {
	c, log := newHTTPTestClient(t, HTTPOptions{}, alwaysStatus(200, okEnvelope("")))

	err := c.Reissue(context.Background(), ReissueRequest{
		UpstreamOrderNo: "20201232020789", DcvMethod: "dns_txt",
	})
	if err == nil {
		t.Fatal("缺少 CSR 应当被拒绝")
	}
	if log.count() != 0 {
		t.Error("缺少必填参数时不该发出请求")
	}
}

// TestCancelOrderUsesGET 验证取消订单用的是 **GET**。
//
// 上游文档里取消是 GET（不是 POST），也没有请求体。写成 POST 会打到
// 一个不存在的路由上，表现为一个与「取消」无关的网关错误。
func TestCancelOrderUsesGET(t *testing.T) {
	c, log := newHTTPTestClient(t, HTTPOptions{}, alwaysStatus(200, okEnvelope("")))

	if err := c.CancelOrder(context.Background(), "20201232020789"); err != nil {
		t.Fatalf("取消失败: %v", err)
	}
	method, path, _, body := log.call(0)
	if method != http.MethodGet {
		t.Errorf("取消订单应为 GET，实际 %s", method)
	}
	if path != "/certificates/cancel/20201232020789" {
		t.Errorf("路径错了：%s", path)
	}
	if len(body) != 0 {
		t.Errorf("取消订单没有请求体，实际发出 %s", body)
	}
}

// ── 提交验证与重发邮件 ────────────────────────────

// TestVerifyDomainsIsNotImplemented 验证未确认请求体的接口会明确报错。
//
// 上游文档给了这个接口的路径与响应，但**没有参数表也没有示例请求体**。
// 猜一个 {"domains":[...]} 发出去，最坏的情况是上游不报错但什么都没做——
// 用户点了「提交验证」，界面显示成功，而上游根本没开始校验。
// 所以这里刻意让它失败，把问题暴露在联调阶段。
func TestVerifyDomainsIsNotImplemented(t *testing.T) {
	c, log := newHTTPTestClient(t, HTTPOptions{}, alwaysStatus(200, okEnvelope("")))

	err := c.VerifyDomains(context.Background(), VerifyDomainsRequest{
		UpstreamOrderNo: "20201232020789",
		Domains:         []string{"example.com"},
		Method:          "dns_txt",
	})
	if !errors.Is(err, ErrNotSupported) {
		t.Fatalf("应返回 ErrNotSupported，实际 %v", err)
	}
	if log.count() != 0 {
		t.Error("请求体未确认时不该发出请求：一个「看起来成功但什么都没做」的请求比报错更糟")
	}
}

// TestResendDcvEmailIgnoresDomains 验证重发邮件忽略平台侧的域名参数。
//
// 上游的接口是 PUT /certificates/reSendDcvEmail/:orderNo，没有请求体，
// 按整单重发。忽略而不是报错：多发给几个域名是无害的（邮件验证是逐域名
// 独立收信），报错会让「只想重发一个域名」这个平台侧用法彻底不可用。
func TestResendDcvEmailIgnoresDomains(t *testing.T) {
	c, log := newHTTPTestClient(t, HTTPOptions{}, alwaysStatus(200, okEnvelope("")))

	if err := c.ResendDcvEmail(context.Background(), "20201232020789",
		[]string{"example.com"}); err != nil {
		t.Fatalf("重发邮件失败: %v", err)
	}
	method, path, _, body := log.call(0)
	if method != http.MethodPut {
		t.Errorf("重发邮件应为 PUT，实际 %s", method)
	}
	if path != "/certificates/reSendDcvEmail/20201232020789" {
		t.Errorf("路径错了：%s", path)
	}
	if len(body) != 0 {
		t.Errorf("上游这个接口没有请求体，实际发出 %s", body)
	}
}

// ── 重新生成 dcvToken ─────────────────────────────

// TestRegenerateDcvTokenPerDomain 验证整单重生成被翻译成逐域名调用。
//
// 上游按**单个域名**操作（要传 domain），平台按**整单**操作。
// 这个翻译只能由适配器做，而且必须**逐域名都调用**：漏掉一个域名，
// 那个域名的旧 token 仍然有效，而用户以为整单都换过了。
func TestRegenerateDcvTokenPerDomain(t *testing.T) {
	domains := `{"domainList":[{"dnsNames":["a.test.com"],"status":"2001",
		"dcvMethod":"DNS_TXT","recordType":"TXT","hostRecord":"_dnsauth.a.test.com",
		"hashValue":"tok1","fileDcvPath":"","email":""},
		{"dnsNames":["b.test.com"],"status":"2001","dcvMethod":"DNS_TXT","recordType":"TXT",
		"hostRecord":"_dnsauth.b.test.com","hashValue":"tok2","fileDcvPath":"","email":""}]}`

	var regenerated []string
	c, log := newHTTPTestClient(t, HTTPOptions{}, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/certificates/dcv" && r.Method == http.MethodPost {
			w.WriteHeader(200)
			_, _ = io.WriteString(w, okEnvelope(`{"dcvToken":"new"}`))
			return
		}
		w.WriteHeader(200)
		_, _ = io.WriteString(w, okEnvelope(domains))
	})

	if _, err := c.RegenerateDcvToken(context.Background(), "20201232020789"); err != nil {
		t.Fatalf("重新生成 token 失败: %v", err)
	}

	// 期望的调用序列：先拉域名列表，逐个域名 POST，最后再拉一次列表。
	for i := 0; i < log.count(); i++ {
		method, path, _, body := log.call(i)
		if path != "/certificates/dcv" {
			continue
		}
		if method != http.MethodPost {
			// 同一个路径上 PUT 是「改验证方式」，是另一件事。
			t.Errorf("第 %d 次调用用了 %s，重生成 token 必须是 POST", i, method)
		}
		var sent struct {
			OrderNo string `json:"orderNo"`
			Domain  string `json:"domain"`
		}
		if err := json.Unmarshal(body, &sent); err != nil {
			t.Fatalf("请求体不是 JSON: %v", err)
		}
		if sent.OrderNo != "20201232020789" {
			t.Errorf("orderNo 错了：%q", sent.OrderNo)
		}
		regenerated = append(regenerated, sent.Domain)
	}
	if strings.Join(regenerated, ",") != "a.test.com,b.test.com" {
		t.Errorf("应对每个域名各调一次，实际重生成的域名：%v", regenerated)
	}
	// 1 次列表 + 2 次重生成 + 1 次列表。
	if log.count() != 4 {
		t.Errorf("期望 4 次调用（列表、逐域名重生成、再列表），实际 %d 次", log.count())
	}
}

// ── 订单筛选（反查） ──────────────────────────────

// TestFindOrderDateCreatedRange 验证 dateCreated 的区间格式。
//
// 上游的区间用**省略号**分隔（2022-05-19T12:00:00…2022-05-20T13:00:00）。
// 用短横线或逗号是无效的，而上游对无效参数返回 11001
// 「解析 url query 错误」——一个完全看不出是「时间格式不对」的错误。
func TestFindOrderDateCreatedRange(t *testing.T) {
	payload := `{"currentPage":1,"returnCount":1,"totalOrders":1,"totalPages":1,"orders":[
		{"orderNo":"2021062217143494639798","productNo":"197","commonName":"example.com",
		 "orderStatus":"1002","ovValidationStatus":"2000","evValidationStatus":"2000",
		 "dcvStatus":"2001","certPrepareStatus":"2001","certStatus":"3002",
		 "submitDateStamp":1624353275000,"payDateStamp":1624353275000}]}`
	c, log := newHTTPTestClient(t, HTTPOptions{}, alwaysStatus(200, okEnvelope(payload)))

	from := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	to := time.Date(2026, 9, 17, 13, 0, 0, 0, time.UTC)
	got, err := c.FindOrder(context.Background(), FindOrderRequest{
		CommonName:    "example.com",
		CreatedAfter:  from,
		CreatedBefore: to,
	})
	if err != nil {
		t.Fatalf("反查失败: %v", err)
	}

	_, path, query, _ := log.call(0)
	if path != "/certificates/orders" {
		t.Errorf("路径错了：%s", path)
	}
	if !strings.Contains(query, "commonName=example.com") {
		t.Errorf("查询串缺少 commonName：%s", query)
	}
	if !strings.Contains(query, "dateCreated=2026-09-17T12%3A00%3A00%E2%80%A62026-09-17T13%3A00%3A00") {
		t.Errorf("dateCreated 应为省略号分隔的区间（URL 编码后是 %%E2%%80%%A6），实际 %s", query)
	}

	if len(got) != 1 {
		t.Fatalf("应返回 1 条，实际 %d", len(got))
	}
	if got[0].UpstreamOrderNo != "2021062217143494639798" {
		t.Errorf("订单号错了：%s", got[0].UpstreamOrderNo)
	}
	if !got[0].PaidAt.Equal(time.UnixMilli(1624353275000).UTC()) {
		t.Errorf("支付时间应为毫秒时间戳，实际 %v", got[0].PaidAt)
	}
}

// ── 业务码分类：与钱直接相关的一张表 ────────────────

// TestClassifyBusinessCodes 验证业务码到哨兵错误的映射。
//
// **上游把业务失败放在 HTTP 200 的报文里**，所以只看 HTTP 状态会把每一次
// 业务失败都当成成功；而这一层分类的方向错了，代价都是钱：
// 判成「明确拒绝」会让订单域解冻（平台白付一张证书），
// 判成「结果未知」会让用户的钱永远冻着。
func TestClassifyBusinessCodes(t *testing.T) {
	cases := []struct {
		code     int
		msg      string
		unknown  bool
		sentinel error
		reason   string
	}{
		{6000, "账户余额不足", false, ErrPlatformBalance,
			"平台自己的预存余额不够，重试不会变多，必须解冻并告警"},
		{6010, "订单号错误", false, ErrOrderNotFound, "上游明确说没有这个订单"},
		{500, "服务端错误", true, nil, "上游内部错误可能发生在建单之后"},
		{6801, "订单生成中", true, nil, "上游正在处理，可能已经建单"},
		{6002, "参数类型或参数名有误", false, nil, "我们自己的 bug，但重试无用"},
		{6004, "dcvMethod 不支持", false, nil, "产品能力问题，重试无用"},
		{6005, "globalsign暂不支持email", false, nil, "产品能力问题"},
		{6100, "不能重复取消订单", false, nil, "业务性拒绝"},
		{6303, "域名已验证", false, nil, "业务性拒绝"},
		{7112, "DateOfIncorporation 错误", false, nil, "企业信息字段错误"},
	}
	for _, tc := range cases {
		t.Run(strings.ReplaceAll(tc.msg, " ", "_"), func(t *testing.T) {
			c, _ := newHTTPTestClient(t, HTTPOptions{},
				alwaysStatus(200, businessEnvelope(tc.code, tc.msg)))

			_, err := c.Balance(context.Background())
			if err == nil {
				t.Fatal("业务失败应当返回错误")
			}
			if got := errors.Is(err, ErrUnavailable); got != tc.unknown {
				t.Errorf("code=%d 的「结果未知」判定应为 %v，实际 %v（%s）\n错误：%v",
					tc.code, tc.unknown, got, tc.reason, err)
			}
			if tc.sentinel != nil && !errors.Is(err, tc.sentinel) {
				t.Errorf("code=%d 应命中 %v，实际 %v", tc.code, tc.sentinel, err)
			}
			// 余额不足不能同时被当成「结果未知」：那会让每一张订单都冻着
			// 等补偿，而补偿用的还是那个不够的余额。
			if tc.sentinel == ErrPlatformBalance && errors.Is(err, ErrUnavailable) {
				t.Error("余额不足不能同时被当成结果未知")
			}
			// 错误信息里要带上原始码与上游的说明，否则运维无从下手。
			if !strings.Contains(err.Error(), "code="+strconv.Itoa(tc.code)) {
				t.Errorf("错误信息应带上业务码：%v", err)
			}
		})
	}
}

// TestNonEnvelopeSuccessIsUnknown 验证 2xx 但报文不是信封时按结果未知处理。
//
// 典型来源是中间网关插入的 HTML 页。我们无法判断上游到底执行没执行
// 这次操作，保守方向是假设它执行了——判成「明确拒绝」会让订单域解冻。
func TestNonEnvelopeSuccessIsUnknown(t *testing.T) {
	c, _ := newHTTPTestClient(t, HTTPOptions{}, alwaysStatus(200, `<html>502 Bad Gateway</html>`))

	_, err := c.Balance(context.Background())
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("非信封响应应归入结果未知，实际 %v", err)
	}
}

// TestNullDataIsAccepted 验证 data 为 null 的成功应答不算错。
//
// 上游有多个接口成功时 data 就是 null（提交验证、重发邮件、取消订单）。
// 把「没有 data」当成错误会让这些接口永远失败。
func TestNullDataIsAccepted(t *testing.T) {
	c, _ := newHTTPTestClient(t, HTTPOptions{}, alwaysStatus(200, okEnvelope("")))

	if err := c.CancelOrder(context.Background(), "20201232020789"); err != nil {
		t.Fatalf("data 为 null 应当成功，实际 %v", err)
	}
}
