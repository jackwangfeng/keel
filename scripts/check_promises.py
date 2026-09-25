#!/usr/bin/env python3
"""检查 README 宣称存在的东西是否真的存在，以及有没有占位/虚标。"""
import io
import os
import re
import sys

# README 承诺存在的文件，相对仓库根目录
PROMISED_FILES = [
    'LICENSE',
    'CONTRIBUTING.md',
    'docs/电商系统-总体架构.md',
    'docs/电商系统-数据模型设计.md',
    'docs/电商系统-语义检索层设计.md',
    'docs/电商系统-商品理解服务设计.md',
    'docs/电商系统-OpenAPI.yaml',
]

READMES = ['README.md', 'README.zh-CN.md']
PLACEHOLDER_LINK_RE = re.compile(r'\[[^\]]*\]\(#\)')


def main():
    root = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
    problems = []

    for rel in PROMISED_FILES:
        if not os.path.exists(os.path.join(root, rel)):
            problems.append('README 承诺的文件不存在: %s' % rel)

    for rel in READMES:
        path = os.path.join(root, rel)
        if not os.path.exists(path):
            problems.append('README 本身缺失: %s' % rel)
            continue
        lines = io.open(path, encoding='utf-8').read().split('\n')
        for lineno, line in enumerate(lines, 1):
            for hit in PLACEHOLDER_LINK_RE.findall(line):
                problems.append(
                    '%s:%d 占位链接（指向 "#"）: %s' % (rel, lineno, hit.strip()))
            if 'img.shields.io' in line and 'CI' in line:
                problems.append(
                    '%s:%d 挂着 CI 徽章但仓库没有 CI，属于虚标' % (rel, lineno))

    if problems:
        print('发现 %d 处问题：' % len(problems))
        for p in problems:
            print('  ' + p)
        return 1
    print('README 承诺全部兑现')
    return 0


if __name__ == '__main__':
    sys.exit(main())
