#!/usr/bin/env bash
# 按 docs/性能压测-2026-10.md 的场景表跑一轮（用法见同目录 README.md）。
#
#   FIXTURE=/tmp/lt/fixture.json OUT=/tmp/lt/results KEEL_LT_ADMIN_TOKEN=... scripts/loadtest/run-all.sh [阶段...]
#
# 阶段：read search cart order hot flash admin mix（不给就全跑）。每一级之间停 PAUSE 秒，
# 压测前后各记一次 uptime 与后台积压（backlog.sh）。结果：$OUT/<阶段>.txt（人读）与 $OUT/results.jsonl（机读）。
set -euo pipefail
cd "$(dirname "$0")/../.."
P=${PROJECT:-keelchaos}
BASE=${BASE:-http://127.0.0.1:38180}
FIXTURE=${FIXTURE:?要给 FIXTURE（scripts/loadtest/seed 的输出）}
OUT=${OUT:-loadtest-results}
PAUSE=${PAUSE:-15s}
D=${D:-45s}
mkdir -p "$OUT"
LT="$OUT/keel-loadtest"
[ -x "$LT" ] || go build -o "$LT" ./cmd/keel-loadtest
unset HTTP_PROXY HTTPS_PROXY http_proxy https_proxy ALL_PROXY all_proxy

common=(-base "$BASE" -fixture "$FIXTURE" -tokens "$OUT/tokens.json" -json "$OUT/results.jsonl" -pause "$PAUSE"
    -sample "$P-app-1,$P-inventory-1,$P-postgres-1,$P-postgres-inventory-1"
    -pg "$P-postgres-1:keel,$P-postgres-inventory-1:keel_inventory")
run() { # run <阶段> <场景> <并发列表> [时长] [其余参数...]
    local phase=$1 sc=$2 c=$3 d=${4:-$D}
    shift 4 || shift $#
    echo ">>> $sc  -c $c  -d $d" | tee -a "$OUT/$phase.txt"
    "$LT" "${common[@]}" -scenario "$sc" -c "$c" -d "$d" "$@" 2>&1 | tee -a "$OUT/$phase.txt"
    sleep "${PAUSE%s}"
}
phase_read() {
    run read list-default 8,16,32
    run read list-category 16
    run read list-instock 16
    run read list-deep 16
    run read detail 16,64
    run read resolve 32
    run read addresses 32
}
phase_search() { run search search 16,32; }
phase_cart() { run cart cart 32; run cart preview 32; }
phase_order() { run order order 16,32,64 60s; scripts/loadtest/backlog.sh | tee -a "$OUT/order.txt"; }
phase_hot() { run hot order-hot 64,256 60s -users 2000; scripts/loadtest/backlog.sh | tee -a "$OUT/hot.txt"; }
phase_flash() {
    "$LT" "${common[@]}" -scenario setup-flash | tee -a "$OUT/flash.txt"
    sleep 5
    run flash order-flash 256 60s -users 2000
    scripts/loadtest/backlog.sh | tee -a "$OUT/flash.txt"
}
phase_admin() {
    run admin admin-orders 16
    run admin admin-orders-search 16
    run admin admin-products 16
    run admin admin-stock-same 32
    run admin admin-stock-diff 32
}
phase_mix() { run mix mix 16,32,64,128 60s -users 2000; scripts/loadtest/backlog.sh | tee -a "$OUT/mix.txt"; }

phases=("$@")
[ ${#phases[@]} -gt 0 ] || phases=(read search cart order hot flash admin mix)
scripts/loadtest/backlog.sh | tee -a "$OUT/backlog-before.txt"
for ph in "${phases[@]}"; do
    "phase_$ph"
done
scripts/loadtest/backlog.sh | tee -a "$OUT/backlog-after.txt"
