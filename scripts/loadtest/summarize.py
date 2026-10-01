#!/usr/bin/env python3
"""把 keel-loadtest -json 写出的 results.jsonl 汇总成一张表（每级一行 + 各接口）。

    python3 scripts/loadtest/summarize.py results.jsonl [--labels]
"""
import json
import sys


def cpu(r, name):
    c = (r.get("cpu_pct") or {}).get(name)
    return f"{c['Avg'] / 100:.1f}" if c else "-"


def pg(r, key):
    c = (r.get("pg") or {}).get(key)
    return f"{c['Avg']:.0f}/{c['Max']:.0f}" if c else "-"


def main():
    show_labels = "--labels" in sys.argv
    rows = [json.loads(l) for l in open(sys.argv[1]) if l.strip()]
    print("| 场景 | 并发 | RPS | p50 | p95 | p99 | 5xx% | 非 2xx | app 核 | inventory 核 | PG core 核 | PG 库存 核 | core 连接 active/峰 | 等锁 均/峰 |")
    print("|---|---|---|---|---|---|---|---|---|---|---|---|---|---|")
    for r in rows:
        t = r["total"]
        non2xx = sum(n for c, n in t["codes"].items() if not str(c).startswith("2"))
        print(f"| {r['scenario']} | {r['concurrency']} | {t['rps']:.0f} | {t['p50_ms']:.1f} | {t['p95_ms']:.1f} | {t['p99_ms']:.1f} "
              f"| {t['err_rate'] * 100:.2f} | {non2xx / max(t['count'], 1) * 100:.1f}% | {cpu(r, 'app')} | {cpu(r, 'inventory')} "
              f"| {cpu(r, 'postgres')} | {cpu(r, 'postgres-inventory')} | {pg(r, 'postgres.active')} | {pg(r, 'postgres.lockwait')} |")
        if show_labels and len(r["labels"]) > 1:
            for l in r["labels"]:
                probs = ", ".join(f"{k}×{v}" for k, v in sorted((l.get("problems") or {}).items()))
                print(f"|  ↳ {l['label']} | | {l['rps']:.0f} | {l['p50_ms']:.1f} | {l['p95_ms']:.1f} | {l['p99_ms']:.1f} "
                      f"| {l['err_rate'] * 100:.2f} | {probs} | | | | | | |")


if __name__ == "__main__":
    main()
