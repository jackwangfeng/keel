#!/usr/bin/env python3
"""从 CHANGELOG.md 里取出某一版的段落，取不到就非零退出。

这个脚本同时是**发布闸门**和**发布说明的来源**，两件事刻意由一段代码做。

分成两段的后果是具体的：闸门查「有没有这一节」，流水线另写一段把正文捞出来，
于是一个「标题在、正文空」的版本能过闸门、发出去一份空的 release notes。
让正文的提取动作本身当闸门，那种状态过不去——捞不到正文就是失败。

用法:
    scripts/changelog_section.py v0.1.0        # 打印那一节的正文
    scripts/changelog_section.py --unreleased  # 打印 Unreleased 那一节

判据（刻意只认 Keep a Changelog 的形状，不去猜别的写法）:
  · 版本标题是 `## [0.1.0] - 2026-09-26` 或 `## [Unreleased]`；
  · 正文是到下一个 `## ` 为止的内容；
  · 正文里除了空白之外什么都没有 → 失败，不是返回空串。

tag 与标题的对应关系: tag 写 `v0.1.0`，标题写 `[0.1.0]`。前缀 v 只属于 tag。
两边都写 v 或都不写也行，但**现在这个约定已经生效**，改它要同时改
.github/workflows/release.yml 里的传参。
"""

import argparse
import pathlib
import re
import sys

ROOT = pathlib.Path(__file__).resolve().parent.parent
CHANGELOG = ROOT / 'CHANGELOG.md'

HEADING = re.compile(r'^## \[([^\]]+)\]')


def section(text, wanted):
    lines = text.splitlines()
    start = None
    for i, line in enumerate(lines):
        m = HEADING.match(line)
        if m is None:
            continue
        if start is not None:
            # 撞上下一个版本标题，正文到此为止
            return lines[start:i]
        if m.group(1).lower() == wanted.lower():
            start = i + 1
    if start is None:
        return None
    return lines[start:]


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument('version', nargs='?', help='版本号，带不带前缀 v 都行')
    ap.add_argument('--unreleased', action='store_true')
    args = ap.parse_args()

    if args.unreleased == bool(args.version):
        ap.error('要么给一个版本号，要么给 --unreleased，不能都给也不能都不给')

    wanted = 'Unreleased' if args.unreleased else args.version.lstrip('v')

    if not CHANGELOG.exists():
        print(f'找不到 {CHANGELOG}', file=sys.stderr)
        return 1

    text = CHANGELOG.read_text(encoding='utf-8')
    body = section(text, wanted)

    if body is None:
        print(
            f'CHANGELOG.md 里没有 [{wanted}] 这一节。\n'
            f'发布一个版本之前要先给它写变更说明 —— 这不是格式要求，'
            f'是「发了什么」这个问题必须有人回答一次。',
            file=sys.stderr)
        return 1

    joined = '\n'.join(body).strip('\n')
    if not joined.strip():
        print(
            f'CHANGELOG.md 里 [{wanted}] 这一节是空的。\n'
            f'标题在、正文空，比整节都没有更糟：闸门会绿，而发出去的 '
            f'release notes 是一片空白。',
            file=sys.stderr)
        return 1

    print(joined)
    return 0


if __name__ == '__main__':
    sys.exit(main())
