package foxssl

// 本文件是真实 FoxSSL 上游的**入站回调**：验签、解析、应答。
//
// ── 上游文档给的签名方案 ────────────────────────────
//
//   - 请求头：X-Webhook-Signature
//   - 算法：HMAC-SHA256，结果做 Base64
//   - 密钥：**API Key**（不是独立的 webhook secret）
//   - 签名对象：请求的**原始正文**
//   - 应答：8 秒内返回 HTTP 200 + {"status":"success"}
//
// ── 三件容易做错的事 ────────────────────────────────
//
//  1. **必须对原始字节验签。** 先反序列化再重新序列化会改变字节
//     （键顺序、空白、转义），导致验签必然失败；而为了让它通过而
//     实现成「重新序列化后再验签」，等于把验签变成一道装饰——
//     任何能构造出等价 JSON 的人都能伪造回调。所以签名与解析
//     合并在一个方法里，见 Client 接口的说明。
//
//  2. **比较要用 hmac.Equal。** 普通字符串比较的耗时随匹配前缀增长，
//     可以被逐字节猜出合法签名。
//
//  3. **certID 要用 int64 解析。** 文档示例值 142932802604630016
//     超出 float64 的精确整数范围，用 float64 会静默丢精度，
//     表现为「证书编号最后几位不对」——而它是对账时用的键。

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// signatureHeader 是签名所在的请求头。上游文档明确指定。
const signatureHeader = "X-Webhook-Signature"

// ackBody 是上游期望的成功应答体。
//
// 上游只在收到 HTTP 200 + 这个报文时才算「确认接收」，否则按 7 个阶段
// 重推（1 分钟内 3 次、5 分钟 5 次、1 小时 1 次……直到 24 小时 1 次）。
// 返回本服务的统一响应信封会被判为失败并触发无限重推。
const ackBody = `{"status":"success"}`

// SignatureHeader 返回签名所在的请求头名，供 HTTP 层取用。
//
// 导出是为了让接收回调的 handler 不必自己写一遍字符串字面量——
// 写错了的表现是「所有回调都验签失败」，而原因看起来像密钥不对。
func SignatureHeader() string { return signatureHeader }

// Sign 计算报文的签名：HMAC-SHA256 后取 Base64。
//
// 放在这里而不是 Mock 里：签名方案是**上游的**约定，真实客户端与 Mock
// 用的是同一套。放在 Mock 的文件里会让人以为它只服务于模拟投递。
//
// 传进来的必须是**原始字节**。先反序列化再重新序列化会改变字节
// （键顺序、空白、转义），算出来的签名必然与上游给的不一致。
func Sign(secret string, raw []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(raw)
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// Ack 返回上游期望的成功应答。
func (c *HTTPClient) Ack() Ack {
	return Ack{ContentType: "application/json", Body: []byte(ackBody)}
}

// wireNotification 是上游回调报文的格式。
//
// 两个事件类型共用这一个结构：证书签发（status 3004）带完整的证书信息，
// 证书取消（status 3005）只有前四个字段。用 omitempty 不必要——
// 解码方向不关心缺失字段，它自然留零值。
type wireNotification struct {
	// Auth 是上游的「身份验证对象」，文档标注为**已弃用，仅做保留**。
	// 解析出来但不使用，也不据此做任何判断：一个已弃用的字段
	// 随时可能变成空值或换个含义，把校验建立在上面对将来是个陷阱。
	Auth wireNotificationAuth `json:"auth"`
	// NotifyInfo 是真正的通知内容。
	NotifyInfo wireNotifyInfo `json:"notifyInfo"`
}

type wireNotificationAuth struct {
	AuthToken string `json:"authToken"`
	RandomStr string `json:"randomStr"`
}

// wireNotifyInfo 是通知内容。
//
// 前四个字段两个事件都有，其余只在签发事件里有。
type wireNotifyInfo struct {
	// OrderNo 是**上游**订单号。报文里没有平台订单号，
	// 平台据此反查本地订单。
	OrderNo string `json:"orderNo"`
	// CertID 是证书编号。**必须 int64**：文档示例值 142932802604630016
	// 超出 float64 的精确整数范围（2^53），用 float64 解析会静默丢精度。
	CertID int64 `json:"certID"`
	// Status 是状态码。签发事件是 3004（已签发），取消事件是 3005（已取消）。
	//
	// **注意它是证书状态码，不是订单状态码。** 两者取值区间不同
	// （订单 1001-1006，证书 3002-3006），订单域的映射表要能同时认。
	Status string `json:"status"`
	// StatusDesc 是状态的英文描述，文档示例为 "issued" / "canceld"
	// （后者是上游自己的拼写）。**纯展示用，不参与任何判定。**
	StatusDesc string `json:"statusDesc"`

	// 以下是签发事件独有的证书信息。平台当前不使用它们：
	// 收到签发事件后，订单域会调 DownloadCertificate 走同一条
	// 解析路径（PEM 解析、有效期计算、落库），而不是在这里
	// 再实现一遍。报文原文已经存在 Raw 里留档。
	SerialNumber   string   `json:"serialNumber"`
	CertContent    string   `json:"certContent"`
	MidCertContent string   `json:"midCertContent"`
	NotBefore      int64    `json:"notBefore"`
	NotAfter       int64    `json:"notAfter"`
	CommonName     string   `json:"commonName"`
	DomainNames    []string `json:"domainNames"`
}

// ParseNotification 验签并解析上游回调。
//
// 先验签再解析，顺序不可交换：先解析会让未经认证的报文进入 JSON 解码器，
// 把攻击面扩大到解码器的所有实现细节上。
//
// 验签密钥是 **API Key**（见 HTTPOptions.APIKey），不是独立的 webhook
// secret——上游文档给的示例就是 crypto.createHmac('sha256', apiKey)。
func (c *HTTPClient) ParseNotification(
	raw []byte, signature string,
) (*Notification, error) {
	// hmac.Equal 而不是 ==：普通字符串比较耗时随匹配前缀增长，
	// 可以被逐字节猜测出合法签名。
	expected := Sign(c.apiKey, raw)
	if !hmac.Equal([]byte(expected), []byte(strings.TrimSpace(signature))) {
		return nil, ErrInvalidSignature
	}

	var msg wireNotification
	if err := json.Unmarshal(raw, &msg); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrMalformedPayload, err)
	}

	// 两个必填字段。缺 orderNo 时平台无法定位订单，缺 status 时
	// 无法判断该做什么——两者都不是「可以跳过」的情况。
	//
	// 校验放在验签之后：验签之前连报文体都不该被信任，
	// 更不该根据它给出「缺字段」这种细节化的错误。
	if strings.TrimSpace(msg.NotifyInfo.OrderNo) == "" {
		return nil, fmt.Errorf("%w: 缺少 notifyInfo.orderNo", ErrMalformedPayload)
	}
	if strings.TrimSpace(msg.NotifyInfo.Status) == "" {
		return nil, fmt.Errorf("%w: 缺少 notifyInfo.status", ErrMalformedPayload)
	}

	return &Notification{
		// 上游回调报文**没有事件类型字段**，只有 status。这里留空而不是
		// 拿 statusDesc 顶上：statusDesc 是给人类看的文案（上游把它拼成了
		// "canceld"），把它用作幂等键的一部分是一个隐蔽的耦合——
		// 上游改一次文案，同一个事件就会被当成新事件重复处理。
		//
		// 留空不削弱幂等性：事件的幂等键里还有**报文哈希**，
		// 不同 status 的报文内容必然不同，见 order.buildEventKey。
		EventType:       "",
		UpstreamOrderNo: msg.NotifyInfo.OrderNo,
		Status:          msg.NotifyInfo.Status,
		CertID:          int64ToString(msg.NotifyInfo.CertID),
		// 真实回调里没有域名验证状态，也没有事件发生时间。
		// 两者都留空/零值：域名的验证状态由 ListDomains 拉取，
		// 乱序保护靠订单域的状态机而不是时间戳。
		Domains:    nil,
		OccurredAt: time.Time{},
		// 验签通过后的原始报文字节，用于落库留档与幂等键。
		Raw: raw,
	}, nil
}
