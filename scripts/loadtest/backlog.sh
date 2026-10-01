#!/usr/bin/env bash
# 压测前后看一眼后台任务有没有积压（docs/性能压测-2026-10.md「后台任务」）。
#
#   PROJECT=keelchaos scripts/loadtest/backlog.sh
#
# 看四样：core 的 jobs 队列（按 queue × status）、过了 expire_at 还没关的待支付单、
# 两边协调器（SAGA / 二阶段消息，sqlite）里没走完的全局事务、两个库的连接按 state 计数。
# 协调器的 sqlite 在容器卷里，distroless 镜像里没有 sqlite3，所以 docker cp 出来用宿主机的 python3 读。
set -euo pipefail
P=${PROJECT:-keelchaos}
core() { docker exec -i "$P-postgres-1" psql -U keel -d keel -XAt -F ' | ' -c "$1"; }
inv() { docker exec -i "$P-postgres-inventory-1" psql -U keel -d keel_inventory -XAt -F ' | ' -c "$1"; }

echo "== $(date '+%F %T')  uptime:$(uptime | sed 's/.*load average//')"
echo "-- jobs（queue | status 0 待办 1 进行中 2 完成 3 死信 | 行数 | 最老待办秒数）"
core "SELECT queue, status, count(*), coalesce(round(extract(epoch FROM now() - min(run_after) FILTER (WHERE status = 0))), 0)
        FROM jobs WHERE status <> 2 GROUP BY 1, 2 ORDER BY 1, 2" || true
echo "-- 过期未关的待支付单（status=10 且 expire_at 已过）"
core "SELECT count(*), coalesce(round(extract(epoch FROM now() - min(expire_at))), 0) AS oldest_s
        FROM orders WHERE status = 10 AND expire_at < now()"
echo "-- 订单状态分布（最近 1 小时下的单）"
core "SELECT status, count(*) FROM orders WHERE created_at > now() - interval '1 hour' GROUP BY 1 ORDER BY 1"
echo "-- 连接（core / 库存）"
core "SELECT coalesce(state, '?'), count(*) FROM pg_stat_activity WHERE datname = 'keel' GROUP BY 1"
inv "SELECT coalesce(state, '?'), count(*) FROM pg_stat_activity WHERE datname = 'keel_inventory' GROUP BY 1"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
for spec in "app:/var/lib/keel/dtm.db" "inventory:/var/lib/keel/inventory-dtm.db"; do
    c=${spec%%:*} f=${spec#*:}
    mkdir -p "$tmp/$c"
    for suf in "" -wal -shm; do docker cp -q "$P-$c-1:$f$suf" "$tmp/$c/" 2>/dev/null || true; done
    db="$tmp/$c/$(basename "$f")"
    [ -f "$db" ] || { echo "-- 协调器 $c：没有 $f"; continue; }
    python3 - "$c" "$db" <<'PY'
import sqlite3, sys
c = sqlite3.connect(sys.argv[2])
rows = c.execute("SELECT status, count(*) FROM trans_global GROUP BY 1 ORDER BY 1").fetchall()
print(f"-- 协调器 {sys.argv[1]}：全局事务按状态", rows)
PY
done
