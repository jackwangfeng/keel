#!/usr/bin/env python3
"""校验查询源**和 sqlc 产物**里都没有应用层的租户过滤。

租户由 RLS 在数据库层过滤，查询里**不得**出现 merchant_id。这是 Task 3 的核心
决定，理由写在 db/queries/products.sql 的注释里：应用层再加一遍 WHERE
merchant_id，「RLS 到底有没有生效」就变得测不出来了 —— 跨租户读不到数据既可能
是 RLS 拦住的，也可能只是那个 WHERE 拦住的，而 RLS 失效不报错、不变慢、
不留痕迹，项目里唯一能发现它的那条测试恰好被这个 WHERE 喂饱了。

所以这条规矩不能只活在注释里。有人补一个 `AND merchant_id = $3`，全部测试照样
绿，而 TestTenantsSeeOnlyTheirOwnProducts 从此空转 —— 它会一直是绿的，
包括在 RLS 被整个删掉的那一天。

**这个检查不是万能的**：它挡的是「应用层重复过滤」，挡不住别的绕过 RLS 的写法
（比如 SECURITY DEFINER 函数）。但它挡住的正是那条最顺手、最像是在做好事、
一旦落地就再也没人能发现 RLS 已经死了的写法。

### 为什么要同时扫产物

因为进程跑的是产物，不是源。实测：手改 internal/repository/internal/db 里的
ListProducts，给它加上 `AND merchant_id = current_merchant()`，`check-all.sh`
**全绿**，而且这个脚本还会理直气壮地打印「1 个查询文件里没有应用层租户过滤」
—— 它读的是 db/queries/products.sql，而那个文件确实没被动过。

check-all.sh 里的 sqlc 漂移闸门是第一道（产物必须能由源重生成出来），
这里是第二道：就算有人绕过重生成，那句 WHERE 也逃不掉。两道都要有，
因为它们失守的方式不一样 —— 漂移闸门在 sqlc 没装的环境里会整体失败，
而这一条只需要读文件。

### 想加一条真的需要 merchant_id 的查询怎么办

先想清楚它为什么不能靠 RLS。想清楚之后，把表名加进 ALLOW 并写明理由 ——
和 check_tenancy.py 的 EXEMPT 一样，这是一个需要解释的动作，不是默认行为。
"""
import io
import os
import re
import sys

QUERY_DIR = 'db/queries'
# sqlc 产物目录。真正被执行的 SQL 在这里，以反引号字面量的形式嵌在 .sql.go 里。
GEN_DIR = 'internal/repository/internal/db'
NEEDLE = 'merchant_id'

# 确实需要在应用层写 merchant_id 的查询，按「文件名:查询名」豁免，每条写明理由。
# 眼下是空的 —— 空着本身就是当前的结论。
ALLOW = {}

# 行注释与块注释。SQL 的字符串字面量里出现 -- 的情况这里不处理：
# 真出现了，这个脚本会多报一次，而多报一次是安全的失败方向。
BLOCK_COMMENT = re.compile(r'/\*.*?\*/', re.S)
NAME_RE = re.compile(r'^--\s*name:\s*(\S+)', re.M)


def strip_comments(line):
    """去掉行尾的 -- 注释，返回剩下的可执行部分。"""
    i = line.find('--')
    return line if i < 0 else line[:i]


def current_query_name(lines, upto):
    """返回 upto 行之前最近的一个 `-- name: X` 里的 X。"""
    for line in reversed(lines[:upto]):
        m = NAME_RE.match(line)
        if m:
            return m.group(1)
    return '(文件头，尚未进入任何查询)'


def check_file(path):
    text = io.open(path, encoding='utf-8').read()
    # 先把块注释整体抹成等长的空白，行号才不会错位。
    text = BLOCK_COMMENT.sub(lambda m: re.sub(r'[^\n]', ' ', m.group(0)), text)
    lines = text.split('\n')

    bad = []
    for n, line in enumerate(lines, 1):
        code = strip_comments(line)
        if NEEDLE not in code:
            continue
        name = current_query_name(lines, n - 1)
        if f'{os.path.basename(path)}:{name}' in ALLOW:
            continue
        bad.append((n, name, line.strip()))
    return bad


# sqlc 产物里，SQL 只出现在 `const xxx = `...`` 这样的反引号字面量中；
# 中文注释是 Go 的 // 行注释，在字面量之外，不会被误伤。
GO_RAW_RE = re.compile(r'`([^`]*)`', re.S)


def check_generated(path):
    """返回产物里出现应用层 merchant_id 过滤的 [(行号, 查询名, 行内容)]。"""
    text = io.open(path, encoding='utf-8').read()
    bad = []
    for m in GO_RAW_RE.finditer(text):
        body = m.group(1)
        if NEEDLE not in body:
            continue
        # 字面量第一行是 sqlc 写的 `-- name: X :many`。
        first = body.split('\n', 1)[0]
        nm = re.match(r'--\s*name:\s*(\S+)', first.strip())
        name = nm.group(1) if nm else '(未命名字面量)'
        base = text[:m.start()].count('\n')
        for off, line in enumerate(body.split('\n')):
            if NEEDLE not in strip_comments(line):
                continue
            n = base + off + 1
            if f'{os.path.basename(path)}:{name}' in ALLOW:
                continue
            bad.append((n, name, line.strip()))

    return bad


def main():
    root = os.path.join(os.path.dirname(os.path.abspath(__file__)), '..')
    qdir = os.path.join(root, QUERY_DIR)
    if not os.path.isdir(qdir):
        print(f'FAIL: 找不到 {QUERY_DIR}/ —— 查询目录挪了地方，这个检查已经形同虚设')
        return 1

    files = sorted(f for f in os.listdir(qdir) if f.endswith('.sql'))
    if not files:
        # 一个空目录会让这个脚本永远「通过」。那不是通过，是没在检查。
        print(f'FAIL: {QUERY_DIR}/ 里一个 .sql 都没有，这个检查没有检查任何东西')
        return 1

    failed = 0
    for f in files:
        bad = check_file(os.path.join(qdir, f))
        for n, name, line in bad:
            failed += 1
            print(f'FAIL: {QUERY_DIR}/{f}:{n} 查询 {name} 的非注释行里出现了 '
                  f'{NEEDLE} —— 租户由 RLS 过滤，应用层不要再加一遍')
            print(f'      {line}')

    gdir = os.path.join(root, GEN_DIR)
    if not os.path.isdir(gdir):
        print(f'FAIL: 找不到 {GEN_DIR}/ —— sqlc 产物挪了地方，而进程跑的正是它')
        return 1
    gfiles = sorted(f for f in os.listdir(gdir) if f.endswith('.sql.go'))
    if not gfiles:
        # 同 QUERY_DIR：一个空目录会让这个脚本永远「通过」，那不是通过。
        print(f'FAIL: {GEN_DIR}/ 里一个 .sql.go 都没有，产物侧没有被检查')
        return 1
    for f in gfiles:
        for n, name, line in check_generated(os.path.join(gdir, f)):
            failed += 1
            print(f'FAIL: {GEN_DIR}/{f}:{n} 查询 {name} 的 SQL 里出现了 {NEEDLE} —— '
                  f'租户由 RLS 过滤，应用层不要再加一遍')
            print(f'      {line}')
            print(f'      （这是**产物**。产物和 db/queries 不一致时，'
                  f'check-all.sh 的 sqlc 漂移闸门也会红。）')

    if failed:
        print()
        print('应用层重复过滤会让「RLS 有没有生效」变得测不出来：跨租户读不到数据')
        print('既可能是 RLS 拦住的，也可能只是这个 WHERE 拦住的。真要加，')
        print('先在 scripts/check_query_tenancy.py 的 ALLOW 里写明理由。')
        return 1

    print(f'check_query_tenancy OK: {len(files)} 个查询文件 + {len(gfiles)} 个 sqlc 产物文件'
          f'里都没有应用层租户过滤')
    return 0


if __name__ == '__main__':
    sys.exit(main())
