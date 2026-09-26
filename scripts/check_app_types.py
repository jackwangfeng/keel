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

.uvue 里的模板表达式与 <script> 块。tsc 不解析 SFC。客户端为此把契约字段的
读取与**构造**全部收进 app/src/api/view.uts，页面只碰那里定义的类型与函数。
真的编 .uvue 的是 DCloud 自己的编译器，由 scripts/check_app_build.py 包一层：
裸的 `uni build` 把类型错误打成 warning 然后 exit 0，那一层把 warning 当失败。
**它拦得住** —— 但要 500 多个 npm 包，只在 CI 的独立 client job 里跑。

所以「页面不碰契约类型」在这一层原本是**一条靠人守的约定，不是闸门**。它被
违反过一次：多门店（00020）把 store_id 加进了 OrderCreateRequest 的 required，
结算页在 .uvue 里写着 `const req: OrderCreateRequest = { items, address_id }`。
这里看不见 .uvue；check_app_build.py 看得见（实测把旧写法塞回去它 exit 1，
报的正是 `Property 'store_id' is missing`），但合并时本地只跑了 check-all.sh
与 make test-db，它没被跑到。结果是 App 对着新后端结算一律 422。

下面的 check_pages() 不是替代那道闸门，是**把发现的时间提前**：从一个要装
500 个包的 CI job，挪进每个人本地都跑、零 node_modules 的 check-all.sh。

现在它有了一个执行者，check_pages()，查两件事：
  1. .uvue 里**不许 import schema.uts**。页面要的类型从 view.uts 拿；
  2. 收契约请求的那几个接口函数（REQUEST_FNS），**不许直接收一个对象字面量**。
     字面量在 .uvue 里写，契约改了字段名它不会红；换成 view.uts 里的构造函数，
     这里就看得见。

**它的边界**：这是正则，不是类型检查。先把字面量赋给一个变量、再把变量传进去，
它看不出来。它守的是「在页面里就地写契约请求」这个真实发生过的形状，
不是所有可能的绕法。
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


# 收契约请求类型的接口函数（app/src/api/client.uts）。它们的第一个参数是一个
# 契约 schema；页面必须传 view.uts 里某个构造函数的返回值，不许就地写字面量。
REQUEST_FNS = ('listProducts', 'search', 'previewOrder', 'createOrder')

SCHEMA_IMPORT_RE = re.compile(r"""from\s+['"][^'"]*/schema\.uts['"]""")
INLINE_LITERAL_RE = re.compile(r'\b(%s)\(\s*\{' % '|'.join(REQUEST_FNS))


def check_pages():
    """.uvue 里不许直接碰契约类型。返回违规清单（空表示通过）。见模块文档第二节。"""
    problems = []
    for dirpath, _dirs, files in os.walk(SRC):
        for name in files:
            if not name.endswith('.uvue'):
                continue
            path = os.path.join(dirpath, name)
            rel = os.path.relpath(path, ROOT)
            with open(path, encoding='utf-8') as f:
                text = f.read()
            lines = text.splitlines()
            # **对整个文件匹配，不逐行**。这个仓库的页面里调用几乎都是分行写的：
            #     listProducts(
            #       { page: 1, page_size: 20 },
            # 逐行匹配的第一版在这个形状上是全绿的 —— 变异验证时把字面量写回列表页，
            # 闸门照样 OK。INLINE_LITERAL_RE 里的 \s* 本来就能跨行，只是没给它机会。
            for regex, what in ((SCHEMA_IMPORT_RE, None), (INLINE_LITERAL_RE, 'literal')):
                for m in regex.finditer(text):
                    lineno = text.count('\n', 0, m.start()) + 1
                    src = lines[lineno - 1].strip()
                    if what is None:
                        problems.append('%s:%d  直接 import 了 schema.uts：%s'
                                        % (rel, lineno, src))
                    else:
                        problems.append('%s:%d  %s() 直接收了一个对象字面量：%s'
                                        % (rel, lineno, m.group(1), src))
    return problems


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
    page_problems = check_pages()
    if page_problems:
        print('FAIL: 页面（.uvue）里直接碰了契约类型：')
        for p in page_problems:
            print('  - ' + p)
        print('')
        print('.uvue 不在 tsc 的视野里，DCloud 编译器又只打 warning —— 契约改了')
        print('这些地方不会红。把请求的构造挪进 app/src/api/view.uts（那里有')
        print('productListQuery / searchRequest / orderRequest 可以照着写），')
        print('页面只传原始值。理由见本脚本的模块文档。')
        return 1

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
