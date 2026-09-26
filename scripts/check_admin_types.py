#!/usr/bin/env python3
"""用 vue-tsc --strict 编译 web/admin，并核对编译范围真的盖住了它的每一个源文件。

## 这道闸门挡的是什么

「契约改了而商家后台没跟上」。后台的每一个请求 / 响应类型都从
`web/src/api/schema.d.ts` 取（那是 `make generate` 从
`docs/电商系统-OpenAPI.yaml` 生成并入库的产物）。契约里把
`min_price_cents` 改个名字、重新生成之后，读它的那些 `.vue` 就对不上了 ——
这条检查让那件事**当场变红**，而不是等到线上解析出 undefined。

变异验证（真的跑过一次，步骤记在 web/admin/README.md）：
改契约里的一个字段名 → `make generate-ts` → 这个脚本红。

## 为什么它不是 scripts/check_ts_scope.py 的一部分

那一条守的是 `web/src`：契约产物本身，加上一份零运行时依赖的 SDK。
它的全部价值在于**不需要 node_modules** —— `npx` 拉一个钉死版本的 `tsc`，
`types: []` 不去扫任何 `@types`。把一个 Vue 应用塞进那个范围会毁掉这条性质：

  · `tsc` 读不了 `.vue`（SFC 不是 TypeScript，`--allowArbitraryExtensions`
    只管 `.d.*.ts`）。而 check_ts_scope.py 的 SUFFIXES 里没有 `.vue`，
    于是几十个文件会静默地**不在任何闸门的视野里**，而闸门照样报绿 ——
    那正是 check_ts_scope.py 的文件头写着要防的那件事。
  · 换成 `vue-tsc` 就等于把契约产物那道闸门的成败绑在一棵 UI 框架依赖树上。
    element-plus 的某个 `.d.ts` 在新版 TS 下报错，契约的闸门会变红，
    而那和契约一点关系都没有。

所以后台自带这一份，用 `vue-tsc`，认 `.vue`。
**web/src 那道闸门的范围一个字节都没动**（web/tsconfig.json 的 include 仍是
`["src"]`，check_ts_scope.py 仍然走 `web/src`）—— 这里是**新增**一道，
不是把旧的那道改松。

## 为什么还要核对范围

和 check_ts_scope.py 一字不差的理由：`tsc` / `vue-tsc` 对「范围里没有这个
文件」是**静默**的，它编译剩下的部分然后退出 0。这个项目的 include 写成
三条 glob（`src/**/*.ts` / `src/**/*.d.ts` / `src/**/*.vue`），少一条就漏掉
一整类文件 —— 而删那一条的人会觉得自己写得更精确了。用 `--listFiles` 让
编译器自己报出它读了哪些文件，比重新解释一遍匹配规则可靠。

## 它还核对一件 check_ts_scope.py 不核对的事

**契约产物必须真的在编译范围里。** 后台读契约类型的路径是
`@contract/schema.js` → `web/src/api/schema.d.ts`，没有第二份。如果哪天有人
把那个文件复制进 `web/admin/src`（「这样 import 路径短一点」），编译照样过，
而后台从此读的是一份会和契约分叉的副本 —— 一个不会有任何东西报错的退化。
所以这里直接断言那个绝对路径出现在 --listFiles 的输出里，并拒绝
web/admin/src 下出现同名文件。
"""
import os
import subprocess
import sys

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
PROJECT = os.path.join(ROOT, 'web', 'admin')
SRC = os.path.join(PROJECT, 'src')
CONFIG = os.path.join(PROJECT, 'tsconfig.json')
# 契约产物。后台必须编译到这一份，不能是副本。
CONTRACT_TS = os.path.join(ROOT, 'web', 'src', 'api', 'schema.d.ts')
# 源文件后缀。`.vue` 在这里，这正是「让闸门认识 .vue」那条路的落点。
SUFFIXES = ('.ts', '.mts', '.cts', '.tsx', '.vue')
BIN = os.path.join(PROJECT, 'node_modules', '.bin', 'vue-tsc')


def main() -> int:
    if not os.path.exists(BIN):
        # 诚实的失败，不是静默跳过。跳过的话，「后台的类型检查跑过了」
        # 这句话在没装依赖的机器上会是假的，而它看起来是真的。
        print('FAIL: 没找到 %s' % os.path.relpath(BIN, ROOT))
        print('      后台的类型检查要它自己那棵依赖树（vue-tsc 认 .vue，npx 拉不到一个能用的组合）。')
        print('      先跑：make admin-install')
        return 1

    proc = subprocess.run(
        [BIN, '--noEmit', '--listFiles', '-p', CONFIG],
        cwd=PROJECT, capture_output=True, text=True)
    if proc.returncode != 0:
        # 编译器的报错原样透出去，别藏在这个脚本的输出后面。
        sys.stdout.write(proc.stdout)
        sys.stderr.write(proc.stderr)
        return proc.returncode

    compiled = set()
    for line in proc.stdout.splitlines():
        path = line.strip()
        if path:
            compiled.add(os.path.realpath(path))

    want = []
    for dirpath, dirnames, filenames in os.walk(SRC):
        dirnames[:] = [d for d in dirnames if d != 'node_modules']
        for name in filenames:
            if name.endswith(SUFFIXES):
                want.append(os.path.realpath(os.path.join(dirpath, name)))

    if not want:
        print('FAIL: %s 下一个源文件都没找到——这个检查本身失效了'
              % os.path.relpath(SRC, ROOT))
        return 1

    fail = False

    missing = sorted(p for p in want if p not in compiled)
    if missing:
        print('FAIL: 这些文件在 web/admin/src 下，但不在 %s 的编译范围里：'
              % os.path.relpath(CONFIG, ROOT))
        for p in missing:
            print('  - %s' % os.path.relpath(p, ROOT))
        print('')
        print('vue-tsc 对此不会报错，它会编译剩下的部分然后退出 0。')
        print('检查 tsconfig.json 的 include —— 它是三条 glob，`src` 这个目录形式不收 .vue。')
        fail = True

    # 契约产物必须是**那一份**，不是副本。
    if os.path.realpath(CONTRACT_TS) not in compiled:
        print('FAIL: 后台的编译范围里没有 web/src/api/schema.d.ts。')
        print('      后台的每一个请求 / 响应类型都该从那份契约产物来；它不在范围里，')
        print('      说明类型是从别处来的（手写的，或者一份副本），而那两种都不会跟着契约改。')
        fail = True

    copies = [p for p in want if os.path.basename(p) == 'schema.d.ts']
    if copies:
        print('FAIL: web/admin/src 下出现了 schema.d.ts：')
        for p in copies:
            print('  - %s' % os.path.relpath(p, ROOT))
        print('      契约产物只有 web/src/api/schema.d.ts 一份。复制一份进来，')
        print('      编译照样过，而后台从此读的是一份会和契约分叉的副本。')
        fail = True

    if fail:
        return 1

    print('admin-type-check OK: web/admin/src 下 %d 个文件（含 .vue）全部在 --strict 下'
          '编译通过，且类型来自入库的契约产物' % len(want))
    return 0


if __name__ == '__main__':
    sys.exit(main())
