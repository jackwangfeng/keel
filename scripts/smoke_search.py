#!/usr/bin/env python3
"""核对一次 /search 的响应：命中的是不是**对的**那件商品。

单独一个文件而不是 smoke.sh 里的 heredoc，理由同 smoke_pick_sku.py：
那段 shell 已经嵌了一层 curl 的引号。

用法：
    smoke_search.py <响应 json> <期望命中的标题> [必须排在它后面的标题]

输出一行：
    <命中条数> <期望标题的名次(1 起)> <它的 recall_source 或 -> <strategy>

判据有三条，少一条都不成立：

 1. items 非空。

 2. **期望的那件商品真的在里面。** 只断言非空的话，一条「把全店商品原样
    倒出来」的实现照样绿 —— 而那正是检索最容易坏成的样子。

 3. **不相干的那件必须排在它后面（或者干脆不在里面）。** 这一条是上一条的
    反面：没有它，「搜什么都返回全部且顺序随机」同样绿。

    为什么是「排在后面」而不是「不许出现」：**召回层刻意没有相似度阈值**
    （internal/service/search.go 第四节）。ANN 返回的是最近的 N 条，不是
    「足够近的那些」；定阈值需要语义检索层 §9.1 的离线评测集，而那个东西
    还不存在。本机实测（真 BGE-M3）搜「连衣裙」的余弦相似度：
    雪纺碎花连衣裙 0.588 / 真丝吊带长裙 0.535 / 挂耳咖啡 0.340 /
    陶瓷马克杯 0.324 / 手冲咖啡壶 0.269 —— 顺序是对的，阈值是没有的。

    **M4 把演示种子从 5 件加到了 23 件**，起因正是这一条的另一面：库里只有
    5 件时 `size` 再小也是全部，搜「咖啡」连裙子都在结果里（相似度 0.28，
    与咖啡那几件的 0.54 之间有 0.09 的断崖，排序完全正确）。解法是让库里有
    足够的商品把不相关的挤出 `size`，而不是拍一个阈值 —— 拍的那个数会在换
    模型时失效，且失效时不报错。

recall_source 只打印、不断言：默认那一栈（`docker compose up`，不带
compose.inference.yaml）没有推理引擎，向量那一路走不了，命中只会是 keyword；
带上引擎的那一栈里它是 both。两种都对 —— 而「引擎不在时仍然返回结果」
正是语义检索层 §8 要求的降级链。要断言它是 both 的话，这段 smoke 在
README 承诺的那条命令上就是红的。
"""
import json
import sys

with open(sys.argv[1]) as f:
    body = json.load(f)

want = sys.argv[2]
forbidden = sys.argv[3] if len(sys.argv) > 3 else None

items = body["items"]
if not isinstance(items, list):
    raise SystemExit("items 不是数组")
if not items:
    raise SystemExit("检索返回了 0 条 —— 派生数据（search_text / 向量）没有落到库里")

titles = [it["title"] for it in items]
if want not in titles:
    raise SystemExit("检索结果里没有 %r，实际是 %r" % (want, titles))
rank = titles.index(want) + 1
if forbidden is not None and forbidden in titles:
    if titles.index(forbidden) < rank - 1:
        raise SystemExit(
            "%r 排在 %r 前面 —— 排序没有任何区分力。实际是 %r"
            % (forbidden, want, titles))
hit = items[rank - 1]
print(len(items), rank, hit.get("recall_source", "-"), body["strategy"])
