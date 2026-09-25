#!/usr/bin/env python3
"""校验架构文档 §11 声称的 AI 能力条目数与 ai-capabilities.md 实际条目数一致。"""
import io
import os
import re
import sys

ARCH = 'docs/电商系统-总体架构.md'
CAPS = 'docs/ai-capabilities.md'
# 匹配「完整清单（37 项，...」这类声明
CLAIM_RE = re.compile(r'完整清单[（(]\s*(\d+)\s*项')
# 能力清单表格的数据行：| 1 | 名称 | ...
ROW_RE = re.compile(r'^\|\s*(\d+)\s*\|')


def main():
    root = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
    arch_path = os.path.join(root, ARCH)
    caps_path = os.path.join(root, CAPS)

    if not os.path.exists(caps_path):
        print('能力清单文档不存在: %s' % CAPS)
        return 1

    arch = io.open(arch_path, encoding='utf-8').read()
    caps = io.open(caps_path, encoding='utf-8').read()

    claims = CLAIM_RE.findall(arch)
    if not claims:
        print('架构文档中找不到「完整清单（N 项」的声明，无法校验')
        return 1

    nums = [int(ROW_RE.match(ln).group(1))
            for ln in caps.split('\n') if ROW_RE.match(ln)]
    actual = len(nums)
    problems = []
    if nums != list(range(1, actual + 1)):
        problems.append('能力编号不连续或有重复，实际序列首尾为 %r..%r'
                        % (nums[:3], nums[-3:]))
    for claimed in claims:
        if int(claimed) != actual:
            problems.append(
                '架构 §11 声称 %s 项，%s 实际 %d 项' % (claimed, CAPS, actual))

    if problems:
        print('发现 %d 处不一致：' % len(problems))
        for p in problems:
            print('  ' + p)
        print('\n处理方向：改架构文档里的数字，不要为了凑数往清单里注水。')
        return 1
    print('架构 §11 声称的条目数与实际一致（%d 项）' % actual)
    return 0


if __name__ == '__main__':
    sys.exit(main())
