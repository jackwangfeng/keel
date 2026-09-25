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

echo "全部通过。"
