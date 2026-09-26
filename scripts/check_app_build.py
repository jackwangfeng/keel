#!/usr/bin/env python3
"""用 DCloud 自己的编译器真编一遍客户端，并且**把类型告警当成失败**。

## 为什么需要这一层包装，而不是直接 `uni build`

`uni build` 对类型问题的处理是：打印出来，然后**照样退出 0**。本机实测
（HBuilderX 5.2.6 那条线的 npm 包）：在一个 .uvue 里写 `const s: string = 1`，
输出里出现

    warning: Type 'number' is not assignable to type 'string'.
    at pages/index/index.uvue:8:12
    DONE  Build complete.

退出码 0。也就是说，**直接把 `uni build` 接到 CI 上等于接了一个永远不会红的
闸门** —— 比没有更糟，它还会让人放心。

所以这里解析输出：任何一行 `warning:` 或 `error TS`（uts 插件的类型诊断走这两种
前缀）都判失败。代价是要跟着 DCloud 的输出格式走；换来的是这条链路真的能红。
一旦它哪天不打印诊断了，下面那个「没有任何编译输出」的检查会红。

## 覆盖面：它比 scripts/check_app_types.py 多盖什么

多盖 **.uvue**（模板表达式、样式、pages.json 路由）。tsc 不解析 SFC，
所以模板里 `{{ item.min_price_cents }}` 这种写法在那道闸门的视野之外。

代价是它要 500 多个 npm 包和一个 16MB 的原生 binding，所以它**不在**
scripts/check-all.sh 里（那个脚本全程零 node_modules 是刻意的），
而是 CI 的一个独立 job。和 e2e 与 gates 分开的理由一样：前置条件不相交。
"""
import os
import re
import subprocess
import sys

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
APP = os.path.join(ROOT, 'app')

# uts 插件把类型诊断打成前两种形状；第三种是 app-android 的样式检查
# （`[plugin:uni:app-uvue-css] ERROR: property value `grid` is not supported ...`）——
# 原生端只支持 CSS 的一个子集，写了不支持的属性，uni build 打一行 ERROR 然后照样 exit 0，
# 而那条样式在真机上就是静默不生效。三种都要抓。
DIAG_RE = re.compile(r'(^|\s)warning:|error TS\d+|\]\s*ERROR:')
# 终端色彩会把上面的匹配搅乱，先剥掉。
ANSI_RE = re.compile(r'\x1b\[[0-9;]*[A-Za-z]|​')


def main():
    platform = sys.argv[1] if len(sys.argv) > 1 else 'h5'
    # 其余参数原样交给 `uni build`（例如自动化构建的 --auto-host / --auto-port）。
    extra = sys.argv[2:]

    if not os.path.isdir(os.path.join(APP, 'node_modules')):
        print('FAIL: app/node_modules 不在。先跑 `make app-install`。')
        print('      （这道闸门刻意不自己装依赖：装依赖是有网络与时间成本的动作，'
              '藏在一个叫 check 的脚本里会让它在 CI 上看起来莫名其妙地慢。）')
        return 1

    env = dict(os.environ)
    # 颜色码会把下面的匹配搅乱。NO_COLOR 不够（vite 认 FORCE_COLOR）。
    env['NO_COLOR'] = '1'
    env['FORCE_COLOR'] = '0'

    proc = subprocess.run(
        ['npx', 'uni', 'build', '--platform', platform] + extra,
        cwd=APP, capture_output=True, text=True, env=env)
    output = ANSI_RE.sub('', proc.stdout + proc.stderr)
    sys.stdout.write(output)

    if proc.returncode != 0:
        print('\nFAIL: uni build --platform %s 退出码 %d' % (platform, proc.returncode))
        return 1

    if 'Build complete' not in output and 'DONE' not in output:
        print('\nFAIL: uni build 退出 0 但没有「构建完成」的输出 —— '
              '这个闸门对输出格式有依赖，DCloud 换了格式就要跟着改。')
        return 1

    diags = [line for line in output.splitlines() if DIAG_RE.search(line)]
    if diags:
        print('\nFAIL: uni build 报了 %d 条类型诊断。'
              '它自己会退出 0（本机实测），所以这里按失败处理：' % len(diags))
        for line in diags:
            print('  %s' % line.strip())
        return 1

    print('\ncheck_app_build OK: --platform %s 编译通过，且没有任何类型诊断' % platform)
    return 0


if __name__ == '__main__':
    sys.exit(main())
