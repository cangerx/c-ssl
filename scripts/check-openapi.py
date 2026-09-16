#!/usr/bin/env python3
"""校验 openapi/ 契约文件。

检查项：
  1. YAML 语法
  2. 所有 $ref 可解析（跨文件 + JSON Pointer）
  3. 每个 path item 至少含一个 HTTP 操作
  4. 每个操作有 operationId 且全局唯一
  5. 每个操作声明了 responses

用法：make spec-check
"""
from __future__ import annotations

import sys
from pathlib import Path

try:
    import yaml
except ImportError:
    sys.exit("需要 PyYAML：pip install pyyaml")

ROOT = Path(__file__).resolve().parent.parent
SPEC = ROOT / "openapi" / "openapi.yaml"
HTTP_METHODS = {"get", "put", "post", "delete", "patch", "head", "options", "trace"}

errors: list[str] = []
_doc_cache: dict[Path, object] = {}
refs_seen = 0


def rel(path: Path) -> str:
    try:
        return str(path.relative_to(ROOT))
    except ValueError:
        return str(path)


def load(path: Path):
    """加载并缓存 YAML，避免同一文件重复解析与重复报错。"""
    if path in _doc_cache:
        return _doc_cache[path]
    try:
        with path.open(encoding="utf-8") as fh:
            doc = yaml.safe_load(fh)
    except yaml.YAMLError as exc:
        mark = getattr(exc, "problem_mark", None)
        loc = f" 第 {mark.line + 1} 行" if mark else ""
        errors.append(f"YAML 解析失败 {rel(path)}{loc}: {getattr(exc, 'problem', exc)}")
        doc = None
    _doc_cache[path] = doc
    return doc


def split_ref(ref: str, from_file: Path) -> tuple[Path, str]:
    file_part, _, pointer = ref.partition("#")
    target = from_file if not file_part else (from_file.parent / file_part).resolve()
    return target, pointer


def follow(doc, pointer: str):
    """按 JSON Pointer 在 doc 内下钻，失败返回 None。"""
    if not pointer or pointer == "/":
        return doc
    node = doc
    for token in pointer.lstrip("/").split("/"):
        token = token.replace("~1", "/").replace("~0", "~")
        if isinstance(node, dict) and token in node:
            node = node[token]
        elif isinstance(node, list) and token.isdigit() and int(token) < len(node):
            node = node[int(token)]
        else:
            return None
    return node


def resolve_ref(ref: str, from_file: Path):
    """解析一个 $ref，返回 (节点, 所在文件)。失败返回 (None, None)。"""
    target, pointer = split_ref(ref, from_file)
    if not target.exists():
        errors.append(f"引用文件不存在: {ref}  （来自 {rel(from_file)}）")
        return None, None
    doc = load(target)
    if doc is None:
        return None, None
    node = follow(doc, pointer)
    if node is None:
        errors.append(f"引用路径不存在: {ref}  （来自 {rel(from_file)}）")
        return None, None
    return node, target


def deref(node, from_file: Path, depth: int = 0):
    """若节点是纯 $ref，跟进解析（最多 5 层，防循环）。"""
    while isinstance(node, dict) and "$ref" in node and depth < 5:
        node, from_file = resolve_ref(node["$ref"], from_file)
        if node is None:
            return None, from_file
        depth += 1
    return node, from_file


def walk(node, path: Path) -> None:
    """递归遍历，校验每个 $ref 是否可解析。"""
    global refs_seen
    if isinstance(node, dict):
        for key, value in node.items():
            if key == "$ref" and isinstance(value, str):
                refs_seen += 1
                resolve_ref(value, path)
            else:
                walk(value, path)
    elif isinstance(node, list):
        for value in node:
            walk(value, path)


def check_operations(doc: dict) -> None:
    paths = doc.get("paths") or {}
    if not paths:
        errors.append("openapi.yaml 未声明任何 paths")
        return

    operation_ids: dict[str, str] = {}
    for route, raw_item in paths.items():
        item, item_file = deref(raw_item, SPEC)
        if not isinstance(item, dict):
            errors.append(f"path {route} 无法解析为对象")
            continue

        ops = [m for m in item if m.lower() in HTTP_METHODS]
        if not ops:
            errors.append(f"path {route} 未定义任何 HTTP 操作")
            continue

        for method in ops:
            op, _ = deref(item[method], item_file)
            op = op or {}
            label = f"{method.upper()} {route}"

            oid = op.get("operationId")
            if not oid:
                errors.append(f"{label} 缺少 operationId")
            elif oid in operation_ids:
                errors.append(f"operationId 重复: {oid}（{operation_ids[oid]} 与 {label}）")
            else:
                operation_ids[oid] = label

            if not op.get("responses"):
                errors.append(f"{label} 未声明 responses")


def main() -> int:
    if not SPEC.exists():
        print(f"找不到 {SPEC}")
        return 1

    doc = load(SPEC)
    if doc is None:
        for e in dict.fromkeys(errors):
            print(f"  ✗ {e}")
        return 1

    walk(doc, SPEC)
    check_operations(doc)

    if errors:
        print(f"契约校验失败（{rel(SPEC)}）：")
        for e in dict.fromkeys(errors):
            print(f"  ✗ {e}")
        return 1

    print(f"契约校验通过：{rel(SPEC)}")
    print(f"  已解析 $ref {refs_seen} 处，接口 {len(doc.get('paths', {}))} 个")
    return 0


if __name__ == "__main__":
    sys.exit(main())
