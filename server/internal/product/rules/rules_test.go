package rules

import (
	"testing"
)

func intPtr(v int) *int { return &v }

func TestNormalizeEVRules(t *testing.T) {
	// 规则 1：EV 不支持 IP 和通配符
	cap := Capability{
		Brand:                "DigiCert",
		ValidationType:       EV,
		WildcardSupported:    true,
		IPSupported:          true,
		MultiDomainSupported: true,
		KeyAlgorithms:        []KeyAlgorithm{RSA},
		DcvMethods:           []DcvMethod{DnsTxt},
	}

	corrections := Normalize(&cap)

	if cap.WildcardSupported {
		t.Error("EV 的通配符支持应被强制关闭")
	}
	if cap.IPSupported {
		t.Error("EV 的 IP 支持应被强制关闭")
	}

	// 断言具体修正了哪些字段，而不是数条数——后续新增规则时不会误伤本用例
	fields := make(map[string]bool, len(corrections))
	for _, c := range corrections {
		fields[c.Field] = true
	}
	for _, want := range []string{"wildcard_supported", "ip_supported"} {
		if !fields[want] {
			t.Errorf("应记录 %s 的修正", want)
		}
	}
}

func TestNormalizeOrganizationInfo(t *testing.T) {
	// 规则 5：OV / EV 必须提交完整企业信息
	for _, vt := range []ValidationType{OV, EV} {
		cap := Capability{
			Brand:          "DigiCert",
			ValidationType: vt,
			KeyAlgorithms:  []KeyAlgorithm{RSA},
			DcvMethods:     []DcvMethod{DnsTxt},
		}
		Normalize(&cap)
		if !cap.RequireOrgInfo {
			t.Errorf("%s 应被强制要求提交企业信息", vt)
		}
	}

	// DV 不受影响
	dv := Capability{
		Brand:          "Certum",
		ValidationType: DV,
		KeyAlgorithms:  []KeyAlgorithm{RSA},
		DcvMethods:     []DcvMethod{DnsTxt},
	}
	Normalize(&dv)
	if dv.RequireOrgInfo {
		t.Error("DV 不应被强制要求提交企业信息")
	}
}

func TestNormalizeFreeCertificate(t *testing.T) {
	// 规则 7：免费证书不能重签和取消
	cap := Capability{
		Brand:            "Let's Encrypt",
		ValidationType:   DV,
		KeyAlgorithms:    []KeyAlgorithm{RSA},
		DcvMethods:       []DcvMethod{DnsTxt},
		ReissueSupported: true,
		CancelSupported:  true,
		IsFree:           true,
	}

	Normalize(&cap)

	if cap.ReissueSupported {
		t.Error("免费证书的重签应被强制关闭")
	}
	if cap.CancelSupported {
		t.Error("免费证书的取消应被强制关闭")
	}
}

func TestNormalizeNoEmailBrands(t *testing.T) {
	// 规则 3：GlobalSign、AlphaSSL 不支持邮件验证
	for _, brand := range []string{"GlobalSign", "AlphaSSL", "globalsign"} {
		cap := Capability{
			Brand:          brand,
			ValidationType: DV,
			KeyAlgorithms:  []KeyAlgorithm{RSA},
			DcvMethods:     []DcvMethod{DnsTxt, Email, HTTPFile},
		}
		Normalize(&cap)

		for _, m := range cap.DcvMethods {
			if m == Email {
				t.Errorf("%s 的邮件验证应被移除", brand)
			}
		}
		if len(cap.DcvMethods) != 2 {
			t.Errorf("%s 应保留 2 种验证方式，实际 %d 种", brand, len(cap.DcvMethods))
		}
	}

	// 其他品牌保留邮件验证
	other := Capability{
		Brand:          "DigiCert",
		ValidationType: DV,
		KeyAlgorithms:  []KeyAlgorithm{RSA},
		DcvMethods:     []DcvMethod{DnsTxt, Email},
	}
	Normalize(&other)
	if len(other.DcvMethods) != 2 {
		t.Error("DigiCert 的邮件验证不应被移除")
	}
}

func TestNormalizeDnsOnlyBrand(t *testing.T) {
	// 规则 4：Certum 只支持 dns_txt 与 dns_cname
	cap := Capability{
		Brand:             "Certum",
		ValidationType:    DV,
		WildcardSupported: true,
		KeyAlgorithms:     []KeyAlgorithm{RSA},
		DcvMethods:        []DcvMethod{DnsTxt, DnsCname, HTTPFile, HTTPSFile},
	}

	Normalize(&cap)

	if len(cap.DcvMethods) != 2 {
		t.Fatalf("Certum 应只保留 2 种验证方式，实际 %d 种: %v", len(cap.DcvMethods), cap.DcvMethods)
	}
	for _, m := range cap.DcvMethods {
		if m != DnsTxt && m != DnsCname {
			t.Errorf("Certum 不应保留 %s", m)
		}
	}
}

func TestNormalizeWildcardRequiresDNS(t *testing.T) {
	// 支持通配符的产品必须至少有一种 DNS 验证，否则通配符域名无法验证
	cap := Capability{
		Brand:             "SomeBrand",
		ValidationType:    DV,
		WildcardSupported: true,
		KeyAlgorithms:     []KeyAlgorithm{RSA},
		DcvMethods:        []DcvMethod{HTTPFile},
	}

	Normalize(&cap)

	if !hasDNSMethod(cap.DcvMethods) {
		t.Error("支持通配符的产品应被补入 DNS 验证方式")
	}
}

func TestAllowedDcvMethodsWithWildcard(t *testing.T) {
	// 规则 2：含通配符的证书不支持文件验证
	cap := Capability{
		DcvMethods: []DcvMethod{DnsTxt, DnsCname, HTTPFile, HTTPSFile, Email},
	}

	normal := AllowedDcvMethods(cap, []string{"example.com"})
	if len(normal) != 5 {
		t.Errorf("非通配符域名应保留全部 5 种方式，实际 %d 种", len(normal))
	}

	wildcard := AllowedDcvMethods(cap, []string{"example.com", "*.example.com"})
	for _, m := range wildcard {
		if m.IsFileBased() {
			t.Errorf("含通配符时不应保留文件验证方式 %s", m)
		}
	}
	if len(wildcard) != 3 {
		t.Errorf("含通配符时应保留 3 种方式，实际 %d 种", len(wildcard))
	}
}

func TestValidate(t *testing.T) {
	valid := Capability{
		Brand:          "DigiCert",
		ValidationType: DV,
		KeyAlgorithms:  []KeyAlgorithm{RSA, ECC},
		DcvMethods:     []DcvMethod{DnsTxt},
	}
	if err := Validate(valid); err != nil {
		t.Errorf("合法配置不应报错: %v", err)
	}

	tests := []struct {
		name string
		cap  Capability
	}{
		{"品牌为空", Capability{ValidationType: DV, KeyAlgorithms: []KeyAlgorithm{RSA}, DcvMethods: []DcvMethod{DnsTxt}}},
		{"验证等级非法", Capability{Brand: "X", ValidationType: "xx", KeyAlgorithms: []KeyAlgorithm{RSA}, DcvMethods: []DcvMethod{DnsTxt}}},
		{"无密钥算法", Capability{Brand: "X", ValidationType: DV, DcvMethods: []DcvMethod{DnsTxt}}},
		{"无验证方式", Capability{Brand: "X", ValidationType: DV, KeyAlgorithms: []KeyAlgorithm{RSA}}},
		{"算法非法", Capability{Brand: "X", ValidationType: DV, KeyAlgorithms: []KeyAlgorithm{"dsa"}, DcvMethods: []DcvMethod{DnsTxt}}},
		{"验证方式非法", Capability{Brand: "X", ValidationType: DV, KeyAlgorithms: []KeyAlgorithm{RSA}, DcvMethods: []DcvMethod{"carrier_pigeon"}}},
		{"域名数下限大于上限", Capability{Brand: "X", ValidationType: DV, KeyAlgorithms: []KeyAlgorithm{RSA}, DcvMethods: []DcvMethod{DnsTxt}, MinSans: intPtr(5), MaxSans: intPtr(2)}},
		{"EV 仍支持通配符", Capability{Brand: "X", ValidationType: EV, KeyAlgorithms: []KeyAlgorithm{RSA}, DcvMethods: []DcvMethod{DnsTxt}, WildcardSupported: true}},
		{"免费证书仍支持重签", Capability{Brand: "X", ValidationType: DV, KeyAlgorithms: []KeyAlgorithm{RSA}, DcvMethods: []DcvMethod{DnsTxt}, IsFree: true, ReissueSupported: true}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := Validate(tt.cap); err == nil {
				t.Error("非法配置应报错")
			}
		})
	}
}

func TestValidateSelection(t *testing.T) {
	cap := Capability{
		Brand:                "DigiCert",
		ValidationType:       OV,
		WildcardSupported:    true,
		MultiDomainSupported: true,
		MinSans:              intPtr(1),
		MaxSans:              intPtr(3),
		KeyAlgorithms:        []KeyAlgorithm{RSA, ECC},
		DcvMethods:           []DcvMethod{DnsTxt, Email},
		RequireOrgInfo:       true,
	}
	years := []int{1, 2}

	tests := []struct {
		name    string
		sel     Selection
		wantErr bool
	}{
		{"合法单选", Selection{Years: 1, KeyAlgorithm: RSA, Domains: []string{"example.com"}}, false},
		{"合法通配符", Selection{Years: 2, KeyAlgorithm: ECC, Domains: []string{"*.example.com"}}, false},
		{"合法多域名", Selection{Years: 1, KeyAlgorithm: RSA, Domains: []string{"a.com", "b.com", "c.com"}}, false},
		{"年限不支持", Selection{Years: 3, KeyAlgorithm: RSA, Domains: []string{"example.com"}}, true},
		{"算法不支持", Selection{Years: 1, KeyAlgorithm: "dsa", Domains: []string{"example.com"}}, true},
		{"无域名", Selection{Years: 1, KeyAlgorithm: RSA, Domains: nil}, true},
		{"超出域名上限", Selection{Years: 1, KeyAlgorithm: RSA, Domains: []string{"a.com", "b.com", "c.com", "d.com"}}, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateSelection(cap, tt.sel, years)
			if tt.wantErr && err == nil {
				t.Error("期望报错但没有")
			}
			if !tt.wantErr && err != nil {
				t.Errorf("不应报错: %v", err)
			}
		})
	}
}

func TestValidateSelectionWildcardOnSingleDomainProduct(t *testing.T) {
	// 单域名产品收到通配符域名必须拒绝
	cap := Capability{
		Brand:                "AlphaSSL",
		ValidationType:       DV,
		WildcardSupported:    false,
		MultiDomainSupported: false,
		KeyAlgorithms:        []KeyAlgorithm{RSA},
		DcvMethods:           []DcvMethod{DnsTxt},
	}

	err := ValidateSelection(cap, Selection{Years: 1, KeyAlgorithm: RSA, Domains: []string{"*.example.com"}}, []int{1})
	if err == nil {
		t.Error("不支持通配符的产品应拒绝通配符域名")
	}

	// 多域名也要拒绝
	err = ValidateSelection(cap, Selection{Years: 1, KeyAlgorithm: RSA, Domains: []string{"a.com", "b.com"}}, []int{1})
	if err == nil {
		t.Error("不支持多域名的产品应拒绝多个域名")
	}
}
