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
# 任何形如「CI / build / tests + passing/success」的徽章图片。
# 不能只认 shields.io —— GitHub Actions 的官方徽章走 github.com/.../badge.svg，
# 那恰恰是最可能被真加回来的写法。
BUILD_BADGE_RE = re.compile(
    r'!\[[^\]]*\]\((https?://[^)]*?(?:'
    r'badge\.svg|shields\.io[^)]*?(?:ci|build|test|workflow)'
    r')[^)]*)\)', re.IGNORECASE)


def main():
    root = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
    problems = []
    # 徽章是否虚标，取决于仓库里到底有没有 CI
    wf = os.path.join(root, '.github', 'workflows')
    has_ci = os.path.isdir(wf) and any(
        f.endswith(('.yml', '.yaml')) for f in os.listdir(wf))

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
            for url in BUILD_BADGE_RE.findall(line):
                if not has_ci:
                    problems.append(
                        '%s:%d 挂着构建状态徽章但仓库没有 CI（.github/workflows/ 不存在），'
                        '属于虚标: %s' % (rel, lineno, url))

    if problems:
        print('发现 %d 处问题：' % len(problems))
        for p in problems:
            print('  ' + p)
        return 1
    print('README 承诺全部兑现')
    return 0


if __name__ == '__main__':
    sys.exit(main())
