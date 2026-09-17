package order

import (
	"strings"
	"testing"

	"github.com/cangerx/c-ssl/server/internal/product/rules"
	"github.com/cangerx/c-ssl/server/internal/upstream/foxssl"
)

// 本文件覆盖 DCV 材料处理的纯函数。
//
// 这里最需要守住的一条是：**含 {FQDN} 的模板绝不能流到用户可见的字段里**。
// 上游返回的是 `/.well-known/pki-validation/{FQDN}/ABC.txt`，原样展示的话，
// 用户会照着一个含花括号的地址去放文件，然后怎么试都不通过，
// 而报错来自上游的「未检测到验证文件」——排查方向从一开始就是错的。

// TestExpandFQDNReplacesWildcardWithBareDomain 是本文件最重要的一条。
//
// 通配符域名必须替换成**裸域名**：`*.example.com` 不是一台主机，
// 没有任何地方能承载验证文件。把 `*.example.com` 填进路径会得到一个
// 永远访问不通的地址，而且它看起来「很合理」，不会有人怀疑。
func TestExpandFQDNReplacesWildcardWithBareDomain(t *testing.T) {
	const template = "/.well-known/pki-validation/" + foxssl.PlaceholderFQDN + "/ABC.txt"

	got := ExpandFQDN(template, "*.example.com")

	if strings.Contains(got, foxssl.PlaceholderFQDN) {
		t.Fatalf("展开后不应残留占位符，实际 %q", got)
	}
	if strings.Contains(got, "*.") {
		t.Fatalf("通配符域名应替换成裸域名，实际 %q", got)
	}
	if want := "/.well-known/pki-validation/example.com/ABC.txt"; got != want {
		t.Errorf("期望 %q，实际 %q", want, got)
	}
}

func TestExpandFQDNKeepsTemplateWithoutPlaceholder(t *testing.T) {
	const template = "/.well-known/acme/ABC.txt"

	if got := ExpandFQDN(template, "example.com"); got != template {
		t.Errorf("不含占位符的路径应原样返回，实际 %q", got)
	}
}

// TestExpandFQDNReplacesEveryOccurrence 验证同一模板里的多次出现都被替换。
//
// 用 strings.Replace 只换第一处的话，模板里出现两次占位符时会漏掉一处，
// 而漏掉的那处正好会在用户看到的路径里留下花括号。
func TestExpandFQDNReplacesEveryOccurrence(t *testing.T) {
	const template = "/" + foxssl.PlaceholderFQDN + "/" + foxssl.PlaceholderFQDN + ".txt"

	got := ExpandFQDN(template, "example.com")
	if strings.Contains(got, foxssl.PlaceholderFQDN) {
		t.Errorf("模板里的每一处占位符都应被替换，实际 %q", got)
	}
	if want := "/example.com/example.com.txt"; got != want {
		t.Errorf("期望 %q，实际 %q", want, got)
	}
}

// TestNewDomainNeverExposesPlaceholder 从入口处再钉一次。
//
// 与 TestExpandFQDNReplacesWildcardWithBareDomain 的区别是：那条测的是
// 展开函数本身，这条测的是「构造 Domain 的那条路径确实调用了它」。
// 只测函数的话，把 NewDomain 里的 ExpandFQDN 换成直接赋值，测试依然全绿。
func TestNewDomainNeverExposesPlaceholder(t *testing.T) {
	for _, domain := range []string{"example.com", "*.example.com", "a.b.example.com"} {
		t.Run(domain, func(t *testing.T) {
			d := NewDomain("CS20260917120000A7K3M9", foxssl.Domain{
				Domain:      domain,
				FileDcvPath: "/.well-known/pki-validation/" + foxssl.PlaceholderFQDN + "/T.txt",
				FileContent: "T",
			}, true)

			if strings.Contains(d.File.Path, foxssl.PlaceholderFQDN) {
				t.Errorf("Domain.File.Path 残留占位符: %q", d.File.Path)
			}
			if strings.Contains(d.File.Path, "*.") {
				t.Errorf("Domain.File.Path 残留通配符: %q", d.File.Path)
			}
			if !strings.Contains(d.File.Path, BaseDomain(domain)) {
				t.Errorf("Domain.File.Path 应包含裸域名 %q，实际 %q",
					BaseDomain(domain), d.File.Path)
			}
		})
	}
}

// TestAvailableMethodsDerivedFromUpstreamMaterial 验证可用方式由材料反推。
//
// 刻意不读产品配置：产品可能在下单之后被下架或改配置，而在途订单还要走完。
// 依赖产品读取会让已下架产品的订单连域名都验证不了。
func TestAvailableMethodsDerivedFromUpstreamMaterial(t *testing.T) {
	cases := []struct {
		name string
		in   Domain
		want []rules.DcvMethod
	}{
		{
			name: "只有 TXT 记录",
			in:   Domain{Record: DNSRecord{Type: "TXT", Value: "token"}},
			want: []rules.DcvMethod{rules.DnsTxt},
		},
		{
			name: "只有 CNAME 记录",
			in:   Domain{Record: DNSRecord{Type: "CNAME", Value: "token"}},
			want: []rules.DcvMethod{rules.DnsCname},
		},
		{
			name: "记录类型缺失但给了值，两种 DNS 方式都算可用",
			in:   Domain{Record: DNSRecord{Value: "token"}},
			want: []rules.DcvMethod{rules.DnsTxt, rules.DnsCname},
		},
		{
			name: "有记录类型但没有值，不算可用",
			in:   Domain{Record: DNSRecord{Type: "TXT"}},
			want: []rules.DcvMethod{},
		},
		{
			name: "文件材料齐全",
			in:   Domain{File: FileChallenge{Path: "/x.txt", Content: "c"}},
			want: []rules.DcvMethod{rules.HTTPFile, rules.HTTPSFile},
		},
		{
			name: "有路径但没有内容，不算可用",
			in:   Domain{File: FileChallenge{Path: "/x.txt"}},
			want: []rules.DcvMethod{},
		},
		{
			name: "有邮件地址",
			in:   Domain{Emails: []string{"admin@example.com"}},
			want: []rules.DcvMethod{rules.Email},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := AvailableMethods(tc.in)
			if len(got) != len(tc.want) {
				t.Fatalf("期望 %v，实际 %v", tc.want, got)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("期望 %v，实际 %v", tc.want, got)
				}
			}
		})
	}
}

// TestAvailableMethodsExcludesFileForWildcard 验证通配符域名恒不含文件验证。
//
// 上游有时也会为通配符域名返回一个文件路径，但那个路径没有任何地方能访问到。
// 给出来只会让用户按一个不可能成功的方式去配。
func TestAvailableMethodsExcludesFileForWildcard(t *testing.T) {
	d := Domain{
		Domain:   "*.example.com",
		Wildcard: true,
		Record:   DNSRecord{Type: "TXT", Value: "token"},
		File:     FileChallenge{Path: "/.well-known/pki-validation/example.com/T.txt", Content: "T"},
	}

	for _, m := range AvailableMethods(d) {
		if m == rules.HTTPFile || m == rules.HTTPSFile {
			t.Fatalf("通配符域名不应提供文件验证，实际可用方式 %v", AvailableMethods(d))
		}
	}

	// 非通配符的同一个域名则应当有文件验证——否则上面那条断言
	// 可能只是因为「材料本来就不全」而通过，测不到通配符这条规则
	plain := d
	plain.Domain = "example.com"
	plain.Wildcard = false
	if !MethodAllowed(plain, rules.HTTPFile) {
		t.Error("非通配符域名应当提供文件验证")
	}
}

// TestIntersectMethods 验证取交集，且结果顺序稳定。
//
// 提交验证时选定的方式必须对所有提交的域名都可用：只对其中一部分可用的方式，
// 会让另一部分域名在验证阶段才失败，而那时用户已经按错误的方式配好了记录。
func TestIntersectMethods(t *testing.T) {
	withFile := Domain{
		Record: DNSRecord{Type: "TXT", Value: "t"},
		File:   FileChallenge{Path: "/x.txt", Content: "c"},
	}
	dnsOnly := Domain{Record: DNSRecord{Type: "TXT", Value: "t"}}

	both := IntersectMethods([]Domain{withFile, withFile})
	if len(both) != 3 {
		t.Fatalf("两个都能用文件验证时，交集应有 3 项（dns_txt/http_file/https_file），实际 %v", both)
	}

	mixed := IntersectMethods([]Domain{withFile, dnsOnly})
	if len(mixed) != 1 || mixed[0] != rules.DnsTxt {
		t.Fatalf("一个域名不能做文件验证时，交集只应剩 dns_txt，实际 %v", mixed)
	}

	if got := IntersectMethods(nil); got != nil {
		t.Errorf("空输入应返回 nil，实际 %v", got)
	}
}

func TestBaseDomainAndIsWildcard(t *testing.T) {
	cases := []struct {
		domain   string
		base     string
		wildcard bool
	}{
		{"example.com", "example.com", false},
		{"*.example.com", "example.com", true},
		{"  *.example.com  ", "example.com", true},
		{"a.b.example.com", "a.b.example.com", false},
	}
	for _, tc := range cases {
		if got := BaseDomain(tc.domain); got != tc.base {
			t.Errorf("BaseDomain(%q) 期望 %q，实际 %q", tc.domain, tc.base, got)
		}
		if got := IsWildcard(tc.domain); got != tc.wildcard {
			t.Errorf("IsWildcard(%q) 期望 %v，实际 %v", tc.domain, tc.wildcard, got)
		}
	}
}

// TestMapUpstreamDomainStatusDefaultsToPending 验证未识别的上游状态按 pending 处理。
//
// 猜成 verified 会让平台在上游还没验证通过时就往下走，
// 而 pending 是「还不能签发」这一侧的安全默认值。
func TestMapUpstreamDomainStatusDefaultsToPending(t *testing.T) {
	cases := map[string]DomainStatus{
		"verified":            DomainVerified,
		"VALID":               DomainVerified,
		"verifying":           DomainVerifying,
		"failed":              DomainFailed,
		"expired":             DomainExpired,
		"some_new_status":     DomainPending,
		"":                    DomainPending,
		"   ":                 DomainPending,
		"Verified (upstream)": DomainPending,
	}
	for in, want := range cases {
		if got := mapUpstreamDomainStatus(in); got != want {
			t.Errorf("mapUpstreamDomainStatus(%q) 期望 %s，实际 %s", in, want, got)
		}
	}
}

// TestNewPendingDomainMarksWildcard 验证下单时就能识别通配符。
//
// 上游还没返回材料，但「这个域名是不是通配符」是域名本身的属性，
// 不该依赖一次网络调用的成败——它决定了哪些验证方式会被展示出来。
func TestNewPendingDomainMarksWildcard(t *testing.T) {
	wild := NewPendingDomain("CS1", "*.example.com", true)
	if !wild.Wildcard {
		t.Error("通配符域名应被标记为通配符")
	}
	if !wild.Primary {
		t.Error("primary 参数应被保留")
	}
	if wild.Status != DomainPending {
		t.Errorf("初始状态应为 pending，实际 %s", wild.Status)
	}
	if got := AvailableMethods(wild); len(got) != 0 {
		t.Errorf("还没有验证材料时不应有可用方式，实际 %v", got)
	}
}
