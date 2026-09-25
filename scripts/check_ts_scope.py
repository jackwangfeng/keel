#!/usr/bin/env python3
"""编译 web/src，并确认编译范围真的覆盖了 web/src 下的每一个源文件。

为什么需要第二件事：

`tsc` 对「范围里没有这个文件」是**静默**的——它不会说「你漏了 client.mts」，
它会安安静静地编译剩下的部分然后报绿。实测过一次假绿：把契约里的
`ProductSummary.min_price_cents` 改名、重新生成产物，此时 SDK 的调用点已经
对不上了，只要把 tsconfig 的 include 从 `src` 写成 `src/**/*.ts`，
`tsc` 退出码就是 0 —— 因为 `**/*.ts` **不匹配 .mts**。

也就是说，闸门的覆盖面本身没有闸门守着，而让它失效只需要改一行 glob，
改的人还会觉得自己写得更精确了。这里把「范围覆盖到哪些文件」从一个
无人核对的配置项，变成一条会红的断言。

用 --listFiles 让 tsc 自己报出它读了哪些文件，比重新解释一遍 include/exclude
的匹配规则可靠：那等于把 tsc 的实现抄一份，而抄错的那份不会有人发现。
"""
import os
import subprocess
import sys

SRC = 'web/src'
CONFIG = 'web/tsconfig.json'
# 源文件后缀。.d.ts 也算——node.d.ts 和 schema.d.ts 都在范围里才算数。
SUFFIXES = ('.ts', '.mts', '.cts', '.tsx')


def main():
    if len(sys.argv) != 2:
        print('用法: check_ts_scope.py <typescript 的 npx 包名，例如 typescript@5.9.2>')
        return 2
    root = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))

    proc = subprocess.run(
        ['npx', '--yes', '-p', sys.argv[1], 'tsc',
         '--noEmit', '--listFiles', '-p', os.path.join(root, CONFIG)],
        cwd=root, capture_output=True, text=True)
    if proc.returncode != 0:
        # tsc 的报错原样透出去，别把它藏在这个脚本的输出后面。
        sys.stdout.write(proc.stdout)
        sys.stderr.write(proc.stderr)
        return proc.returncode

    compiled = set()
    for line in proc.stdout.splitlines():
        path = line.strip()
        if path:
            compiled.add(os.path.realpath(path))

    want = []
    for dirpath, _, filenames in os.walk(os.path.join(root, SRC)):
        for name in filenames:
            if name.endswith(SUFFIXES):
                want.append(os.path.realpath(os.path.join(dirpath, name)))

    if not want:
        print('FAIL: %s 下一个 TypeScript 源文件都没找到——这个检查本身失效了' % SRC)
        return 1

    missing = sorted(p for p in want if p not in compiled)
    if missing:
        print('FAIL: 这些文件在 %s 下，但不在 %s 的编译范围里：' % (SRC, CONFIG))
        for p in missing:
            print('  - %s' % os.path.relpath(p, root))
        print('')
        print('tsc 对此不会报错，它会编译剩下的部分然后退出 0。')
        print('检查 %s 的 include/exclude —— 注意 `src/**/*.ts` 不匹配 .mts。' % CONFIG)
        return 1

    print('schema-check OK: %s 下 %d 个文件全部在 --strict 下编译通过'
          % (SRC, len(want)))
    return 0


if __name__ == '__main__':
    sys.exit(main())
