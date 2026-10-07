#!/usr/bin/env bash
# 多商家形态验收：证明「一套部署里跑几家店」这件事真的成立，而不只是单测里成立。
#
# 为什么需要它：`compose.multi.yaml` 从 M1 起就在仓库里，但没有任何自动化跑过它 ——
# CI 的 e2e job 只起单商家形态，演示站与生产栈都必填 `KEEL_DEFAULT_MERCHANT`。
# 于是租户管理这条路径（开店 → 解析得到 → 停用 → 买家 404）只有单测，没有端到端的证据。
# 单测与 `check_query_tenancy.py` 那几道闸门守的是「查询里不许写 merchant_id」，
# 它们管不到「Host 解析配对不对」「新开的店真进得去吗」「停用之后买家是不是打不开」。
#
# 判据全部是「必须等于某个具体值」，与 scripts/verify-demo.sh 同一个取舍：宁可误报也不漏报。
# 拿不到凭据的那些项**判为失败，不跳过** —— 一条永远不会红的检查比没有检查更糟。
#
# 会对这一栈**写数据**：开一家 verify-<随机> 的店、把它停用再启用回来。所以它只该跑在
# 多商家测试栈上；项目名是 keeldemo 时直接拒绝，防止对着演示站手滑。
#
# 为什么不调用 scripts/smoke.sh：它最后一步搜「连衣裙」，断言的是 single.sql 播的那份目录 ——
# 那种子里商品有真的标题与 search_text。dev.sql 的商品叫「shop-a 的商品 1」，而
# search_text 一个都没填（派生检索数据由服务层写，种子的裸 INSERT 绕过它），
# 于是任何检索词在这套种子上都是 0 条。复用它会得到一个与租户无关的红灯。
# 所以买家侧那几条在本脚本里自己走 —— 而「同一个号码在两家店是两个账号」
# 这条恰恰只有多商家形态能验。
#
# 用法：
#   scripts/multi-verify.sh          全查（栈要已由 scripts/multi-up.sh 起好）
set -uo pipefail

cd "$(dirname "$0")/.."

PROJECT="${COMPOSE_PROJECT_NAME:-keelmulti}"
PORT="${KEEL_HTTP_PORT:-28185}"
CONSOLE_PORT="${KEEL_CONSOLE_PORT:-28186}"
BASE="http://127.0.0.1:${PORT}"
API=/api/v1
APP="${PROJECT}-app-1"

# 种子 dev.sql 播的六家店，每一家都是某个分支的用例（见那个文件的开头）：
#   shop-a 3 件商品 / shop-b 2 件 —— 件数刻意不同，RLS 一旦失效两边会同时变成 5
#   shop-c 只绑自定义域名 custom.example.net（子域名那一支拿它当不了靶子）
#   shop-nodomain 没登记域名，只能靠子域名可达
#   shop-closed 停用、shop-deleted 软删 —— 两家都必须表现为「不存在」
A_HOST=shop-a.example.com
B_HOST=shop-b.example.com
CUSTOM_HOST=custom.example.net
# dev.sql 在 shop-a 与 shop-b 各播了一个同号买家 13800000001（口令见那个文件：keel-dev-2026）。
BUYER_PHONE=13800000001
BUYER_PASSWORD=keel-dev-2026

if [ "$PROJECT" = "keeldemo" ]; then
    echo "拒绝执行：COMPOSE_PROJECT_NAME=keeldemo 是演示栈，这个脚本会往里写数据。" >&2
    exit 1
fi

declare -a passed=() failed=()
section() { printf '\n\033[1m== %s ==\033[0m\n' "$1"; }
ok()  { printf '  \033[32mOK\033[0m   %s\n' "$1"; passed+=("$1"); }
bad() { printf '  \033[31mFAIL\033[0m %s\n' "$1"; [ $# -gt 1 ] && printf '       %s\n' "$2"; failed+=("$1"); }
expect_eq() {
    if [ "$2" = "$3" ]; then ok "$1 = $2"; else bad "$1" "期望 $3，实际 ${2:-空}"; fi
}
# expect_code <名字> <期望状态码>：拿上一发 call 的 $CODE 来比。
expect_code() { expect_eq "$1" "$CODE" "$2"; }
# 只断言「至少这么多」的检查：它不是退让，是给下面那些「等于 0」的断言立靶子
# —— 靶子本身空不空必须查，两边都是 0 的观察证明不了任何隔离。
expect_ge() {
    if [ -n "$2" ] && [ "$2" -ge "$3" ] 2>/dev/null; then
        ok "$1 = $2（≥ $3）"
    else
        bad "$1" "期望 ≥ $3，实际 ${2:-空}"
    fi
}

for a in "$@"; do
    case "$a" in
        -h|--help) sed -n '2,29p' "$0"; exit 0 ;;
        *) echo "未知参数：$a（这个脚本没有 --quick，理由见文件头「为什么不调 smoke.sh」）" >&2; exit 2 ;;
    esac
done

tmp_body=$(mktemp)
tmp_hdr=$(mktemp)
trap 'rm -f "$tmp_body" "$tmp_hdr"' EXIT

# call <host> <method> <path> <body> <bearer> [额外的头...]
# 结果进 $CODE（状态码）与 $tmp_body / $tmp_hdr。
# Host 用 -H 传而不是写进 URL：这一栈不发 80 端口、也没有本地 DNS，写进 URL 会让
# curl 连不上，而那个症状和「服务挂了」一模一样。
call() {
    local host=$1 method=$2 path=$3 body=$4 bearer=$5
    shift 5
    local args=(-s -S -o "$tmp_body" -D "$tmp_hdr" -w '%{http_code}'
                --max-time 25 -X "$method" -H "Host: $host")
    local h
    [ -n "$body" ] && args+=(-H "Content-Type: application/json" --data "$body")
    [ -n "$bearer" ] && args+=(-H "Authorization: Bearer $bearer")
    for h in "$@"; do args+=(-H "$h"); done
    CODE=$(curl "${args[@]}" "${BASE}${path}" 2>/dev/null || echo 000)
}

# 从上一份响应体里按点分路径取值（items.0.id）。取不到打印空串。
jval() {
    python3 - "$tmp_body" "$1" <<'PY'
import json, sys
try:
    cur = json.load(open(sys.argv[1]))
except Exception:
    print('')
    raise SystemExit
for part in sys.argv[2].split('.'):
    if not part:
        continue
    try:
        cur = cur[int(part)] if isinstance(cur, list) else cur.get(part)
    except Exception:
        cur = None
print('' if cur is None else cur)
PY
}

# 上一份商家目录响应里按 code 查它的 status；code 不在列表里就打印空串。
# 「不在」正是几条断言要的观测（软删的店就不该出现在目录里），所以空串是合法值，
# 不是错误 —— 用 expect_eq 比空串就行。
status_of() {
    python3 - "$tmp_body" "$1" <<'PY'
import json, sys
try:
    d = json.load(open(sys.argv[1]))
except Exception:
    print('')
    raise SystemExit
for it in d.get('items') or []:
    if it.get('code') == sys.argv[2]:
        print(it.get('status'))
        raise SystemExit
print('')
PY
}

# 只看条数不够：「两边各 3 件」和「两边都看到全部 3 件」是同一个观察，所以比 id 集合。
product_ids() {
    python3 - "$tmp_body" <<'PY'
import json, sys
try:
    body = json.load(open(sys.argv[1]))
except Exception:
    print('parse-error')
    raise SystemExit
print(' '.join(str(i['id']) for i in body.get('items') or []))
PY
}

printf '\033[1m多商家形态验收\033[0m  项目=%s  基址=%s  基础域名=example.com\n' "$PROJECT" "$BASE"

# --- 0. 栈在不在，形态对不对 --------------------------------------------
section "部署形态"
if ! docker ps --format '{{.Names}}' | grep -qx "$APP"; then
    bad "app 容器在跑" "找不到 $APP。先跑 ./scripts/multi-up.sh"
    printf '\n栈没起来，后面每一项都没法验。\n'
    exit 1
fi

# 读运行中容器里的环境变量，而不是读 compose 解析结果 —— 与 verify-demo.sh 同一条理由：
# 只有进程里那一份算数。
env_of() { docker exec "$APP" sh -c "printf '%s' \"\${$1:-}\"" 2>/dev/null | tr -d '\r'; }
expect_eq "KEEL_DEFAULT_MERCHANT 为空（进多商家分支）" "$(env_of KEEL_DEFAULT_MERCHANT)" ""
expect_eq "KEEL_BASE_DOMAIN" "$(env_of KEEL_BASE_DOMAIN)" "example.com"

# 判据是 healthz，不是 docker ps：应用会因 tenant.Preflight 不过而退出，
# 而 `docker compose up -d` 对这件事照样返回 0（同一条坑写在 ci.yml 里）。
ready=""
for _ in $(seq 1 60); do
    code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 "${BASE}/healthz" 2>/dev/null || true)
    [ "$code" = "200" ] && { ready=1; break; }
    sleep 1
done
if [ -z "$ready" ]; then
    bad "app /healthz 200" "60 秒内没就绪。看 ./scripts/multi-up.sh --logs"
    printf '\n应用没起来，后面每一项都没法验。\n'
    exit 1
fi
ok "app /healthz 200"

# --- 1. 买家侧：Host 解析 -----------------------------------------------
section "商家解析（GET ${API}/products）"
# 停用 / 软删 / 保留字 / apex / 对不上的名字都必须是 404 而不是 500：
# 404 是「这家店不存在」，500 是「服务器出错」，契约里是两回事。
check_resolve() {
    local host=$1 want=$2 why=$3
    call "$host" GET "${API}/products" "" ""
    if [ "$CODE" = "$want" ]; then
        ok "$host → $CODE（$why）"
    else
        bad "$host → $want" "实际 $CODE：$(head -c 200 "$tmp_body" | tr -d '\n')"
    fi
}
check_resolve "$A_HOST"        200 "子域名按 code 解析"
check_resolve "$B_HOST"        200 "子域名按 code 解析"
check_resolve "$CUSTOM_HOST"   200 "第二个匹配分支：shop_settings.domain"
check_resolve shop-nodomain.example.com 200 "没登记域名的店靠子域名可达"
check_resolve shop-closed.example.com   404 "停用的店当不存在"
check_resolve shop-deleted.example.com  404 "软删的店当不存在"
check_resolve ghost.example.com         404 "库里没有的 code"
check_resolve example.com               404 "基础域名本身归平台，不归任何商家"
check_resolve api.example.com           404 "保留字：不查库直接 404"
# 二级子域名也不能落到任何商家上（label 带点就不查）。
check_resolve shop-a.shop-b.example.com 404 "基础域名下多一级的名字"

# --- 2. 租户隔离：两边看到的是各自的东西 -------------------------------
section "租户隔离（两边都有数据，且互不相交）"
call "$A_HOST" GET "${API}/products" "" ""
ids_a=$(product_ids)
call "$B_HOST" GET "${API}/products" "" ""
ids_b=$(product_ids)
title_b=$(jval 'items.0.title')

expect_eq "shop-a 商品数" "$(echo "$ids_a" | wc -w | tr -d ' ')" "3"
expect_eq "shop-b 商品数" "$(echo "$ids_b" | wc -w | tr -d ' ')" "2"
if [ -n "${ids_a// /}" ] && [ -n "${ids_b// /}" ]; then
    inter=$(comm -12 <(printf '%s\n' $ids_a | sort) <(printf '%s\n' $ids_b | sort) | tr -d '\n')
    expect_eq "两边 id 交集" "${inter:-（空）}" "（空）"
else
    bad "两边 id 交集" "有一边拿到空列表 —— 空列表既可能是隔离生效，也可能是那家没商品"
fi
# 标题里带着商家 code（'shop-b 的商品 2'），所以这一条是上面那条的人话版本：
# b 家的列表里出现 a 家的商品名就是泄露，不需要先读懂 id。
case "$title_b" in
    *"shop-a 的商品"*) bad "shop-b 列表里出现 shop-a 的商品" "$title_b" ;;
    *"shop-b 的商品"*) ok "shop-b 只看到自己的商品（$title_b）" ;;
    *)                 bad "shop-b 商品标题" "读不出归属：${title_b:-空}" ;;
esac

# --- 3. 买家侧跨租户：同一个号码在两家店是两个账号 ---------------------
section "买家账号按商家独立（同一号码两家店）"
# 号码在 users 上的唯一键是 (merchant_id, phone) 而不是 phone，所以两家用得着同一个号。
login_as() {
    call "$1" POST "${API}/auth/login" \
        "$(printf '{"phone":"%s","password":"%s"}' "$BUYER_PHONE" "$BUYER_PASSWORD")" ""
    [ "$CODE" = "200" ] && jval access_token
}
buyer_a=$(login_as "$A_HOST")
buyer_b=$(login_as "$B_HOST")
[ -n "$buyer_a" ] && ok "$A_HOST 用 ${BUYER_PHONE} 登录成功" || bad "$A_HOST 买家登录" "实际 $CODE：$(head -c 160 "$tmp_body")"
[ -n "$buyer_b" ] && ok "$B_HOST 用同一个号码也登录成功" || bad "$B_HOST 买家登录" "实际 $CODE：$(head -c 160 "$tmp_body")"

call "$A_HOST" GET "${API}/me" "" "$buyer_a"; id_a=$(jval id)
call "$B_HOST" GET "${API}/me" "" "$buyer_b"; id_b=$(jval id)
if [ -n "$id_a" ] && [ -n "$id_b" ] && [ "$id_a" != "$id_b" ]; then
    ok "同号买家在两家店是两个账号（${id_a} 与 ${id_b}）"
else
    bad "同号买家在两家店是两个账号" "读到 id_a=${id_a:-空} id_b=${id_b:-空} —— 相同就是共用账号"
fi
# a 家的令牌打到 b 家：必须明确拒绝，不能被当成未登录、更不能读到 a 的资料。
# 契约给这一种拒绝单独的 type。
call "$B_HOST" GET "${API}/me" "" "$buyer_a"
if [ "$CODE" = "401" ] && grep -q 'token-tenant-mismatch' "$tmp_body"; then
    ok "a 家的令牌打 b 家 → 401 token-tenant-mismatch"
else
    bad "a 家的令牌打 b 家 → 401 token-tenant-mismatch" "实际 $CODE：$(head -c 200 "$tmp_body")"
fi

# --- 4. 平台级会话 ------------------------------------------------------
section "平台级会话"
TOKEN_CACHE=tmp/multi-verify-platform-token
platform=""
# 引导 token 只在「库里还没有在岗平台管理员」时签发，而且用掉即失效 ——
# 第二次跑这个脚本时它已经不存在了。会话默认 7 天有效，所以缓存是唯一不自欺的办法；
# 缓存也救不回来就要求一次干净重来（./scripts/multi-up.sh --wipe），而不是跳过这几项。
if [ -s "$TOKEN_CACHE" ]; then
    candidate=$(head -1 "$TOKEN_CACHE")
    call "$A_HOST" GET "${API}/admin/merchants" "" "$candidate"
    [ "$CODE" = "200" ] && platform=$candidate
fi
if [ -z "$platform" ]; then
    bt=$(docker logs "$APP" 2>&1 | grep -oE 'bootstrap_token=[A-Za-z0-9_-]+' | tail -1 | cut -d= -f2)
    if [ -n "$bt" ]; then
        call "$A_HOST" POST "${API}/admin/auth/bootstrap" \
            "$(printf '{"token":"%s","email":"platform-verify@keel.invalid"}' "$bt")" ""
        platform=$(jval token)
        [ -n "$platform" ] && mkdir -p tmp && printf '%s\n' "$platform" > "$TOKEN_CACHE"
    fi
fi
if [ -z "$platform" ]; then
    bad "拿到平台级会话" "引导 token 已用掉、缓存会话又失效。跑 ./scripts/multi-up.sh --wipe 起一份干净的库"
    printf '\n没有平台会话，开店 / 停用那几项没法验，判为失败而不是跳过。\n'
else
    ok "拿到平台级会话"
    call "$A_HOST" GET "${API}/admin/merchants?page_size=100" "" "$platform"
    expect_code "商家目录 200" "200"
    # 断的是「哪几家在、各自什么状态」，不是「看见几家」。上一版断的是总数 = 5，
    # 而这个脚本每跑一次就往目录里留一家 verify-* 的店，第二遍就成了 6 ——
    # 那条红灯发现的不是回归，是自己上次留下的数据。按 code 断的每一条
    # 都与跑过几次无关，而且真把查询条件改坏（漏了 deleted_at、或把停用过滤掉了）时，
    # 总数未必变，code 一定变。
    #
    # 含停用：shop-closed 在目录里，且状态就是 2。
    expect_eq "目录里能看见停用的店（shop-closed 的状态）" "$(status_of shop-closed)" "2"
    # 不含软删：出现即说明目录查询漏了 deleted_at 条件。
    expect_eq "目录里看不见软删的店（shop-deleted）" "$(status_of shop-deleted)" ""
    # 种子那四家在营的，一个都不能少。
    for code in shop-a shop-b shop-c shop-nodomain; do
        expect_eq "目录里有 $code（状态 1）" "$(status_of "$code")" "1"
    done
    expect_ge "目录里的商家不少于种子那五家" "$(jval total)" "5"
    # 这个标志是后台把「开店」按钮置灰的依据；多商家形态下它必须是 false，
    # 否则界面会告诉管理员「这套部署不能开店」，而它明明能。
    expect_eq "商家目录里的 single_merchant_mode" "$(jval single_merchant_mode)" "False"

    # 商家后台那个源（nginx 静态产物 + /api 反代）能不能拿到同一份目录。
    # 不复用上面那条：它打的是 app 的端口，而后台界面走的是 console 那个源 ——
    # 反代丢了的时候页面照样打得开，只有一点就全是网络错误（CI 里单商家那条查的就是这个）。
    # Host 仍然要带：多商家形态下不认 Host 就解析不出任何店，那时的 404 会被误读成「后台坏了」。
    code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 20 \
               -H "Host: $A_HOST" -H "Authorization: Bearer $platform" \
               "http://127.0.0.1:${CONSOLE_PORT}/api/v1/admin/merchants" 2>/dev/null || echo 000)
    expect_eq "经商家后台这个源也拿得到目录（反代与 Host 都活着）" "$code" "200"
fi

# --- 5. 开店 -------------------------------------------------------------
section "开店：POST ${API}/admin/merchants"
shop="verify-$RANDOM"
idem=$(cat /proc/sys/kernel/random/uuid)
body=$(printf '{"code":"%s","name":"多商家验收店","admin_email":"admin-%s@keel.invalid"}' "$shop" "$shop")
call "$A_HOST" POST "${API}/admin/merchants" "$body" "$platform" "Idempotency-Key: $idem"
new_id=$(jval id)
expect_code "开店 201" "201"
if [ -n "$new_id" ]; then ok "新店 id = $new_id（code=$shop）"; else bad "新店 id" "响应体：$(head -c 200 "$tmp_body")"; fi

# 同一把幂等键重发：回放首次的 201，不建第二家店，**也不再签第二条登录链接**。
call "$A_HOST" POST "${API}/admin/merchants" "$body" "$platform" "Idempotency-Key: $idem"
expect_code "同幂等键重放 201" "201"
replayed=$(grep -icE '^idempotency-replayed: *true' "$tmp_hdr")
expect_eq "重放响应头 Idempotency-Replayed" "$([ "${replayed:-0}" -ge 1 ] && echo true || echo false)" "true"

# 换一个键用同一个 code：必须是「code 已被占用」，而不是别的 409。
call "$A_HOST" POST "${API}/admin/merchants" "$body" "$platform" \
    "Idempotency-Key: $(cat /proc/sys/kernel/random/uuid)"
expect_code "重复 code 开店 409" "409"
if grep -q 'merchant-code-taken' "$tmp_body"; then
    ok "409 的 type 是 merchant-code-taken"
else
    bad "409 的 type" "期望 merchant-code-taken，响应体：$(head -c 200 "$tmp_body")"
fi

# --- 6. 新开的店真的能进去，而且里面是空的 -------------------------------
section "新店可达且与别家无关"
shop_host="${shop}.example.com"
call "$shop_host" GET "${API}/products" "" ""
expect_code "新店买家侧 200" "200"
# 0 件这件事是有靶子的：shop-a 有 3 件。解析器认错 Host、或 RLS 没生效，这里都不会是 0。
expect_eq "新店商品数" "$(jval total)" "0"

# 新店第一个管理员的一次性登录 token 只出现在日志里（本项目没接 SMTP）——
# 这本身就是开店流程今天真实的样子：交付方式是「去 grep 日志」。验收顺手把它抓出来用。
admin_link=$(docker logs "$APP" 2>&1 | grep "merchant_code=$shop" \
    | grep -oE 'token=[A-Za-z0-9_-]+' | tail -1 | cut -d= -f2)
merchant_tok=""
if [ -n "$admin_link" ]; then
    call "$shop_host" POST "${API}/admin/auth/session" "$(printf '{"token":"%s"}' "$admin_link")" ""
    merchant_tok=$(jval token)
fi
if [ -n "$merchant_tok" ]; then
    ok "新店管理员用一次性链接换到商家级会话"
else
    bad "新店管理员换会话" "日志里没有 code=$shop 的登录 token，或兑换失败"
fi

# --- 7. 越权：商家级碰不到租户管理 --------------------------------------
section "商家级会话没有平台权限"
# 这一组必须打在**新店自己的 Host** 上。StaffBearer 的第 4 步先比租户：令牌里的
# 租户对不上本请求解析出的租户就 401，连查库都不做（那一步的理由写在
# internal/auth/staff_middleware.go 的文件头）。拿这家店的会话去撞 shop-a，
# 测到的是那道租户比对，压根到不了权限这一层 —— 上一版就是这么错成 401 的。
if [ -n "$merchant_tok" ]; then
    call "$A_HOST" GET "${API}/admin/products" "" "$platform"
    expect_code "平台会话在 Host 那家读商品目录 200" "200"
    # 这个数非空是下面那条「切过去就是 0 件」的靶子：两边都是 0 的话，
    # 头有没有生效根本区分不出来。
    expect_ge "Host 那家（shop-a）的商品目录非空" "$(jval total)" "1"
    # 切店前后必须是两个数，否则「切过去了」只是响应头好看。0 有靶子：
    # 刚开的店一件商品都没有，而 Host 那家有 a_total 件。
    call "$A_HOST" GET "${API}/admin/products" "" "$platform" "X-Keel-Merchant: ${shop}"
    expect_code "平台会话经 X-Keel-Merchant 切进新店 200" "200"
    expect_eq "切店后读到的就是新店的商品目录（0 件）" "$(jval total)" "0"
    # 头的值是 **code**，不是 id（staff_tenant.go 按 ByCodeForPlatform 查）。
    # 传 id 会走规则 4：422 unknown-merchant，而且刻意不回落 Host 那家。
    call "$A_HOST" GET "${API}/admin/products" "" "$platform" "X-Keel-Merchant: ${new_id}"
    expect_code "X-Keel-Merchant 传 id（不是 code）→ 422" "422"
    expect_eq "422 的 type 是 unknown-merchant" "$(jval type)" "https://keel.dev/problems/unknown-merchant"

    call "$shop_host" GET "${API}/admin/merchants" "" "$merchant_tok"
    expect_code "商家级读商家目录 → 403" "403"
    # 403 还不够：三种 403 各自要客户端做的事相反（见 problem.go:378 那段），
    # 所以比 type。这里必须是 platform-only，不是 unauthorized 也不是 forbidden。
    expect_eq "403 的 type 是 platform-only" "$(jval type)" "https://keel.dev/problems/platform-only"
    # X-Keel-Merchant 是平台级会话切店用的头。商家级带上必须被拒，**不能被悄悄忽略**：
    # 忽略的话它会继续在自己那家店执行，看着一切正常，而调用者以为操作的是别家。
    # 用一条商家级本来就有权限的路由（admin/products）来测，这样 403 只可能来自头本身。
    call "$shop_host" GET "${API}/admin/products" "" "$merchant_tok" "X-Keel-Merchant: shop-a"
    expect_code "商家级带 X-Keel-Merchant → 403" "403"
    # code 是真实存在的（shop-a），所以这不是「找不到商家」，是「你没资格切」。
    expect_eq "403 的 type 是 tenant-switch-forbidden" "$(jval type)" "https://keel.dev/problems/tenant-switch-forbidden"
    call "$shop_host" POST "${API}/admin/merchants" "$body" "$merchant_tok" \
        "Idempotency-Key: $(cat /proc/sys/kernel/random/uuid)"
    expect_code "商家级开店 → 403" "403"
    call "$shop_host" PATCH "${API}/admin/merchants/${new_id}" '{"name":"改名试试"}' "$merchant_tok"
    expect_code "商家级改商家目录 → 403" "403"
else
    bad "商家级越权那组" "没有商家级会话可用（第 6 节没拿到），不能当作通过"
fi

# --- 8. 停用与启用 ------------------------------------------------------
section "停用 / 启用（买家侧随之间断）"
# 解析结果缓存 30 秒，所以生效有延迟。轮询而不是 sleep 一个固定值 ——
# 固定 31 秒在缓存刚好临界时会假红。
wait_for() {
    local host=$1 want=$2 limit=${3:-45} i
    for ((i = 0; i < limit; i++)); do
        call "$host" GET "${API}/products" "" ""
        [ "$CODE" = "$want" ] && return 0
        sleep 1
    done
    return 1
}
call "$A_HOST" PATCH "${API}/admin/merchants/${new_id}" '{"status":2}' "$platform"
expect_code "停用 200" "200"
if wait_for "$shop_host" 404; then
    ok "停用后买家侧 404（半分钟内生效）"
else
    bad "停用后买家侧 404" "45 秒后仍是 $CODE"
fi
# 平台仍然管得到这家店 —— 进不去就修不好、也启不回来（契约里写明这条是设计）。
# 两条都要，因为它们走的是不同的闸门：
#   · 按 id 读目录：平台作用域，与停用与否无关，证明停用没有把它从目录里抹掉；
#   · 经 X-Keel-Merchant 切进去读商品：解析用的是 ByCodeForPlatform（**含停用**），
#     买家侧那条只认 status=1。停用的店运营进得去、买家进不去，正是这套设计的全部意思。
call "$A_HOST" GET "${API}/admin/merchants/${new_id}" "" "$platform"
expect_code "停用后平台仍读得到这家店的目录条目" "200"
call "$A_HOST" GET "${API}/admin/products" "" "$platform" "X-Keel-Merchant: ${shop}"
expect_code "停用后平台仍能切进这家店管理" "200"
call "$A_HOST" PATCH "${API}/admin/merchants/${new_id}" '{"status":1}' "$platform"
expect_code "启用 200" "200"
if wait_for "$shop_host" 200; then
    ok "启用后买家侧回到 200（停得掉也启得回来，留下的店是活的）"
else
    bad "启用后买家侧 200" "45 秒后仍是 $CODE"
fi

# --- 汇总 ---------------------------------------------------------------
printf '\n\033[1m-- 汇总 --\033[0m  通过 %d，失败 %d\n' "${#passed[@]}" "${#failed[@]}"
if [ "${#failed[@]}" -gt 0 ]; then
    printf '失败项：\n'; printf '  - %s\n' "${failed[@]}"
    echo
    echo "先看 app 日志：./scripts/multi-up.sh --logs"
    exit 1
fi
echo "多商家形态全绿。商家管理页：http://localhost:${CONSOLE_PORT}/admin/（要平台级会话）"
echo "买家侧按 Host 才解析得到，例：curl -H 'Host: ${A_HOST}' ${BASE}${API}/products"
