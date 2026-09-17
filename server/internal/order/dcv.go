package order

import (
	"strings"
	"time"

	"github.com/cangerx/c-ssl/server/internal/product/rules"
	"github.com/cangerx/c-ssl/server/internal/upstream/foxssl"
)

// ExpandFQDN 把上游返回的路径模板里的域名占位符替换成实际域名。
//
// **通配符域名替换成裸域名。** `*.example.com` 的验证文件要放在
// `example.com` 下：`*.example.com` 不是一台主机，没有任何地方能放文件。
// 直接把 `*.example.com` 填进路径会得到一个永远访问不通的地址。
//
// 这个函数是「不能把供应商原始模板直接交给用户」这条要求的落点。
// 上游返回的模板形如 `/.well-known/pki-validation/{FQDN}/ABC.txt`，
// 原样展示给用户的话，用户会照着一个含花括号的地址去放文件，
// 然后怎么试都不通过——而报错来自上游的「未检测到验证文件」，
// 排查方向从一开始就是错的。
func ExpandFQDN(template, domain string) string {
	if !strings.Contains(template, foxssl.PlaceholderFQDN) {
		// 上游没给模板（或已经给的是最终值），原样返回。
		// 这里不报错：占位符只是「可能包含」，不是「一定包含」。
		return template
	}
	return strings.ReplaceAll(template, foxssl.PlaceholderFQDN, BaseDomain(domain))
}

// BaseDomain 去掉通配符前缀。
func BaseDomain(domain string) string {
	return strings.TrimPrefix(strings.TrimSpace(domain), "*.")
}

// IsWildcard 判断是否通配符域名。
func IsWildcard(domain string) bool {
	return strings.HasPrefix(strings.TrimSpace(domain), "*.")
}

// AvailableMethods 推导某个域名可用的验证方式。
//
// **来源是上游返回的验证材料，而不是产品配置。** 理由：
//
//  1. 上游才是「它准备了什么、会接受什么」的权威。它没返回邮件地址，
//     就说明这个产品走不了邮件验证——不管产品配置怎么写。
//  2. 产品可能在下单之后被下架或改配置，而订单还要继续走完。
//     依赖产品读取会让已下架产品的在途订单连域名都验证不了。
//  3. 数据驱动不会与产品配置漂移：改了产品配置不会让存量订单的
//     可用方式跟着变，这在用户已经按某种方式配好记录之后尤其重要。
//
// 产品能力（rules 包）仍然是下单页的选项来源——那是「用户可以选什么」，
// 这里是「这张已创建的订单在上游侧能做到什么」，两者本来就不同。
// 但本函数的结果**不是**最终下发给前端的那一份：产品声明还要再收窄一次
// （见 NarrowToDeclared），下发前端的字段是两者的交集，由 Service 统一算好。
//
// 通配符域名仍然强制排除文件验证：上游有时会为通配符域名也返回一个
// 文件路径，但那个路径没有任何地方能访问到，给出来只会误导用户。
func AvailableMethods(d Domain) []rules.DcvMethod {
	out := make([]rules.DcvMethod, 0, 4)

	switch strings.ToUpper(strings.TrimSpace(d.Record.Type)) {
	case "TXT":
		if d.Record.Value != "" {
			out = append(out, rules.DnsTxt)
		}
	case "CNAME":
		if d.Record.Value != "" {
			out = append(out, rules.DnsCname)
		}
	default:
		// 上游没给类型但给了值：两种 DNS 方式都可能可用
		if d.Record.Value != "" {
			out = append(out, rules.DnsTxt, rules.DnsCname)
		}
	}

	if d.File.Path != "" && d.File.Content != "" && !d.Wildcard {
		out = append(out, rules.HTTPFile, rules.HTTPSFile)
	}

	if len(d.Emails) > 0 {
		out = append(out, rules.Email)
	}

	return out
}

// NarrowToDeclared 用产品声明的方式收窄材料侧推导出的方式。
//
// declared 为 nil 表示没有产品侧约束（产品读不到），此时原样返回。
//
// 收窄的方向只能是「材料有、声明也有」，两个来源都不能单独成立：
// 上游没准备邮件地址时提交邮件验证只会换回一个上游报错；
// 产品页写着不支持邮件验证时接受它，产品配置就成了纯装饰。
func NarrowToDeclared(methods, declared []rules.DcvMethod) []rules.DcvMethod {
	if declared == nil {
		return methods
	}
	out := make([]rules.DcvMethod, 0, len(methods))
	for _, m := range methods {
		if containsMethod(declared, m) {
			out = append(out, m)
		}
	}
	return out
}

// MethodAllowed 判断某个验证方式对该域名是否可用。
func MethodAllowed(d Domain, method rules.DcvMethod) bool {
	for _, m := range AvailableMethods(d) {
		if m == method {
			return true
		}
	}
	return false
}

// IntersectMethods 返回一组域名共同可用的验证方式。
//
// 提交验证时选定的方式必须对**所有**提交的域名都可用：
// 只对其中一部分可用的方式，会让另一部分域名在验证阶段才失败，
// 而那时用户已经按错误的方式配好了记录。
func IntersectMethods(domains []Domain) []rules.DcvMethod {
	if len(domains) == 0 {
		return nil
	}

	counts := make(map[rules.DcvMethod]int)
	order := make([]rules.DcvMethod, 0, 5)
	for _, d := range domains {
		for _, m := range AvailableMethods(d) {
			if counts[m] == 0 {
				order = append(order, m)
			}
			counts[m]++
		}
	}

	// 按首次出现的顺序输出，保证结果稳定可测
	out := make([]rules.DcvMethod, 0, len(order))
	for _, m := range order {
		if counts[m] == len(domains) {
			out = append(out, m)
		}
	}
	return out
}

// mapUpstreamDcvMethod 把上游的验证方式翻译成平台取值。
//
// **上游的请求侧与响应侧不是同一套取值。** 请求要小写
// （file / dns / email / dns_txt / dns_cname），而响应返回的是大写常量名：
//
//	HTTP_CSR_HASH  → http_file
//	CNAME_CSR_HASH → dns_cname
//	DNS_CNAME      → dns_cname
//	DNS_TXT        → dns_txt
//	EMAIL          → email
//
// 直接把响应里的取值当平台取值用（rules.DcvMethod(src.Method)），会让
// 订单里存着一个平台不认识的字符串——它会被写进数据库、出现在接口响应里，
// 而前端的验证方式匹配不到任何一项，用户看到的是一个空白的方式。
//
// CNAME_CSR_HASH 映射到 dns_cname 而不是平台的「dns」：平台根本没有 dns
// 这个取值（见 foxssl.toWireDcvMethod 的说明），而两者都是「在域名下加一条
// CNAME 记录」，语义一致。
//
// 也接受平台自己的取值——Mock 上游返回的就是它们。这样 Mock 与真实上游
// 走同一条路径，调用处不需要按 provider 分支。
//
// 未识别的取值返回空串，而不是原样存下来：调用方据此知道「上游给了个
// 我们不认识的方式」，可以按「没有方式」处理。
func mapUpstreamDcvMethod(method string) rules.DcvMethod {
	switch strings.ToUpper(strings.TrimSpace(method)) {
	case "HTTP_CSR_HASH":
		return rules.HTTPFile
	case "CNAME_CSR_HASH", "DNS_CNAME":
		return rules.DnsCname
	case "DNS_TXT":
		return rules.DnsTxt
	case "EMAIL":
		return rules.Email
	}

	if platform := rules.DcvMethod(strings.ToLower(strings.TrimSpace(method))); platform.Valid() {
		return platform
	}
	return ""
}

// NewDomain 由上游返回的域名材料构造本域的 Domain。
//
// 路径模板在这里展开——这是唯一一处把上游模板转成用户可见路径的地方。
func NewDomain(orderNo string, src foxssl.Domain, primary bool) Domain {
	return Domain{
		OrderNo:  orderNo,
		Domain:   src.Domain,
		Wildcard: IsWildcard(src.Domain),
		Primary:  primary,
		Status:   mapUpstreamDomainStatus(src.Status),
		Method:   mapUpstreamDcvMethod(src.Method),
		Record: DNSRecord{
			Type:  src.DnsRecordType,
			Name:  src.DnsRecordName,
			Value: src.DnsRecordValue,
		},
		File: FileChallenge{
			// 展开后才是最终路径。存模板再在读取时替换的话，
			// 任何一条忘了替换的读取路径都会把含 {FQDN} 的地址展示给用户。
			Path:    ExpandFQDN(src.FileDcvPath, src.Domain),
			Content: src.FileContent,
		},
		Emails:     append([]string(nil), src.EmailAddresses...),
		VerifiedAt: zeroToNil(src.VerifiedAt),
		ExpiresAt:  zeroToNil(src.ExpiresAt),
	}
}

// NewPendingDomain 由下单时提交的域名构造一条只有名字的记录。
//
// 上游还没返回验证材料时使用。这些行先落库是为了让订单的域名集合
// 在创建时就确定下来——上游调用可能失败，但「用户订了哪几个域名」
// 是订单本身的事实，不该依赖一次网络调用的成败。
func NewPendingDomain(orderNo, domain string, primary bool) Domain {
	return Domain{
		OrderNo:  orderNo,
		Domain:   domain,
		Wildcard: IsWildcard(domain),
		Primary:  primary,
		Status:   DomainPending,
	}
}

// mapUpstreamDomainStatus 把上游的域名状态原文映射成本域状态。
//
// 未识别的取值一律当作 pending：上游新增状态值时不应该让本服务出错，
// 而 pending 是「还不能签发」这一侧的安全默认值——
// 猜成 verified 会让平台在上游还没验证通过时就往下走。
//
// **真实上游给的是数字码**：2001 未验证、2002 已验证（Mock 给的是
// "pending" / "verified" 这类字符串，两张表都要有）。
//
// 上游只有两态，分不出平台的 pending（尚未提交）与 verifying（已提交
// 待校验），这里取更保守的 pending——不声称「正在验证中」。
// 代价是：用户提交验证后再刷新材料，本地的 verifying 会被上游的 2001
// 打回 pending。这是已知的行为差异，见 docs/07。
func mapUpstreamDomainStatus(s string) DomainStatus {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "verified", "valid", "success", "active":
		return DomainVerified
	case "verifying", "pending_validation", "processing", "checking":
		return DomainVerifying
	case "failed", "invalid", "rejected", "error":
		return DomainFailed
	case "expired":
		return DomainExpired
	// 真实上游的数字码。
	case "2002":
		return DomainVerified
	case "2001":
		return DomainPending
	default:
		return DomainPending
	}
}

// zeroToNil 把零值时间转成 nil，便于落库为 NULL。
func zeroToNil(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	utc := t.UTC()
	return &utc
}
