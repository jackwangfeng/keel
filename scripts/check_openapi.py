#!/usr/bin/env python3
"""校验 OpenAPI 契约：可解析、零悬空 $ref、无孤儿 schema、必备路径齐全。"""
import io
import os
import re
import sys

import yaml

SPEC = 'docs/电商系统-OpenAPI.yaml'

# 契约必须提供的路径与方法。每个任务往这里追加，就是在写失败的测试。
REQUIRED_PATHS = [
    ('/uploads', 'post'),
]

# 契约必须定义的 schema。
REQUIRED_SCHEMAS = [
    'UploadTarget',
    'Upload',
]


# (schema 名, 必须存在的属性名)
REQUIRED_FIELDS = [
    ('Cart', 'selected_total_cents'),   # S1
    ('OrderItem', 'refunding_qty'),     # S2
    ('OrderDetail', 'refunds'),         # S15
]


def walk_refs(text):
    return set(re.findall(r"\$ref:\s*['\"]?(#[^'\"\s]+)", text))


def resolve(doc, ref):
    node = doc
    for part in ref.lstrip('#/').split('/'):
        part = part.replace('~1', '/').replace('~0', '~')
        if isinstance(node, dict) and part in node:
            node = node[part]
        else:
            return False
    return True


def main():
    root = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
    path = os.path.join(root, SPEC)
    text = io.open(path, encoding='utf-8').read()
    problems = []

    try:
        doc = yaml.safe_load(text)
    except yaml.YAMLError as exc:
        print('YAML 无法解析: %s' % exc)
        return 1

    if str(doc.get('openapi', '')).split('.')[0:2] != ['3', '1']:
        problems.append('openapi 版本不是 3.1，实际为 %r' % doc.get('openapi'))

    for ref in sorted(walk_refs(text)):
        if not resolve(doc, ref):
            problems.append('悬空 $ref: %s' % ref)

    schemas = doc.get('components', {}).get('schemas', {})
    referenced = {r.rsplit('/', 1)[-1] for r in walk_refs(text)
                  if '/schemas/' in r}
    for name in sorted(set(schemas) - referenced):
        problems.append('孤儿 schema（无人引用）: %s' % name)

    paths = doc.get('paths', {})
    for p, method in REQUIRED_PATHS:
        if p not in paths:
            problems.append('缺少路径: %s' % p)
        elif method not in paths[p]:
            problems.append('路径 %s 缺少方法: %s' % (p, method))

    for name in REQUIRED_SCHEMAS:
        if name not in schemas:
            problems.append('缺少 schema: %s' % name)

    def props_of(schema_name, _seen=None):
        """取 schema 的属性名集合。OrderDetail 用 allOf 继承 Order，
        只查顶层 properties 会漏掉继承来的字段，必须展开 allOf。"""
        _seen = _seen or set()
        if schema_name in _seen:
            return set()
        _seen.add(schema_name)
        node = schemas.get(schema_name, {})
        names = set(node.get('properties', {}))
        for branch in node.get('allOf', []):
            if '$ref' in branch:
                names |= props_of(branch['$ref'].rsplit('/', 1)[-1], _seen)
            else:
                names |= set(branch.get('properties', {}))
        return names

    for schema_name, field in REQUIRED_FIELDS:
        if field not in props_of(schema_name):
            problems.append('schema %s 缺少字段: %s' % (schema_name, field))

    # S3: GET /orders 必须支持按售后状态筛选
    get_orders = paths.get('/orders', {}).get('get', {})
    names = {p.get('name') for p in get_orders.get('parameters', [])
             if isinstance(p, dict)}
    if 'refund_status' not in names:
        problems.append('GET /orders 缺少 refund_status 查询参数')

    if problems:
        print('发现 %d 处问题：' % len(problems))
        for p in problems:
            print('  ' + p)
        return 1
    print('OpenAPI 契约校验通过（%d 个路径，%d 个 schema）'
          % (len(paths), len(schemas)))
    return 0


if __name__ == '__main__':
    sys.exit(main())
