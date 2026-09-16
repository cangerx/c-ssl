#!/usr/bin/env bash
# 创建 c-ssl 开发库、测试库与专用用户（幂等，可重复执行）
# 用法：make db-create
# 需要 MySQL 管理员权限。口令来源优先级：
#   1. 环境变量 MYSQL_ROOT_PASSWORD
#   2. 交互式输入
set -euo pipefail

cd "$(dirname "$0")/.." || exit 1

envval() { [ -f .env ] && grep -E "^$1=" .env | head -1 | cut -d= -f2-; }

DSN="$(envval MYSQL_DSN)"
if [ -z "$DSN" ]; then
  echo "错误：.env 中未配置 MYSQL_DSN，无法推导库名与用户。请先复制 .env.example 并填写。" >&2
  exit 1
fi

DB_USER="${DSN%%:*}"
REST="${DSN#*:}"
DB_PASS="${REST%%@*}"
REST2="${REST#*@tcp(}"
DB_HOSTPORT="${REST2%%)*}"
DB_HOST="${DB_HOSTPORT%%:*}"
DB_PORT="${DB_HOSTPORT##*:}"
DB_NAME="${REST2#*)/}"; DB_NAME="${DB_NAME%%\?*}"
DB_TEST="${DB_NAME%_dev}_test"

ROOT_PW="${MYSQL_ROOT_PASSWORD:-}"
if [ -z "$ROOT_PW" ]; then
  read -r -s -p "MySQL root 口令（$DB_HOST:$DB_PORT）：" ROOT_PW
  echo
fi

run_root() {
  mysql -h "$DB_HOST" -P "$DB_PORT" -u root -p"$ROOT_PW" --connect-timeout=5 "$@"
}

if ! run_root -e "SELECT 1" >/dev/null 2>&1; then
  echo "错误：无法以 root 连接 $DB_HOST:$DB_PORT，请检查口令。" >&2
  exit 1
fi

echo "→ 创建数据库 $DB_NAME、$DB_TEST"
run_root <<SQL
CREATE DATABASE IF NOT EXISTS \`$DB_NAME\`  CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci;
CREATE DATABASE IF NOT EXISTS \`$DB_TEST\` CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci;
SQL

echo "→ 创建用户 $DB_USER 并授权"
run_root <<SQL
CREATE USER IF NOT EXISTS '$DB_USER'@'127.0.0.1' IDENTIFIED BY '$DB_PASS';
CREATE USER IF NOT EXISTS '$DB_USER'@'localhost'  IDENTIFIED BY '$DB_PASS';
ALTER USER '$DB_USER'@'127.0.0.1' IDENTIFIED BY '$DB_PASS';
ALTER USER '$DB_USER'@'localhost'  IDENTIFIED BY '$DB_PASS';
GRANT ALL PRIVILEGES ON \`$DB_NAME\`.*  TO '$DB_USER'@'127.0.0.1';
GRANT ALL PRIVILEGES ON \`$DB_TEST\`.* TO '$DB_USER'@'127.0.0.1';
GRANT ALL PRIVILEGES ON \`$DB_NAME\`.*  TO '$DB_USER'@'localhost';
GRANT ALL PRIVILEGES ON \`$DB_TEST\`.* TO '$DB_USER'@'localhost';
FLUSH PRIVILEGES;
SQL

echo "→ 验证应用账号连接"
mysql -h "$DB_HOST" -P "$DB_PORT" -u "$DB_USER" -p"$DB_PASS" \
      -N -B -e "SELECT CONCAT('  ', CURRENT_USER(), ' @ ', VERSION());" "$DB_NAME"

echo
echo "完成。库：$DB_NAME / $DB_TEST    用户：$DB_USER"
