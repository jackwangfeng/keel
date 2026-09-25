#!/usr/bin/env python3
"""检查仓库内全部 Markdown 的相对链接是否指向真实存在的文件与锚点。"""
import io
import os
import re
import sys
import urllib.parse

LINK_RE = re.compile(r'\[[^\]]*\]\(([^)]+)\)')
# 引用式链接的定义行：[ref]: ./path.md
REF_DEF_RE = re.compile(r'^\s{0,3}\[[^\]]+\]:\s*(\S+)')
# HTML 写法（README 里做居中徽章时很常见）
HTML_SRC_RE = re.compile(r'<(?:a|img|source)\b[^>]*?(?:href|src)\s*=\s*["\']([^"\']+)["\']',
                         re.IGNORECASE)
HEADING_RE = re.compile(r'^#{1,6}\s+(.*?)\s*$', re.MULTILINE)
FENCE_RE = re.compile(r'^\s*(```|~~~)')
INLINE_CODE_RE = re.compile(r'`[^`]*`')
SKIP_DIRS = {'.git', '.superpowers', 'node_modules', 'target', 'vendor'}


def slugify(heading):
    """GitHub 风格锚点：小写、去标点、空格转连字符。中文原样保留。"""
    s = heading.strip().lower()
    s = re.sub(r'[^\w一-鿿\s-]', '', s)
    return re.sub(r'\s+', '-', s)


def anchors_of(path):
    try:
        text = io.open(path, encoding='utf-8').read()
    except (OSError, UnicodeDecodeError):
        return set()
    return {slugify(h) for h in HEADING_RE.findall(text)}


def prose_lines(text):
    """产出 (行号, 正文行)，跳过围栏代码块——块内是示例，不是真链接。"""
    in_fence = False
    for lineno, line in enumerate(text.split('\n'), 1):
        if FENCE_RE.match(line):
            in_fence = not in_fence
            continue
        if not in_fence:
            # 行内代码里的 [文本](路径) 是示例写法，不是真链接
            yield lineno, INLINE_CODE_RE.sub('', line)


def md_files(root):
    for dirpath, dirnames, filenames in os.walk(root):
        dirnames[:] = [d for d in dirnames if d not in SKIP_DIRS]
        for name in filenames:
            if name.endswith('.md'):
                yield os.path.join(dirpath, name)


def main():
    root = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
    problems = []
    for path in sorted(md_files(root)):
        text = io.open(path, encoding='utf-8').read()
        for lineno, line in prose_lines(text):
            targets = list(LINK_RE.findall(line))
            targets += REF_DEF_RE.findall(line)
            targets += HTML_SRC_RE.findall(line)
            for target in targets:
                target = target.strip()
                # 剥离链接标题：[x](./a.md "标题")
                m = re.match(r'^(\S+)\s+["\'(].*$', target)
                if m:
                    target = m.group(1)
                # 跳过外链、页内锚点、邮件、内联数据
                if target.startswith(('http://', 'https://', 'mailto:', 'data:')):
                    continue
                if target.startswith('#'):
                    # 同文件锚点：以前直接跳过，于是加目录时写错小节名没人发现
                    a = urllib.parse.unquote(target[1:])
                    if a and slugify(a) not in anchors_of(path):
                        problems.append('%s:%d 同文件锚点不存在 -> %s'
                                        % (os.path.relpath(path, root), lineno, target))
                    continue
                filepart, _, anchor = target.partition('#')
                # 关键：中文文件名常被写成百分号编码，必须先解码再比对
                filepart = urllib.parse.unquote(filepart)
                anchor = urllib.parse.unquote(anchor)
                if not filepart:
                    continue
                resolved = os.path.normpath(
                    os.path.join(os.path.dirname(path), filepart))
                rel = os.path.relpath(path, root)
                if not os.path.exists(resolved):
                    problems.append('%s:%d 指向不存在的路径 -> %s' % (rel, lineno, target))
                elif anchor and resolved.endswith('.md'):
                    if slugify(anchor) not in anchors_of(resolved):
                        problems.append(
                            '%s:%d 文件存在但锚点缺失 -> %s' % (rel, lineno, target))

    if problems:
        print('发现 %d 处坏链接：' % len(problems))
        for p in problems:
            print('  ' + p)
        return 1
    print('全部 Markdown 链接有效')
    return 0


if __name__ == '__main__':
    sys.exit(main())
