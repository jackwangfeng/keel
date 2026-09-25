#!/usr/bin/env python3
"""检查 README 宣称存在的东西是否真的存在，以及有没有占位/虚标。"""
import glob
import io
import os
import re
import sys

# README 承诺存在的文件，相对仓库根目录
PROMISED_FILES = [
    'LICENSE',
    'CONTRIBUTING.md',
    'docs/电商系统-总体架构.md',
    'docs/电商系统-数据模型设计.md',
    'docs/电商系统-语义检索层设计.md',
    'docs/电商系统-商品理解服务设计.md',
    'docs/电商系统-OpenAPI.yaml',
]

READMES = ['README.md', 'README.zh-CN.md']

# README 里出现的 localhost 端口，必须在某个 compose 文件里真的映射出来。
#
# 这条检查是补一个已经发生过的坑：README 的快速开始长期写着「打开
# http://localhost:3000」，而 compose 里从来没有 3000 这个端口——在
# `docker compose up` 这条命令还不存在的时候，那段话是路线图；命令一旦存在，
# 同一段话就变成了对一个已发布命令的错误描述，读者照着跑会打开一个死链。
# 原来的检查只验「承诺的文件存在吗」，验不了这种行为描述。
#
# 判据刻意只认 localhost/127.0.0.1 的 URL 写法：那是「你现在就能打开它」的意思。
# 说一个还不存在的端口（路线图那一节）请写成「3000 端口上还没有页面」，
# 别写成一个能点的链接——链接本身就是承诺。
COMPOSE_GLOB = 'compose*.y*ml'
README_LOCALHOST_RE = re.compile(r'(?:localhost|127\.0\.0\.1):(\d{2,5})')
# compose 的端口映射列表项：`- "8080:8080"` / `- "${KEEL_HTTP_PORT:-8080}:8080"`。
# 宿主机一侧只认纯数字或带默认值的变量——写成别的形状时这条检查宁可漏报，
# 也不去猜一个它读不懂的写法到底映射了什么。
COMPOSE_PORT_RE = re.compile(
    r'^\s*-\s*["\']?(?P<host>\$\{[A-Za-z_][A-Za-z0-9_]*:-(?P<default>\d+)\}|\d+)'
    r':(?P<container>\d+)(?:/(?:tcp|udp))?["\']?\s*$')
PLACEHOLDER_LINK_RE = re.compile(r'\[[^\]]*\]\(#\)')
# README 里所有指向本仓库的 GitHub URL 的「组织名」那一段。
#
# 这个仓库还没有远端，所以 clone 地址与构建徽章都写着 <org> 占位符。
# 占位符本身不算虚标：它显然不是一个真地址，读者一眼就知道要自己替换。
#
# 真正会出事的是**只替换了一半**：定下组织名之后把 clone 地址改了、
# 忘了徽章（或反过来）。那时徽章指向一个不存在的仓库，GitHub 上显示成裂图，
# 而这是一个「看起来已经填好了」的状态，没人会再去检查。
#
# 所以这里不检查「有没有占位符」，而是检查**一致性**：要么都还是占位符，
# 要么都已填好。这样这条检查会在替换动作发生的那一刻自己到期——
# 不需要谁记得回来把豁免删掉。
GITHUB_OWNER_RE = re.compile(r'https://github\.com/([^/\s)]+)/keel')
# 任何形如「CI / build / tests + passing/success」的徽章图片。
# 不能只认 shields.io —— GitHub Actions 的官方徽章走 github.com/.../badge.svg，
# 那恰恰是最可能被真加回来的写法。
BUILD_BADGE_RE = re.compile(
    r'!\[[^\]]*\]\((https?://[^)]*?(?:'
    r'badge\.svg|shields\.io[^)]*?(?:ci|build|test|workflow)'
    r')[^)]*)\)', re.IGNORECASE)


def published_ports(root):
    """所有 compose 文件里映射到宿主机的端口。"""
    ports = set()
    for path in sorted(glob.glob(os.path.join(root, COMPOSE_GLOB))):
        for line in io.open(path, encoding='utf-8').read().split('\n'):
            m = COMPOSE_PORT_RE.match(line)
            if m:
                ports.add(int(m.group('default') or m.group('host')))
    return ports


def main():
    root = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
    problems = []
    owners = []
    # 徽章是否虚标，取决于仓库里到底有没有 CI
    wf = os.path.join(root, '.github', 'workflows')
    has_ci = os.path.isdir(wf) and any(
        f.endswith(('.yml', '.yaml')) for f in os.listdir(wf))
    ports = published_ports(root)

    for rel in PROMISED_FILES:
        if not os.path.exists(os.path.join(root, rel)):
            problems.append('README 承诺的文件不存在: %s' % rel)

    for rel in READMES:
        path = os.path.join(root, rel)
        if not os.path.exists(path):
            problems.append('README 本身缺失: %s' % rel)
            continue
        lines = io.open(path, encoding='utf-8').read().split('\n')
        for lineno, line in enumerate(lines, 1):
            for hit in PLACEHOLDER_LINK_RE.findall(line):
                problems.append(
                    '%s:%d 占位链接（指向 "#"）: %s' % (rel, lineno, hit.strip()))
            for port in README_LOCALHOST_RE.findall(line):
                if int(port) not in ports:
                    problems.append(
                        '%s:%d 让读者打开 localhost:%s，但没有任何 compose 文件把这个'
                        '端口映射到宿主机（已映射的是 %s）。要么在 compose 里映射它，'
                        '要么别用能点的链接去说一个还不存在的端口。'
                        % (rel, lineno, port,
                           ', '.join(str(x) for x in sorted(ports)) or '（没有）'))
            for url in BUILD_BADGE_RE.findall(line):
                if not has_ci:
                    problems.append(
                        '%s:%d 挂着构建状态徽章但仓库没有 CI（.github/workflows/ 不存在），'
                        '属于虚标: %s' % (rel, lineno, url))
            for owner in GITHUB_OWNER_RE.findall(line):
                owners.append((rel, lineno, owner))

    # 占位符一致性：要么都还是 <org>，要么都已填好。
    placeholder = [o for o in owners if o[2].startswith('<')]
    real = [o for o in owners if not o[2].startswith('<')]
    if placeholder and real:
        problems.append(
            '指向本仓库的 GitHub 地址只替换了一半 —— 组织名已填好 %d 处、'
            '仍是占位符 %d 处。徽章指向一个不存在的仓库时在 GitHub 上是裂图，'
            '而这是个「看起来已经填好了」的状态，不会有人再去检查。'
            % (len(real), len(placeholder)))
        for rel, lineno, owner in placeholder:
            problems.append('    %s:%d 还是 %s' % (rel, lineno, owner))
        for rel, lineno, owner in real:
            problems.append('    %s:%d 已是 %s' % (rel, lineno, owner))
    if len({o[2] for o in real}) > 1:
        problems.append(
            '指向本仓库的 GitHub 地址用了不止一个组织名：%s'
            % ', '.join(sorted({o[2] for o in real})))

    if problems:
        print('发现 %d 处问题：' % len(problems))
        for p in problems:
            print('  ' + p)
        return 1
    print('README 承诺全部兑现')
    return 0


if __name__ == '__main__':
    sys.exit(main())
