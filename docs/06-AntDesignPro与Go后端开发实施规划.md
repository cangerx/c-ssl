# SSL 证书平台：Ant Design Pro + Go 后端开发实施规划

> 版本：v1.0  
> 编制日期：2026-09-16  
> 适用项目：c-ssl SSL 证书签发与销售平台

## 1. 项目目标

建设一套面向个人站长、企业客户和运营人员的 SSL 证书销售与签发平台，覆盖：

- 会员注册、登录和权限管理
- 用户钱包、充值、支付回调和资金流水
- SSL 产品目录、产品规则和价格策略
- CSR 提交、证书下单和域名验证
- FoxSSL 订单状态同步、Webhook 接收和证书下载
- 订单取消、重签、续期提醒和异常处理
- Ant Design Pro 管理后台

FoxSSL 仅提供上游证书签发能力，不提供终端会员、钱包、支付、权限和订单归属能力。因此平台必须将“自建业务系统”和“FoxSSL 上游适配层”严格分离。

## 2. 总体技术架构

```text
用户端 Web（Next.js / React）       运营后台（Ant Design Pro）
             │                              │
             └──────────────┬───────────────┘
                            │ HTTPS / REST / OpenAPI
                        Go API 服务
                            │
        ┌───────────────────┼───────────────────┐
        │                   │                   │
      MySQL               Redis             Worker/Cron
        │                   │                   │
   业务数据与账本       幂等/限流/队列       轮询/通知/对账
                            │
                    FoxSSL / 支付平台
```

### 2.1 技术选型

| 层级 | 选型 | 说明 |
|---|---|---|
| 用户站 | Next.js + React + TypeScript | 产品展示、SEO、会员和下单 |
| 管理后台 | Ant Design Pro + Umi + TypeScript | ProTable、ProForm、Access 权限 |
| 后端 | Go 1.22+ | 并发、任务处理和单二进制部署 |
| HTTP | chi | 轻量、贴近标准库、便于中间件治理 |
| 数据库 | MySQL 8 | 事务、行锁和账本一致性 |
| DB 访问 | database/sql + sqlc | 类型安全，避免 ORM 隐式行为 |
| 缓存/队列 | Redis 7 | 幂等、限流、短期状态和任务队列 |
| 迁移 | golang-migrate | 版本化 SQL |
| 日志 | log/slog | 结构化日志和 Trace ID |
| 部署 | Docker Compose → Kubernetes | 先控制复杂度，后续平滑扩展 |

Ant Design Pro 用于运营管理后台；用户端建议使用 Next.js，而不是把 Pro 直接当作商城首页框架。

## 3. 仓库与工程结构

```text
c-ssl/
├── apps/
│   ├── web/                       # 用户端：产品、会员、下单、订单
│   └── admin/                     # Ant Design Pro 管理后台
├── packages/
│   ├── api-types/                 # OpenAPI 生成的 TypeScript 类型
│   ├── product-rules/             # 产品元数据与前端展示规则
│   └── ui/                        # 公共 React 组件
├── server/
│   ├── cmd/
│   │   ├── api/main.go
│   │   ├── worker/main.go
│   │   └── cron/main.go
│   ├── internal/
│   │   ├── config/
│   │   ├── server/
│   │   │   ├── router.go
│   │   │   ├── response.go
│   │   │   └── middleware/
│   │   ├── domain/
│   │   ├── auth/
│   │   ├── user/
│   │   ├── wallet/
│   │   ├── recharge/
│   │   ├── payment/
│   │   ├── product/
│   │   ├── order/
│   │   ├── certificate/
│   │   ├── webhook/
│   │   ├── admin/
│   │   ├── notify/
│   │   ├── queue/
│   │   ├── task/
│   │   └── upstream/foxssl/
│   ├── migrations/
│   ├── go.mod
│   └── go.sum
├── deploy/
├── docs/
└── docker-compose.yml
```

依赖方向必须保持：

```text
handler → service → repository → database
              ↓
       foxssl / payment interface
```

Handler 只负责 HTTP 参数和响应；业务规则放在 service；FoxSSL 和支付平台必须通过接口注入，方便 Mock 和替换供应商。

## 4. 核心业务域

### 4.1 会员与认证

- 邮箱注册、登录、刷新 Token
- 密码使用 Argon2id 或 bcrypt，不保存明文
- JWT Access Token + Refresh Token
- 登录限流、失败次数限制和设备记录
- 用户、企业主体、联系人资料分离
- 管理端使用独立管理员账号和 RBAC

管理员角色建议：超级管理员、运营、财务、客服、技术运维、审计只读。

### 4.2 钱包和充值

用户充值和 FoxSSL 商户余额是两条独立资金链路，不能混用。

平台钱包最少包含：

```text
wallet_accounts       用户可用余额、冻结余额
wallet_ledger         所有加款、扣款、解冻、退款流水
recharge_orders       用户充值订单
payment_transactions  支付渠道原始流水
```

金额统一使用 `int64` 分。任何余额变化都必须在事务中写入账本，严禁直接覆盖余额。

证书下单流程（Phase 2 实现时确定，见下方说明）：

```text
校验用户余额与产品规则
  ↓
事务内冻结零售价（金额与结算额完全相等，不存在预估）
  ↓
调用 FoxSSL 下单（网络 I/O，不在事务内）
  ├─ 成功：冻结转为实扣（settle），记录上游订单号与上游成本价
  └─ 失败：解冻（unfreeze），订单置 failed 并记录失败原因
```

**关于「按实际 cost 结算，多退少补」的修正。** 初版规划写的是冻结预估价、
按上游返回的真实 `cost` 多退少补。实现时改为**按零售价扣款**，理由：

1. 上游 `cost` 是平台付给 FoxSSL 的成本，不是用户的应付金额。用它决定用户扣款，
   等于把平台的采购成本直接透传给用户，定价体系失去意义。
2. 用户在下单页看到的价格必须等于实际扣款。按 `cost` 扣款会让用户看到的价与
   扣的价不一致，差额方向还不确定。
3. `cost` 高于零售价时，冻结额可能不足以实扣，订单会卡在「上游已下单、
   用户没扣够钱」的中间态，需要额外设计补扣流程——而补扣又可能失败。
   按零售价扣款则冻结额恒等于结算额，这个中间态在结构上不存在。

上游返回的 `cost` 仍然记录到订单上（`cost_price` 字段），供运营对账与毛利分析；
它不参与任何用户余额计算。

冻结与结算的具体时序见 `docs/仓库与目录结构设计.md` 的订单域章节。
注意失败路径用 **unfreeze** 而不是「先扣款再退款」：后者会在账本里留下
一对 settle/refund 流水，把失败订单算进平台收入，污染财务报表。

### 4.3 产品和规则

产品由后端管理，前端读取产品元数据。产品字段包括：

- 产品 ID、品牌、DV/OV/EV 等级
- flex / 非 flex
- 单域名、多域名、通配符、IP 支持
- 支持年限
- RSA / ECC 支持
- 支持的 DCV 方式
- 是否支持重签、取消和重新生成 dcvToken
- 成本价、零售价、上下架状态

后端必须统一校验以下规则：

1. EV 不支持 IP 和通配符。
2. 含通配符的证书不支持文件验证。
3. GlobalSign、AlphaSSL 不支持邮件验证。
4. Certum 支持 `dns_txt`、`dns_cname`。
5. OV/EV 必须提交完整企业信息。
6. 不同品牌支持的年限和 CSR 算法不同。
7. 免费证书不能重签和取消。

**这 7 条全部在后端校验，不依赖前端。** 下单时校验 1、2、5、6、7
（`product/rules` 的 `Normalize` / `Validate` 与 `order.Service.Create`）；
提交域名验证时校验 2、3、4（`order.Service.declaredDcvMethods`）。

域名验证这一处值得单独说明，因为它同时受两个来源约束，而两个来源含义不同：

- **产品声明的能力**（`rules.AllowedDcvMethods`）——「我们卖的是什么」。
  规则 2、3、4 在这里落地：AlphaSSL 的订单提交邮件验证会被拒。
  判据取订单的**全部**域名，而不是本次提交的那些：规则 2 约束的是整张证书，
  只看本次提交的域名会让「只提交非通配符那个域名」绕过它。
- **上游实际准备好的验证材料**——「现在实际能做到什么」。上游没返回邮件地址，
  就说明它没为这个订单准备邮件验证，提交了只会换回一个上游报错。
  判据取**本次提交的**域名：验证方式逐域名生效，同一张证书上 A 走 DNS、
  B 走文件是正常用法，按整张证书取交集会把这种用法一并拒掉。

下发前端的 `availableMethods` 是两者的交集，且与「提交什么会被接受」严格一致——
界面上列出了、用户选了却被 400 拒掉，是这个域最让人费解的故障，
所以这个字段只能由 Service 统一算好（`order.Service.withAllowedMethods`），
handler 不得另行推导。

产品下架后不再施加产品侧约束，退回到只按上游材料判断：用户已经付过钱、
上游也已备好材料，卡住验证只会让一张已付款的订单烂在那里。这与取消/重签的
保守方向相反——那两件事判断错了会多花钱，这里判断错了只是让一个合法的
验证提交不了。

### 4.4 订单和证书

平台同时保留三类编号：

```text
local_order_no       平台订单号
upstream_order_no    FoxSSL 订单号
cert_id              FoxSSL 证书编号
```

平台状态建议：

```text
pending_payment
paid
submitting
waiting_dcv
issuing
issued
cancelled
failed
```

同时保存 FoxSSL 原始状态字段，便于运营排查和对账：

```text
upstream_order_status
upstream_cert_status
upstream_prepare_status
upstream_reissue_status
```

### 4.5 DCV 域名验证

后端封装以下 FoxSSL 接口：

```text
GET  /certificates/domains/:orderNo
PUT  /certificates/verifyDomains/:orderNo
PUT  /certificates/dcv
POST /certificates/dcv
PUT  /certificates/reSendDcvEmail/:orderNo
```

用户端需要展示 DNS 记录、文件路径、邮件地址、验证状态和一键复制按钮。

FoxSSL 返回的 `fileDcvPath` 可能包含 `{FQDN}`，后端必须逐个域名替换；不能把供应商原始模板直接交给用户。

## 5. FoxSSL 适配层

业务服务只依赖接口，不直接依赖 HTTP 实现。以下为 `client.go` 里的实际接口（已按上游官方文档校正）：

```go
type Client interface {
    Name() string
    Balance(ctx context.Context) (*Balance, error)
    // 不幂等：重复调用会在上游产生第二张证书并扣第二次钱。
    CreateOrder(ctx context.Context, req CreateOrderRequest) (*CreateOrderResponse, error)
    OrderStatus(ctx context.Context, upstreamOrderNo string) (*OrderStatus, error)
    ListDomains(ctx context.Context, upstreamOrderNo string) (*DomainsResponse, error)
    // 真实上游该接口请求体在文档里缺失，调用返回 ErrNotSupported（见下）。
    VerifyDomains(ctx context.Context, req VerifyDomainsRequest) error
    ResendDcvEmail(ctx context.Context, upstreamOrderNo string, domains []string) error
    // 平台按整单操作，上游按单域名操作，适配器负责翻译。
    RegenerateDcvToken(ctx context.Context, upstreamOrderNo string) (*DomainsResponse, error)
    DownloadCertificate(ctx context.Context, upstreamOrderNo string) (*Certificate, error)
    Reissue(ctx context.Context, req ReissueRequest) error
    CancelOrder(ctx context.Context, upstreamOrderNo string) error
    // CreateOrder 不幂等的必要配套：结果未知时先反查，再决定是否重试。
    FindOrder(ctx context.Context, req FindOrderRequest) ([]OrderSummary, error)
    // 验签与解析合并为一个方法，让「不验签就解析」这条路径不存在。
    ParseNotification(raw []byte, signature string) (*Notification, error)
    Ack() Ack
}
```

文件划分（按职责，而不是按 Mock / 真实分文件——两者共用报文定义）：

```text
internal/upstream/foxssl/client.go     接口、请求/响应模型、哨兵错误
internal/upstream/foxssl/http.go       传输层：鉴权头、信封拆解、重试、错误分类
internal/upstream/foxssl/methods.go    12 个出站方法的报文映射
internal/upstream/foxssl/wire.go       上游报文定义（路径、请求体、响应体）
internal/upstream/foxssl/notify.go     入站回调：验签、解析、应答体
internal/upstream/foxssl/mock.go       开发与自动化测试用的模拟上游
```

接口的完整报文对照、状态码字典与错误码表见 `docs/07-FoxSSL上游接口对照.md`。

三条容易踩错、已经写进代码注释的约定：

1. **响应是 HTTP 200 + 业务码信封。** 上游把业务失败也放在 HTTP 200 里，成败在报文体的 `code`。所以错误分类分两层：HTTP 状态层（`classify`）与业务码层（`classifyBusiness`），只看 HTTP 状态会把每次业务失败当成功。
2. **回调验签的密钥就是 API Key**，不是独立的 webhook secret。`FOXSSL_WEBHOOK_SECRET` 只服务于 Mock 上游，仅在 `FOXSSL_PROVIDER=mock` 时必填。
3. **`apiKey` 是自定义请求头、取值裸放**（不带 `Bearer` 之类前缀），且不能与 `Authorization` 同时出现。它只能存在服务端环境变量或密钥管理系统中，不能进入浏览器、前端构建产物和日志；错误信息里出现该值时会被替换成掩码。

## 6. Webhook 与任务系统

FoxSSL Webhook：

- 请求头：`X-Webhook-Signature`
- 算法：HMAC-SHA256 + Base64
- 签名密钥：**就是 API Key**（上游文档明确如此），不是独立的 webhook secret
- 签名输入：原始请求 body 字节（先反序列化再重新序列化会改变字节，导致验签必然失败）
- 8 秒内返回 `200` 和 `{"status":"success"}`
- 按 `orderNo + status + payloadHash` 幂等处理

上游回调报文里**没有事件类型字段**，只有 `status`。幂等键因此不含事件类型——不要拿 `statusDesc` 顶上，那是给人类看的文案（上游把它拼成了 `canceld`）。报文里还有一个已弃用的 `auth` 字段，只解析不使用。

Webhook 接收接口只做验签、落库和投递任务，不在请求中执行耗时的证书下载或复杂业务。

Worker 负责：

- Webhook 事件处理
- 证书详情下载
- 用户通知
- 失败重试

Cron 负责：

- Webhook 兜底轮询：**先 `FindOrder` 反查再决定是否重试**，不能按商户订单号直接重下单（上游不接受商户标识，重复提交就是真买第二张证书）
- 上游余额检查
- 用户钱包与订单对账
- 长时间未验证提醒
- 证书到期提醒

## 7. API 规范

统一响应格式：

```json
{
  "code": 0,
  "message": "success",
  "data": {}
}
```

核心接口：

```text
POST /api/v1/auth/register
POST /api/v1/auth/login
GET  /api/v1/me

GET  /api/v1/products
GET  /api/v1/products/:id

POST /api/v1/recharge/orders
GET  /api/v1/recharge/orders
POST /api/v1/payments/webhook/{channel}

GET  /api/v1/orders
POST /api/v1/orders
GET  /api/v1/orders/:orderNo
POST /api/v1/orders/:orderNo/cancel
POST /api/v1/orders/:orderNo/reissue

GET  /api/v1/orders/:orderNo/domains
POST /api/v1/orders/:orderNo/domains/verify
POST /api/v1/orders/:orderNo/domains/resend-email
POST /api/v1/orders/:orderNo/domains/regenerate-token
GET  /api/v1/orders/:orderNo/certificate

POST /api/v1/orders/:orderNo/mock/issue

POST /api/v1/webhooks/foxssl

GET  /api/v1/admin/dashboard
GET  /api/v1/admin/orders
GET  /api/v1/admin/users
GET  /api/v1/admin/finance/ledger
```

OpenAPI 作为唯一接口契约：Go 服务校验接口实现，Ant Design Pro 和用户站生成 TypeScript 类型，避免前后端字段漂移。

关于支付回调路径的一点细化（Phase 1 实现时确定）：回调地址带上了渠道标识
`POST /api/v1/payments/webhook/{channel}`，而不是单一的无参路径。
原因有两个：渠道回调报文里没有「我是哪个渠道」这种字段（解析报文之前
就需要知道用哪个密钥验签），以及真实渠道各自要求在商户后台配置回调地址——
每个渠道一条独立 URL 才配得清楚。开发环境另有一个只在
`APP_ENV=development` 时注册的 `POST /api/v1/payments/mock/notify`，
用于手工模拟渠道投递回调。

订单域有一个同类的开发辅助接口：只在 `FOXSSL_PROVIDER=mock` 时注册的
`POST /api/v1/orders/:orderNo/mock/issue`。它把**上游侧**的订单推进到已签发，
**不改动本地订单**——本地状态仍由上游回调驱动。这是刻意的：如果它顺手把本地
订单也改成 `issued`，端到端冒烟就会绕开回调路径，而回调（验签、幂等、乱序、
资金释放）恰恰是最需要被完整验证的一段。真实 CA 不会因为一个 HTTP 请求就
立刻签发证书，所以这个能力只可能属于模拟上游；它因此不是 `foxssl.Client`
的方法，而是模拟客户端自己的方法。

## 8. Ant Design Pro 后台页面

### 8.1 经营驾驶舱

- 今日订单数、销售额、充值额
- 已签发、待验证、异常订单
- FoxSSL 上游余额
- 用户钱包负债总额
- 近 30 天订单和收入趋势

### 8.2 订单管理

- ProTable 查询、筛选和分页
- 按本地单号、上游单号、域名、品牌、状态筛选
- 查看上游原始状态
- 同步订单状态
- 重发验证邮件
- 取消、重签和下载证书
- 异常订单人工处理

### 8.3 产品管理

- 产品上下架
- 多年价格
- 成本价和零售价
- 产品推荐标签
- 支持算法、年限、验证方式配置

### 8.4 财务管理

- 充值订单
- 支付流水
- 钱包账本
- 退款记录
- 上游余额与消费对账
- 人工调账审批

人工调账必须执行：

```text
提交申请 → 财务审核 → 主管复核 → 写入账本 → 审计记录
```

### 8.5 运维和审计

- DCV 待验证大盘
- Webhook 接收记录
- FoxSSL API 调用日志
- 上游余额告警
- 管理员操作审计
- Trace ID 检索

## 9. 数据库核心表

```text
users
admin_users
roles
permissions
user_roles

wallet_accounts
wallet_ledger
recharge_orders
payment_transactions

products
product_prices
product_rule_versions

certificate_orders
order_domains
order_contacts
order_orgs
certificates

webhook_events
upstream_api_logs
notifications
audit_logs
```

关键约束：

- 钱包流水不可更新，只能追加和冲正。
- 充值支付回调使用渠道交易号唯一索引。
- FoxSSL 订单号唯一索引。
- Webhook 事件使用事件哈希或供应商订单状态组合幂等。
- 证书私钥不上传平台，CSR 由浏览器端生成时应向用户明确提示私钥保管责任。

## 10. 分阶段交付

### Phase 0：工程初始化

- 初始化 Go API、Ant Design Pro 和用户站
- Docker Compose 启动 MySQL、Redis
- OpenAPI 基础契约
- 环境变量、日志、错误码和 CI

### Phase 1：会员与钱包

- 注册登录
- 用户中心
- 钱包账户和账本
- 充值订单
- 支付回调验签和幂等

### Phase 2：产品和下单

- 产品目录
- 规则引擎
- CSR / 联系人 / 企业表单
- 钱包冻结和补偿
- FoxSSL Mock 适配器

### Phase 3：FoxSSL 真实签发

上游官方文档（Postman documenter）到位后，适配器已按真实报文实现完毕；剩下的是平台侧接线。逐条状态：

| 条 | 适配器 | 平台侧 | 说明 |
|----|--------|--------|------|
| 真实下单 | 已完成 | 已完成 | `CreateOrder` 报文映射 + `submit` 已接真实客户端。**注意不幂等**，结果未知时须先 `FindOrder` 反查 |
| DCV 域名列表 | 已完成 | 已完成 | `ListDomains`；`dnsNames` 兼容数组与裸字符串两种形态；响应侧验证方式走 `mapUpstreamDcvMethod` 翻译，不能直接当平台取值存库 |
| Webhook | 已完成 | **待接线** | 验签、解析、应答体均已实现；但订单域 `submit` 传的 `notifyUrl` 是空串，上游只在收到该参数时才推送 |
| 状态轮询 | 已完成 | **未开始** | `OrderStatus` 在适配器与 Mock 里都可用，但生产代码里一次都没被调用；`cmd/cron` 仍是 Phase 0 的桩 |
| 证书下载 | 已完成 | 已完成 | `DownloadCertificate`；时间戳为毫秒，空 `content` 归入「结果未知」而非成功 |
| 取消和重签 | 已完成 | 已完成 | `CancelOrder` 用 GET；`Reissue` 带 CSR / 验证方式 / 域名，差价 `priceDiff` 只记日志、不进结算 |
| 提交域名验证 | **阻塞** | 未开始 | `VerifyDomains` 的请求体在上游文档里缺失（Postman 集合连 `originalRequest` 都没有），适配器直接返回 `ErrNotSupported`，需向上游确认参数格式 |

**本阶段最要紧的下一步是状态轮询**：回调路径依赖 `notifyUrl` 接线，在它接通之前，订单状态没有任何自动同步手段。而 `cmd/cron` 与 `cmd/worker` 目前仍是桩，意味着「结果未知」的订单会一直冻着等人工补偿。

顺带修正了 5 处被文档推翻的既有假设（详见 `docs/07`）：鉴权头不是 `Authorization: Bearer`；`CreateOrder` 不幂等；回调密钥不是独立 secret；HTTP 404 不等于上游订单不存在（上游用业务码 6010）；`domainNames` 不含主域名。

### Phase 4：Ant Design Pro 管理后台

- Dashboard
- 订单管理
- 产品和价格管理
- 财务管理
- 会员管理
- DCV 运维中心
- RBAC 和审计

### Phase 5：生产化

- HTTPS 和密钥托管
- 限流、风控和验证码
- 监控、告警、备份
- 压力测试、安全测试
- 对账和灾备演练

## 11. 第一阶段验收标准

1. 用户可以完成注册、登录和退出。
2. 充值回调重复提交不会重复入账。
3. 用户余额不足时无法下单。
4. 下单失败会自动解冻余额。
5. 同一订单重复提交不会创建重复 FoxSSL 订单。
6. 产品规则在后端校验，不能依赖前端绕过。
7. Webhook 验签失败不会修改订单状态。
8. 订单状态可由 Webhook 更新，也可由轮询补偿。
9. 管理员操作有权限校验和审计记录。
10. FoxSSL API Key 不出现在前端和普通日志中。

已经落地的部分与覆盖方式：

| 条 | 覆盖方式 |
|----|----------|
| 1 | `make smoke-auth` |
| 2 | `make smoke-recharge` + `recharge` 包的幂等测试 |
| 3 | `make smoke-order` 第 13 节 + `TestInsufficientBalanceLeavesNoOrder` |
| 4 | `TestDeterministicUpstreamRejectionUnfreezes`。需要注入上游失败，HTTP 冒烟造不出来 |
| 5 | `TestRetrySubmitConvergesToSingleUpstreamOrder` |
| 6 | `make smoke-order` 第 3、4、15 节 + `order` / `product/rules` 的规则测试 |
| 7 | `make smoke-order` 第 7 节 + `TestWebhookRejectsTamperedSignatureWithoutWriting` |
| 8 | 回调一侧已覆盖（`make smoke-order` 第 6 节）；轮询补偿待 Phase 3 |
| 9、10 | 待 Phase 4 管理后台落地 |

## 12. 当前建议

推荐的实际开发顺序：

1. 先完成 OpenAPI 契约和数据库迁移。
2. 再完成 Go 的认证、钱包、产品和订单基础域。
3. 使用 Mock FoxSSL 打通完整下单流程。
4. Ant Design Pro 优先开发订单、财务、产品三个后台模块。
5. 最后接入真实 FoxSSL、支付平台和生产 Webhook。

第一版不要直接把真实支付和真实签发混在初始开发中，先用 Mock 供应商和测试支付验证资金状态机，验收通过后再切换生产凭证。
