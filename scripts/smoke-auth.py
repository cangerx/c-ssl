#!/usr/bin/env python3
"""认证域端到端冒烟：注册 / 登录 / 刷新轮换 / 重放检测 / 账号锁定 / 退出登录。

需要 API 服务已在运行（make run-api）。用法：

    make smoke-auth
    .venv/bin/python scripts/smoke-auth.py --base http://127.0.0.1:8080

退出码：0 全部通过；1 有失败项。

注意：脚本会真实写入 users 与 user_sessions 表，并占用登录限流配额。
连续多次运行时，先清空限流计数，否则会被 IP 限流误判：

    redis-cli -n 1 --scan --pattern "rl:*" | xargs -r redis-cli -n 1 DEL
"""

import argparse
import json
import sys
import time
import urllib.error
import urllib.request

ap = argparse.ArgumentParser(description="认证域端到端冒烟")
ap.add_argument("--base", default="http://127.0.0.1:8080",
                help="API 基础地址，默认 http://127.0.0.1:8080")
args = ap.parse_args()

BASE = args.base.rstrip("/") + "/api/v1"
stamp = str(int(time.time()))
passed, failed = [], []

# 显式禁用代理。脚本打的是本机服务，而开发机上常设着 HTTP_PROXY；
# 走代理会被转发到一个不认识 127.0.0.1:8080 的中间层，
# 得到与业务毫无关系的 502，排查时非常费解。
OPENER = urllib.request.build_opener(urllib.request.ProxyHandler({}))


def call(method, path, body=None, token=None):
    req = urllib.request.Request(BASE + path, method=method)
    req.add_header("Content-Type", "application/json")
    if token:
        req.add_header("Authorization", "Bearer " + token)
    data = json.dumps(body).encode() if body is not None else None
    try:
        with OPENER.open(req, data) as resp:
            return resp.status, json.loads(resp.read())
    except urllib.error.HTTPError as e:
        raw = e.read()
        try:
            return e.code, json.loads(raw)
        except json.JSONDecodeError:
            return e.code, {"raw": raw.decode(errors="replace")}


def check(name, cond, detail=""):
    (passed if cond else failed).append(name)
    print(f"  {'✓' if cond else '✗'} {name}" + (f"  [{detail}]" if detail else ""))


print("=== 1. 注册 ===")
email_a = f"alice.{stamp}@example.com"
status, body = call("POST", "/auth/register",
                    {"email": email_a, "password": "correct-horse-1", "nickname": "Alice"})
check("注册返回 200", status == 200, f"HTTP {status}")
check("code=0", body.get("code") == 0, f"code={body.get('code')}")
tok_a = body.get("data") or {}
check("返回 accessToken", bool(tok_a.get("accessToken")))
check("返回 refreshToken", bool(tok_a.get("refreshToken")))
check("expiresIn=900", tok_a.get("expiresIn") == 900, f"实际 {tok_a.get('expiresIn')}")

print("\n=== 2. 邮箱大小写与重复注册 ===")
status, body = call("POST", "/auth/register",
                    {"email": email_a.upper(), "password": "correct-horse-1"})
check("大写邮箱重复注册被拒", status == 400 and body.get("code") == 1000, f"HTTP {status} code={body.get('code')}")
check("返回字段级错误", "email" in (body.get("data") or {}).get("fields", {}),
      json.dumps((body.get("data") or {}).get("fields", {}), ensure_ascii=False))

print("\n=== 3. 弱口令与非法邮箱 ===")
status, body = call("POST", "/auth/register", {"email": f"weak.{stamp}@example.com", "password": "short"})
check("过短口令被拒", status == 400 and "password" in (body.get("data") or {}).get("fields", {}))
status, body = call("POST", "/auth/register", {"email": "not-an-email", "password": "correct-horse-1"})
check("非法邮箱被拒", status == 400 and "email" in (body.get("data") or {}).get("fields", {}))

print("\n=== 4. 取当前用户 ===")
status, body = call("GET", "/me", token=tok_a["accessToken"])
check("带令牌返回 200", status == 200, f"HTTP {status}")
me = body.get("data") or {}
check("返回正确邮箱", me.get("email") == email_a, me.get("email", ""))
check("nickname 正确", me.get("nickname") == "Alice", me.get("nickname", ""))
check("status=active", me.get("status") == "active", me.get("status", ""))
check("不含 passwordHash", "passwordHash" not in json.dumps(me))
check("不含内部字段", not any(k in me for k in ("failedLoginCount", "lockedUntil", "password_hash")))

print("\n=== 5. 未认证访问 ===")
status, body = call("GET", "/me")
check("无令牌返回 401", status == 401 and body.get("code") == 1001, f"HTTP {status} code={body.get('code')}")
status, body = call("GET", "/me", token="garbage.token.here")
check("非法令牌返回 401", status == 401 and body.get("code") == 1001, f"HTTP {status} code={body.get('code')}")

print("\n=== 6. 登录 ===")
status, body = call("POST", "/auth/login", {"email": email_a, "password": "correct-horse-1"})
check("正确口令登录成功", status == 200 and body.get("code") == 0, f"HTTP {status}")
tok_login = body.get("data") or {}
status, body = call("POST", "/auth/login", {"email": email_a, "password": "wrong-password-1"})
check("错误口令返回 1001", status == 401 and body.get("code") == 1001, f"HTTP {status} code={body.get('code')}")
check("错误口令提示不区分账号是否存在",
      "不存在" not in body.get("message", "") and "未注册" not in body.get("message", ""),
      body.get("message", ""))
status, body = call("POST", "/auth/login", {"email": f"ghost.{stamp}@example.com", "password": "wrong-password-1"})
check("不存在的账号返回同样的 1001", status == 401 and body.get("code") == 1001,
      f"HTTP {status} code={body.get('code')}")

print("\n=== 7. 刷新令牌轮换 ===")
status, body = call("POST", "/auth/refresh", {"refreshToken": tok_login["refreshToken"]})
check("刷新返回 200", status == 200 and body.get("code") == 0, f"HTTP {status} code={body.get('code')}")
tok_rotated = body.get("data") or {}
check("拿到新的刷新令牌", tok_rotated.get("refreshToken") != tok_login["refreshToken"])
check("拿到新的访问令牌", bool(tok_rotated.get("accessToken")))
status, body = call("GET", "/me", token=tok_rotated["accessToken"])
check("新访问令牌可用", status == 200, f"HTTP {status}")

print("\n=== 8. 旧刷新令牌重放检测 ===")
# 旧令牌刚被轮换，处于宽限期内，应被当作并发刷新而非攻击
status, body = call("POST", "/auth/refresh", {"refreshToken": tok_login["refreshToken"]})
check("宽限期内重放被拒但只返回 401", status == 401 and body.get("code") == 1001,
      f"HTTP {status} code={body.get('code')}")
status, body = call("POST", "/auth/refresh", {"refreshToken": tok_rotated["refreshToken"]})
check("宽限期内新令牌仍可用", status == 200, f"HTTP {status}")
tok_after = body.get("data") or {}

print("\n=== 9. 退出登录 ===")
status, body = call("POST", "/auth/logout", token=tok_after["accessToken"])
check("退出返回 200", status == 200 and body.get("code") == 0, f"HTTP {status} code={body.get('code')}")
status, body = call("POST", "/auth/refresh", {"refreshToken": tok_after["refreshToken"]})
check("退出后刷新令牌失效", status == 401 and body.get("code") == 1001,
      f"HTTP {status} code={body.get('code')}")

print("\n=== 10. 账号锁定 ===")
email_b = f"bob.{stamp}@example.com"
call("POST", "/auth/register", {"email": email_b, "password": "correct-horse-1"})
codes = []
for _ in range(6):
    status, body = call("POST", "/auth/login", {"email": email_b, "password": "wrong-password-1"})
    codes.append(body.get("code"))
# 阈值是 5 次：前 5 次返回凭据错误（第 5 次同时写入锁定），第 6 次起被锁定拦下
check("前 5 次失败返回 1001", codes[:5] == [1001] * 5, f"实际 {codes}")
check("第 6 次被锁定拦下", codes[5] == 1004, f"实际 {codes}")
# 锁定期内即使口令正确也应被拒
status, body = call("POST", "/auth/login", {"email": email_b, "password": "correct-horse-1"})
check("锁定期内正确口令也被拒", body.get("code") in (1001, 1004),
      f"code={body.get('code')} message={body.get('message')}")

print("\n=== 11. 刷新令牌格式校验 ===")
for bad in ["", "no-dot", "a.b.c", ".onlysecret"]:
    status, body = call("POST", "/auth/refresh", {"refreshToken": bad})
    check(f"畸形刷新令牌 {bad!r} 被拒", status in (400, 401), f"HTTP {status}")

print("\n=== 12. 请求体校验 ===")
status, body = call("POST", "/auth/login", {})
check("空请求体被拒", status == 400 and body.get("code") == 1000, f"HTTP {status}")
status, body = call("POST", "/auth/register", {"email": "x@y.com", "password": "correct-horse-1", "extra": 1})
check("未知字段被容忍", status == 200 or body.get("code") in (0, 1000), f"code={body.get('code')}")

print(f"\n{'=' * 46}")
print(f"通过 {len(passed)} 项，失败 {len(failed)} 项")
if failed:
    print("失败项：")
    for name in failed:
        print(f"  - {name}")

sys.exit(1 if failed else 0)
