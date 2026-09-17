#!/usr/bin/env python3
"""订单域端到端冒烟：下单 / 验证材料 / 上游回调 / 证书下载 / 取消退款 / 重签。

需要 API 服务已在运行（make run-api）。用法：

    make smoke-order
    .venv/bin/python scripts/smoke-order.py --base http://127.0.0.1:8080

验签密钥从环境变量 FOXSSL_WEBHOOK_SECRET 读取，缺失时回落到仓库根的 .env。
脚本自己按 Mock 上游的报文格式计算 HMAC-SHA256 签名，因此走的是与真实上游
完全相同的验签路径——而不是绕开验签直接改库。

本脚本还依赖开发辅助接口 `POST /orders/{orderNo}/mock/issue`：真实 CA 不会
因为一个 HTTP 请求就立刻签发证书，所以「让上游签发」这一步只能靠它。它
**只动上游**，本地订单状态仍由回调驱动——脚本接下来必须自己签名并投递回调，
订单才会变成 issued。因此这条链路覆盖的仍是真实的事件处理路径。

退出码：0 全部通过；1 有失败项。

注意：脚本会真实写入 users / recharge_orders / payment_transactions /
wallet_ledger / certificate_orders / order_domains / certificates /
webhook_events 表。用的是随机邮箱与随机域名，不会与既有数据冲突，
但会留下测试数据。
"""

import argparse
import base64
import hashlib
import hmac
import json
import os
import sys
import time
import urllib.error
import urllib.request
from pathlib import Path

ap = argparse.ArgumentParser(description="订单域端到端冒烟")
ap.add_argument("--base", default="http://127.0.0.1:8080",
                help="API 基础地址，默认 http://127.0.0.1:8080")
args = ap.parse_args()

BASE = args.base.rstrip("/") + "/api/v1"
stamp = str(int(time.time()))
passed, failed = [], []

# 迁移里种下的产品。
PRODUCT_DV = 1        # AlphaSSL DV 单域名：零售价 29800，支持取消与重签
PRODUCT_WILDCARD = 2  # GlobalSign DV 通配符：1 年零售价 89800，支持通配符
PRODUCT_FREE = 6      # 免费 DV：零售价 0，不支持取消与重签
DV_RETAIL = 29800
WILDCARD_RETAIL = 89800
FREE_RETAIL = 0

# 期望的可用余额。每完成一笔资金动作就同步更新，断言统一读它。
#
# 不写死数字：一条订单会产生「冻结 + 结算」两个动作，脚本里又有多张订单，
# 写死的话每加一张订单都要改十几处断言，改漏一处就会得到一个假的失败，
# 而那种失败最容易被人用「把断言改松」的方式消掉。
expect_available = 0

# 显式禁用代理。脚本打的是本机服务，而开发机上常设着 HTTP_PROXY；
# 走代理会被转发到一个不认识 127.0.0.1:8080 的中间层，
# 得到与业务毫无关系的 502，排查时非常费解。
OPENER = urllib.request.build_opener(urllib.request.ProxyHandler({}))


def load_secret():
    """读取上游回调验签密钥，与服务的取值来源保持一致。"""
    if os.environ.get("FOXSSL_WEBHOOK_SECRET"):
        return os.environ["FOXSSL_WEBHOOK_SECRET"]

    directory = Path(__file__).resolve().parent
    for _ in range(4):
        env_file = directory / ".env"
        if env_file.is_file():
            for line in env_file.read_text(encoding="utf-8").splitlines():
                line = line.strip()
                if line.startswith("FOXSSL_WEBHOOK_SECRET="):
                    return line.split("=", 1)[1].strip().strip('"').strip("'")
        directory = directory.parent
    return ""


SECRET = load_secret()
if not SECRET:
    print("找不到 FOXSSL_WEBHOOK_SECRET（环境变量与仓库根 .env 都没有），无法验签。")
    sys.exit(1)


def sign(raw: bytes) -> str:
    """对原始字节做 HMAC-SHA256 再 Base64，与 Mock 上游的约定一致。"""
    return base64.b64encode(
        hmac.new(SECRET.encode(), raw, hashlib.sha256).digest()
    ).decode()


def call(method, path, body=None, token=None, raw_body=None, headers=None):
    req = urllib.request.Request(BASE + path, method=method)
    req.add_header("Content-Type", "application/json")
    for key, value in (headers or {}).items():
        req.add_header(key, value)
    if token:
        req.add_header("Authorization", "Bearer " + token)

    if raw_body is not None:
        data = raw_body
    elif body is not None:
        data = json.dumps(body).encode()
    else:
        data = None

    try:
        with OPENER.open(req, data) as resp:
            return resp.status, json.loads(resp.read())
    except urllib.error.HTTPError as e:
        payload = e.read()
        try:
            return e.code, json.loads(payload)
        except json.JSONDecodeError:
            return e.code, {"raw": payload.decode(errors="replace")}


def check(name, cond, detail=""):
    (passed if cond else failed).append(name)
    print(f"  {'✓' if cond else '✗'} {name}" + (f"  [{detail}]" if detail else ""))


# ── 业务辅助 ──────────────────────────────────────

def register(email):
    status, body = call("POST", "/auth/register",
                        {"email": email, "password": "correct-horse-1",
                         "nickname": "订单冒烟"})
    if status != 200:
        print(f"注册失败（HTTP {status}）：{body}")
        sys.exit(1)
    return body["data"]["accessToken"]


def recharge(token, amount):
    _, body = call("POST", "/recharge/orders", {"amount": amount}, token=token)
    order_no = body["data"]["orderNo"]
    status, _ = call("POST", "/payments/mock/notify",
                     {"orderNo": order_no}, token=token)
    return status


def wallet(token):
    _, body = call("GET", "/wallet", token=token)
    return body["data"]


def ledger(token):
    _, body = call("GET", "/wallet/ledger?limit=100", token=token)
    return body["data"]["items"]


def detail(token, order_no):
    status, body = call("GET", "/orders/" + order_no, token=token)
    if status != 200:
        return {"__status__": status, "__body__": body}
    return body["data"]


def status_of(token, order_no):
    return detail(token, order_no).get("status")


def domains_of(token, order_no):
    _, body = call("GET", "/orders/" + order_no + "/domains", token=token)
    return body["data"]["items"]


def create_order(token, domains, product_id=PRODUCT_DV, **extra):
    body = {
        "productId": product_id,
        "years": 1,
        "keyAlgorithm": "rsa",
        "domains": domains,
        "contact": {
            "name": "张三",
            "email": "smoke@example.com",
            "phone": "+86.13800138000",
        },
    }
    body.update(extra)
    return call("POST", "/orders", body, token=token)


def encode_event(event, upstream_no, status, cert_id=None, domains=None):
    """按上游报文格式编码事件。字段名必须与适配器解析的一致。"""
    payload = {
        "event": event,
        "orderNo": upstream_no,
        "status": status,
        "occurredAt": int(time.time()),
    }
    if cert_id:
        payload["certId"] = cert_id
    if domains:
        payload["domains"] = domains
    # 不带空白：签名是对这些字节算的，压缩编码让报文与 Go 侧的 json.Marshal 一致。
    return json.dumps(payload, separators=(",", ":")).encode()


def post_event(raw, signature=None):
    headers = {"X-Webhook-Signature": signature} if signature else {}
    return call("POST", "/webhooks/foxssl", raw_body=raw, headers=headers)


# ── 1. 注册与充值 ─────────────────────────────────

print("=== 1. 注册并充值 ===")
token = register(f"order.{stamp}@example.com")
check("注册成功并拿到令牌", bool(token))

status = recharge(token, 100_000)
check("充值成功", status == 200, f"HTTP {status}")
expect_available = 100_000
check("充值后可用余额为 100000", wallet(token)["availableBalance"] == expect_available,
      str(wallet(token)["availableBalance"]))

# ── 2. 下单 ───────────────────────────────────────

print("=== 2. 创建证书订单 ===")
domain_a = f"smoke-{stamp}-a.example.com"
# 一个像样的 CSR。Mock 上游不校验它的内容，但它会走一遍
# 「写入可空列 → 读回来」的完整路径——那段路径曾经因为把 NULL 扫进
# string 而在真实数据库上直接报错。
csr = (
    "-----BEGIN CERTIFICATE REQUEST-----\n"
    "MIICWjCCAUICAQAwFTETMBEGA1UEAwwKZXhhbXBsZS5jb20wggEiMA0GCSqGSIb3\n"
    "DQEBAQUAA4IBDwAwggEKAoIBAQC7VJTUt9Us8cKjMzEfYyjiWA4R4/M2bS1GB4t7\n"
    "-----END CERTIFICATE REQUEST-----\n"
)

status, body = create_order(token, [domain_a], csr=csr)
check("下单返回 200", status == 200, f"HTTP {status}")
if status != 200:
    print(f"    响应：{body}")
    sys.exit(1)

order_a = body["data"]["orderNo"]
check("订单号带 CS 前缀", order_a.startswith("CS"), order_a)
check("上游受理后状态为 waiting_dcv", body["data"]["status"] == "waiting_dcv",
      str(body["data"]["status"]))
check(f"订单金额为零售价 {DV_RETAIL}", body["data"]["amount"] == DV_RETAIL,
      str(body["data"]["amount"]))
check("下单响应带上游订单号", bool(body["data"]["upstreamOrderNo"]),
      str(body["data"].get("upstreamOrderNo")))
check("下单响应回显域名", body["data"]["domains"] == [domain_a],
      str(body["data"]["domains"]))

expect_available -= DV_RETAIL
w = wallet(token)
check(f"上游受理后钱已实扣（可用余额 {expect_available}）",
      w["availableBalance"] == expect_available, str(w["availableBalance"]))
check("冻结余额已清零", w["frozenBalance"] == 0, str(w["frozenBalance"]))
check("总资产等于可用余额（没有悬空的冻结）",
      w["totalBalance"] == w["availableBalance"], str(w["totalBalance"]))

# 带 CSR 与不带 CSR 的订单都必须读得出来：csr 列可空，扫描写错就是 500。
status, body = create_order(token, [f"smoke-{stamp}-no-csr.example.com"])
order_no_csr = body["data"]["orderNo"]
check("不带 CSR 也能下单", status == 200, f"HTTP {status}")
check("不带 CSR 的订单详情可读",
      detail(token, order_no_csr).get("status") == "waiting_dcv",
      str(detail(token, order_no_csr).get("status")))
expect_available -= DV_RETAIL

# ── 3. 域名验证材料 ───────────────────────────────

print("=== 3. 域名验证材料 ===")
items = domains_of(token, order_a)
check("订单含 1 个域名", len(items) == 1, f"{len(items)} 个")

if items:
    d = items[0]
    check("域名与下单时一致", d["domain"] == domain_a, str(d["domain"]))
    check("域名初始状态为 pending", d["status"] == "pending", str(d["status"]))

    path = d.get("filePath") or ""
    check("文件验证路径不含占位符 {FQDN}", "{FQDN}" not in path, path)
    check("文件验证路径不含通配符", "*." not in path, path)
    check("文件验证路径已按域名展开",
          path.startswith("/.well-known/pki-validation/" + domain_a + "/"), path)
    check("文件内容非空", bool(d.get("fileContent")), str(d.get("fileContent")))
    check("文件路径里含文件内容", bool(d.get("fileContent")) and d["fileContent"] in path, path)
    check("DNS 记录类型为 TXT", d.get("dnsRecordType") == "TXT", str(d.get("dnsRecordType")))
    check("DNS 记录名在裸域名上", d.get("dnsRecordName") == "_dnsauth." + domain_a,
          str(d.get("dnsRecordName")))
    check("DNS 记录值非空", bool(d.get("dnsRecordValue")), str(d.get("dnsRecordValue")))

    methods = d.get("availableMethods") or []
    # 可用验证方式是**两个来源的交集**：上游实际准备好的材料 ∩ 产品声明的能力。
    # 这里逐条断言而不是只看「非空」：界面列出了、用户选了却被 400 拒掉，
    # 是这个域最让人费解的故障——他会以为自己哪里配错了记录。
    check("可用验证方式非空", len(methods) > 0, str(methods))
    check("可用验证方式取值都在枚举内",
          set(methods) <= {"dns_txt", "dns_cname", "http_file", "https_file", "email"},
          str(methods))

    # 前提：上游材料里有收件地址，所以「材料侧支持邮件验证」这一层拦不住 email。
    # 没有这条前提，下面那条断言换成「材料里本来就没邮件」也照样通过，
    # 测的就不是产品侧把关了。
    check("上游材料里有邮件地址", bool(d.get("emailAddresses")), str(d.get("emailAddresses")))
    # AlphaSSL（产品 1）声明里没有 email，而材料里有——正是产品侧把关要拦的场景。
    check("产品未声明的 email 不出现在可用方式里", "email" not in methods, str(methods))
    # 反向：产品声明过的必须还在，否则上面那条可能只是因为「什么都收没了」。
    check("产品声明过的 dns_txt 仍在可用方式里", "dns_txt" in methods, str(methods))

# ── 4. 提交域名验证 ───────────────────────────────

print("=== 4. 提交域名验证 ===")
status, body = call("POST", f"/orders/{order_a}/domains/verify",
                    {"domains": [domain_a], "method": "dns_txt"}, token=token)
check("提交验证返回 200", status == 200, f"HTTP {status}")
if status == 200:
    check("提交后域名状态为 verifying",
          body["data"]["items"][0]["status"] == "verifying",
          str(body["data"]["items"][0]["status"]))

status, body = call("POST", f"/orders/{order_a}/domains/verify",
                    {"domains": [domain_a], "method": "carrier_pigeon"}, token=token)
check("未知验证方式返回 400", status == 400, f"HTTP {status}")
check("错误里带 method 字段提示",
      "method" in ((body.get("data") or {}).get("fields") or {}),
      str((body.get("data") or {}).get("fields")))

# 产品规则必须在后端把关（验收标准第 6 条：不能依赖前端绕过）。
# 产品页写着 AlphaSSL 不支持邮件验证，绕过界面直接提交也必须被拒，
# 而且理由要指向产品——说成「这些域名不支持」会把用户引去改域名，
# 而问题在产品上，他无论怎么配记录都不会通过。
status, body = call("POST", f"/orders/{order_a}/domains/verify",
                    {"domains": [domain_a], "method": "email"}, token=token)
check("产品未声明的 email 直接提交返回 400", status == 400, f"HTTP {status}")
check("拒绝理由指向产品而不是域名",
      "产品" in body.get("message", ""), str(body.get("message")))

status, body = call("POST", f"/orders/{order_a}/domains/verify",
                    {"domains": ["not-in-this-order.example.com"], "method": "dns_txt"},
                    token=token)
check("提交不属于订单的域名返回 400", status == 400, f"HTTP {status}")

# ── 5. 推进模拟上游 ───────────────────────────────

print("=== 5. 推进模拟上游（只动上游） ===")
status, body = call("POST", f"/orders/{order_a}/mock/issue", token=token)
check("推进模拟上游返回 200", status == 200, f"HTTP {status}")
upstream_no = (body.get("data") or {}).get("upstreamOrderNo") if status == 200 else None
check("应答带回上游订单号", bool(upstream_no), str(body.get("data")))
check("上游订单号与订单详情一致",
      upstream_no == detail(token, order_a).get("upstreamOrderNo"), str(upstream_no))

# 这是本接口的关键语义：本地状态必须还是 waiting_dcv，只能由回调推进。
check("推进上游不改本地订单状态",
      status_of(token, order_a) == "waiting_dcv", str(status_of(token, order_a)))
status, _ = call("GET", f"/orders/{order_a}/certificate", token=token)
check("推进上游后证书仍不可下载（409）", status == 409, f"HTTP {status}")

# ── 6. 签发回调 ───────────────────────────────────

print("=== 6. 上游签发回调 ===")
raw_issued = encode_event(
    "certificate_issued", upstream_no, "issued",
    cert_id="CERT-" + str(upstream_no),
    domains=[{"domain": domain_a, "status": "verified", "method": "dns_txt"}],
)
signature_issued = sign(raw_issued)

status, body = post_event(raw_issued, signature_issued)
check("回调返回 200", status == 200, f"HTTP {status}")
check("应答是上游约定的信封 {\"status\":\"success\"}",
      body.get("status") == "success", str(body))
check("应答不含本服务的统一信封字段",
      not any(k in body for k in ("code", "message", "data")), str(body))

check("回调后订单状态为 issued", status_of(token, order_a) == "issued",
      str(status_of(token, order_a)))
d = detail(token, order_a)
check("回调后订单记录 certId", bool(d.get("certId")), str(d.get("certId")))
check("回调后订单记录签发时间", bool(d.get("issuedAt")), str(d.get("issuedAt")))
check("回调后订单记录到期时间", bool(d.get("expiresAt")), str(d.get("expiresAt")))

items = domains_of(token, order_a)
check("回调后域名状态为 verified",
      items[0]["status"] == "verified", str(items[0]["status"]))
# 刻意不断言 verifiedAt：上游事件里只有状态、没有验证时间
# （见 foxssl.Notification.Domains），本地那行要等下一次向上游补拉材料
# 才会带上时间。断言一个系统并不保证的东西，只会让人把测试改绿，
# 而不是把「已验证却没有时间」这个问题修好。

# ── 7. 幂等与验签 ─────────────────────────────────

print("=== 7. 重复回调与验签失败 ===")
ledger_before_replay = len(ledger(token))
for attempt in range(1, 4):
    status, body = post_event(raw_issued, signature_issued)
    check(f"第 {attempt} 次重放返回 200（要让上游停止重试）", status == 200, f"HTTP {status}")

check("重放后订单仍为 issued", status_of(token, order_a) == "issued",
      str(status_of(token, order_a)))
check("重放后可用余额未变", wallet(token)["availableBalance"] == expect_available,
      str(wallet(token)["availableBalance"]))
check("重放没有新增账本流水", len(ledger(token)) == ledger_before_replay,
      f"{ledger_before_replay} → {len(ledger(token))} 条")

bad_cases = [
    ("缺少签名头", None),
    ("签名被篡改", signature_issued[:-2] + "AB"),
    ("用别的密钥签名", base64.b64encode(b"x" * 32).decode()),
]
for name, bad in bad_cases:
    status, body = post_event(raw_issued, bad)
    check(f"{name} 返回 401", status == 401, f"HTTP {status}")
    check(f"{name} 业务码为 1001", body.get("code") == 1001, str(body.get("code")))

# 篡改报文（签名对应的是原始报文）
tampered = raw_issued.replace(b'"status":"issued"', b'"status":"failed"')
check("确实改到了报文", tampered != raw_issued)
status, body = post_event(tampered, signature_issued)
check("篡改报文验签失败返回 401", status == 401, f"HTTP {status}")
check("篡改报文未改变订单状态", status_of(token, order_a) == "issued",
      str(status_of(token, order_a)))

# ── 8. 下载证书 ───────────────────────────────────

print("=== 8. 下载证书 ===")
status, body = call("GET", f"/orders/{order_a}/certificate", token=token)
check("证书接口返回 200", status == 200, f"HTTP {status}")
cert = body["data"]
check("证书内容是 PEM 文本", "BEGIN CERTIFICATE" in (cert.get("certificate") or ""),
      (cert.get("certificate") or "")[:40])
check("证书主域名为第一个域名", cert.get("commonName") == domain_a, str(cert.get("commonName")))
check("证书含域名列表", cert.get("domains") == [domain_a], str(cert.get("domains")))
check("证书含 CA 链", bool(cert.get("caBundle")), str(cert.get("caBundle"))[:40])
check("证书含序列号", bool(cert.get("serialNumber")), str(cert.get("serialNumber")))
check("证书含密钥算法", cert.get("keyAlgorithm") == "rsa", str(cert.get("keyAlgorithm")))
check("证书订单号与订单一致", cert.get("orderNo") == order_a, str(cert.get("orderNo")))

cert_id_before = cert.get("certId")

# ── 9. 重签 ───────────────────────────────────────

print("=== 9. 重签 ===")
status, body = call("POST", f"/orders/{order_a}/reissue", {"reason": "冒烟重签"}, token=token)
check("重签返回 200", status == 200, f"HTTP {status}")
check("重签后订单仍为 issued", status_of(token, order_a) == "issued",
      str(status_of(token, order_a)))
check("重签不额外扣费", wallet(token)["availableBalance"] == expect_available,
      str(wallet(token)["availableBalance"]))

status, body = call("GET", f"/orders/{order_a}/certificate", token=token)
check("重签后可下载到新证书",
      status == 200 and body["data"].get("certId") != cert_id_before,
      f"{cert_id_before} → {body['data'].get('certId')}")

status, body = call("POST", f"/orders/{order_a}/cancel",
                    {"reason": "已签发的订单不该能取消"}, token=token)
check("已签发订单不能取消（409）", status == 409, f"HTTP {status}")
check("被拒的取消没有退款", wallet(token)["availableBalance"] == expect_available,
      str(wallet(token)["availableBalance"]))

# ── 10. 取消已实扣的订单 ──────────────────────────

print("=== 10. 取消已实扣的订单必须退款 ===")
domain_b = f"smoke-{stamp}-b.example.com"
status, body = create_order(token, [domain_b])
order_b = body["data"]["orderNo"]
check("第二张订单创建成功", status == 200, f"HTTP {status}")
expect_available -= DV_RETAIL
check(f"第二张订单也已实扣（可用余额 {expect_available}）",
      wallet(token)["availableBalance"] == expect_available,
      str(wallet(token)["availableBalance"]))

ledger_before_cancel = len(ledger(token))

# 不带请求体：契约里取消的 requestBody 是 required: false
status, body = call("POST", f"/orders/{order_b}/cancel", token=token)
check("不带原因取消返回 200", status == 200, f"HTTP {status}")
check("取消后状态为 cancelled", body.get("data", {}).get("status") == "cancelled",
      str(body.get("data", {}).get("status")))

expect_available += DV_RETAIL
w = wallet(token)
check(f"取消后钱退回可用余额（{expect_available}）",
      w["availableBalance"] == expect_available, str(w["availableBalance"]))
check("取消后冻结余额为 0", w["frozenBalance"] == 0, str(w["frozenBalance"]))
check("取消后总资产等于可用余额", w["totalBalance"] == w["availableBalance"],
      str(w["totalBalance"]))

entries = ledger(token)
check("取消只新增一条流水（退款）", len(entries) == ledger_before_cancel + 1,
      f"{ledger_before_cancel} → {len(entries)} 条")
if entries:
    last = entries[0]
    check("最后一条流水是退款", last["op"] == "refund", str(last["op"]))
    check("退款流水指向被取消的订单", last["bizNo"] == order_b, str(last["bizNo"]))
    check("退款金额等于订单金额", last["availableDelta"] == DV_RETAIL,
          str(last["availableDelta"]))

status, body = call("POST", f"/orders/{order_b}/cancel", token=token)
check("重复取消返回 409", status == 409, f"HTTP {status}")
check("重复取消没有重复退款", wallet(token)["availableBalance"] == expect_available,
      str(wallet(token)["availableBalance"]))

status, body = call("POST", f"/orders/{order_b}/domains/verify",
                    {"domains": [domain_b], "method": "dns_txt"}, token=token)
check("已取消订单不能提交域名验证（409）", status == 409, f"HTTP {status}")

# ── 11. 免费证书 ──────────────────────────────────

print("=== 11. 免费证书 ===")
status, body = create_order(token, [f"smoke-{stamp}-free.example.com"],
                            product_id=PRODUCT_FREE)
check("免费证书下单成功", status == 200, f"HTTP {status}")
order_free = body["data"]["orderNo"]
check("免费证书金额为 0", body["data"]["amount"] == FREE_RETAIL,
      str(body["data"]["amount"]))
check("免费证书不动余额", wallet(token)["availableBalance"] == expect_available,
      str(wallet(token)["availableBalance"]))

status, body = call("POST", f"/orders/{order_free}/cancel", token=token)
check("免费证书不支持取消（409）", status == 409, f"HTTP {status}")

# ── 12. 参数校验 ──────────────────────────────────

print("=== 12. 参数校验 ===")
cases = [
    ("缺域名", [], {}),
    ("通配符域名买单域名产品", [f"*.smoke-{stamp}.example.com"], {}),
    ("DV 产品不该带企业信息", [f"smoke-{stamp}-org.example.com"], {
        "organization": {
            "name": "某某科技", "registrationNo": "91310000MA1K3XYZ8N",
            "country": "CN", "province": "上海", "city": "上海",
            "address": "张江路 1 号", "postalCode": "201203",
            "phone": "+86.13800138000",
        },
    }),
    ("密钥算法不支持", [f"smoke-{stamp}-alg.example.com"], {"keyAlgorithm": "dsa"}),
    ("年限不在价格表里", [f"smoke-{stamp}-yr.example.com"], {"years": 9}),
    ("联系人邮箱不合法", [f"smoke-{stamp}-mail.example.com"], {
        "contact": {"name": "张三", "email": "not-an-email", "phone": "123"},
    }),
]
for name, domains, extra in cases:
    status, body = create_order(token, domains, **extra)
    check(f"{name} 返回 400", status == 400, f"HTTP {status}")
    fields = (body.get("data") or {}).get("fields") or {}
    check(f"{name} 带字段级提示", bool(fields), str(fields))

status, body = call("POST", "/orders", {
    "productId": 999999, "years": 1, "keyAlgorithm": "rsa",
    "domains": [f"smoke-{stamp}-nope.example.com"],
    "contact": {"name": "张三", "email": "smoke@example.com", "phone": "123"},
}, token=token)
check("不存在的产品返回 404", status == 404, f"HTTP {status}")

# ── 13. 越权与未登录 ──────────────────────────────

print("=== 13. 越权与未登录 ===")
other = register(f"order.other.{stamp}@example.com")

status, body = call("POST", "/orders", {
    "productId": PRODUCT_DV, "years": 1, "keyAlgorithm": "rsa",
    "domains": [f"smoke-{stamp}-poor.example.com"],
    "contact": {"name": "李四", "email": "smoke@example.com", "phone": "123"},
}, token=other)
check("余额不足返回 400", status == 400, f"HTTP {status}")
check("余额不足的业务码为 2000", body.get("code") == 2000, str(body.get("code")))

for method, path, payload in [
    ("GET", f"/orders/{order_a}", None),
    ("GET", f"/orders/{order_a}/domains", None),
    ("GET", f"/orders/{order_a}/certificate", None),
    ("POST", f"/orders/{order_a}/cancel", {"reason": "越权"}),
    ("POST", f"/orders/{order_a}/reissue", {"reason": "越权"}),
    ("POST", f"/orders/{order_a}/mock/issue", None),
]:
    status, body = call(method, path, payload, token=other)
    check(f"他人访问 {method} {path} 返回 404", status == 404, f"HTTP {status}")

check("越权尝试后订单仍为 issued", status_of(token, order_a) == "issued",
      str(status_of(token, order_a)))

for method, path, payload in [
    ("POST", "/orders", {"productId": PRODUCT_DV}),
    ("GET", "/orders", None),
    ("GET", f"/orders/{order_a}", None),
    ("GET", f"/orders/{order_a}/certificate", None),
    ("POST", f"/orders/{order_a}/mock/issue", None),
]:
    status, body = call(method, path, payload)
    check(f"未登录 {method} {path} 返回 401", status == 401, f"HTTP {status}")

# ── 14. 列表、过滤与分页 ──────────────────────────

print("=== 14. 列表、过滤与分页 ===")
_, body = call("GET", "/orders?limit=100", token=token)
all_items = body["data"]["items"]
check("列表返回全部 4 张订单", len(all_items) == 4, f"{len(all_items)} 张")

by_no = {item["orderNo"]: item for item in all_items}
summary = by_no.get(order_a)
check("列表项含域名", summary is not None and summary.get("domains") == [domain_a],
      str(summary.get("domains") if summary else None))
check("列表项不含联系人（那是详情才有的字段）",
      summary is not None and "contact" not in summary, str(sorted(summary or {})))
check("列表项不含企业信息", summary is not None and "organization" not in summary)
check("列表项不含 CSR", summary is not None and "csr" not in summary)
check("列表项不含上游成本价", summary is not None and "costPrice" not in summary)
check("列表项不含取消原因", summary is not None and "cancelReason" not in summary)

_, body = call("GET", "/orders?status=issued", token=token)
check("按 issued 过滤只剩 1 张", len(body["data"]["items"]) == 1,
      f"{len(body['data']['items'])} 张")
_, body = call("GET", "/orders?status=cancelled", token=token)
check("按 cancelled 过滤只剩 1 张", len(body["data"]["items"]) == 1,
      f"{len(body['data']['items'])} 张")
status, body = call("GET", "/orders?status=nonsense", token=token)
check("非法状态返回 400", status == 400, f"HTTP {status}")

# 用列表记录顺序，不用 set：集合的迭代顺序对字符串来说是任意的
# （哈希随机化），拿它断言「最新在前」会得到一个永远为真的空断言。
seen_order, seen_set, cursor, pages = [], set(), 0, 0
while True:
    path = "/orders?limit=2" + (f"&cursor={cursor}" if cursor else "")
    status, body = call("GET", path, token=token)
    if status != 200:
        check("翻页请求成功", False, f"HTTP {status}")
        break
    pages += 1
    for item in body["data"]["items"]:
        if item["orderNo"] in seen_set:
            check("翻页无重复", False, item["orderNo"])
        seen_set.add(item["orderNo"])
        seen_order.append(item["orderNo"])
    cursor = body["data"].get("nextCursor")
    if not cursor:
        break

check("翻页覆盖全部 4 张订单", len(seen_order) == 4, f"{len(seen_order)} 张")
check("确实分了多页", pages >= 2, f"{pages} 页")
check("翻页顺序为最新在前",
      seen_order == [order_free, order_b, order_no_csr, order_a], str(seen_order))

status, body = call("GET", "/orders?limit=abc", token=token)
check("非法 limit 返回 400", status == 400, f"HTTP {status}")
status, body = call("GET", "/orders?limit=99999", token=token)
check("超上限 limit 被截断而不是报错", status == 200, f"HTTP {status}")

# ── 15. 通配符订单不能走文件验证 ──────────────────

print("=== 15. 通配符订单不能走文件验证 ===")
# 放在最后：它会多创建一张订单，前面那些断言的是固定的订单张数与余额。
status = recharge(token, 100_000)
check("追加充值成功", status == 200, f"HTTP {status}")
expect_available += 100_000

wildcard = f"*.smoke-{stamp}.example.com"
status, body = create_order(token, [wildcard], product_id=PRODUCT_WILDCARD)
check("通配符订单创建成功", status == 200, f"HTTP {status}")
if status == 200:
    order_wild = body["data"]["orderNo"]
    check(f"通配符订单金额为 {WILDCARD_RETAIL}",
          body["data"]["amount"] == WILDCARD_RETAIL, str(body["data"]["amount"]))

    wd = domains_of(token, order_wild)[0]
    methods = wd.get("availableMethods") or []
    # 通配符域名放不了验证文件：`*.example.com` 不是一台主机。
    # 上游有时仍会返回一个文件路径，把它列成可用方式只会误导用户。
    check("通配符域名不提供文件验证",
          "http_file" not in methods and "https_file" not in methods, str(methods))
    check("通配符域名仍提供 DNS 验证",
          "dns_txt" in methods or "dns_cname" in methods, str(methods))
    check("通配符域名的文件路径已按裸域名展开",
          (wd.get("filePath") or "").find("*.") < 0
          and ("/pki-validation/" + wildcard[2:] + "/") in (wd.get("filePath") or ""),
          str(wd.get("filePath")))
    check("通配符域名的 DNS 记录名在裸域名上",
          wd.get("dnsRecordName") == "_dnsauth." + wildcard[2:],
          str(wd.get("dnsRecordName")))

    status, body = call("POST", f"/orders/{order_wild}/domains/verify",
                        {"domains": [wildcard], "method": "http_file"}, token=token)
    check("通配符订单提交文件验证返回 400", status == 400, f"HTTP {status}")
    check("错误里带 method 字段提示",
          "method" in ((body.get("data") or {}).get("fields") or {}),
          str((body.get("data") or {}).get("fields")))

    status, body = call("POST", f"/orders/{order_wild}/domains/verify",
                        {"domains": [wildcard], "method": "dns_txt"}, token=token)
    check("通配符订单提交 DNS 验证返回 200", status == 200, f"HTTP {status}")

# ── 结果 ──────────────────────────────────────────

print()
print(f"通过 {len(passed)} 项，失败 {len(failed)} 项")
if failed:
    print("失败项：")
    for name in failed:
        print(f"  - {name}")
    sys.exit(1)
print("订单域冒烟全部通过。")
