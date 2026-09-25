#!/usr/bin/env python3
"""校验多租户隔离：每张业务表都必须带 merchant_id，豁免必须写明理由。

共享 schema 的多租户里，跨租户泄露不是「小概率 bug」，是「漏一次就是事故」。
靠人记得加 WHERE 守不住，所以这里做机械检查：新加一张表却忘了租户列，
在提交前就会被拦下。
"""
import io
import os
import re
import sys

SCHEMA = 'docs/电商系统-数据模型设计.md'

# 不需要 merchant_id 的表，每一条都要写明理由。
# 往这里加表是一个需要解释的动作，不是默认行为。
EXEMPT = {
    'merchants':                '租户表自身',
    'shop_settings':            '主键就是 merchant_id',
    'barrier':                  'dtmrs 第三方组件的表，不由本项目定义',
    'order_status_transitions': '静态参考数据，所有租户共用同一张状态机',
    'refund_status_transitions': '同上',
    'staff_tokens':             '跟随 staff，租户归属由 staff.merchant_id 决定',
    'inventories':              '主键是 sku_id，租户归属由 skus 决定；'
                                '且它是下单 SAGA 的热点表，多一列索引成本不划算',
}

CREATE_RE = re.compile(r'^CREATE TABLE (?:IF NOT EXISTS )?([a-z_]+)\s*\(', re.M)


def tables_with_columns(text):
    """返回 {表名: 该表 DDL 正文}。"""
    out = {}
    for m in CREATE_RE.finditer(text):
        name = m.group(1)
        body = text[m.end():]
        end = body.find('\n);')
        out[name] = body[:end if end >= 0 else 0]
    return out


def main():
    root = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
    text = io.open(os.path.join(root, SCHEMA), encoding='utf-8').read()
    tables = tables_with_columns(text)
    problems = []

    for name, body in sorted(tables.items()):
        if name in EXEMPT:
            continue
        if not re.search(r'\bmerchant_id\b', body):
            problems.append('表 %s 没有 merchant_id，也不在豁免清单里' % name)

    for name in sorted(EXEMPT):
        if name not in tables:
            problems.append('豁免清单里的 %s 并不存在，清单该清理了' % name)

    if problems:
        print('发现 %d 处问题：' % len(problems))
        for p in problems:
            print('  ' + p)
        print('\n每张业务表都要能独立判定租户归属。若确实不需要，'
              '加进 EXEMPT 并写明理由——那是个需要解释的动作。')
        return 1
    print('租户隔离校验通过（%d 张表，%d 张豁免）'
          % (len(tables), len(EXEMPT)))
    return 0


if __name__ == '__main__':
    sys.exit(main())
