#!/usr/bin/env python3
"""从发起支付的响应里取出沙箱的结算指令。

判据里带一条断言：payload 必须自报 sandbox。没有它的话，真接了渠道之后
这段 smoke 会拿着一份真实的支付参数去「原样投回去」，而那是另一回事。

用法：
    smoke_settle.py head <intent json>   → "<url> <signature>"
    smoke_settle.py body <intent json>   → 回调报文原文
"""
import json
import sys

with open(sys.argv[2]) as f:
    payload = json.load(f)["payload"]

if not payload.get("sandbox"):
    raise SystemExit("payload 里没有 sandbox 标记——这不该被当成沙箱响应")

settle = payload["settle"]
if sys.argv[1] == "head":
    print(settle["url"], settle["headers"]["X-Keel-Signature"])
else:
    print(settle["body"])
