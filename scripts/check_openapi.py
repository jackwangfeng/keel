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
