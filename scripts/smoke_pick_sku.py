#!/usr/bin/env python3
"""从商品详情里挑一个有货的 SKU，或读出指定 SKU 的水位。

单独一个文件而不是 smoke.sh 里的 heredoc：那段 shell 已经嵌了一层 curl 的引号，
再嵌一层 Python heredoc 之后，引号层数会多到写错了也看不出来（我写坏过一次）。

用法：
    smoke_pick_sku.py <详情 json>            → "<sku_id> <available_qty>"
    smoke_pick_sku.py <详情 json> <sku_id>   → "<available_qty>"
"""
import json
import sys

with open(sys.argv[1]) as f:
    skus = json.load(f)["skus"]

if len(sys.argv) > 2:
    want = int(sys.argv[2])
    for s in skus:
        if s["id"] == want:
            print(s.get("available_qty", 0))
            sys.exit(0)
    raise SystemExit("详情里没有 sku %d 了" % want)

live = [s for s in skus if s.get("available_qty", 0) >= 1]
if not live:
    raise SystemExit("这件商品一个有货的 SKU 都没有——种子或库存扣减出了问题")
print(live[0]["id"], live[0]["available_qty"])
