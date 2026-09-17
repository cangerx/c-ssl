package order

// 本文件钉住**上游取值到平台取值的映射**。
//
// 真实上游与 Mock 上游的取值形态完全不同：Mock 用 "issued" / "verified"
// 这类字符串，真实上游用数字码（订单 1001-1006、证书 3002-3006、
// 重签 5001-5004、域名验证 2001-2002），验证方式更是请求侧一套
// 小写取值、响应侧一套大写常量名。
//
// 这一层写错的后果不是报错，而是**静默的误判**：状态认不出来就只记录
// 不改状态（订单永远停着），验证方式认不出来就存下一个平台不认识的
// 字符串（前端下拉框空白）。

import (
	"testing"

	"github.com/cangerx/c-ssl/server/internal/domain/orderstate"
	"github.com/cangerx/c-ssl/server/internal/product/rules"
)

// TestUpstreamStateCodesMapped 验证真实上游的数字状态码被正确映射。
func TestUpstreamStateCodesMapped(t *testing.T) {
	cases := []struct {
		code string
		want orderstate.State
		why  string
	}{
		{"1002", orderstate.Issuing, "已支付：上游收到钱，正在处理"},
		{"1004", orderstate.Cancelled, "已取消"},
		{"3002", orderstate.Issuing, "已支付，等待签发"},
		{"3004", orderstate.Issued, "已签发（回调也用它）"},
		{"3005", orderstate.Cancelled, "证书已取消"},
		{"5001", orderstate.Issuing, "重签申请中"},
		{"5002", orderstate.Issued, "重签申请成功"},
		{"5003", orderstate.Failed, "重签申请失败"},
	}
	for _, tc := range cases {
		t.Run(tc.code, func(t *testing.T) {
			if got := stateFromUpstream(tc.code, ""); got != tc.want {
				t.Errorf("上游状态 %s（%s）应映射成 %s，实际 %q", tc.code, tc.why, tc.want, got)
			}
		})
	}
}

// TestUpstreamStatesNeedingHumanAreNotMapped 验证需要人工介入的状态不被自动映射。
//
// 上游文档对 1003 / 1006 / 3003 / 3006 的说明都是「请联系客服」，
// 对 5004 的说明是「重签需要补差价但没有补交」，1001 是「未支付」
// （它同时出现在重签场景，退款动作与首单不同）。
//
// 这几个状态不会自愈，而平台能做的自动动作只有「退款」——在不知道
// 上游会不会人工修好的情况下退款，等于替上游做了决定，而退款之后
// 证书又被签出来时，对账才会发现。
//
// 这条断言的价值在于：如果有人为了让「卡住的订单动起来」而把它们加进
// 映射表，这里会立刻变红，逼他先想清楚那笔钱由谁承担。
func TestUpstreamStatesNeedingHumanAreNotMapped(t *testing.T) {
	for _, code := range []string{"1001", "1003", "1006", "3003", "3006", "5004"} {
		if got := stateFromUpstream(code, ""); got != "" {
			t.Errorf("上游状态 %s 需要人工介入，不该被自动映射，实际映射成 %q", code, got)
		}
	}
}

// TestUnrecognizedUpstreamStateStaysEmpty 验证未识别的状态返回空串。
//
// 返回空串而不是一个默认状态：调用方据此只记录不改状态。猜一个状态
// 比不动更危险——猜成 issued 会让未签发的订单出现一个下载不了的证书入口。
func TestUnrecognizedUpstreamStateStaysEmpty(t *testing.T) {
	for _, status := range []string{"", "9999", "some_new_status"} {
		if got := stateFromUpstream(status, ""); got != "" {
			t.Errorf("未识别的状态 %q 应返回空串，实际 %q", status, got)
		}
	}
}

// TestUpstreamDomainStatusCodes 验证域名验证状态的数字码映射。
func TestUpstreamDomainStatusCodes(t *testing.T) {
	// 2002 是「已验证」。猜错这个方向的代价最直接：判成未验证只是让用户
	// 多等，判成已验证会让平台在上游还没通过时就往下走。
	if got := mapUpstreamDomainStatus("2002"); got != DomainVerified {
		t.Errorf("上游 2002 应映射成 verified，实际 %q", got)
	}
	// 2001 是「未验证」。上游只有两态，分不出平台的 pending（尚未提交）
	// 与 verifying（已提交待校验），这里取更保守的 pending——
	// 不声称「正在验证中」。
	if got := mapUpstreamDomainStatus("2001"); got != DomainPending {
		t.Errorf("上游 2001 应映射成 pending，实际 %q", got)
	}
	// Mock 的字符串形态仍然要能认。
	if got := mapUpstreamDomainStatus("verified"); got != DomainVerified {
		t.Errorf("字符串形态的 verified 应仍被识别，实际 %q", got)
	}
}

// TestMapUpstreamDcvMethod 验证验证方式的响应侧常量名被翻成平台取值。
//
// **上游的请求侧与响应侧不是同一套取值**：请求要小写
// （file / dns / email / dns_txt / dns_cname），响应返回大写常量名。
// 把响应里的取值直接当平台取值存下来，订单里就会出现一个平台不认识的
// 字符串——它会被写进数据库、出现在接口响应里，而前端的验证方式
// 匹配不到任何一项。
func TestMapUpstreamDcvMethod(t *testing.T) {
	cases := map[string]rules.DcvMethod{
		"HTTP_CSR_HASH":  rules.HTTPFile,
		"CNAME_CSR_HASH": rules.DnsCname,
		"DNS_CNAME":      rules.DnsCname,
		"DNS_TXT":        rules.DnsTxt,
		"EMAIL":          rules.Email,
		// Mock 上游返回的就是平台取值，同一条路径也要能处理，
		// 否则调用处就得按 provider 分支。
		"dns_txt":   rules.DnsTxt,
		"http_file": rules.HTTPFile,
		"email":     rules.Email,
		// 大小写不该影响识别。
		"http_csr_hash": rules.HTTPFile,
	}
	for raw, want := range cases {
		t.Run(raw, func(t *testing.T) {
			if got := mapUpstreamDcvMethod(raw); got != want {
				t.Errorf("%q 应翻成 %q，实际 %q", raw, want, got)
			}
		})
	}

	// 未识别的取值返回空串，而不是原样存下来。
	if got := mapUpstreamDcvMethod("SOMETHING_NEW"); got != "" {
		t.Errorf("未识别的验证方式应返回空串，实际 %q", got)
	}
	if got := mapUpstreamDcvMethod(""); got != "" {
		t.Errorf("空取值应返回空串，实际 %q", got)
	}
}

// TestInitialDcvMethodTakesFirstDeclared 验证下单时的初始验证方式取产品声明的第一个。
//
// 上游下单接口把 dcvMethod 列为必填，而平台把「选哪种方式」放在域名验证
// 阶段，两者错位，只能在下单时先给一个初始值。
//
// 取第一个而不是写死 dns：产品声明是运营配置的，顺序由他们控制。
// 写死 dns 会让只支持邮件验证的产品直接下单失败（上游返回 6004）。
func TestInitialDcvMethodTakesFirstDeclared(t *testing.T) {
	cap := rules.Capability{DcvMethods: []rules.DcvMethod{rules.Email, rules.DnsTxt}}
	if got := initialDcvMethod(cap); got != rules.Email {
		t.Errorf("应取产品声明的第一个方式，实际 %q", got)
	}

	// 产品一个方式都没声明时返回空串，让适配层明确拒绝。
	// 这比猜一个方式发出去好：猜错的话，用户拿到的验证材料是另一种
	// 方式的，而界面上显示的又是产品声明的那几种，两边对不上。
	if got := initialDcvMethod(rules.Capability{}); got != "" {
		t.Errorf("产品未声明验证方式时应返回空串，实际 %q", got)
	}
}
