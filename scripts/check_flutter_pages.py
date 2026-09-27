#!/usr/bin/env python3
"""页面闸门：flutter_app/lib/pages 与 lib/widgets 下不许 import api/schema.g.dart。

契约字段只在 lib/api/ 里读写（view.dart 翻成行模型）；页面直接碰契约类型，契约改名时
报错不会集中在 api/ 这一层 —— 与 app/（uni-app x）的 check_app_types.py 同一条规矩。
"""
import os
import re
import sys

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
DIRS = [os.path.join(ROOT, 'flutter_app', 'lib', d) for d in ('pages', 'widgets')]
PAT = re.compile(r"""^\s*import\s+['"][^'"]*schema\.g\.dart['"]""", re.M)


def main():
    bad = []
    for d in DIRS:
        for dp, _, files in os.walk(d):
            for f in files:
                if f.endswith('.dart'):
                    p = os.path.join(dp, f)
                    for m in PAT.finditer(open(p, encoding='utf-8').read()):
                        bad.append(os.path.relpath(p, ROOT))
    if bad:
        sys.exit('check_flutter_pages FAIL：这些文件直接 import 了 schema.g.dart（改用 api/view.dart 的行模型）：\n  ' + '\n  '.join(bad))
    print('check_flutter_pages OK')


if __name__ == '__main__':
    main()
