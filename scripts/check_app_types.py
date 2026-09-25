#!/usr/bin/env python3
"""用 tsc --strict 编译 app/src 下的全部 .uts，并确认编译范围真的覆盖了它们。

## 这道闸门挡的是什么

「契约改了而客户端没跟上」。app/src/api/schema.uts 是从契约生成的产物
（scripts/check_uts_contract.py 保证它没漂移），客户端读契约字段的每一处都在
.uts 里 —— 契约把 `min_price_cents` 改个名字、重新生成之后，这里会红。

变异验证的做法记在 app/README.md：改一个契约字段名 -> `make generate-uts`
-> 这个脚本红。

## 为什么要先把 .uts 复制成 .ts

tsc 不认 .uts 后缀（`--allowArbitraryExtensions` 只管 .d.*.ts，不管源文件），
而源文件不能改名成 .ts —— uni-app x 的编译器认的就是 .uts。两边都要伺候，
所以在临时目录里做两条机械改写：

  1. 文件名 .uts -> .ts
  2. import/export 里的 `'./x.uts'` -> `'./x.ts'`

改写只有这两条，并且**对工作区只读**（复制出去改，源文件一个字节不碰），
与 scripts/check-all.sh 里 contract-check / sqlc-check 的做法一致。

## 为什么还要核对范围

`tsc` 对「范围里没有这个文件」是**静默**的：它编译剩下的部分然后退出 0。
web 那边踩过一次假绿（tsconfig 的 include 写成 `src/**/*.ts` 就漏掉全部 .mts，
见 scripts/check_ts_scope.py 的文件头）。这里同一个坑更浅：漏复制一个文件就行。
所以用 --listFiles 让 tsc 自己报出它读了哪些文件，再和磁盘上的 .uts 清单对。

## 它**没有**覆盖什么（说出来，别让读者以为都盖住了）

.uvue 里的模板表达式。tsc 不解析 SFC。客户端为此把契约字段的读取全部收进
app/src/api/view.uts，模板只碰那里定义的 Row 类型 —— 但这是一条靠人守的约定，
不是闸门。真的编 .uvue 的是 DCloud 自己的编译器，那道闸门在
scripts/check_app_build.py（要 node_modules，跑在 CI 的独立 job 里）。
"""
import os
import re
import shutil
import subprocess
import sys
import tempfile

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
SRC = os.path.join(ROOT, 'app', 'src')
TYPECHECK = os.path.join(ROOT, 'app', 'typecheck')

IMPORT_RE = re.compile(r"""(from\s+|import\s*\(\s*)(['"])([^'"]+)\.uts\2""")


def collect():
    """app/src 下要送进 tsc 的源文件：全部 .uts，外加 .d.ts。

    .d.ts 也要：src/uts-builtin.d.ts 声明的 UTSJSONObject 与 `JSON.parse<T>` 是
    .uts 里天天用的东西。**共用同一份声明**，而不是在 typecheck/ 下再抄一份 ——
    抄一份就又是两个会分叉的真相源，而这个仓库正在为此付第四次账。
    """
    found = []
    for dirpath, _, filenames in os.walk(SRC):
        for name in filenames:
            if name.endswith('.uts') or name.endswith('.d.ts'):
                found.append(os.path.join(dirpath, name))
    return sorted(found)


def main():
    if len(sys.argv) != 2:
        print('用法: check_app_types.py <typescript 的 npx 包名，例如 typescript@5.9.2>')
        return 2

    sources = collect()
    if not sources:
        print('FAIL: app/src 下一个源文件都没找到 —— 这个检查本身失效了')
        return 1

    tmp = tempfile.mkdtemp(prefix='keel-app-tsc-')
    try:
        # 1) 参数表与 uni API 的声明，原样复制。
        for name in os.listdir(TYPECHECK):
            shutil.copy2(os.path.join(TYPECHECK, name), os.path.join(tmp, name))

        # 2) .uts -> .ts，顺带改写 import 里的后缀。.d.ts 原样搬。
        want = set()
        for src in sources:
            rel = os.path.relpath(src, SRC)
            if rel.endswith('.d.ts'):
                dst = os.path.join(tmp, 'src', rel)
            else:
                dst = os.path.join(tmp, 'src', rel[:-len('.uts')] + '.ts')
            os.makedirs(os.path.dirname(dst), exist_ok=True)
            with open(src, encoding='utf-8') as f:
                text = f.read()
            with open(dst, 'w', encoding='utf-8') as f:
                f.write(IMPORT_RE.sub(lambda m: '%s%s%s.ts%s' % (
                    m.group(1), m.group(2), m.group(3), m.group(2)), text))
            want.add(os.path.realpath(dst))

        proc = subprocess.run(
            ['npx', '--yes', '-p', sys.argv[1], 'tsc',
             '--noEmit', '--listFiles', '-p', os.path.join(tmp, 'tsconfig.json')],
            cwd=tmp, capture_output=True, text=True)
        if proc.returncode != 0:
            # --listFiles 的那几百行文件清单在这里是噪音：失败时要看的是诊断。
            # 诊断的形状是 `path(line,col): error TSxxxx: msg`，续行以空白开头。
            diags = [line for line in proc.stdout.splitlines()
                     if ': error ' in line or line[:1].isspace()]
            # tsc 报的是临时目录里的路径（绝对，或相对 tmp 的 src/...），对读的人没用。
            # 换回源码树里的真实路径，并且把 .ts 改回 .uts —— 不然报错指向的文件
            # 在工作区里根本不存在，读的人第一反应是「这是不是哪个生成产物」。
            # `.d.ts` 不动：那几个声明文件本来就是 .d.ts。
            out = '\n'.join(diags)
            out = out.replace(os.path.join(tmp, 'src') + os.sep, 'app/src/')
            out = re.sub(r'(?m)^src/', 'app/src/', out)
            out = re.sub(r'(app/src/\S+?)(?<!\.d)\.ts(?=[(\s:])', r'\1.uts', out)
            print(out)
            sys.stderr.write(proc.stderr)
            return proc.returncode

        compiled = {os.path.realpath(line.strip())
                    for line in proc.stdout.splitlines() if line.strip()}
        missing = sorted(p for p in want if p not in compiled)
        if missing:
            print('FAIL: 这些文件复制进去了，但不在 tsc 的编译范围里：')
            for p in missing:
                print('  - %s' % os.path.relpath(p, tmp))
            print('')
            print('tsc 对此不会报错，它会编译剩下的部分然后退出 0。')
            print('检查 app/typecheck/tsconfig.json 的 include。')
            return 1

        print('app-type-check OK: app/src 下 %d 个源文件全部在 --strict 下编译通过'
              % len(sources))
        return 0
    finally:
        shutil.rmtree(tmp, ignore_errors=True)


if __name__ == '__main__':
    sys.exit(main())
