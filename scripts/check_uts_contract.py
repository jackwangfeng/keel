#!/usr/bin/env python3
"""app/src/api/schema.uts 与契约是否同步。

与 scripts/check-all.sh 里 contract-check / sqlc-check 同一个做法：
**生成到临时目录再 diff，对工作区只读**。入库的那一份一个字节都不会被碰
（上一版的 contract-check 直接重生成到源码树再 git diff，两个毛病：生成失败会毁掉
入库产物；未 git add 的手改会被悄悄冲掉 —— 而那正是闸门该抓的）。

它挡的是「契约改了却没重生成」和「有人手改了生成文件」。
**它不挡「客户端没跟上」** —— 那是 scripts/check_app_types.py 的事：
重生成之后字段改了名，读它的每一处都会在 tsc --strict 下红。
两道要一起看：只有前一道，客户端可以一直读一个已经不存在的字段；
只有后一道，谁手改一下生成文件就能把红色抹掉。
"""
import os
import subprocess
import sys
import tempfile

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
COMMITTED = os.path.join(ROOT, 'app', 'src', 'api', 'schema.uts')
GENERATOR = os.path.join(ROOT, 'scripts', 'gen_uts_schema.py')


def main():
    if not os.path.exists(COMMITTED):
        print('FAIL: 没有 app/src/api/schema.uts。跑 `make generate-uts`。')
        return 1

    with tempfile.TemporaryDirectory(prefix='keel-uts-schema-') as tmp:
        fresh = os.path.join(tmp, 'schema.uts')
        proc = subprocess.run([sys.executable, GENERATOR, '-o', fresh],
                              capture_output=True, text=True)
        if proc.returncode != 0:
            print('FAIL: 从契约生成 UTS 类型失败：')
            sys.stdout.write(proc.stdout)
            sys.stderr.write(proc.stderr)
            return 1

        with open(COMMITTED, encoding='utf-8') as f:
            committed = f.read()
        with open(fresh, encoding='utf-8') as f:
            regenerated = f.read()

        if committed != regenerated:
            print('FAIL: app/src/api/schema.uts 与契约重生成的结果不一致。')
            print('      契约改了却没重生成，或者有人手改了生成文件。'
                  '请跑 `make generate-uts` 并一起提交：')
            # 截断到 40 行：契约里一个字段名往往在好几个 schema 里出现，
            # 整份 diff 能刷几屏，而判断「哪里对不上」头几段就够了。
            diff = subprocess.run(['diff', '-u', COMMITTED, fresh],
                                  capture_output=True, text=True)
            lines = diff.stdout.splitlines()
            print('\n'.join(lines[:40]))
            if len(lines) > 40:
                print('...（还有 %d 行，跑 `make generate-uts` 后 git diff 看全部）'
                      % (len(lines) - 40))
            return 1

    print('check_uts_contract OK: app/src/api/schema.uts 与契约同步')
    return 0


if __name__ == '__main__':
    sys.exit(main())
