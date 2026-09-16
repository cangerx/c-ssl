#!/usr/bin/env bash
# 由 OpenAPI 契约生成 TypeScript 类型。
#
# 用法：make gen
#
# 生成物 packages/api-types/src/generated/ 禁止手工修改，
# 契约变更后重新执行本脚本即可。
set -euo pipefail

cd "$(dirname "$0")/.." || exit 1

SPEC="openapi/openapi.yaml"
OUT="packages/api-types/src/generated/schema.d.ts"

if [ ! -f "$SPEC" ]; then
  echo "错误：找不到契约文件 $SPEC" >&2
  exit 1
fi

if [ ! -d node_modules ]; then
  echo "错误：依赖未安装，请先执行 pnpm install" >&2
  exit 1
fi

mkdir -p "$(dirname "$OUT")"

# 本机沙箱通过 NODE_OPTIONS 注入了一个 fs shim，只支持递归 mkdir，
# 会导致部分 node 工具在创建目录时失败，这里清掉该变量。
env -u NODE_OPTIONS pnpm exec openapi-typescript "$SPEC" -o "$OUT"

echo "已生成 $OUT"
