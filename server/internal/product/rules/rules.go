// Package rules 实现产品能力的硬约束。
//
// 这里是产品规则的**唯一真源**。前端不得复制这些判断——产品接口会把
// 每个产品可用的年限、算法、验证方式直接下发给前端，前端只负责渲染。
// 规则变更只需改本包，前端无需发版。
//
// 与数据库的分工：
//   - products 表存的是**运营可维护的配置**（某产品是否支持通配符等）。
//   - 本包是**无论怎么配都不能违反的硬约束**。运营把 EV 产品的
//     wildcard_supported 勾成 1 也无效，Normalize 会在读取时强制改回。
//
// 规则来源：docs/06 第 4.3 节。
package rules

import (
	"fmt"
	"strings"
)

// ValidationType 是证书验证等级。
type ValidationType string

const (
	// DV 域名验证，只校验域名控制权。
	DV ValidationType = "dv"
	// OV 组织验证，需核验企业主体。
	OV ValidationType = "ov"
	// EV 扩展验证，最严格，浏览器展示企业名称。
	EV ValidationType = "ev"
)

// Valid 判断是否为已定义的验证等级。
func (v ValidationType) Valid() bool {
	switch v {
	case DV, OV, EV:
		return true
	}
	return false
}

// KeyAlgorithm 是证书密钥算法。
type KeyAlgorithm string

const (
	RSA KeyAlgorithm = "rsa"
	ECC KeyAlgorithm = "ecc"
)

// Valid 判断是否为已定义的算法。
func (k KeyAlgorithm) Valid() bool {
	return k == RSA || k == ECC
}

// DcvMethod 是域名验证方式。
type DcvMethod string

const (
	DnsTxt    DcvMethod = "dns_txt"
	DnsCname  DcvMethod = "dns_cname"
	HTTPFile  DcvMethod = "http_file"
	HTTPSFile DcvMethod = "https_file"
	Email     DcvMethod = "email"
)

// Valid 判断是否为已定义的验证方式。
func (m DcvMethod) Valid() bool {
	switch m {
	case DnsTxt, DnsCname, HTTPFile, HTTPSFile, Email:
		return true
	}
	return false
}

// IsFileBased 判断是否为文件验证方式。
func (m DcvMethod) IsFileBased() bool {
	return m == HTTPFile || m == HTTPSFile
}

// Capability 是产品与规则相关的全部属性。
type Capability struct {
	Brand                string
	ValidationType       ValidationType
	WildcardSupported    bool
	IPSupported          bool
	MultiDomainSupported bool
	MinSans              *int
	MaxSans              *int
	KeyAlgorithms        []KeyAlgorithm
	DcvMethods           []DcvMethod
	ReissueSupported     bool
	CancelSupported      bool
	RequireOrgInfo       bool
	// IsFree 表示零售价为 0。由价格数据推导，不是独立字段。
	IsFree bool
}

// 规则 3：这两个品牌不提供邮件验证。
var noEmailBrands = map[string]struct{}{
	"globalsign": {},
	"alphassl":   {},
}

// 规则 4：Certum 只支持 DNS 验证。
var dnsOnlyBrands = map[string]struct{}{
	"certum": {},
}

// Correction 描述一条被强制修正的配置。
type Correction struct {
	Field  string
	From   string
	To     string
	Reason string
}

// Normalize 就地施加硬约束，返回被修正的项。
//
// 调用时机：从数据库读出产品后、运营保存产品前。这样即使有人绕过界面直接
// 改库，规则也不会被突破。返回的修正项应当写入审计日志。
func Normalize(cap *Capability) []Correction {
	var corrections []Correction

	fix := func(field string, from, to bool, reason string) {
		if from == to {
			return
		}
		corrections = append(corrections, Correction{
			Field:  field,
			From:   fmt.Sprintf("%t", from),
			To:     fmt.Sprintf("%t", to),
			Reason: reason,
		})
	}

	// 规则 1：EV 不支持 IP 和通配符
	if cap.ValidationType == EV {
		fix("ip_supported", cap.IPSupported, false, "EV 证书不支持 IP 地址")
		cap.IPSupported = false
		fix("wildcard_supported", cap.WildcardSupported, false, "EV 证书不支持通配符域名")
		cap.WildcardSupported = false
	}

	// 规则 5：OV / EV 必须提交完整企业信息
	if cap.ValidationType == OV || cap.ValidationType == EV {
		fix("require_organization_info", cap.RequireOrgInfo, true, "OV/EV 必须核验企业主体")
		cap.RequireOrgInfo = true
	}

	// 规则 7：免费证书不能重签和取消
	if cap.IsFree {
		fix("reissue_supported", cap.ReissueSupported, false, "免费证书不支持重签")
		cap.ReissueSupported = false
		fix("cancel_supported", cap.CancelSupported, false, "免费证书不支持取消")
		cap.CancelSupported = false
	}

	// 规则 3：GlobalSign、AlphaSSL 不支持邮件验证
	if _, banned := noEmailBrands[strings.ToLower(cap.Brand)]; banned {
		if filtered, removed := removeMethod(cap.DcvMethods, Email); removed {
			corrections = append(corrections, Correction{
				Field:  "dcv_methods",
				From:   "包含 email",
				To:     "已移除 email",
				Reason: cap.Brand + " 不支持邮件验证",
			})
			cap.DcvMethods = filtered
		}
	}

	// 规则 4：Certum 只支持 dns_txt 与 dns_cname
	if _, dnsOnly := dnsOnlyBrands[strings.ToLower(cap.Brand)]; dnsOnly {
		allowed := map[DcvMethod]struct{}{DnsTxt: {}, DnsCname: {}}
		var filtered []DcvMethod
		removedAny := false
		for _, m := range cap.DcvMethods {
			if _, ok := allowed[m]; ok {
				filtered = append(filtered, m)
			} else {
				removedAny = true
			}
		}
		if removedAny {
			corrections = append(corrections, Correction{
				Field:  "dcv_methods",
				From:   "包含非 DNS 验证方式",
				To:     "仅保留 dns_txt、dns_cname",
				Reason: cap.Brand + " 仅支持 DNS 验证",
			})
			cap.DcvMethods = filtered
		}
	}

	// 规则 2 的配置侧：不支持通配符的产品，其 DCV 方式集合不受影响；
	// 但支持通配符的产品必须至少有一种 DNS 验证方式，否则通配符域名无法验证。
	if cap.WildcardSupported && !hasDNSMethod(cap.DcvMethods) {
		corrections = append(corrections, Correction{
			Field:  "dcv_methods",
			From:   "无 DNS 验证方式",
			To:     "已补入 dns_txt",
			Reason: "支持通配符的产品必须提供 DNS 验证",
		})
		cap.DcvMethods = append([]DcvMethod{DnsTxt}, cap.DcvMethods...)
	}

	return corrections
}

// Validate 检查能力配置是否自洽。Normalize 之后再调用，此时硬约束已满足。
func Validate(cap Capability) error {
	if strings.TrimSpace(cap.Brand) == "" {
		return fmt.Errorf("品牌不能为空")
	}
	if !cap.ValidationType.Valid() {
		return fmt.Errorf("未知的验证等级: %q", cap.ValidationType)
	}
	if len(cap.KeyAlgorithms) == 0 {
		return fmt.Errorf("至少需要一种密钥算法")
	}
	for _, a := range cap.KeyAlgorithms {
		if !a.Valid() {
			return fmt.Errorf("未知的密钥算法: %q", a)
		}
	}
	if len(cap.DcvMethods) == 0 {
		return fmt.Errorf("至少需要一种域名验证方式")
	}
	for _, m := range cap.DcvMethods {
		if !m.Valid() {
			return fmt.Errorf("未知的验证方式: %q", m)
		}
	}
	if cap.MinSans != nil && cap.MaxSans != nil && *cap.MinSans > *cap.MaxSans {
		return fmt.Errorf("域名数下限 %d 大于上限 %d", *cap.MinSans, *cap.MaxSans)
	}
	if cap.MinSans != nil && *cap.MinSans < 1 {
		return fmt.Errorf("域名数下限不能小于 1")
	}
	if cap.ValidationType == EV && (cap.WildcardSupported || cap.IPSupported) {
		return fmt.Errorf("EV 证书不支持通配符或 IP")
	}
	if cap.IsFree && (cap.ReissueSupported || cap.CancelSupported) {
		return fmt.Errorf("免费证书不支持重签或取消")
	}
	return nil
}

// Selection 描述用户在下单页做出的选择。
type Selection struct {
	Years        int
	KeyAlgorithm KeyAlgorithm
	Domains      []string
}

// ValidateSelection 校验用户选择是否落在产品允许的范围内。
//
// availableYears 来自价格表——能买几年取决于有没有配价格，
// 而不是产品能力字段。
func ValidateSelection(cap Capability, sel Selection, availableYears []int) error {
	if !containsInt(availableYears, sel.Years) {
		return fmt.Errorf("该产品不支持 %d 年有效期", sel.Years)
	}
	if !containsAlgorithm(cap.KeyAlgorithms, sel.KeyAlgorithm) {
		return fmt.Errorf("该产品不支持 %s 算法", sel.KeyAlgorithm)
	}
	if len(sel.Domains) == 0 {
		return fmt.Errorf("至少需要一个域名")
	}

	wildcards := 0
	for _, d := range sel.Domains {
		if isWildcard(d) {
			wildcards++
		}
	}

	// 规则 1：EV 不支持通配符
	if wildcards > 0 && !cap.WildcardSupported {
		return fmt.Errorf("该产品不支持通配符域名")
	}

	// 域名数量限制：多域名产品才有 SAN 概念，单域名产品只允许一个域名
	if !cap.MultiDomainSupported && len(sel.Domains) > 1 {
		return fmt.Errorf("该产品不支持多域名")
	}
	if cap.MinSans != nil && len(sel.Domains) < *cap.MinSans {
		return fmt.Errorf("该产品至少需要 %d 个域名", *cap.MinSans)
	}
	if cap.MaxSans != nil && len(sel.Domains) > *cap.MaxSans {
		return fmt.Errorf("该产品最多支持 %d 个域名", *cap.MaxSans)
	}

	return nil
}

// AllowedDcvMethods 返回在给定域名形态下可用的验证方式。
//
// 规则 2：含通配符的证书不支持文件验证——通配符域名无法放置验证文件，
// 因此只要提交的域名里有通配符，就必须走 DNS 或邮件验证。
func AllowedDcvMethods(cap Capability, domains []string) []DcvMethod {
	hasWildcard := false
	for _, d := range domains {
		if isWildcard(d) {
			hasWildcard = true
			break
		}
	}

	out := make([]DcvMethod, 0, len(cap.DcvMethods))
	for _, m := range cap.DcvMethods {
		if hasWildcard && m.IsFileBased() {
			continue
		}
		out = append(out, m)
	}
	return out
}

// ── 内部辅助 ──────────────────────────────────────

func isWildcard(domain string) bool {
	return strings.HasPrefix(strings.TrimSpace(domain), "*.")
}

func hasDNSMethod(methods []DcvMethod) bool {
	for _, m := range methods {
		if m == DnsTxt || m == DnsCname {
			return true
		}
	}
	return false
}

func removeMethod(methods []DcvMethod, target DcvMethod) ([]DcvMethod, bool) {
	out := make([]DcvMethod, 0, len(methods))
	removed := false
	for _, m := range methods {
		if m == target {
			removed = true
			continue
		}
		out = append(out, m)
	}
	return out, removed
}

func containsInt(values []int, target int) bool {
	for _, v := range values {
		if v == target {
			return true
		}
	}
	return false
}

func containsAlgorithm(values []KeyAlgorithm, target KeyAlgorithm) bool {
	for _, v := range values {
		if v == target {
			return true
		}
	}
	return false
}
