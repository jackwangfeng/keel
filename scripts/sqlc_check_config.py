#!/usr/bin/env python3
"""拼一份把产物指向别处的临时 sqlc 配置，供 scripts/check-all.sh 的漂移闸门用。

用法：sqlc_check_config.py <仓库根> <临时目录> <产物子目录名>

在 <临时目录> 下产出：
  sqlc.yaml          —— 只改了 out 的配置副本
  db -> <仓库根>/db  —— 符号链接，让 schema / queries 那两条相对路径照旧解析
  <产物子目录名>/    —— sqlc 的落点

### 为什么要这么一层

sqlc 的产物路径写死在 sqlc.yaml 里，没有 oapi-codegen 那样的 `-o` 参数，
也就没法像 contract-check 那样直接 `make ... GO_OUT=/tmp/...` 把产物挪走。
而闸门必须**对工作区只读**：上一版契约闸门是重生成到源码树再 `git diff`，
那会让「跑一次检查」这个动作本身有破坏性（生成失败当场毁掉入库产物），
还会把未 git add 的手改悄悄冲掉 —— 而手改正是它该抓的东西。

### 为什么是符号链接而不是把路径改成绝对的

试过，sqlc 会把配置里的路径**朴素地拼**在配置文件所在目录后面，
于是绝对路径变成 `/tmp/xxx/sqlc/home/jeffwang/.../db/migrations`。
所以三条路径全部保持相对，靠一个 db 符号链接把它们指回仓库。

### 失败方向必须是「拒绝跑」

`out:` 没被改到，产物就会落进工作区 —— 悄悄地，并且看起来是通过的。
所以：`out:` 必须恰好命中一次，且 schema / queries 必须都还在 db/ 下面
（符号链接只搭了这一个）。有一条不成立就退出 1，绝不按老路径跑。
"""
import io
import os
import re
import sys


def main(argv):
    if len(argv) != 4:
        print('用法: sqlc_check_config.py <仓库根> <临时目录> <产物子目录名>',
              file=sys.stderr)
        return 2
    root, tmpdir, out_name = argv[1:4]

    src = io.open(os.path.join(root, 'sqlc.yaml'), encoding='utf-8').read()

    paths = {k: re.findall(r'(?m)^\s*%s:\s*(\S+)\s*$' % k, src)
             for k in ('schema', 'queries')}
    for key, found in paths.items():
        if len(found) != 1 or not found[0].startswith('db/'):
            print('FAIL: sqlc.yaml 里的 %s 是 %r，期望恰好一条、且在 db/ 下面。' % (key, found),
                  file=sys.stderr)
            print('      这道闸门靠一个 db 符号链接把相对路径指回仓库，写法变了就不成立。',
                  file=sys.stderr)
            print('      拒绝继续——再跑下去 sqlc 要么找不到输入，要么写进工作区。',
                  file=sys.stderr)
            return 1

    text, n_out = re.subn(r'(?m)^(\s*)out:.*$', r'\g<1>out: ' + out_name, src)
    if n_out != 1:
        print('FAIL: 改写 sqlc.yaml 的 out 命中了 %d 次，期望恰好 1 次。' % n_out,
              file=sys.stderr)
        print('      产物路径没被挪走时，这个声称对工作区只读的检查会把产物',
              file=sys.stderr)
        print('      生成进源码树。拒绝继续。', file=sys.stderr)
        return 1

    link = os.path.join(tmpdir, 'db')
    if not os.path.islink(link):
        os.symlink(os.path.join(root, 'db'), link)
    os.makedirs(os.path.join(tmpdir, out_name), exist_ok=True)
    io.open(os.path.join(tmpdir, 'sqlc.yaml'), 'w', encoding='utf-8').write(text)
    return 0


if __name__ == '__main__':
    sys.exit(main(sys.argv))
