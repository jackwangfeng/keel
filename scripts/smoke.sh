#!/usr/bin/env bash
# 冒烟：证明 `docker compose up` 起来的那一栈，从 HTTP 一路通到种子数据。
#
# 它断言的是链路，不是某个函数：迁移跑过了吗、种子加载了吗、租户解析配对了吗、
# RLS 下 keel_app 真读得到自己的商品吗 —— 这五件事里任何一件断掉，
# 下面那个非空断言就红。单元测试一条都盖不住它们，因为它们全在进程之外。
#
# 退出码 0 表示链路通。
set -euo pipefail

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
