# FoxSSL 上游接口对照

本文是 FoxSSL（`api.foxssl.com`）真实上游接口的**事实记录**，来源是上游官方的
Postman 在线文档。适配层的实现落点在 `server/internal/upstream/foxssl/`，
本文只记事实与对照关系，不重复代码里的注释。

写这份文档的理由：报文层的每一条事实都只能从上游文档来，而文档是在线的、
会变的。把当时读到的东西固化到仓库里，才能在下一次上游改接口时看出**变了什么**。

> **平台与上游的边界**：适配器只做「平台结构 ⇄ 上游报文」的翻译，不做业务判断。
> 上游状态、域名状态、验证方式一律**以原文返回**，翻译成平台取值是订单域的职责。

---

## 1. 鉴权与响应格式

### 1.1 鉴权

每个接口的请求头都是：

```
apiKey: <your apiKey>
```

**自定义头，且不带前缀。** 不是 `Authorization: Bearer`。

适配器里对应 `defaultAuthHeader = "apiKey"`、`defaultAuthScheme = "-"`。

### 1.2 响应信封

**所有接口都用 HTTP 200 返回**，成败体现在报文体的业务码里：

```json
{ "code": 200, "msg": "ok", "data": { } }
```

失败时 `data` 为 `null`，`code` 是错误码，`msg` 是错误描述：

```json
{ "code": 6000, "msg": "账户余额不足", "data": null }
```

这条事实决定了两件事：

1. **只看 HTTP 状态会把每一次业务失败都当成成功**，然后拿着 `data` 里的
   `null` 往下走，表现为「下单成功了但订单号是空的」这类现象。
2. 错误分类要分两层：HTTP 状态层（网关、鉴权、限流）与业务码层
   （见 `http.go` 的 `classify` 与 `classifyBusiness`）。

`data` 为 `null` 是**正常成功**（提交验证、重发邮件、取消订单都是），
不能当成错误。

---

## 2. 接口清单

平台侧的适配方法在 `methods.go` 与 `notify.go`。

| 上游接口 | 方法 | 路径 | 平台侧 | 状态 |
| --- | --- | --- | --- | --- |
| 余额查询 | GET | `/finance/balance` | `Balance` | 已实现 |
| 下订单 | POST | `/certificates/id/:pNo` | `CreateOrder` | 已实现 |
| 获取域名列表 | GET | `/certificates/domains/:orderNo` | `ListDomains` | 已实现 |
| 请求验证域名 | PUT | `/certificates/verifyDomains/:orderNo` | `VerifyDomains` | **阻塞**（见 §7.1） |
| 查看订单状态 | GET | `/certificates/status/:orderNo` | `OrderStatus` | 已实现 |
| 下载证书 | GET | `/certificates/download/:orderNo` | `DownloadCertificate` | 已实现 |
| 重签订单 | POST | `/certificates/reissue` | `Reissue` | 已实现 |
| 更新域名验证方式 | PUT | `/certificates/dcv` | —— | 平台无此用例（见 §2.1） |
| 重发邮件 | PUT | `/certificates/reSendDcvEmail/:orderNo` | `ResendDcvEmail` | 已实现 |
| 重新生成 dcvToken | POST | `/certificates/dcv` | `RegenerateDcvToken` | 已实现 |
| 取消订单 | GET | `/certificates/cancel/:orderNo` | `CancelOrder` | 已实现 |
| 证书日志查询 | GET | `/certificates/ctLogs` | —— | 平台不需要 |
| 证书日志某证书详情 | GET | `/certificates/ctLogs/cert` | —— | 平台不需要 |
| 订单筛选 | GET | `/certificates/orders` | `FindOrder` | 已实现 |

### 2.1 「重新生成 dcvToken」与「更新域名验证方式」共用一个路径

两者都是 `/certificates/dcv`，**只有 HTTP 方法不同**：

- `POST` = 重新生成 dcvToken（按**单个域名**操作，请求体 `{orderNo, domain}`）
- `PUT` = 更新域名验证方式（请求体 `{approverEmailPrefix, newMethod, orderNo}`）

写错方法不会报错，会静默地做另一件事。适配器只实现 POST 那一个
（平台没有「改验证方式」的用例）；PUT 的请求体格式留在这里备查：

| 参数 | 必填 | 说明 |
| --- | --- | --- |
| `approverEmailPrefix` | 否 | `newMethod` 为 `email` 时必填，取值 `admin` / `webmaster` / `hostmaster` / `postmaster` / `administrator` |
| `newMethod` | 是 | 新验证方式，**必须小写** |
| `orderNo` | 是 | 订单号 |

---

## 3. 请求与响应的几个陷阱

### 3.1 `certID` 超出 float64 精度

文档示例值 `142932802604630016` 大于 2^53。用 `float64` 解析会静默丢精度，
表现为「证书编号最后几位不对」——而它是对账时用的键。

**所有 ID 字段一律 `int64`。**

### 3.2 `dnsNames` 有两种形态

获取域名列表的响应里，`dnsNames` 可能是数组，也可能是**裸字符串**：

```json
{ "dnsNames": ["1.test.com", "2.test.com"] }   // 常规域名
{ "dnsNames": "140.12.56.8" }                   // IP，文档说明「ip 单独」
```

用 `[]string` 直接解析，IP 订单会在解码这一步整体失败。而上游返回的是
HTTP 200 + code 200，所以错误表现成「报文格式错误」，把排查方向引到
「上游改了格式」上，真实原因却是「这一单是 IP 证书」。

### 3.3 时间戳一律是**毫秒**

`notBefore` / `notAfter` / `submitDateStamp` / `payDateStamp` 都是毫秒。
当成秒会让证书有效期落在 1970 年附近——不报错，只是让「证书已过期」
的判断全部反过来。

未支付订单的 `payDateStamp` 为 `0`。

### 3.4 `domainNames` 不含主域名

下订单与重签订单的 `domainNames` 是**逗号连接的字符串**，且**不包含主域名**
（主域名由上游从 CSR 里取）。没有子域名时整个字段不传。

重复传主域名会被当成重复的 SAN，返回 `6002`。

### 3.5 验证方式：请求一套取值，响应另一套

| 请求侧（小写） | 响应侧（大写常量名） | 平台取值 |
| --- | --- | --- |
| `file` | `HTTP_CSR_HASH` | `http_file` |
| `dns` | `CNAME_CSR_HASH` | —— **平台无对应取值** |
| `dns_txt` | `DNS_TXT` | `dns_txt` |
| `dns_cname` | `DNS_CNAME` | `dns_cname` |
| `email` | `EMAIL` | `email` |

把响应里的取值直接回填到请求里，上游会返回 `6004`「dcvMethod 不支持」。

平台的 `http_file` 与 `https_file` 都映射到上游的 `file`（上游不区分
用户把文件放在 http 还是 https 站点下），所以这个合并**不可逆**——
反向只能回到 `http_file`。

**上游的 `dns` 平台侧没有对应取值**：它只对非 certum 品牌开放，
而平台的 `dns_txt` / `dns_cname` 对应的上游取值只对 certum 品牌开放。
非 certum 品牌要提交 DNS 验证时，当前产品配置无法表达，见 §7.3。

### 3.6 `dateCreated` 的区间用省略号分隔

订单筛选接口的 `dateCreated` 接受两种形态：

```
2022-05-19T12:00:00
2022-05-19T12:00:00…2022-05-20T13:00:00
```

区间用 **`…`（U+2026）** 分隔。用短横线或逗号是无效的，而上游对无效参数
返回 `11001`「解析 url query 错误」——一个看不出是「时间格式不对」的错误。

---

## 4. 状态码字典

上游的状态是**四条独立的线**，平台不做合并（合并会让运营无法还原上游的真实情况）。

### 4.1 订单状态（`orderStatus`）

| 码 | 含义 | 平台映射 |
| --- | --- | --- |
| 1001 | 未支付（订单未支付，或重签的差额未支付） | **不映射** |
| 1002 | 已支付 | `issuing` |
| 1003 | 支付成功，但提交到 CA 申请订单异常，请联系客服 | **不映射** |
| 1004 | 已取消 | `cancelled` |
| 1006 | 支付成功，但提交到 CA 申请订单超时，请联系客服 | **不映射** |

### 4.2 证书状态（`certStatus`）

| 码 | 含义 | 平台映射 |
| --- | --- | --- |
| 3002 | 已支付，等待签发 | `issuing` |
| 3003 | 支付成功，但提交到 CA 申请订单异常，请联系客服 | **不映射** |
| 3004 | 已签发 | `issued` |
| 3005 | 已取消 | `cancelled` |
| 3006 | 支付成功，但提交到 CA 申请订单超时，请联系客服 | **不映射** |

### 4.3 重签状态（`reissueStatus`）

| 码 | 含义 | 平台映射 |
| --- | --- | --- |
| 空 | 还未申请重签 | 不映射 |
| 5001 | 重签申请中 | `issuing` |
| 5002 | 重签申请成功 | `issued` |
| 5003 | 重签申请失败 | `failed` |
| 5004 | 重签需要补差价但没有补交 | **不映射** |

> 上游的「查看订单状态」接口里**没有**独立的 `reissueStatus` 字段，
> 只有一个 `isReSignOrder`（Y/N，表示这一单是不是重签产生的）。
> 语义不同，不能互相顶替。

### 4.4 域名验证状态（`status`，在域名列表里）

| 码 | 含义 | 平台映射 |
| --- | --- | --- |
| 2001 | 未验证 | `pending` |
| 2002 | 已验证 | `verified` |

上游只有两态，分不出平台的 `pending`（尚未提交）与 `verifying`
（已提交待校验），取更保守的 `pending`——不声称「正在验证中」。

### 4.5 OV / EV 验证状态

| 码 | 含义 |
| --- | --- |
| 2000 | 不需要此验证 |
| 2001 | 未验证 |
| 2002 | 已验证 |

### 4.6 为什么「需要人工介入」的状态一律不映射

1003 / 1006 / 3003 / 3006 的文档说明都是「**请联系客服**」，
5004 是「需要补差价但没有补交」，1001 是「未支付」（它同时出现在重签场景，
退款动作与首单不同）。

这几个状态不会自愈，而平台能做的自动动作只有「退款」。在不知道上游会不会
人工修好的情况下退款，等于替上游做了决定——而退款之后证书又被签出来时，
只有对账才会发现。

不映射的后果是订单停在原地、钱冻着，人工介入后仍可推进；
映射错了的后果是钱已经退出去、证书又签出来了。

---

## 5. 错误码

`code` 的取值按区间分段。平台侧的分类原则见 `http.go` 的 `classifyBusiness`：
**判据是「上游到底有没有执行这次操作」**，只有拿不准的那几个归入「结果未知」。

### 5.1 平台直接处理的那几个

| 码 | 含义 | 平台分类 |
| --- | --- | --- |
| 200 | 成功 | 正常返回 `data` |
| 500 | 服务端错误 | **结果未知**（`ErrUnavailable`） |
| 6000 | 账户余额不足 | `ErrPlatformBalance`（平台侧问题，不重试） |
| 6010 | 订单号错误 | `ErrOrderNotFound` |
| 6801 | 订单生成中 | **结果未知**（`ErrUnavailable`） |

> 6000 是**平台在上游的预存余额不足**，不是用户的余额不足。重试不会让余额
> 变多，而把它当成「结果未知」会让订单一直冻着等补偿，补偿用的还是那个
> 不够的余额。它与 `ErrUnauthorized` 同类：平台侧问题，运维要据此告警。

其余所有业务码都是「上游在业务逻辑里明确拒绝」，重试一百次还是同一个结果。

### 5.2 完整错误码表

```
// 下订单接口
SuccessCode = 200                    // success
ServerError = 500                    // 服务端错误
BalanceNotEnough         = 6000      // 账户余额不足
NoProduct                = 6001      // 暂未提供相应产品
ParamFormatInvalid       = 6002      // 参数类型或参数名有误
YearInvalid              = 6003      // 年限不支持
DcvMethodInvalid         = 6004      // dcvMethod 不支持
GlobalsignNoEmail        = 6005      // globalsign 暂不支持 email 域名验证方式
CsrInvalid               = 6006      // csr 不合法
UnSupportIP              = 6007      // 不支持 IP
DomainInvalid            = 6008      // 域名不合法
UnSupportWildCard        = 6009      // 不支持通配
OrderNoInvalid           = 6010      // 订单号错误
RecommitDenied           = 6011      // 此订单不满足重新提交的条件
OrderOverYears           = 6012      // 订单已超过有效期
FreeCertNoResign         = 6013      // 免费证书不能重签
CertIssueNotDoneNoResign = 6014      // 证书未颁发完成不能重签
CsrNoOrgInvalid          = 6015      // csr 必须包含企业信息
OrderPayedAlready        = 6016      // csr 订单已支付
// 取消订单
ReCancelDenied            = 6100     // 不能重复取消订单
ReSignedCancelDenied      = 6101     // 重签成功的订单不允许取消
ReChargeCancelDenied      = 6102     // 不支持取消充值订单
FreeCertCancelDenied      = 6103     // 免费证书不允许取消
IssuedAfter30CancelDenied = 6104     // 证书颁发 30 天后不能退款
// 下载证书
CertUnIssued = 6200                  // 证书未签发完成
// 修改验证方式
UpdateDcvUnSupported   = 6300        // 此产品不支持修改验证方式
IPUpdateDcvUnSupported = 6301        // ip 不允许修改验证方式
DomainNotExist         = 6302        // 域名不存在，请重试
DomainHasBeenVerified  = 6303        // 域名已验证，无需更改验证方式
CanNotUpdateSameDcv    = 6304        // 不能更换相同的验证方式
NoNeed2VerifyDcv       = 6305        // 此证书无需请求验证
LackOfOrderInfo        = 6306        // 缺少必要订单信息
// 重新生成 dcvToken
ReGenDcvTokenUnSupport = 6400        // 不支持此品牌
DomainNotFound         = 6401        // 未找到此域名
// 删除域名
DeleteDomainDenied    = 6500         // 域名已经验证，不允许删除
DeleteDomainUnSupport = 6501         // 此产品不支持删除域名，请联系客服处理
// 获取域名列表
PageNumError  = 6600                 // 页码输入有误
PageSizeError = 6601                 // 记录数输入有误
// 重发邮件
ReSendEmailFailed     = 6702         // 重发邮件失败
ReSendEmailNotSupport = 6703         // 不支持邮件重发
// 订单状态
TryReCommit   = 6800                 // 订单处理失败，请尝试重新提交
OrderCreating = 6801                 // 订单生成中
// 下载证书
CertCancelled = 6900                 // 证书已退款，被取消使用
// 联系人信息参数错误
ContactNull      = 7000              // 联系人不能为空
FirstNameInvalid = 7001              // FirstName 错误
LastNameInvalid  = 7002              // LastName 错误
PositionInvalid  = 7003              // Position 错误
EmailInvalid     = 7004              // Email 错误
TelephoneInvalid = 7005              // 电话错误
// 企业信息参数错误
OrgInfoNull                    = 7100 // 企业信息不能为空
OrgNameInvalid                 = 7101 // OrgName 错误
CreditCodeInvalid              = 7102 // CreditCode 错误
CountryInvalid                 = 7103 // Country 错误
ProvinceInvalid                = 7104 // Province 错误
LocalityInvalid                = 7105 // Locality 错误
AddressInvalid                 = 7106 // Address 错误
PostalCodeInvalid              = 7107 // PostalCode 错误
JoiCountryInvalid              = 7108 // JoiCountry 错误
JoiProvinceInvalid             = 7109 // JoiProvince 错误
JoiLocalityInvalid             = 7110 // JoiLocality 错误
RegistAddrInvalid              = 7111 // RegistAddr 错误
DateOfIncorporationInvalid     = 7112 // DateOfIncorporation 错误
OrgExitsAlready                = 7113 // 企业信息已存在
OrgNotExist                    = 7114 // 公司不存在
ProfileAddSuccessCertsExceeded = 8000 // 任务添加成功，但证书数量已到达上限
ProfileAddSuccessTaskRunError  = 8001 // 任务添加成功，立即执行失败
// 生成 csr
KeyCurveInvalid           = 9000      // 请检查加密曲线参数格式
KeySizeInvalid            = 9001      // 不支持您输入的加密位数
EncryptionHashSignInvalid = 9002      // encryption 与 hashSign 参数格式错误
// 多语言另加
PayAimInvalid        = 10000          // 支付目的错误
ReIssueErrToReCommit = 10001          // 重签出错，联系客服
// ct log
ParseURLError   = 11000               // 解析 url 错误
ParseQueryError = 11001               // 解析 url query 错误
ParamNullError  = 11002               // 参数为空
```

---

## 6. 回调（PUSH 通知）

### 6.1 开启方式

下单时给 `notifyUrl` 赋一个合法的回调地址，上游在**签发**与**取消**两种
操作完成时推送。不传则上游不推送，平台只能靠轮询。

### 6.2 签名

| 项 | 值 |
| --- | --- |
| 请求头 | `X-Webhook-Signature` |
| 算法 | HMAC-SHA256，结果做 Base64 |
| **密钥** | **API Key**（不是独立的 webhook secret） |
| 签名对象 | 请求的**原始正文** |

文档给的示例：`apiKey` 为 `apiKey123`、`rawBody` 为 `data_test` 时，
签名为 `ATyqaNp4B+lhM/xFRKyk4//VdVAbxYNbPvrlQrbbMj4=`。

两条必须守住的实现细节：

1. **对原始字节验签。** 先反序列化再重新序列化会改变字节（键顺序、空白、
   转义），导致验签必然失败；而为了让它通过而实现成「重新序列化后再验签」，
   等于把验签变成一道装饰。
2. **比较用 `hmac.Equal`。** 普通字符串比较的耗时随匹配前缀增长，
   可以被逐字节猜出合法签名。

### 6.3 应答

**8 秒内**返回 HTTP 200 与：

```json
{ "status": "success" }
```

`Content-Type: application/json`。

返回本服务的统一响应信封会被判为**接收失败**并触发重推。

### 6.4 重推策略

未收到确认时按 7 个阶段重推，任一阶段确认成功即停止：

| 阶段 | 频率 | 次数 |
| --- | --- | --- |
| 一 | 实时，立即重试 | 3 |
| 二 | 每 1 分钟 | 5 |
| 三 | 每 5 分钟 | 5 |
| 四 | 每 1 小时 | 1 |
| 五 | 每 2 小时 | 1 |
| 六 | 每 12 小时 | 1 |
| 七 | 每 24 小时 | 1 |

七阶段仍失败则本次通知失效，上游不再保留推送任务，只能通过 API 获取证书信息。

### 6.5 报文结构

```json
{
  "auth": { "authToken": "c55eed1e30e4f3cb36de07c9f76d50c8", "randomStr": "success" },
  "notifyInfo": {
    "orderNo": "2021011221355666",
    "certID": 142932802604630016,
    "status": "3004",
    "statusDesc": "issued"
  }
}
```

签发事件（`status` = 3004）额外带完整的证书信息：`serialNumber`、
`certContent`、`midCertContent`、`notBefore`、`notAfter`、`commonName`、
`domainNames[]`、`sha1`、`sha256`、`issuerCommonName`、`issuerCountry`、
`issuerOrg`、`signatureAlgo`、`encryption`、`keyLength`、`keyCurve`。

取消事件（`status` = 3005）只有前四个字段。

**三件报文里没有的东西**，实现时不能编：

1. **没有平台订单号**，只有上游 `orderNo`，平台据此反查本地订单。
2. **没有事件类型字段**，只有 `status`。平台侧的 `EventType` 留空，
   幂等键里的报文哈希已经承担了区分不同事件的职责。
3. **没有域名验证状态**，也没有发生时间。

`auth` 字段文档标注为「**已弃用，仅做保留**」，解析出来但不使用，
也不据此做任何判断。

`statusDesc` 是给人看的文案（上游把取消拼成了 `canceld`），
**不参与任何判定**。

> 签发事件里带着完整的证书内容，但平台收到后仍会调一次下载接口。
> 这是刻意的：走同一条解析路径（PEM 解析、有效期计算、落库），
> 而不是在回调处理里再实现一遍。

---

## 7. 还没闭合的缺口

### 7.1 提交域名验证的请求体在文档里缺失

文档给了路径（`PUT /certificates/verifyDomains/:orderNo`）与响应
（`{"code":200,"msg":"请求成功","data":null}`），但**没有参数表，
也没有示例请求体**——Postman 集合里这个条目连 `originalRequest` 都没有。

适配器因此**不实现**它（调用返回 `ErrNotSupported`）。猜一个
`{"domains":[...]}` 发出去，最坏的情况是上游不报错但什么都没做：
用户点了「提交验证」，界面显示成功，而上游根本没开始校验。

**需要向上游确认参数格式后才能实现。**

### 7.2 `notifyUrl` 还没有接线

订单域的 `submit` 传的是空串，而上游只在收到 `notifyUrl` 时才会推送。
也就是说真实上游环境下回调永远不会到达。

这块与上游报文无关，是平台侧的接线工作：需要把回调路由地址
（`AppBaseURL` + 回调路径）拼进下单请求。

### 7.3 上游的 `dns` 平台侧没有对应取值

见 §3.5。非 certum 品牌要提交 DNS 验证时，当前的产品配置无法表达。
需要给 `rules.DcvMethod` 增加一个取值（或调整产品配置的语义），
并同步契约与前端。

### 7.4 文件验证的内容字段不在域名列表接口里

上游只返回 `fileDcvPath`（文件名固定为 `gsdv.txt`）与 `hashValue`，
没有单独的文件内容字段。适配器**刻意不填** `FileContent`：

- 填 `hashValue` 是一个把握较高但未经验证的推断；
- 猜错的后果是用户拿着错误的内容去配、验证不通过且难以排查；
- 留空的后果只是文件验证不出现在可用方式里，用户改用 DNS 或邮件。

**联调时确认后补上。**

### 7.5 上游没有给限流约定

文档里没有 QPS 上限、没有 429 的说明。如果联调时遇到限流，
除了适配器的退避重试，调用侧还要做节流。

### 7.6 上游状态同步会把本地的「验证中」打回「待验证」

上游的域名状态只有 2001（未验证）与 2002（已验证），分不出平台的
`pending` 与 `verifying`。用户提交验证后再刷新材料，本地的 `verifying`
会被上游的 2001 覆盖成 `pending`。

### 7.7 重签差价只记日志，不进结算

上游的重签响应里有 `priceDiff`（重签差价，单位分）。平台侧的重签规则是
「有效期内免费重签」，不向用户收取这笔钱，所以适配器只打一条告警日志。

如果上游对某些产品实际收了差价，平台侧的结算里完全看不出来。

---

## 8. 产品表（部分）

上游产品编号（`pNo`）与平台产品 ID 不是一回事。下单时它出现在**请求路径**里
（`/certificates/id/:pNo`）。

文档给出的产品表有 40 多项，包含品牌、类型、级别、重签注意事项、
CSR 算法支持、是否可调用「重新生成 dcvToken」四类信息。与平台逻辑相关的几条：

- **所有 EV 证书不支持 IP 和通配**；只有 OV 支持 IP。
- **所有品牌的含通配域名的证书都不再支持文件验证**（2021-11-30 起）。
- **globalsign 与 alphassl 不支持邮件验证**，也不允许修改验证方式。
- **certum 额外支持 `dns_txt` 与 `dns_cname`**（2022-08-26 起）。
- 免费证书（如 BitCert）不支持重签，也不能取消。
- `year` 的取值范围按品牌不同：globalsign / alphassl 仅 1 年；
  sectigo / positivessl 支持 1-5 年；geotrust / rapidssl / digicert /
  securesite / thawte / securesitechina / geotrustchina 支持 1-6 年。

完整的 40+ 行产品表以在线文档为准，本文不复制——它变动频繁，
而平台侧只需要知道「产品编号从哪来、有哪些硬约束」。
