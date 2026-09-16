#!/usr/bin/env bash
# c-ssl 本地开发环境自检
# 用法：make env-check   或   bash scripts/env-check.sh
set -uo pipefail

cd "$(dirname "$0")/.." || exit 1

PASS=0; FAIL=0; WARN=0
ok()   { printf '  \033[32m✓\033[0m %s\n' "$1"; PASS=$((PASS+1)); }
bad()  { printf '  \033[31m✗\033[0m %s\n' "$1"; FAIL=$((FAIL+1)); }
warn() { printf '  \033[33m!\033[0m %s\n' "$1"; WARN=$((WARN+1)); }
hdr()  { printf '\n\033[1m%s\033[0m\n' "$1"; }

# 从 .env 读取变量。不 source，避免 DSN 里的 & 被 shell 解释
envval() { [ -f .env ] && grep -E "^$1=" .env | head -1 | cut -d= -f2-; }

hdr "工具链"

if command -v go >/dev/null 2>&1; then
  GOV=$(go version | awk '{print $3}' | sed 's/^go//')
  ok "Go $GOV"
else
  bad "Go 未安装（需要 1.22+）"
fi

if command -v node >/dev/null 2>&1; then
  NODEV=$(node -v | sed 's/^v//')
  case "$NODEV" in
    22.*) ok "Node v$NODEV" ;;
    *)    warn "Node v$NODEV（.nvmrc 要求 22.x）" ;;
  esac
else
  bad "Node 未安装"
fi

if command -v pnpm >/dev/null 2>&1; then
  ok "pnpm $(pnpm -v)"
else
  bad "pnpm 未安装（需要 10+）"
fi

MIGRATE_BIN="${MIGRATE_BIN:-$(go env GOPATH 2>/dev/null)/bin/migrate}"
if [ -x "$MIGRATE_BIN" ] || command -v migrate >/dev/null 2>&1; then
  ok "golang-migrate 可用"
else
  bad "golang-migrate 未安装（go install -tags mysql github.com/golang-migrate/migrate/v4/cmd/migrate@latest）"
fi

if command -v golangci-lint >/dev/null 2>&1; then
  ok "golangci-lint $(golangci-lint --version 2>/dev/null | awk '{print $4}')"
else
  warn "golangci-lint 未安装（make lint 将不可用）"
fi

if command -v make >/dev/null 2>&1; then ok "make 可用"; else bad "make 未安装"; fi

hdr "配置文件"

[ -f .env ] && ok ".env 存在" || bad ".env 缺失（复制 .env.example）"
[ -f .env.example ] && ok ".env.example 存在" || bad ".env.example 缺失"

hdr "MySQL"

MYSQL_DSN="$(envval MYSQL_DSN)"
if [ -z "$MYSQL_DSN" ]; then
  bad "MYSQL_DSN 未配置"
elif ! command -v mysql >/dev/null 2>&1; then
  warn "mysql 客户端未安装，跳过连接测试"
else
  MUSER="${MYSQL_DSN%%:*}"
  MREST="${MYSQL_DSN#*:}"
  MPASS="${MREST%%@*}"
  MREST2="${MREST#*@tcp(}"
  MHOSTPORT="${MREST2%%)*}"
  MHOST="${MHOSTPORT%%:*}"
  MPORT="${MHOSTPORT##*:}"
  MDB="${MREST2#*)/}"; MDB="${MDB%%\?*}"

  ERRLOG=$(mktemp)
  # 用 MYSQL_PWD 传口令，避免 mysql 打印 "password on the command line" 警告污染输出
  if OUT=$(MYSQL_PWD="$MPASS" mysql -h "$MHOST" -P "$MPORT" -u "$MUSER" \
           --connect-timeout=5 -N -B -e "SELECT VERSION();" "$MDB" 2>"$ERRLOG"); then
    MVER=$(echo "$OUT" | head -1 | awk '{print $1}')
    ok "连接 $MUSER@$MHOST:$MPORT/$MDB 成功"
    case "$MVER" in
      8.*) ok "MySQL 版本 $MVER" ;;
      9.*) warn "MySQL 版本 $MVER（生产使用 8.4 LTS，注意 sql_mode 与废弃特性差异）" ;;
      *)   warn "MySQL 版本 $MVER（预期 8.4 LTS）" ;;
    esac
  else
    bad "MySQL 连接失败：$(head -1 "$ERRLOG")"
  fi
  rm -f "$ERRLOG"
fi

hdr "Redis"

RADDR="$(envval REDIS_ADDR)"; RDB="$(envval REDIS_DB)"; RPASS="$(envval REDIS_PASSWORD)"
if [ -z "$RADDR" ]; then
  bad "REDIS_ADDR 未配置"
elif ! command -v redis-cli >/dev/null 2>&1; then
  warn "redis-cli 未安装，跳过连接测试"
else
  RHOST="${RADDR%%:*}"; RPORT="${RADDR##*:}"
  # macOS 自带 bash 3.2，set -u 下展开空数组会报 unbound variable，故用 ${A[@]+...} 形式
  AUTH=()
  [ -n "$RPASS" ] && AUTH=(-a "$RPASS" --no-auth-warning)
  RCLI=(redis-cli -h "$RHOST" -p "$RPORT" ${AUTH[@]+"${AUTH[@]}"} -n "${RDB:-0}")
  if [ "$("${RCLI[@]}" PING 2>/dev/null)" = "PONG" ]; then
    KEYS=$("${RCLI[@]}" DBSIZE 2>/dev/null)
    ok "连接 $RHOST:$RPORT db${RDB:-0} 成功（当前 $KEYS 个键）"
    if [ "${RDB:-0}" = "0" ]; then
      warn "使用 db0，本地实例常被其他项目共用，建议改用独立 db index"
    fi
  else
    bad "Redis 连接失败（$RHOST:$RPORT db${RDB:-0}）"
  fi
fi

hdr "上游凭证"

if [ -n "$(envval FOXSSL_API_KEY)" ]; then
  warn "FOXSSL_API_KEY 已配置，请确认不是生产凭证"
else
  ok "FOXSSL_API_KEY 未配置（本地走 mock，符合预期）"
fi

hdr "支付渠道"

# PAYMENT_PROVIDER 缺省是 mock，所以空值不算错，只提示。
PPROVIDER="$(envval PAYMENT_PROVIDER)"
case "$PPROVIDER" in
  ""|mock) ok "PAYMENT_PROVIDER=${PPROVIDER:-mock}（本地模拟渠道，不会真的收钱）" ;;
  *)       ok "PAYMENT_PROVIDER=$PPROVIDER" ;;
esac

# PAYMENT_WEBHOOK_SECRET 是必填项：缺失时服务端会直接拒绝启动，
# 所以这里必须判成 bad 而不是 warn，否则自检全绿但服务起不来。
PSECRET="$(envval PAYMENT_WEBHOOK_SECRET)"
if [ -z "$PSECRET" ]; then
  bad "PAYMENT_WEBHOOK_SECRET 未配置（服务端启动会失败）"
elif [ "${#PSECRET}" -lt 32 ]; then
  warn "PAYMENT_WEBHOOK_SECRET 仅 ${#PSECRET} 位，非开发环境要求 ≥ 32 位"
else
  ok "PAYMENT_WEBHOOK_SECRET 已配置（${#PSECRET} 位）"
fi

# 模拟渠道不会真的收钱：它生成的支付地址是本地页面，回调也由本服务自己发出。
# 配上生产环境等于给所有人免费充值，服务端会拒绝启动，这里提前提醒。
if [ "$(envval APP_ENV)" = "production" ] && [ "${PPROVIDER:-mock}" = "mock" ]; then
  bad "APP_ENV=production 却启用了 mock 支付渠道（服务端会拒绝启动）"
fi

printf '\n\033[1m结果：\033[0m %d 通过, %d 警告, %d 失败\n' "$PASS" "$WARN" "$FAIL"
[ "$FAIL" -gt 0 ] && exit 1 || exit 0
