#!/usr/bin/env bash
# 删除 c-ssl 开发库与测试库（不可恢复）
# 用法：make db-drop CONFIRM=yes
set -euo pipefail

cd "$(dirname "$0")/.." || exit 1

envval() { [ -f .env ] && grep -E "^$1=" .env | head -1 | cut -d= -f2-; }

DSN="$(envval MYSQL_DSN)"
if [ -z "$DSN" ]; then
  echo "错误：.env 中未配置 MYSQL_DSN。" >&2
  exit 1
fi

REST="${DSN#*:}"
REST2="${REST#*@tcp(}"
DB_HOSTPORT="${REST2%%)*}"
DB_HOST="${DB_HOSTPORT%%:*}"
DB_PORT="${DB_HOSTPORT##*:}"
DB_NAME="${REST2#*)/}"; DB_NAME="${DB_NAME%%\?*}"
DB_TEST="${DB_NAME%_dev}_test"

echo "即将删除以下数据库，数据不可恢复："
echo "  - $DB_NAME"
echo "  - $DB_TEST"
echo
echo "注意：专用用户账号会保留，如需一并删除请手动执行 DROP USER。"
echo

ROOT_PW="${MYSQL_ROOT_PASSWORD:-}"
if [ -z "$ROOT_PW" ]; then
  read -r -s -p "MySQL root 口令（$DB_HOST:$DB_PORT）：" ROOT_PW
  echo
fi

mysql -h "$DB_HOST" -P "$DB_PORT" -u root -p"$ROOT_PW" --connect-timeout=5 <<SQL
DROP DATABASE IF EXISTS \`$DB_NAME\`;
DROP DATABASE IF EXISTS \`$DB_TEST\`;
SQL

echo "已删除 $DB_NAME、$DB_TEST"
