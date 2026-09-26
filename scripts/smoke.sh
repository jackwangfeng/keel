#!/usr/bin/env bash
# 冒烟：证明 `docker compose up` 起来的那一栈，从 HTTP 一路通到种子数据。
#
# 它断言的是链路，不是某个函数：迁移跑过了吗、种子加载了吗、租户解析配对了吗、
# RLS 下 keel_app 真读得到自己的商品吗 —— 这五件事里任何一件断掉，
# 下面那个非空断言就红。单元测试一条都盖不住它们，因为它们全在进程之外。
#
# 退出码 0 表示链路通。
set -euo pipefail

SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)

# 端口与 compose.yaml 里的映射用同一个变量，两边不会各说各话。
PORT="${KEEL_HTTP_PORT:-8080}"
BASE="${KEEL_BASE:-http://localhost:$PORT}"

# 多商家形态（compose.multi.yaml）里请求要带 Host，单商家形态不需要。
# 空串时一个 -H 参数都不加 —— 带一个空 Host 头会让 curl 发出非法请求。
HOST="${KEEL_SMOKE_HOST:-}"
curl_args=(-s -S)
if [ -n "$HOST" ]; then
    curl_args+=(-H "Host: $HOST")
fi

# 等多久。默认 60 秒覆盖「镜像刚建完、Postgres 首次初始化 + 迁移 + 种子」那一轮。
TIMEOUT="${KEEL_SMOKE_TIMEOUT:-60}"

echo "==> 等 $BASE/healthz 返回 200（最多 ${TIMEOUT}s）"
for ((i = 1; i <= TIMEOUT; i++)); do
    code=$(curl "${curl_args[@]}" -o /dev/null -w '%{http_code}' "$BASE/healthz" 2>/dev/null || true)
    if [ "$code" = "200" ]; then
        echo "    第 ${i}s 就绪"
        break
    fi
    if [ "$i" -eq "$TIMEOUT" ]; then
        echo "健康检查超时：${TIMEOUT}s 内 $BASE/healthz 没有返回 200（最后一次是 ${code:-无响应}）" >&2
        echo "看一眼 \`docker compose ps\` 与 \`docker compose logs app\`。" >&2
        exit 1
    fi
    sleep 1
done

echo "==> GET $BASE/api/v1/products 应返回非空商品列表"
body_file=$(mktemp)
trap 'rm -f "$body_file"' EXIT
code=$(curl "${curl_args[@]}" -o "$body_file" -w '%{http_code}' "$BASE/api/v1/products")
if [ "$code" != "200" ]; then
    echo "商品列表返回 $code，期望 200：" >&2
    cat "$body_file" >&2
    echo >&2
    exit 1
fi

# 用 python3 解析而不是 grep：grep 分不清「items 里有东西」和「响应体里
# 恰好有 items 这几个字母」——比如一个 Problem 响应里的字段名。
count=$(python3 - "$body_file" <<'PY'
import json, sys
with open(sys.argv[1]) as f:
    body = json.load(f)
items = body["items"]
if not isinstance(items, list):
    raise SystemExit("items 不是数组")
print("%d %d" % (len(items), body["total"]))
PY
) || { echo "响应体不是预期的商品列表 JSON：" >&2; cat "$body_file" >&2; echo >&2; exit 1; }

n=${count%% *}
total=${count##* }
if [ "$n" -lt 1 ]; then
    echo "商品列表为空（total=$total），种子数据没起作用" >&2
    cat "$body_file" >&2
    echo >&2
    exit 1
fi
echo "    本页 $n 件，total=$total"

# ---------------------------------------------------------------------------
# 买家身份（M2 任务 1.5）
# ---------------------------------------------------------------------------
#
# 口令固定在 db/seed/single.sql 里，那个文件写明了为什么可以固定。
# 这一段断言的同样是链路而不是函数：users 表迁出来了吗、种子里那行买家插进去
# 了吗、argon2id 的哈希在**真的跑起来的进程**里验得过吗、令牌签发与 bearer
# 中间件接上了吗、退出登录真的落到 user_tokens 上了吗。单元测试一条都盖不住
# 这些，它们全在进程之外。
SMOKE_PHONE="${KEEL_SMOKE_PHONE:-13800000000}"
SMOKE_PASSWORD="${KEEL_SMOKE_PASSWORD:-keel-demo-2026}"

echo "==> POST $BASE/api/v1/auth/login 用种子里的买家登录"
login_file=$(mktemp)
trap 'rm -f "$body_file" "$login_file"' EXIT
code=$(curl "${curl_args[@]}" -o "$login_file" -w '%{http_code}' \
    -H 'Content-Type: application/json' \
    -d "{\"phone\":\"$SMOKE_PHONE\",\"password\":\"$SMOKE_PASSWORD\"}" \
    "$BASE/api/v1/auth/login")
if [ "$code" != "200" ]; then
    echo "登录返回 $code，期望 200：" >&2
    cat "$login_file" >&2
    echo >&2
    exit 1
fi

# 同样用 python3 解析：grep 分不清「真有 access_token」和「响应体里恰好有
# 这几个字母」—— 一个 Problem 响应里的 detail 就可能带上它。
token=$(python3 - "$login_file" <<'PYEOF'
import json, sys
with open(sys.argv[1]) as f:
    body = json.load(f)
tok = body["access_token"]
if not tok or body["token_type"] != "Bearer":
    raise SystemExit("登录响应里没有可用的 access_token")
print(tok)
PYEOF
) || { echo "登录响应不是预期的 LoginResponse：" >&2; cat "$login_file" >&2; echo >&2; exit 1; }
echo "    拿到 access_token（${#token} 字符）"

# ---------------------------------------------------------------------------
# 下单到收款（M2 主链路）
# ---------------------------------------------------------------------------
#
# 这一段是 M2 验收点名补的。在它之前，`grep -c "orders\|webhooks\|payments"`
# 这个文件的结果是 0：M2 的主链路完全靠 internal/handler 的 httptest 撑着，
# 而那些测试**不经过真实 HTTP server、不经过 compose 起来的那个镜像**。
#
# 也就是说：镜像里 libdtmrs.so 链接坏了、KEEL_DTM_DSN 指到一个不可写的路径、
# webhook 路由忘了挂进 v1 组、沙箱开关被关掉了 —— 以上任何一条成立，全部单元
# 测试照样绿，smoke 也照样绿。产出标志「能下单能支付」在进程外一个闸门都没有。

AUTH=(-H "Authorization: Bearer $token")

# 地址 id 固定取 1：种子（db/seed/single.sql）给这个买家只插一条地址，而 compose
# 起的是一卷全新的库。契约里没有 GET /addresses，没有别的拿法。
SMOKE_ADDRESS_ID="${KEEL_SMOKE_ADDRESS_ID:-1}"

product_id=$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["items"][0]["id"])' "$body_file")

echo "==> GET $BASE/api/v1/products/$product_id 挑一个有货的 SKU"
detail_file=$(mktemp)
order_file=$(mktemp)
intent_file=$(mktemp)
trap 'rm -f "$body_file" "$login_file" "$detail_file" "$order_file" "$intent_file"' EXIT

code=$(curl "${curl_args[@]}" "${AUTH[@]}" -o "$detail_file" -w '%{http_code}' \
    "$BASE/api/v1/products/$product_id")
if [ "$code" != "200" ]; then
    echo "商品详情返回 $code，期望 200：" >&2; cat "$detail_file" >&2; echo >&2; exit 1
fi
read -r sku_id stock_before < <(python3 "$SCRIPT_DIR/smoke_pick_sku.py" "$detail_file") \
    || { echo "商品详情里挑不出有货的 SKU：" >&2; cat "$detail_file" >&2; echo >&2; exit 1; }
echo "    sku=$sku_id 下单前水位=$stock_before"

order_body="{\"items\":[{\"sku_id\":$sku_id,\"quantity\":1}],\"address_id\":$SMOKE_ADDRESS_ID}"

echo "==> POST $BASE/api/v1/orders/preview 试算"
code=$(curl "${curl_args[@]}" "${AUTH[@]}" -o "$order_file" -w '%{http_code}' \
    -H 'Content-Type: application/json' -d "$order_body" "$BASE/api/v1/orders/preview")
if [ "$code" != "200" ]; then
    echo "试算返回 $code，期望 200：" >&2; cat "$order_file" >&2; echo >&2; exit 1
fi
echo "    应付 $(python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["payable_cents"])' "$order_file") 分"

# 幂等键每次跑都换一把。同一把键重放会返回 201 + Idempotency-Replayed: true
# 而**不扣库存** —— 那样下面的水位断言会红在幂等上，而不是红在它要守的事情上。
idem="smoke-$(date +%s%N)"
echo "==> POST $BASE/api/v1/orders 下单（Idempotency-Key: $idem）"
code=$(curl "${curl_args[@]}" "${AUTH[@]}" -o "$order_file" -w '%{http_code}' \
    -H 'Content-Type: application/json' -H "Idempotency-Key: $idem" \
    -d "$order_body" "$BASE/api/v1/orders")
if [ "$code" != "201" ]; then
    echo "下单返回 $code，期望 201：" >&2; cat "$order_file" >&2; echo >&2; exit 1
fi
order_no=$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["order_no"])' "$order_file")
order_status=$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["status"])' "$order_file")
if [ "$order_status" != "10" ]; then
    echo "新建订单状态是 $order_status，期望 10 待支付" >&2; exit 1
fi
echo "    order_no=$order_no status=10"

# 库存真的少了 —— 这一条证明 SAGA 的库存分支在**这个镜像里**真的跑起来了。
# 协调器起不来、分支没注册上、libdtmrs.so 链接坏了，都会红在这里。
curl "${curl_args[@]}" "${AUTH[@]}" -s -o "$detail_file" "$BASE/api/v1/products/$product_id"
stock_after=$(python3 "$SCRIPT_DIR/smoke_pick_sku.py" "$detail_file" "$sku_id")
if [ "$stock_after" != "$((stock_before - 1))" ]; then
    echo "下单后水位是 $stock_after，期望 $((stock_before - 1)) —— SAGA 的库存分支没扣" >&2
    exit 1
fi
echo "    库存 $stock_before → $stock_after"

echo "==> POST $BASE/api/v1/orders/$order_no/payments 发起支付（沙箱）"
code=$(curl "${curl_args[@]}" "${AUTH[@]}" -o "$intent_file" -w '%{http_code}' \
    -H 'Content-Type: application/json' -H "Idempotency-Key: pay-$idem" \
    -d '{"channel":"wechat"}' "$BASE/api/v1/orders/$order_no/payments")
if [ "$code" != "201" ]; then
    echo "发起支付返回 $code，期望 201（沙箱被关掉的话是 501）：" >&2
    cat "$intent_file" >&2; echo >&2; exit 1
fi

# 沙箱交出来的是一份签好名的回调报文，不是一条捷径。把它原样投回去，走的是
# 真实渠道回调**完全相同**的那条路：验签、金额校验、uk_payments_channel_txn
# 幂等、SettleOrder 里 status = 10 那个与超时补偿撞车时的唯一裁判。
read -r settle_url settle_sig < <(python3 "$SCRIPT_DIR/smoke_settle.py" head "$intent_file") \
    || { echo "发起支付的响应里没有可用的沙箱结算指令：" >&2; cat "$intent_file" >&2; echo >&2; exit 1; }
settle_body=$(python3 "$SCRIPT_DIR/smoke_settle.py" body "$intent_file")

echo "==> POST $settle_url 把签好名的回调原样投回去"
code=$(curl "${curl_args[@]}" -o /dev/null -w '%{http_code}' \
    -H 'Content-Type: application/json' -H "X-Keel-Signature: $settle_sig" \
    -d "$settle_body" "$BASE$settle_url")
if [ "$code" != "200" ]; then
    echo "回调返回 $code，期望 200 —— 验签或入账路径断了" >&2; exit 1
fi

curl "${curl_args[@]}" "${AUTH[@]}" -s -o "$order_file" "$BASE/api/v1/orders/$order_no"
paid_status=$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["status"])' "$order_file")
if [ "$paid_status" != "20" ]; then
    echo "回调之后订单状态是 $paid_status，期望 20 已支付：" >&2; cat "$order_file" >&2; echo >&2; exit 1
fi
echo "    订单 $order_no 已支付"

# ---------------------------------------------------------------------------
# 混合检索（M3 Task 4）
# ---------------------------------------------------------------------------
#
# 理由与上面下单那一段完全一样：检索在进程外一个闸门都没有。
# 迁移里 search_vector 的生成表达式写错了、种子里 search_text 忘了写、
# /search 没挂进 v1 组、RLS 把本店的数据也一起挡了 —— 以上任何一条成立，
# 全部单元测试照样绿，而 M3 的产出标志「自然语言搜索可用」在镜像里是假的。
#
# 搜的是种子里真有的词，断言命中的是**对的**那件商品，而且不相干的那件
# **排在它后面** —— 只断言非空的话，「把全店商品原样倒出来」照样绿。
#
# 为什么是「排在后面」而不是「不许出现」：召回层刻意没有相似度阈值，
# 理由与实测数字写在 scripts/smoke_search.py 的文件头。
#
# 它是公开接口（契约里 security: []），所以这里刻意**不带令牌**：
# 带上的话，「不登录也搜得到」这件事就没有靶子了。
echo "==> POST $BASE/api/v1/search 搜「连衣裙」"
search_file=$(mktemp)
trap 'rm -f "$body_file" "$login_file" "$detail_file" "$order_file" "$intent_file" "$search_file"' EXIT

code=$(curl "${curl_args[@]}" -o "$search_file" -w '%{http_code}' \
    -H 'Content-Type: application/json' \
    -d '{"query":"连衣裙","explain":true}' "$BASE/api/v1/search")
if [ "$code" != "200" ]; then
    echo "检索返回 $code，期望 200：" >&2; cat "$search_file" >&2; echo >&2; exit 1
fi

# 期望命中「雪纺碎花连衣裙」，而「手冲咖啡壶」必须排在它后面。
read -r hits rank source strategy < <(python3 "$SCRIPT_DIR/smoke_search.py" \
    "$search_file" '雪纺碎花连衣裙' '手冲咖啡壶') \
    || { echo "检索结果不对：" >&2; cat "$search_file" >&2; echo >&2; exit 1; }
echo "    $hits 条，「雪纺碎花连衣裙」排第 $rank（recall_source=$source，strategy=$strategy）"

# 反例：一串切不出任何词的东西必须被拒（422），不是 200 + 空列表。
# 没有这一条，上面那个 200 既可能是检索真的跑了，也可能是这条路由
# 对什么输入都回 200。
code=$(curl "${curl_args[@]}" -o /dev/null -w '%{http_code}' \
    -H 'Content-Type: application/json' \
    -d '{"query":"，。；"}' "$BASE/api/v1/search")
if [ "$code" != "422" ]; then
    echo "搜一串标点返回 $code，期望 422 —— 「这串东西搜不了」与「这家店没有」是两件事" >&2
    exit 1
fi
echo "    切不出词的查询被拒（422）"

echo "==> POST $BASE/api/v1/auth/logout 带上刚拿到的令牌"
code=$(curl "${curl_args[@]}" -o /dev/null -w '%{http_code}' -X POST \
    -H "Authorization: Bearer $token" "$BASE/api/v1/auth/logout")
if [ "$code" != "204" ]; then
    echo "退出登录返回 $code，期望 204 —— bearer 中间件或会话吊销没接上" >&2
    exit 1
fi

# 反例：不带令牌必须 401。没有这一条，上面那个 204 既可能是鉴权通过，
# 也可能是这条路由压根没挂中间件。
code=$(curl "${curl_args[@]}" -o /dev/null -w '%{http_code}' -X POST \
    "$BASE/api/v1/auth/logout")
if [ "$code" != "401" ]; then
    echo "不带令牌调 logout 返回 $code，期望 401 —— bearer 中间件没挂上" >&2
    exit 1
fi
echo "    令牌可用，且不带令牌会被拒"

echo "全部通过。"
