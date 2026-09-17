package foxssl

// 本文件是真实 FoxSSL 上游的**出站方法实现**：拼路径、调 c.do、解报文。
//
// 报文格式的出处与六个坑见 wire.go 的开头。这里只写三件容易写错的事：
//
//  1. **不要在这里做业务判断。** 本文件只负责把平台结构翻译成上游报文、
//     把上游报文翻译回平台结构。像「这个域名已验证不能重新生成 token」
//     这种规则属于订单域，放在这里会让 Mock 与真实上游的行为分叉。
//  2. **上游没有的字段留空，不要编。** 例如下单响应里没有订单状态，
//     CreateOrderResponse.Status 就是空串。编一个 "pending" 出来，
//     调用方会以为那是上游说的。
//  3. **单位与时间戳。** 金额一律分，时间戳一律**毫秒**。上游所有
//     时间戳都是毫秒，当成秒会让证书有效期落在 1970 年附近。

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/cangerx/c-ssl/server/internal/domain/money"
)

// ── 余额 ──────────────────────────────────────────

// Balance 查询上游账户余额。
func (c *HTTPClient) Balance(ctx context.Context) (*Balance, error) {
	raw, err := c.do(ctx, opBalance, http.MethodGet, pathBalance, nil)
	if err != nil {
		return nil, err
	}
	var data wireBalance
	if err := decodeData(opBalance, raw, &data); err != nil {
		return nil, err
	}
	// 币种上游不返回。平台的上游账户是人民币账户，写死比留空好：
	// 留空会让「币种」这个字段在展示层变成空白，看起来像数据缺失。
	return &Balance{Amount: money.Amount(data.Balance), Currency: "CNY"}, nil
}

// ── 下订单 ────────────────────────────────────────

// CreateOrder 在上游创建订单。
//
// **这个操作不幂等，而且平台无法让它变得幂等。**
//
// 上游的下单接口不接受任何商户侧标识：请求体里只有年限、验证方式、CSR、
// 域名、联系人、企业信息与回调地址，orderNo 由上游生成后返回。也就是说
// 「用同一个商户订单号重试，上游去重」这条路根本不存在——重复提交就是
// 真的再买一张证书，平台被扣两次钱。
//
// 因此调用方在**结果未知**（超时、连接被重置、上游 5xx）时不能直接重试，
// 必须先反查：用 FindOrder 按常用名称与创建时间窗口把上游订单捞回来，
// 确认没有匹配的单之后才允许重试。见 client.go 包注释的约定 1。
func (c *HTTPClient) CreateOrder(
	ctx context.Context, req CreateOrderRequest,
) (*CreateOrderResponse, error) {
	if req.UpstreamProductID <= 0 {
		// 本地入参问题，不是上游问题。发出去只会换回 6001「暂未提供相应产品」，
		// 那个错误会把人引向「产品被下架了」，而真实原因是平台没记下上游产品编号。
		return nil, fmt.Errorf("CreateOrder: 缺少上游产品编号")
	}
	dcvMethod, ok := toWireDcvMethod(req.DcvMethod)
	if !ok {
		return nil, fmt.Errorf("%w: CreateOrder: 验证方式 %q 无法翻译成上游取值",
			ErrNotSupported, req.DcvMethod)
	}

	body := wireCreateOrderRequest{
		Year:      req.Years,
		DcvMethod: dcvMethod,
		CSR:       req.CSR,
		// 上游的 domainNames 不含主域名，主域名由它从 CSR 里取。
		DomainNames: joinDomainNames(req.Domains),
		ContactInfo: wireContactInfo{
			LastName:  req.Contact.Name,
			FirstName: req.Contact.Name,
			Position:  req.Contact.Title,
			Email:     req.Contact.Email,
			Telephone: req.Contact.Phone,
		},
		NotifyURL: req.NotifyURL,
	}
	if req.Organization != nil {
		body.OrgInfo = &wireOrgInfo{
			OrgName:      req.Organization.Name,
			CreditCode:   req.Organization.RegistrationNo,
			Country:      req.Organization.Country,
			Province:     req.Organization.Province,
			Locality:     req.Organization.City,
			Address:      req.Organization.Address,
			PostalCode:   req.Organization.PostalCode,
			Telephone:    req.Organization.Phone,
			JoiCountry:   req.Organization.Country,
			JoiProvince:  req.Organization.Province,
			JoiLocality:  req.Organization.City,
			RegistryAddr: req.Organization.Address,
		}
	}

	path := fmt.Sprintf(pathCreateOrder, req.UpstreamProductID)
	raw, err := c.do(ctx, opCreateOrder, http.MethodPost, path, body)
	if err != nil {
		return nil, err
	}
	var data wireCreateOrderData
	if err := decodeData(opCreateOrder, raw, &data); err != nil {
		return nil, err
	}
	if strings.TrimSpace(data.OrderNo) == "" {
		// 下单成功但没给订单号，等于平台永远查不到这一单。
		// 归入「结果未知」而不是「明确拒绝」：证书很可能已经建了，
		// 解冻并置失败会让平台白付一张证书。
		return nil, fmt.Errorf("%w: CreateOrder: 上游未返回 orderNo", ErrUnavailable)
	}

	return &CreateOrderResponse{
		UpstreamOrderNo: data.OrderNo,
		CertID:          int64ToString(data.CertID),
		// 上游下单响应里没有状态字段，也没有创建时间。
		// 留空而不是编一个：调用方会按「上游没给」处理。
		Status: "",
		Cost:   money.Amount(data.Cost),
	}, nil
}

// ── 订单状态 ──────────────────────────────────────

// OrderStatus 查询订单状态。
func (c *HTTPClient) OrderStatus(
	ctx context.Context, upstreamOrderNo string,
) (*OrderStatus, error) {
	raw, err := c.do(ctx, opOrderStatus, http.MethodGet,
		fmt.Sprintf(pathOrderStatus, url.PathEscape(upstreamOrderNo)), nil)
	if err != nil {
		return nil, err
	}
	var data wireOrderStatusData
	if err := decodeData(opOrderStatus, raw, &data); err != nil {
		return nil, err
	}

	return &OrderStatus{
		UpstreamOrderNo: upstreamOrderNo,
		OrderStatus:     data.Status.OrderStatus,
		CertStatus:      data.Status.CertStatus,
		PrepareStatus:   data.Status.CertPrepareStatus,
		// 上游状态接口没有独立的「重签状态」字段，只有 isReSignOrder
		// （Y/N，表示这一单是不是重签产生的）。语义不同，不能塞进
		// ReissueStatus——运营会把它读成「重签进度」。
		ReissueStatus: "",
		// CertID / CommonName / Domains / ExpiresAt 都不在这个接口里，
		// 要下载证书才有。留空。
	}, nil
}

// ── 域名列表 ──────────────────────────────────────

// ListDomains 查询域名的验证材料。
func (c *HTTPClient) ListDomains(
	ctx context.Context, upstreamOrderNo string,
) (*DomainsResponse, error) {
	raw, err := c.do(ctx, opListDomains, http.MethodGet,
		fmt.Sprintf(pathListDomains, url.PathEscape(upstreamOrderNo)), nil)
	if err != nil {
		return nil, err
	}
	var data wireDomainsData
	if err := decodeData(opListDomains, raw, &data); err != nil {
		return nil, err
	}
	return &DomainsResponse{
		UpstreamOrderNo: upstreamOrderNo,
		Domains:         flattenDomains(data.DomainList),
	}, nil
}

// flattenDomains 把上游的域名条目摊平成平台的域名列表。
//
// 上游按「相同根域名」分组返回，一个条目里 dnsNames 可能有多个域名，
// 而平台是逐域名一条记录。所以要展开。
//
// 注意这里**不做验证方式的翻译**：上游返回的 dcvMethod 是响应侧常量名
// （HTTP_CSR_HASH 等），翻译成平台取值是订单域的职责，与「上游状态一律
// 以原文返回」是同一条原则。
func flattenDomains(src []wireDomain) []Domain {
	out := make([]Domain, 0, len(src))
	for _, item := range src {
		for _, name := range item.DnsNames {
			name = strings.TrimSpace(name)
			if name == "" {
				continue
			}
			out = append(out, Domain{
				Domain: name,
				Status: item.Status,
				Method: item.DcvMethod,
				// 记录三件套与文件路径在分组内是共享的，所以整组用同一份。
				DnsRecordType:  item.RecordType,
				DnsRecordName:  item.HostRecord,
				DnsRecordValue: item.HashValue,
				FileDcvPath:    item.FileDcvPath,
				EmailAddresses: append([]string(nil), item.Email...),
			})
		}
	}
	return out
}

// ── 提交域名验证 ──────────────────────────────────

// VerifyDomains 通知上游去实际校验这些域名。
//
// **这个接口的请求体在上游文档里是缺失的。** 文档给了路径
// （PUT /certificates/verifyDomains/:orderNo）与响应
// （{"code":200,"msg":"请求成功","data":null}），但既没有参数表，
// 也没有示例请求体——Postman 集合里这个条目连 originalRequest 都没有。
//
// 所以这里**不猜**。猜一个 {"domains":[...]} 出来，联调时可能报 6002
// （参数类型或参数名有误），也可能不报错但什么都没做——后者更糟：
// 用户点了「提交验证」，界面显示成功，而上游根本没开始校验。
//
// 拿到确认后的参数格式再实现。在那之前调用它会得到一个明确的错误，
// 而不是一个看起来成功的假象。
func (c *HTTPClient) VerifyDomains(ctx context.Context, req VerifyDomainsRequest) error {
	return fmt.Errorf("%w: VerifyDomains: 上游文档未给出该接口的请求体格式，"+
		"确认前不实现（订单 %s，域名 %v，方式 %s）",
		ErrNotSupported, req.UpstreamOrderNo, req.Domains, req.Method)
}

// ── 重发验证邮件 ──────────────────────────────────

// ResendDcvEmail 重发域名验证邮件。
//
// **domains 参数被忽略。** 上游的接口是
// PUT /certificates/reSendDcvEmail/:orderNo，没有请求体——它按整单重发。
//
// 忽略而不是报错：多发给几个域名是无害的（邮件验证是逐域名独立收信，
// 用户只处理自己关心的那封即可），而报错会让「只想重发一个域名」
// 这个平台侧用法彻底不可用。
func (c *HTTPClient) ResendDcvEmail(
	ctx context.Context, upstreamOrderNo string, domains []string,
) error {
	_, err := c.do(ctx, opResendDcvEmail, http.MethodPut,
		fmt.Sprintf(pathResendDcvEmail, url.PathEscape(upstreamOrderNo)), nil)
	return err
}

// ── 重新生成 dcvToken ─────────────────────────────

// RegenerateDcvToken 重新生成订单全部域名的验证 token。
//
// **上游按单个域名操作，平台按整单操作**，这里做翻译：
// 先拉一次域名列表拿到域名集合，逐个域名调上游，最后再拉一次列表
// 把新材料取回来。
//
// 为什么要重新拉而不是把上游返回的 dcvToken 拼进结果：上游只返回
// {dcvToken} 一个字段，而调用方需要的是完整材料（记录类型、主机记录、
// 文件路径都可能随 token 一起变）。拼一个只填了 token 的半成品，
// 会让用户看到「记录值变了、记录名还是旧的」这种半新半旧的指引。
//
// 部分失败的处理：逐个调用时第 N 个失败，前面几个的 token 已经换掉了。
// 这里**不吞掉错误**，把失败域名报出来——用户手里的旧材料有一部分已经
// 失效，必须让他知道要重新拉一次材料，而不是看到一个笼统的「失败」。
func (c *HTTPClient) RegenerateDcvToken(
	ctx context.Context, upstreamOrderNo string,
) (*DomainsResponse, error) {
	listed, err := c.ListDomains(ctx, upstreamOrderNo)
	if err != nil {
		return nil, err
	}
	if len(listed.Domains) == 0 {
		return nil, fmt.Errorf("%w: RegenerateDcvToken: 上游未返回任何域名",
			ErrOrderNotFound)
	}

	regenerated := make([]string, 0, len(listed.Domains))
	for _, d := range listed.Domains {
		body := wireRegenerateTokenRequest{OrderNo: upstreamOrderNo, Domain: d.Domain}
		raw, err := c.do(ctx, opRegenerateDcvToken, http.MethodPost,
			pathRegenerateToken, body)
		if err != nil {
			return nil, fmt.Errorf("重新生成域名 %s 的 token 失败（已成功重生成 %d 个，"+
				"这些域名的旧材料已失效，请重新拉取材料）: %w",
				d.Domain, len(regenerated), err)
		}
		// 上游说成功但没给 token，说明这次重生成没生效。**不能继续往下走**：
		// 继续的话，后面的域名会正常重生成，最后拉回来的列表看起来
		// 一切正常，而用户以为整单都换过了——那个域名上仍然挂着旧 token。
		var data wireRegenerateTokenData
		if err := decodeData(opRegenerateDcvToken, raw, &data); err != nil {
			return nil, err
		}
		if strings.TrimSpace(data.DcvToken) == "" {
			return nil, fmt.Errorf("%w: RegenerateDcvToken: 域名 %s 未返回新的 dcvToken"+
				"（已成功重生成 %d 个）", ErrUnavailable, d.Domain, len(regenerated))
		}
		regenerated = append(regenerated, d.Domain)
	}

	// 再拉一次拿新材料。这一步失败时 token 已经换过了，错误信息要说明
	// 这一点，否则用户会以为「什么都没发生」而继续用旧记录。
	updated, err := c.ListDomains(ctx, upstreamOrderNo)
	if err != nil {
		return nil, fmt.Errorf("token 已重新生成，但拉取新材料失败（请重新拉取材料）: %w", err)
	}
	return updated, nil
}

// ── 下载证书 ──────────────────────────────────────

// DownloadCertificate 下载已签发的证书。
func (c *HTTPClient) DownloadCertificate(
	ctx context.Context, upstreamOrderNo string,
) (*Certificate, error) {
	raw, err := c.do(ctx, opDownloadCertificate, http.MethodGet,
		fmt.Sprintf(pathDownloadCert, url.PathEscape(upstreamOrderNo)), nil)
	if err != nil {
		return nil, err
	}
	var data wireCertificateData
	if err := decodeData(opDownloadCertificate, raw, &data); err != nil {
		return nil, err
	}
	info := data.CertInfo
	if strings.TrimSpace(info.CertContent) == "" {
		// 上游说成功但没给证书内容。归入「结果未知」：证书可能已经签发，
		// 只是这次没拿到；判成「明确拒绝」会让订单域把一张已签发的订单
		// 当成失败去退款。
		return nil, fmt.Errorf("%w: DownloadCertificate: 上游未返回证书内容", ErrUnavailable)
	}

	return &Certificate{
		Status:       "", // 下载接口不返回状态，状态在 OrderStatus 里
		CommonName:   info.CommonName,
		Domains:      append([]string(nil), info.DomainNames...),
		KeyAlgorithm: info.Encryption,
		SerialNumber: info.SerialNumber,
		Certificate:  info.CertContent,
		CABundle:     info.MidCertContent,
		IssuedAt:     millisToTime(info.NotBefore),
		ExpiresAt:    millisToTime(info.NotAfter),
	}, nil
}

// ── 重签 ──────────────────────────────────────────

// Reissue 申请重签。
//
// 上游的 csr 与 dcvMethod 都是必填，不接受「沿用原 CSR」这种省略——
// 平台侧 ReissueRequest.CSR 为空时直接报错，而不是发一个上游必然
// 拒绝（6006 csr 不合法）的请求。
func (c *HTTPClient) Reissue(ctx context.Context, req ReissueRequest) error {
	if strings.TrimSpace(req.CSR) == "" {
		return fmt.Errorf("Reissue: 上游要求重签时重新提交 CSR，平台未提供")
	}
	dcvMethod, ok := toWireDcvMethod(req.DcvMethod)
	if !ok {
		return fmt.Errorf("%w: Reissue: 验证方式 %q 无法翻译成上游取值",
			ErrNotSupported, req.DcvMethod)
	}

	body := wireReissueRequest{
		CSR:         req.CSR,
		DcvMethod:   dcvMethod,
		DomainNames: joinDomainNames(req.Domains),
		OrderNo:     req.UpstreamOrderNo,
	}
	raw, err := c.do(ctx, opReissue, http.MethodPost, pathReissue, body)
	if err != nil {
		return err
	}
	var data wireReissueData
	if err := decodeData(opReissue, raw, &data); err != nil {
		return err
	}

	// 上游会返回重签差价（priceDiff，单位分）。**平台当前不向用户收取
	// 这笔钱**（平台侧的重签规则是「有效期内免费重签」），所以这里不
	// 把它塞进任何金额字段。
	//
	// 但要留一条日志：如果上游真的对某些产品收了差价，平台侧的结算里
	// 完全看不出来，而成本已经支出去了。静默丢弃一个花钱的信息，
	// 会让差额只在季度对账时才浮出来，那时已经无法追溯到哪一单。
	if data.PriceDiff > 0 {
		slog.WarnContext(ctx, "上游重签产生了差价，平台侧不会向用户收取，成本需运营关注",
			"op", opReissue, "upstream_order_no", req.UpstreamOrderNo,
			"price_diff", data.PriceDiff, "cert_id", int64ToString(data.CertID))
	}
	return nil
}

// ── 取消订单 ──────────────────────────────────────

// CancelOrder 取消订单。
//
// 上游是 **GET**（不是 POST），且没有请求体。写成 POST 会打到不存在的
// 路由上，表现为一个与「取消」无关的网关错误。
//
// 上游对重复取消返回 6100「不能重复取消订单」。这里**不做幂等包装**
// （不把它当成功）：重复取消意味着这一单已经取消过了，平台侧该发生的
// 退款已经发生过；把 6100 吞成成功，会让「取消」这个动作在
// 「上游说没取消成、平台以为取消成了」的方向上撒谎。
func (c *HTTPClient) CancelOrder(ctx context.Context, upstreamOrderNo string) error {
	_, err := c.do(ctx, opCancelOrder, http.MethodGet,
		fmt.Sprintf(pathCancelOrder, url.PathEscape(upstreamOrderNo)), nil)
	return err
}

// ── 订单筛选（反查） ──────────────────────────────

// FindOrder 按常用名称与创建时间窗口反查上游订单。
//
// 这是 CreateOrder 不幂等的**必要配套**：结果未知时不能直接重试，
// 得先看看上游到底建没建单。
//
// 反查的匹配依据只有 commonName 与创建时间——上游不提供按商户标识查询，
// 所以调用方必须把时间窗口卡得足够窄（下单前后几分钟），否则会把
// 同一域名的历史订单一起捞回来，反而误判成「已经建过单了」。
func (c *HTTPClient) FindOrder(
	ctx context.Context, req FindOrderRequest,
) ([]OrderSummary, error) {
	query := url.Values{}
	if name := strings.TrimSpace(req.CommonName); name != "" {
		query.Set("commonName", name)
	}
	if created := formatDateCreated(req.CreatedAfter, req.CreatedBefore); created != "" {
		query.Set("dateCreated", created)
	}

	path := pathFindOrders
	if encoded := query.Encode(); encoded != "" {
		path += "?" + encoded
	}

	raw, err := c.do(ctx, opFindOrder, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	var data wireFindOrdersData
	if err := decodeData(opFindOrder, raw, &data); err != nil {
		return nil, err
	}

	out := make([]OrderSummary, 0, len(data.Orders))
	for _, o := range data.Orders {
		out = append(out, OrderSummary{
			UpstreamOrderNo: o.OrderNo,
			ProductID:       o.ProductNo,
			CommonName:      o.CommonName,
			OrderStatus:     o.OrderStatus,
			CertStatus:      o.CertStatus,
			PrepareStatus:   o.Prepare,
			CreatedAt:       millisToTime(o.SubmitDateStamp),
			PaidAt:          millisToTime(o.PayDateStamp),
		})
	}
	return out, nil
}

// formatDateCreated 拼出上游的 dateCreated 参数。
//
// 上游接受两种形态：单个时间点（2022-05-19T12:00:00）或一个区间，
// 区间用**省略号**分隔（2022-05-19T12:00:00…2022-05-20T13:00:00）。
// 用短横线或逗号分隔是无效的，而上游对无效参数返回 11001
// 「解析 url query 错误」——一个完全看不出是「时间格式不对」的错误。
//
// 两端都给时输出区间；只给一端时只输出那一端。两端都是零值时返回空串，
// 表示不加这个过滤条件。
func formatDateCreated(after, before time.Time) string {
	const layout = "2006-01-02T15:04:05"

	from := ""
	if !after.IsZero() {
		from = after.UTC().Format(layout)
	}
	to := ""
	if !before.IsZero() {
		to = before.UTC().Format(layout)
	}

	switch {
	case from != "" && to != "":
		return from + "…" + to
	case from != "":
		return from
	default:
		return to
	}
}

// ── 报文解码 ──────────────────────────────────────

// decodeData 把信封里的 data 解到目标结构。
//
// data 为 null 或缺失时直接返回：上游有多个接口成功时 data 就是 null
// （提交验证、重发邮件、取消订单都是），把「没有 data」当成错误会让
// 这些接口永远失败。
func decodeData(op string, raw []byte, dst any) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return nil
	}
	if err := json.Unmarshal(trimmed, dst); err != nil {
		// 信封已经拆开、业务码是 200，说明上游认了这次请求并执行完了，
		// 只是字段对不上。这是「上游改了格式」，不是「结果未知」——
		// 前者要改代码，后者要重试，两者的处置完全不同。
		return fmt.Errorf("%w: %s 响应 data 解码失败: %v（%s）",
			ErrMalformedPayload, op, err, summariseBytes(trimmed))
	}
	return nil
}

// summariseBytes 把字节压成一行可读文本，用于错误信息。
//
// 与 HTTPClient.summarise 分开：那个要抹凭证，这个的输入已经是从
// 上游报文里摘出来的一小段，不需要再走一遍替换。
func summariseBytes(raw []byte) string {
	const maxLen = 256
	text := string(bytes.ToValidUTF8(raw, []byte("?")))
	text = strings.Join(strings.Fields(text), " ")
	if len(text) > maxLen {
		text = text[:maxLen] + "…"
	}
	return "data=" + text
}
