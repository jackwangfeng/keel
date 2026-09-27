#!/usr/bin/env python3
"""契约闸门：重新生成到临时文件，与入库的 flutter_app/lib/api/schema.g.dart 逐字比对。"""
import os
import subprocess
import sys
import tempfile

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
CHECKED_IN = os.path.join(ROOT, 'flutter_app', 'lib', 'api', 'schema.g.dart')


def main():
    with tempfile.TemporaryDirectory() as d:
        out = os.path.join(d, 'schema.g.dart')
        subprocess.run([sys.executable, os.path.join(ROOT, 'scripts', 'gen_dart_schema.py'), '-o', out],
                       check=True, stdout=subprocess.DEVNULL)
        want = open(out, encoding='utf-8').read()
    have = open(CHECKED_IN, encoding='utf-8').read() if os.path.exists(CHECKED_IN) else ''
    if want != have:
        sys.exit('check_dart_contract FAIL：flutter_app/lib/api/schema.g.dart 与契约不同步，跑 make flutter-generate 后一起提交')
    print('check_dart_contract OK: flutter_app/lib/api/schema.g.dart 与契约同步')


if __name__ == '__main__':
    main()
