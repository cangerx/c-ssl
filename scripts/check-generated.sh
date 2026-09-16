#!/usr/bin/env bash
# 校验生成物与 OpenAPI 契约一致（CI 用）。
#
# 用法：make gen-check
#
# 重新生成一次并比对哈希：不一致说明有人改了契约但没提交生成物，
# 或直接手改了生成物。两种情况都要在 CI 拦下。
#
# 注意：比对后会把文件还原为比对前的状态。否则校验失败时会把工作区改脏，
# 导致下一次运行出现"假失败"（上一次留下的内容被当成基线）。
set -euo pipefail

cd "$(dirname "$0")/.." || exit 1

OUT="packages/api-types/src/generated/schema.d.ts"

if [ ! -f "$OUT" ]; then
  echo "错误：$OUT 不存在，请先执行 make gen 并提交生成物。" >&2
  exit 1
fi

BACKUP=$(mktemp)
trap 'rm -f "$BACKUP"' EXIT
cp "$OUT" "$BACKUP"

BEFORE=$(shasum -a 256 "$OUT" | awk '{print $1}')
bash scripts/gen-api-types.sh >/dev/null
AFTER=$(shasum -a 256 "$OUT" | awk '{print $1}')

if [ "$BEFORE" != "$AFTER" ]; then
  cp "$BACKUP" "$OUT"
  echo "生成物与契约不一致：$OUT" >&2
  echo "请执行 make gen，并提交更新后的生成物。" >&2
  exit 1
fi

echo "生成物与契约一致：$OUT"
