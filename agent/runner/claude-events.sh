#!/usr/bin/env bash
# Keel AI 员工的参考运行器：被事件唤醒（agent/skills/事件处理.md），而不是每天定时全量巡一遍。
#
#   KEEL_AGENT_KEY=kagt_… KEEL_MCP_URL=https://<店铺域名>/api/v1/mcp agent/runner/claude-events.sh
#
# cron 每 10 分钟跑一次即可（演示站）：
#
#   */10 * * * * KEEL_AGENT_KEY=kagt_… KEEL_MCP_URL=https://eshop.zzss.fun/api/v1/mcp \
#     /path/to/agent/runner/claude-events.sh >>/var/log/keel-events.log 2>&1
#
# 没有新事件就安静退出（exit 0）：不占配额、不留噪声。有事件才拉起 Claude Code。
# 如果 AI 员工配了 webhook（docs/AI接口.md「事件」一节），收到推送后直接跑同一条 PROMPT 即可，
# 不需要这个轮询脚本——但推送本身不含数据，唤醒后照样要靠这里下面的 list_events 去拉。
set -euo pipefail
cd "$(dirname "$0")/.."            # agent/：Claude Code 从这里读 CLAUDE.md（→ AGENTS.md）
: "${KEEL_AGENT_KEY:?要设 KEEL_AGENT_KEY（后台「AI 员工与密钥」发的 kagt_ 密钥）}"
: "${KEEL_MCP_URL:?要设 KEEL_MCP_URL，例如 https://eshop.zzss.fun/api/v1/mcp}"
CLAUDE=${CLAUDE:-claude}

# 先自检密钥：失败就别唤醒 agent（whoami 在 MCP 同一个前缀下）。
WHOAMI_URL="${KEEL_MCP_URL%/mcp}/agent/whoami"
if ! curl -fsS -m 20 -H "Authorization: Bearer $KEEL_AGENT_KEY" "$WHOAMI_URL" >/dev/null; then
  echo "接入密钥自检失败（$WHOAMI_URL），本轮不处理事件" >&2
  exit 1
fi

# 裸打一次 JSON-RPC 探探有没有新事件，不经 claude ——省 token、省一次子进程。
#
# MCP 服务是无状态的（internal/handler/mcp.go：Stateless:true, JSONResponse:true）：不需要先
# initialize 握手。go-sdk 的 streamable handler（mcp/streamable.go 的 ephemeralConnectOpts）在
# 「旧协议」请求——也就是不带 MCP-Protocol-Version: 2026-07-28 或更新、请求体里也没有单独的
# initialize 消息——时，会给这次临时会话自动合成 InitializeParams / InitializedParams，直接放行
# 后续方法。所以一条 tools/call 请求就够；**千万别加 MCP-Protocol-Version 头**，加了会切到新协议，
# 反而要求 _meta 里带 clientCapabilities 三元组才认，一条裸请求会被拒。
# 响应因为 JSONResponse:true 是一整块 JSON（不是 SSE 流），当普通 HTTP JSON 解析即可。
PROBE=$(curl -fsS -m 20 -X POST "$KEEL_MCP_URL" \
  -H "Authorization: Bearer $KEEL_AGENT_KEY" \
  -H "Content-Type: application/json" \
  -H "Accept: application/json, text/event-stream" \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_events","arguments":{"limit":1}}}') \
  || { echo "list_events 探测失败，本轮不处理事件" >&2; exit 1; }

HAS_EVENTS=$(printf '%s' "$PROBE" | python3 -c '
import json, sys
try:
    resp = json.load(sys.stdin)
except Exception:
    print("0"); sys.exit()
result = resp.get("result") or {}
if result.get("isError"):
    # 工具报错（比如越权、内部错误）：当没有事件处理，留给下一轮，不拉起 claude 瞎猜。
    print("0"); sys.exit()
items = (result.get("structuredContent") or {}).get("items") or []
print("1" if items else "0")
')

if [ "$HAS_EVENTS" != "1" ]; then
  exit 0   # 没有新事件，安静退出
fi

CFG=$(mktemp)
trap 'rm -f "$CFG"' EXIT
python3 - "$KEEL_MCP_URL" "$KEEL_AGENT_KEY" > "$CFG" <<'PY'
import json, sys
url, key = sys.argv[1], sys.argv[2]
print(json.dumps({"mcpServers": {"keel": {"type": "http", "url": url, "headers": {"Authorization": "Bearer " + key}}}}))
PY
chmod 600 "$CFG"

NOW=$(date +"%Y-%m-%d %H:%M")
PROMPT="现在是 ${NOW}。有新事件在等你处理。按 skills/事件处理.md 来：list_events 拉取、按类型分派到对应手册、\
处理完用 ack_events 确认到最后一条；只有出现值得播报的情况才用 post_brief 写简报。只用 keel 这个 MCP 服务的工具。"

# 只放行 keel 的 MCP 工具与读本目录的手册：agent 不需要、也不该有 shell 与改文件的能力。
"$CLAUDE" -p "$PROMPT" \
  --mcp-config "$CFG" --strict-mcp-config \
  --allowedTools "mcp__keel__*" "Read" \
  --output-format text
