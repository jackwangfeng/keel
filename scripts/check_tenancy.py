#!/usr/bin/env python3
"""校验设计文档里的多租户三条规矩，清单来自 db/tenancy.json。

共享 schema 的多租户里，跨租户泄露不是「小概率 bug」，是「漏一次就是事故」。
靠人记得加 WHERE 守不住，所以这里做机械检查：新加一张表却忘了租户列、
忘了把父子引用钉成复合外键、把唯一约束写成全局的，在提交前就会被拦下。

### 这个脚本读文档，internal/db/migrate_test.go 读真库

两边**用同一份清单**（db/tenancy.json），这一点是本轮修的东西。此前两份清单
各自维护：这里豁免了 7 张表，Go 侧只豁免了 3 张，于是 M2 一建 barrier
（dtmrs 的屏障表，跨租户基础设施）Go 侧当场红，而这边一声不吭。
两头各自成立、接起来不成立，是最难在评审里看出来的一类脱节。

分工则是刻意的：文档是业务表 DDL 的唯一真相源（§1），所以「设计对不对」在这里查；
「库里是不是真这样」只有系统目录知道，那部分在 Go 侧查。
两边都失守才会出事 —— 而它们失守的方式不一样。

### 三条规矩各自查什么

规矩一：每张业务表都带 merchant_id。
规矩二：父子引用连 merchant_id 一起钉成复合外键；**以及它的对偶** ——
        形如 <x>_id、能解析到一张父表、却根本没有外键的列，要么补外键，
        要么进 fk_missing_ok 并写明理由。对偶这一条是本轮补的：
        此前「把外键降级成单列」会被抓，「把外键整个删掉」反而全绿。
规矩三：唯一约束一律收进租户内（首列是 merchant_id）。
"""
import io
import json
import os
import re
import sys

SCHEMA = 'docs/电商系统-数据模型设计.md'
MANIFEST = 'db/tenancy.json'

CREATE_RE = re.compile(r'^CREATE TABLE (?:IF NOT EXISTS )?([a-z_]+)\s*\(', re.M)
UNIQUE_INDEX_RE = re.compile(
    r'^CREATE UNIQUE INDEX (?:IF NOT EXISTS )?([a-z_0-9]+)\s*\n?\s*ON\s+([a-z_]+)\s*\(([^)]*)\)',
    re.M)
TABLE_FK_RE = re.compile(
    r'FOREIGN KEY\s*\(([^)]*)\)\s*(?:--[^\n]*\n\s*)?REFERENCES\s+([a-z_]+)\s*\(([^)]*)\)',
    re.S)
COL_FK_RE = re.compile(r'REFERENCES\s+([a-z_]+)\s*\(([^)]*)\)')
TABLE_UNIQUE_RE = re.compile(r'^\s*(?:CONSTRAINT\s+[a-z_0-9]+\s+)?UNIQUE\s*\(([^)]*)\)', re.M)
COLDEF_RE = re.compile(r'^\s{2,}([a-z_][a-z_0-9]*)\s+[A-Z]')


def cols(s):
    """把 "(a, b)" 里的列名拆成列表。"""
    return [c.strip() for c in s.split(',') if c.strip()]


class Table(object):
    def __init__(self, name, body):
        self.name = name
        self.body = body
        self.columns = []       # 声明顺序
        self.fk_columns = set()  # 被任何一条外键引用的列
        self.fks = []           # (cols, target_table, target_cols)
        self.uniques = []       # 列名列表，含主键与内联 UNIQUE

        lines = body.split('\n')
        for line in lines:
            code = line.split('--')[0]
            m = COLDEF_RE.match(code)
            if m:
                col = m.group(1)
                self.columns.append(col)
                fk = COL_FK_RE.search(code)
                if fk:
                    self.fk_columns.add(col)
                    self.fks.append(([col], fk.group(1), cols(fk.group(2))))
                if re.search(r'\bUNIQUE\b', code):
                    self.uniques.append([col])
                if re.search(r'\bPRIMARY KEY\b', code):
                    self.uniques.append([col])

        for m in TABLE_FK_RE.finditer(body):
            c = cols(m.group(1))
            self.fk_columns.update(c)
            self.fks.append((c, m.group(2), cols(m.group(3))))

        for m in TABLE_UNIQUE_RE.finditer(body):
            self.uniques.append(cols(m.group(1)))
        for m in re.finditer(r'^\s*PRIMARY KEY\s*\(([^)]*)\)', body, re.M):
            self.uniques.append(cols(m.group(1)))

    def has(self, col):
        return col in self.columns


def parse_tables(text):
    out = {}
    for m in CREATE_RE.finditer(text):
        body = text[m.end():]
        end = body.find('\n);')
        # 同一张表在文档里出现两次时，后一次（分表正文）覆盖前一次（§2 的示意片段）。
        out[m.group(1)] = Table(m.group(1), body[:end if end >= 0 else 0])
    return out


def parse_unique_indexes(text, tables):
    """返回 [(索引名, 表名, 列名列表)]。"""
    out = []
    for m in UNIQUE_INDEX_RE.finditer(text):
        if m.group(2) in tables:
            out.append((m.group(1), m.group(2), cols(m.group(3))))
    return out


def parent_of(col, tables):
    """<x>_id 形如 sku_id 时，返回文档里那张叫 skus 的表名；推不出来就返回 None。

    只认能**直接拼出表名**的那几种复数形式。推不出来一律当作「这不是外键」，
    宁可漏报也不误报 —— 误报会逼着往豁免清单里加一堆不是外键的列
    （target_id、cluster_id、biz_id、trace_id…），而那会把清单变成噪音，
    人就不再逐条读它了。
    """
    if not col.endswith('_id'):
        return None
    stem = col[:-3]
    cands = [stem + 's', stem + 'es']
    if stem.endswith('y'):
        cands.append(stem[:-1] + 'ies')
    for c in cands:
        if c in tables:
            return c
    return None


def pretty(table, columns):
    return '%s(%s)' % (table, ','.join(columns))


def main():
    root = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
    text = io.open(os.path.join(root, SCHEMA), encoding='utf-8').read()
    man = json.load(io.open(os.path.join(root, MANIFEST), encoding='utf-8'))

    classes = {k: v for k, v in man['classes'].items()}
    entries = {k: v for k, v in man['tables'].items()}
    fk_missing_ok = {k: v for k, v in man['fk_missing_ok'].items() if k != '_readme'}
    fk_single_ok = {k: v for k, v in man['fk_single_column_ok'].items() if k != '_readme'}
    unique_ok = {k: v for k, v in man['unique_global_ok'].items() if k != '_readme'}

    tables = parse_tables(text)
    indexes = parse_unique_indexes(text, tables)
    problems = []

    def cls_of(name):
        return entries.get(name, {}).get('class', 'tenant')

    # —— 清单与文档的一致性。清单过期会静默地豁免掉一张将来重名的表。
    for name, e in sorted(entries.items()):
        if e.get('documented') is False:
            continue
        if name not in tables:
            problems.append('清单里的 %s 在设计文档里不存在，该清理了' % name)
        if e['class'] not in classes:
            problems.append('清单里的 %s 类别 %r 不存在' % (name, e['class']))
        if not e.get('reason'):
            problems.append('清单里的 %s 没写理由' % name)

    used_fk_missing, used_fk_single, used_unique = set(), set(), set()

    for name in sorted(tables):
        t = tables[name]
        cls = cls_of(name)
        spec = classes[cls]

        # —— 规矩一
        if spec['merchant_id'] == 'required' and not t.has('merchant_id'):
            problems.append('[规矩一] 表 %s 没有 merchant_id，也没有在 %s 里分类'
                            % (name, MANIFEST))
        if spec['merchant_id'] == 'absent' and t.has('merchant_id'):
            problems.append('[规矩一] 表 %s 归在 %s 类却带着 merchant_id——'
                            '它看起来就是一张普通业务表，该改回 tenant' % (name, cls))

        # —— 规矩二（正）：两端都带 merchant_id 的外键必须是复合的
        for fcols, target, _tcols in t.fks:
            if target == 'merchants':
                continue  # 指向租户表自身，单列就是租户列本身
            tt = tables.get(target)
            if tt is None or not t.has('merchant_id') or not tt.has('merchant_id'):
                continue
            if 'merchant_id' in fcols:
                continue
            key = pretty(name, fcols)
            if key in fk_single_ok:
                used_fk_single.add(key)
                continue
            problems.append(
                '[规矩二] 外键 %s → %s 只引用了 %s，没带 merchant_id——'
                'A 商家的行可以挂到 B 商家的行上，数据库不会拒绝'
                % (key, target, fcols))

        # —— 规矩二（对偶）：看起来是外键、却根本没有外键的列
        for col in t.columns:
            if col in t.fk_columns:
                continue
            target = parent_of(col, tables)
            if target is None or target == name:
                continue
            key = '%s.%s' % (name, col)
            if key in fk_missing_ok:
                used_fk_missing.add(key)
                continue
            problems.append(
                '[规矩二·对偶] %s 指向 %s 却没有任何外键约束——'
                '违规做得更彻底反而绕过了复合外键这条规矩：A 商家的行可以引用 '
                'B 商家的行，数据库不拒、闸门不红。要么补成 '
                'FOREIGN KEY (%s, merchant_id) REFERENCES %s(id, merchant_id)，'
                '要么进 fk_missing_ok 并写明理由' % (key, target, col, target))

        # —— 规矩三：唯一约束一律收进租户内
        if spec.get('unique_scoped'):
            for ucols in t.uniques + [c for _n, tbl, c in indexes if tbl == name]:
                if not ucols or ucols[0] in ('merchant_id', 'id'):
                    continue
                key = pretty(name, ucols)
                if key in unique_ok:
                    used_unique.add(key)
                    continue
                problems.append(
                    '[规矩三] 唯一约束 %s 的首列不是 merchant_id——'
                    '全局唯一的东西在多租户下几乎都是错的（两家店各有一个 '
                    'sku_code = "A001" 是正常的）。收进租户内，或进 '
                    'unique_global_ok 并写明理由' % key)

    for key in sorted(set(fk_missing_ok) - used_fk_missing):
        problems.append('fk_missing_ok 里的 %s 已经不是一处缺失外键了，该清理了' % key)
    for key in sorted(set(fk_single_ok) - used_fk_single):
        problems.append('fk_single_column_ok 里的 %s 已经不是一条单列跨租户外键了，该清理了' % key)
    for key in sorted(set(unique_ok) - used_unique):
        problems.append('unique_global_ok 里的 %s 在文档里不存在，该清理了' % key)

    # 解析器自己失效时要出声：正则跟不上文档写法的变化，表面上就是「全绿」。
    if len(tables) < 30:
        problems.append('只解析出 %d 张表，设计文档有 30 张以上——这个检查本身失效了'
                        % len(tables))
    if len(indexes) < 10:
        problems.append('只解析出 %d 条唯一索引——这个检查本身失效了' % len(indexes))

    if problems:
        print('发现 %d 处问题：' % len(problems))
        for p in problems:
            print('  ' + p)
        print('\n豁免清单在 %s。往那里加一条是一个需要解释的动作，不是默认行为。' % MANIFEST)
        return 1
    print('租户三条规矩校验通过（%d 张表，%d 条唯一索引，%d 张表在清单里分了类）'
          % (len(tables), len(indexes), len(entries)))
    return 0


if __name__ == '__main__':
    sys.exit(main())
