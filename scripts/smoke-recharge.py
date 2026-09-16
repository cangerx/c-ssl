#!/usr/bin/env python3
"""充值域端到端冒烟：下单 / 回调验签 / 重复回调不重复入账 / 金额校验 / 分页。

需要 API 服务已在运行（make run-api）。用法：

    make smoke-recharge
    .venv/bin/python scripts/smoke-recharge.py --base http://127.0.0.1:8080

验签密钥从环境变量 PAYMENT_WEBHOOK_SECRET 读取，缺失时回落到仓库根的 .env。
脚本自己按 Mock 渠道的约定计算 HMAC-SHA256 签名，因此走的是与真实渠道
完全相同的验签路径——而不是绕开验签直接改库。

退出码：0 全部通过；1 有失败项。

注意：脚本会真实写入 users / recharge_orders / payment_transactions /
wallet_ledger 表，用的是随机邮箱，不会与既有数据冲突，但会留下测试数据。
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

ap = argparse.ArgumentParser(description="充值域端到端冒烟")
ap.add_argument("--base", default="http://127.0.0.1:8080",
                help="API 基础地址，默认 http://127.0.0.1:8080")
args = ap.parse_args()

BASE = args.base.rstrip("/") + "/api/v1"
stamp = str(int(time.time()))
passed, failed = [], []

# 显式禁用代理。脚本打的是本机服务，而开发机上常设着 HTTP_PROXY；
# 走代理会被转发到一个不认识 127.0.0.1:8081 的中间层，
# 得到与业务毫无关系的 502，排查时非常费解。
OPENER = urllib.request.build_opener(urllib.request.ProxyHandler({}))


def load_secret():
    """读取支付回调验签密钥，与服务的取值来源保持一致。"""
    if os.environ.get("PAYMENT_WEBHOOK_SECRET"):
        return os.environ["PAYMENT_WEBHOOK_SECRET"]

    directory = Path(__file__).resolve().parent
    for _ in range(4):
        env_file = directory / ".env"
        if env_file.is_file():
            for line in env_file.read_text(encoding="utf-8").splitlines():
                line = line.strip()
                if line.startswith("PAYMENT_WEBHOOK_SECRET="):
                    return line.split("=", 1)[1].strip().strip('"').strip("'")
        directory = directory.parent
    return ""


SECRET = load_secret()
if not SECRET:
    print("找不到 PAYMENT_WEBHOOK_SECRET（环境变量与仓库根 .env 都没有），无法验签。")
    sys.exit(1)


def sign(raw: bytes) -> str:
    """对原始字节做 HMAC-SHA256 再 Base64，与 Mock 渠道的约定一致。"""
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


def notification(order_no, amount, trade_no, status="success", paid_at=None):
    """构造 Mock 渠道的回调报文，返回 (原始字节, 签名)。"""
    payload = {
        "channelTradeNo": trade_no,
        "channelOrderNo": "MOCK-ORDER-" + order_no,
        "orderNo": order_no,
        "amount": amount,
        "status": status,
    }
    if paid_at:
        payload["paidAt"] = paid_at
    raw = json.dumps(payload, separators=(",", ":")).encode()
    return raw, sign(raw)


def balance_of(token):
    _, body = call("GET", "/wallet", token=token)
    return body["data"]["availableBalance"]


def order_status(token, order_no):
    _, body = call("GET", "/recharge/orders?limit=100", token=token)
    for item in body["data"]["items"]:
        if item["orderNo"] == order_no:
            return item["status"]
    return None


# ── 1. 注册并下单 ─────────────────────────────────

print("=== 1. 注册并创建充值订单 ===")
email = f"recharge.{stamp}@example.com"
status, body = call("POST", "/auth/register",
                    {"email": email, "password": "correct-horse-1", "nickname": "充值冒烟"})
check("注册返回 200", status == 200, f"HTTP {status}")
token = body["data"]["accessToken"]

status, body = call("POST", "/recharge/orders", {"amount": 10000}, token=token)
check("创建订单返回 200", status == 200, f"HTTP {status}")
order_no = body["data"]["orderNo"]
check("订单号带 RC 前缀", order_no.startswith("RC"), order_no)
check("订单状态为 pending", body["data"]["status"] == "pending", body["data"]["status"])
check("订单返回支付地址", bool(body["data"]["payUrl"]), body["data"].get("payUrl", ""))
check("创建订单不动余额", balance_of(token) == 0, str(balance_of(token)))

# ── 2. 正常回调 ───────────────────────────────────

print("=== 2. 支付回调入账 ===")
raw, signature = notification(order_no, 10000, f"MOCK-TRADE-{stamp}-A")
status, body = call("POST", "/payments/webhook/mock", raw_body=raw,
                    headers={"X-Webhook-Signature": signature})
check("回调返回 200", status == 200, f"HTTP {status}")
check("应答是渠道约定的信封", body.get("code") == 0, str(body))
check("余额到账 10000", balance_of(token) == 10000, str(balance_of(token)))
check("订单变为 paid", order_status(token, order_no) == "paid", str(order_status(token, order_no)))

_, body = call("GET", "/wallet/ledger", token=token)
entries = body["data"]["items"]
check("账本只有 1 条流水", len(entries) == 1, f"{len(entries)} 条")
if entries:
    check("流水类型为 recharge", entries[0]["op"] == "recharge", entries[0]["op"])
    check("流水指向充值单", entries[0]["bizNo"] == order_no, entries[0]["bizNo"])
    check("流水金额正确", entries[0]["availableDelta"] == 10000, str(entries[0]["availableDelta"]))

# ── 3. 重复回调（验收标准第 2 条）─────────────────

print("=== 3. 重复回调不重复入账 ===")
for attempt in range(1, 4):
    status, body = call("POST", "/payments/webhook/mock", raw_body=raw,
                        headers={"X-Webhook-Signature": signature})
    check(f"第 {attempt} 次重放返回 200（要让渠道停止重试）", status == 200, f"HTTP {status}")
    check(f"第 {attempt} 次重放后余额仍为 10000", balance_of(token) == 10000, str(balance_of(token)))

_, body = call("GET", "/wallet/ledger", token=token)
check("重放后账本仍只有 1 条流水", len(body["data"]["items"]) == 1, f"{len(body['data']['items'])} 条")

# ── 4. 验签失败（验收标准第 7 条）─────────────────

print("=== 4. 验签失败不修改订单状态 ===")
raw2, signature2 = notification(order_no, 10000, f"MOCK-TRADE-{stamp}-FORGED")

bad_cases = [
    ("缺少签名头", {}),
    ("签名被篡改", {"X-Webhook-Signature": signature2[:-2] + "AB"}),
    ("用别的密钥签名", {"X-Webhook-Signature": base64.b64encode(b"x" * 32).decode()}),
]
for name, headers in bad_cases:
    status, body = call("POST", "/payments/webhook/mock", raw_body=raw2, headers=headers)
    check(f"{name} 返回 401", status == 401, f"HTTP {status}")
    check(f"{name} 业务码为 1001", body.get("code") == 1001, str(body.get("code")))

check("验签失败后订单仍为 paid", order_status(token, order_no) == "paid")
check("验签失败后余额仍为 10000", balance_of(token) == 10000, str(balance_of(token)))

# 篡改报文（签名对应的是原始报文）
tampered = raw2.replace(b'"amount":10000', b'"amount":99999999')
check("确实改到了报文", tampered != raw2)
status, body = call("POST", "/payments/webhook/mock", raw_body=tampered,
                    headers={"X-Webhook-Signature": signature2})
check("篡改报文验签失败返回 401", status == 401, f"HTTP {status}")

# ── 5. 金额校验 ───────────────────────────────────

print("=== 5. 金额不符与过期订单 ===")
status, body = call("POST", "/recharge/orders", {"amount": 10000}, token=token)
order2 = body["data"]["orderNo"]
check("第二张订单创建成功", status == 200, f"HTTP {status}")

raw3, signature3 = notification(order2, 1, f"MOCK-TRADE-{stamp}-LOW")
status, body = call("POST", "/payments/webhook/mock", raw_body=raw3,
                    headers={"X-Webhook-Signature": signature3})
check("金额偏小返回 400", status == 400, f"HTTP {status}")
check("金额不符的业务码为 1000", body.get("code") == 1000, str(body.get("code")))

raw4, signature4 = notification(order2, 999999, f"MOCK-TRADE-{stamp}-HIGH")
status, body = call("POST", "/payments/webhook/mock", raw_body=raw4,
                    headers={"X-Webhook-Signature": signature4})
check("金额偏大同样返回 400（不是只判少了才拒）", status == 400, f"HTTP {status}")

check("金额不符后订单仍为 pending", order_status(token, order2) == "pending",
      str(order_status(token, order2)))
check("金额不符后余额未变", balance_of(token) == 10000, str(balance_of(token)))

# 金额不符的尝试失败后，同一个交易号必须还能用（幂等键只在成功时提交）
raw5, signature5 = notification(order2, 10000, f"MOCK-TRADE-{stamp}-LOW")
status, body = call("POST", "/payments/webhook/mock", raw_body=raw5,
                    headers={"X-Webhook-Signature": signature5})
check("失败尝试的交易号仍可正常入账", status == 200, f"HTTP {status}")
check("第二张订单入账后余额为 20000", balance_of(token) == 20000, str(balance_of(token)))

# ── 6. 重复付款被拒 ───────────────────────────────

print("=== 6. 同一订单再次支付 ===")
raw6, signature6 = notification(order2, 10000, f"MOCK-TRADE-{stamp}-SECOND")
status, body = call("POST", "/payments/webhook/mock", raw_body=raw6,
                    headers={"X-Webhook-Signature": signature6})
check("已支付订单的第二次回调返回 409", status == 409, f"HTTP {status}")
check("状态不允许的业务码为 2001", body.get("code") == 2001, str(body.get("code")))
check("余额仍为 20000", balance_of(token) == 20000, str(balance_of(token)))

# ── 7. 交易号复用 ─────────────────────────────────

print("=== 7. 同一交易号核销另一张订单 ===")
status, body = call("POST", "/recharge/orders", {"amount": 5000}, token=token)
order3 = body["data"]["orderNo"]

raw7, signature7 = notification(order3, 5000, f"MOCK-TRADE-{stamp}-LOW")
status, body = call("POST", "/payments/webhook/mock", raw_body=raw7,
                    headers={"X-Webhook-Signature": signature7})
check("复用交易号返回 400", status == 400, f"HTTP {status}")
check("复用交易号的业务码为 1000", body.get("code") == 1000, str(body.get("code")))
check("第三张订单仍为 pending", order_status(token, order3) == "pending",
      str(order_status(token, order3)))

# ── 8. 开发环境模拟回调 ───────────────────────────

print("=== 8. 开发环境模拟回调 ===")
status, body = call("POST", "/payments/mock/notify",
                    {"orderNo": order3, "status": "success"}, token=token)
check("模拟回调返回 200", status == 200, f"HTTP {status}")
check("模拟回调后余额为 25000", balance_of(token) == 25000, str(balance_of(token)))

# 固定交易号的幂等：新建一张订单，同一个交易号投递三次，只应入账一次
status, body = call("POST", "/recharge/orders", {"amount": 7000}, token=token)
check("第四张订单创建成功", status == 200, f"HTTP {status}")
order4 = body["data"]["orderNo"]

fixed_trade_no = f"MOCK-TRADE-{stamp}-FIXED"
for attempt in range(1, 4):
    status, body = call("POST", "/payments/mock/notify",
                        {"orderNo": order4, "status": "success",
                         "channelTradeNo": fixed_trade_no}, token=token)
    check(f"固定交易号的第 {attempt} 次模拟回调返回 200", status == 200, f"HTTP {status}")

check("固定交易号只入账一次（余额 32000）", balance_of(token) == 32000, str(balance_of(token)))

# 已支付订单再收到回调应被拒绝，与真实 webhook 的行为一致
status, body = call("POST", "/payments/mock/notify",
                    {"orderNo": order4, "status": "success",
                     "channelTradeNo": f"MOCK-TRADE-{stamp}-OTHER"}, token=token)
check("已支付订单的模拟回调返回 409", status == 409, f"HTTP {status}")

_, body = call("GET", "/wallet/ledger", token=token)
check("账本流水条数与入账次数一致（4 条）", len(body["data"]["items"]) == 4,
      f"{len(body['data']['items'])} 条")

# ── 9. 参数校验与分页 ─────────────────────────────

print("=== 9. 参数校验与分页 ===")
for amount, label in [(0, "零"), (-100, "负数"), (99, "低于下限"), (10_000_001, "高于上限")]:
    status, body = call("POST", "/recharge/orders", {"amount": amount}, token=token)
    check(f"{label}金额返回 400", status == 400, f"HTTP {status}")
    fields = (body.get("data") or {}).get("fields") or {}
    check(f"{label}金额带 amount 字段提示", "amount" in fields, str(fields))

status, body = call("POST", "/recharge/orders", {"amount": 10000, "channel": "nope"}, token=token)
check("未注册渠道返回 400", status == 400, f"HTTP {status}")

seen, cursor, pages = set(), 0, 0
while True:
    path = "/recharge/orders?limit=2" + (f"&cursor={cursor}" if cursor else "")
    status, body = call("GET", path, token=token)
    if status != 200:
        check("翻页请求成功", False, f"HTTP {status}")
        break
    pages += 1
    for item in body["data"]["items"]:
        check_no = item["orderNo"]
        if check_no in seen:
            check("翻页无重复", False, check_no)
        seen.add(check_no)
    cursor = body["data"].get("nextCursor")
    if not cursor:
        break

check("翻页覆盖全部 4 张订单", len(seen) == 4, f"{len(seen)} 张")
check("确实分了多页", pages >= 2, f"{pages} 页")

status, body = call("GET", "/recharge/orders?limit=abc", token=token)
check("非法 limit 返回 400", status == 400, f"HTTP {status}")
status, body = call("GET", "/recharge/orders?limit=99999", token=token)
check("超上限 limit 被截断而不是报错", status == 200, f"HTTP {status}")

# ── 10. 越权 ──────────────────────────────────────

print("=== 10. 越权与未登录 ===")
status, body = call("GET", "/recharge/orders")
check("未带令牌查询返回 401", status == 401, f"HTTP {status}")
status, body = call("POST", "/recharge/orders", {"amount": 10000})
check("未带令牌下单返回 401", status == 401, f"HTTP {status}")
status, body = call("POST", "/payments/mock/notify", {"orderNo": order_no})
check("未带令牌模拟回调返回 401", status == 401, f"HTTP {status}")

# ── 结果 ──────────────────────────────────────────

print()
print(f"通过 {len(passed)} 项，失败 {len(failed)} 项")
if failed:
    print("失败项：")
    for name in failed:
        print(f"  - {name}")
    sys.exit(1)
print("充值域冒烟全部通过。")
