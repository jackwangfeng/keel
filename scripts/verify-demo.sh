#!/usr/bin/env bash
# 演示栈基线验收：把"这次改动到底有没有弄坏什么"变成一条可执行的检查。
#
# 存在的理由：2026-10-06 我在演示库上连犯四次，每次都过了"我以为的验收"。
# 共同点是**验的和我改的不是同一件事**——
#   · 补完 ACL 就宣布恢复完成，没人去读那 95 条 S3 商品图（图片一直 404）；
#   · 重建栈只看 api/console 都是 200 就发布，地图瓦片、S3、渠道层三处全丢；
#   · 补授权用了 `GRANT ... ON ALL TABLES`，抹掉了 C 档拆分的权限边界。
# 错误日志为空也拦不住——这些都不报错，只是功能悄悄没了。
#
# 所以这里的取舍是：**宁可误报也不漏报**。每条检查都是"必须等于某个具体值"，
# 数值对不上就红，不给"大概没问题"留出口。基线数字集中在 BASELINE 文件里，
# 改动是有意的（数据变了 / 迁移动了）才去改它，改的动作本身就是一个决定。
#
# 对工作区与数据库**只读**：不建表、不改数据、不重启容器。
#
# 用法：
#   scripts/verify-demo.sh              全查
#   scripts/verify-demo.sh --quick      跳过 smoke.sh（省 ~40s）
#   scripts/verify-demo.sh --only smoke 只跑某几条（名字见下）
set -uo pipefail

cd "$(dirname "$0")/.."

BASELINE="${KEEL_BASELINE:-deploy/demo-baseline.env}"
COMPOSE_ENV="${KEEL_DEMO_ENV:-$HOME/.local/share/keel-eshop/demo-env.sh}"

fail=0
declare -a passed=() failed=() skipped=()

ok()   { printf '  \033[32mOK\033[0m   %s\n' "$1"; passed+=("$1"); }
bad()  { printf '  \033[31mFAIL\033[0m %s\n' "$1"; [ $# -gt 1 ] && printf '       %s\n' "$2"; failed+=("$1"); }
skip() { printf '  --   %s（跳过：%s）\n' "$1" "$2"; skipped+=("$1"); }

section() { printf '\n\033[1m== %s ==\033[0m\n' "$1"; }

# 期望值与基线比对。相等才算过——不为"大致相同"留口子。
expect_eq() {
    local name=$1 got=$2 want=$3
    if [ "$got" = "$want" ]; then
        ok "$name = $got"
    else
        bad "$name" "期望 $want，实际 $got"
    fi
}

expect_ge() {
    local name=$1 got=$2 want=$3
    if [ -n "$got" ] && [ "$got" -ge "$want" ] 2>/dev/null; then
        ok "$name = $got（≥ $want）"
    else
        bad "$name" "期望 ≥ $want，实际 ${got:-空}"
    fi
}

# --- 加载基线与演示环境 -------------------------------------------------
if [ ! -r "$BASELINE" ]; then
    echo "找不到基线文件：$BASELINE" >&2
    exit 1
fi
# shellcheck source=/dev/null
. "$BASELINE"

PROJECT="${COMPOSE_PROJECT_NAME:-keeldemo}"
PG="${PROJECT}-postgres-1"
PG_INV="${PROJECT}-postgres-inventory-1"

# 在演示 postgres 容器里跑一条 SQL，返回单值。演示库不对宿主机发端口，
# 只能这样进。-tA 关表头 -tq 静默，-v ON_ERROR_STOP 让 SQL 错误直接冒出来。
sq() { docker exec "$PG" psql -U keel -d keel -tAqc "$1" 2>/dev/null | tr -d '\r' | head -1; }
# 版本表名不一样：core 用 goose_db_version，inventory 用 goose_db_version_inventory
# （实测撞过，别想当然写成同一个）。
sq_inv() { docker exec "$PG_INV" psql -U keel -d keel_inventory -tAqc "$1" 2>/dev/null | tr -d '\r' | head -1; }

# 读运行中容器的环境变量，而不是读 env 文件或 compose 解析结果。
# 今天的两处事故都是"配置声明的"和"实际生效的"不一致——env 文件里写着有 S3，
# 起栈时那个 compose 文件没叠上，于是真的没有。**只有进程里的值算数。**
app_env() { docker exec "${PROJECT}-app-1" printenv "$1" 2>/dev/null | tr -d '\r'; }

quick=0
only=""
while [ $# -gt 0 ]; do
    case "$1" in
        --quick) quick=1 ;;
        --only)  only="${2:-}"; shift ;;
        -h|--help) sed -n '2,18p' "$0"; exit 0 ;;
        *) echo "未知参数：$1" >&2; exit 2 ;;
    esac
    shift
done

want_check() {
    [ -z "$only" ] && return 0
    case " $only " in *" $1 "*) return 0 ;; *) return 1 ;; esac
}

# 服务名 → 完整容器名。全脚本只有这一处拼接（docker compose 的命名规则）。
container_of() { printf '%s-%s-1' "$PROJECT" "$1"; }

if [ -r "$COMPOSE_ENV" ]; then
    # shellcheck source=/dev/null
    . "$COMPOSE_ENV"
    HAS_DEMO_ENV=1
else
    HAS_DEMO_ENV=0
fi

printf '\033[1m演示栈基线验收\033[0m  项目=%s  基线=%s\n' "$PROJECT" "$BASELINE"

# --- 1. 容器 ------------------------------------------------------------
if want_check containers; then
section "容器"
if ! docker ps --format '{{.Names}}' | grep -q "^${PG}$"; then
    bad "postgres 容器在跑" "找不到 $PG，栈可能没起或项目名不对"
else
    ok "postgres 容器在跑"

    # 这里列的是 compose 服务名，完整名字由 container_of 统一拼，别自己再拼一遍。
    for c in app inventory dtmrs console seaweedfs postgres postgres-inventory; do
        name=$(container_of "$c")
        if docker ps --format '{{.Names}}' | grep -q "^${name}$"; then
            unhealthy=$(docker inspect -f '{{if .State.Health}}{{.State.Health.Status}}{{else}}no-healthcheck{{end}}' "$name")
            case "$unhealthy" in
                healthy|no-healthcheck|starting) ok "$c Up（$unhealthy）" ;;
                *) bad "$c 健康状态" "$unhealthy" ;;
            esac
        else
            bad "$c 容器在跑" "找不到 $name"
        fi
    done
fi
fi

# --- 2. 端点可达 --------------------------------------------------------
if want_check endpoints; then
section "端点"
for pair in "api:http://127.0.0.1:${KEEL_HTTP_PORT:-18099}" \
            "console:http://127.0.0.1:${KEEL_CONSOLE_PORT:-18100}"; do
    name=${pair%%:*}; url=${pair#*:}
    code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 10 "$url/healthz" 2>/dev/null || echo 000)
    if [ "$code" = "200" ]; then ok "$name /healthz 200"; else bad "$name /healthz" "实际 $code"; fi
done
if [ -n "${KEEL_PUBLIC_URL:-}" ]; then
    code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 15 "$KEEL_PUBLIC_URL" 2>/dev/null || echo 000)
    if [ "$code" = "200" ]; then ok "外网 $KEEL_PUBLIC_URL 200"; else bad "外网可达" "$KEEL_PUBLIC_URL 实际 $code"; fi
else
    skip "外网可达" "基线里没有 KEEL_PUBLIC_URL"
fi
fi

# --- 3. 迁移版本 --------------------------------------------------------
if want_check migrate; then
section "迁移"
ver=$(sq "SELECT max(version_id) FROM goose_db_version WHERE is_applied")
expect_eq "core 迁移版本" "$ver" "$EXPECT_MIGRATION_CORE"
inv_ver=$(sq_inv "SELECT max(version_id) FROM goose_db_version_inventory WHERE is_applied")
expect_ge "inventory 迁移版本" "$inv_ver" "$EXPECT_MIGRATION_INVENTORY"
# 迁移跑过了不等于列没了：00333 删的是列，残留列意味着 down 没生效或半路失败。
leftover=$(sq "SELECT count(*) FROM information_schema.columns
               WHERE table_name='promotion_skus' AND column_name IN ('stock_qty','sold_qty')")
expect_eq "promotion_skus 残留冗余列" "$leftover" "0"
cons=$(sq "SELECT count(*) FROM pg_constraint WHERE conname='chk_promotion_sku_limit'")
expect_ge "chk_promotion_sku_limit 存在" "$cons" "1"
fi

# --- 4. 权限边界（C 档拆分） -------------------------------------------
if want_check grants; then
section "权限"
# 这一条是被 GRANT ON ALL TABLES 破坏过的。scripts/split-migrate.sh 判断
# 「已经切过」的唯一依据就是 has_table_privilege(...,'SELECT')=f，
# 一把补全就会让它认定没切过 → 核对行数不一致 → app 卡在 service_completed 起不来。
leak=0
for t in inventories inventory_logs activity_stocks; do
    v=$(sq "SELECT has_table_privilege('keel_app','public.${t}','SELECT')")
    [ "$v" = "t" ] && leak=$((leak + 1))
done
expect_eq "C 档收回的表被误授权数" "$leak" "0"
# barrier 只有 INSERT 是设计约定（迁移 00008 明写），不是漏授权。
b_ins=$(sq "SELECT has_table_privilege('keel_app','public.barrier','INSERT')")
b_sel=$(sq "SELECT has_table_privilege('keel_app','public.barrier','SELECT')")
expect_eq "barrier 有 INSERT" "$b_ins" "t"
expect_eq "barrier 无 SELECT" "$b_sel" "f"
n_grant=$(sq "SELECT count(*) FROM information_schema.role_table_grants WHERE grantee='keel_app'")
expect_eq "keel_app 授权条数" "$n_grant" "$EXPECT_GRANT_COUNT"
n_acl=$(sq "SELECT count(*) FROM pg_default_acl")
expect_ge "pg_default_acl 条数" "$n_acl" "$EXPECT_DEFAULT_ACL_MIN"
fi

# --- 5. 业务数据存量 ----------------------------------------------------
if want_check data; then
section "数据"
expect_ge "订单数"       "$(sq "SELECT count(*) FROM orders")"                   "$EXPECT_ORDERS_MIN"
expect_ge "商品数"       "$(sq "SELECT count(*) FROM products")"                 "$EXPECT_PRODUCTS_MIN"
expect_ge "用户数"       "$(sq "SELECT count(*) FROM users")"                    "$EXPECT_USERS_MIN"
expect_ge "商家数"       "$(sq "SELECT count(DISTINCT merchant_id) FROM products")" "$EXPECT_MERCHANTS_MIN"
# uploads 里 driver=2 是对象存储。库里存着不代表读得出来——丢 compose.s3.yaml
# 时它们一条不少，只是每条都 404。这条断言的是真能下载。
s3_n=$(sq "SELECT count(*) FROM uploads WHERE driver=2")
expect_ge "S3 图片记录数" "$s3_n" "$EXPECT_S3_UPLOADS_MIN"
if [ "${s3_n:-0}" -gt 0 ] 2>/dev/null; then
    # 读图路径是 `/api/v1/uploads/{id}`——**只认 id**，不认 storage_key。
    # 拼成 /api/v1/uploads/1/se/xxx.jpg 会 404（85 字节的 JSON 错误体），
    # 看着像图片坏了，其实是路径错了。判断要看 size_download，
    # 因为 404 也返回字节数，不看会被"有响应"骗过去。
    sid=$(sq "SELECT id FROM uploads WHERE driver=2 ORDER BY id LIMIT 1")
    if [ -n "$sid" ]; then
        read -r code size < <(curl -sL -o /dev/null -w '%{http_code} %{size_download}' --max-time 20 \
                              "http://127.0.0.1:${KEEL_HTTP_PORT:-18099}/api/v1/uploads/${sid}" 2>/dev/null || echo "000 0")
        if [ "$code" = "200" ] && [ "${size:-0}" -gt 1000 ] 2>/dev/null; then
            ok "S3 图片可下载（uploads/${sid}，${size} 字节）"
        else
            bad "S3 图片可下载" "uploads/${sid} 返回 ${code:-无响应}，${size:-0} 字节。记录在库里但读不出来，通常是 compose.s3.yaml 没叠上"
        fi
    else
        skip "S3 图片可下载" "driver=2 的记录拿不到 id"
    fi
fi
# 残留的 status=0 幂等键会让后续下单一直 409，冒烟因此会莫名失败。
stuck=$(sq "SELECT count(*) FROM idempotency_keys WHERE status=0")
if [ "${stuck:-0}" -eq 0 ]; then
    ok "无残留的 in-flight 幂等键"
else
    bad "残留的 in-flight 幂等键" "${stuck} 条 status=0，会让后续下单持续 409。清理：DELETE FROM idempotency_keys WHERE status=0"
fi
fi

# --- 6. 功能开关（漏配不报错，只是功能悄悄没了） -----------------------
if want_check features; then
section "功能开关（读运行中的 app 容器）"
if ! docker ps --format '{{.Names}}' | grep -q "^${PROJECT}-app-1$"; then
    skip "功能开关" "app 容器没在跑"
else
    expect_eq "地图底图来源" "$(app_env KEEL_MAP_TILES)"       "$EXPECT_MAP_TILES"
    expect_eq "渠道层"       "$(app_env KEEL_CHANNELS)"        "on"
    # KEEL_PAYMENT_SANDBOX **空 = 开**（app.go 的 sandboxEnabled：空则开，
    # 只有 off/false/0 才关），compose.yaml 里演示栈不设它。所以判据是
    # 「不是关的」，不是「等于 on」——照抄字面会在演示栈上误报。
    sb=$(app_env KEEL_PAYMENT_SANDBOX)
    case "${sb:-}" in
        off|false|0|Off|False) bad "沙箱支付" "容器里 KEEL_PAYMENT_SANDBOX=$sb，沙箱支付是关的，演示站买家无法走通支付回调" ;;
        *)                      ok  "沙箱支付（KEEL_PAYMENT_SANDBOX=${sb:-未设置=开}）" ;;
    esac
    if [ -n "$(app_env KEEL_S3_ACCESS_KEY)" ]; then
        ok "S3 凭据已注入（secret $(app_env KEEL_S3_SECRET_KEY | wc -c | tr -d ' ') 字符）"
    else
        bad "S3 凭据已注入" "容器里 KEEL_S3_ACCESS_KEY 为空，图片会退回本地卷，而库里记录都是 driver=2"
    fi
fi
fi

# --- 7. 错误日志 --------------------------------------------------------
if want_check logs; then
section "错误日志"
# 服务名 → 完整容器名由 container_of 统一提供。
for c in app inventory console; do
    name=$(container_of "$c")
    if ! docker ps --format '{{.Names}}' | grep -qx "$name"; then
        bad "$c 错误日志" "容器 $name 不在运行，这项没法验 —— 不能当作通过"
        continue
    fi
    # 2>&1 因为容器日志走 stderr。ERROR/FATAL/panic 才算。
    n=$(docker logs --since "${KEEL_LOG_WINDOW:-30m}" "$name" 2>&1 \
        | grep -cE '(^|[^a-z])(ERROR|FATAL|panic:)' || true)
    if [ "${n:-0}" -eq 0 ]; then
        ok "$c 近 ${KEEL_LOG_WINDOW:-30m} 无 ERROR"
    else
        bad "$c 有 ERROR 日志" "${n} 条。最近一条：$(docker logs --tail 400 "$name" 2>&1 | grep -E '(^|[^a-z])(ERROR|FATAL|panic:)' | tail -1 | cut -c1-160)"
    fi
done
fi

# --- 8. 冒烟 ------------------------------------------------------------
if want_check smoke; then
section "冒烟"
if [ "$quick" = "1" ]; then
    skip "scripts/smoke.sh" "--quick"
else
    if ./scripts/smoke.sh >/tmp/verify-demo-smoke.log 2>&1; then
        ok "scripts/smoke.sh 通过"
    else
        bad "scripts/smoke.sh" "退出码非 0，日志末尾：$(tail -5 /tmp/verify-demo-smoke.log | tr '\n' ' ' | cut -c1-300)"
    fi
fi
fi

# --- 汇总 ---------------------------------------------------------------
printf '\n\033[1m-- 汇总 --\033[0m  通过 %d，失败 %d，跳过 %d\n' "${#passed[@]}" "${#failed[@]}" "${#skipped[@]}"
if [ "${#skipped[@]}" -gt 0 ]; then
    # 跳过的项**不算通过**，也不该在报告里被当成「反正全绿」。逐条说明为什么跳。
    printf '跳过（这些没有获得保证）：\n'
    printf '  - %s\n' "${skipped[@]}"
fi
if [ "${#failed[@]}" -gt 0 ]; then
    printf '失败项：\n'
    printf '  - %s\n' "${failed[@]}"
    echo
    echo "**别急着发布。** 先定位上面每一条：多数不是新改坏的，是之前就悄悄坏了、"
    echo "今天才被发现。改完再跑一次本脚本，全绿才算完。"
    exit 1
fi
echo '演示栈基线全绿。'