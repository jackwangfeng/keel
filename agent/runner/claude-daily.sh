#!/usr/bin/env bash
# Keel AI 员工的参考运行器：用 Claude Code 的无头模式做一次每日巡店（agent/skills/巡店日报.md）。
#
#   KEEL_AGENT_KEY=kagt_… KEEL_MCP_URL=https://<店铺域名>/api/v1/mcp agent/runner/claude-daily.sh
#
# 放进 cron 每天跑一次即可（演示站：每天 08:00）。换别的 harness 只需要换这个脚本：
# 工具在 MCP 上，手册是 agent/ 下的 Markdown，与 harness 无关。
set -euo pipefail
cd "$(dirname "$0")/.."            # agent/：Claude Code 从这里读 CLAUDE.md（→ AGENTS.md）
: "${KEEL_AGENT_KEY:?要设 KEEL_AGENT_KEY（后台「AI 员工与密钥」发的 kagt_ 密钥）}"
: "${KEEL_MCP_URL:?要设 KEEL_MCP_URL，例如 https://eshop.zzss.fun/api/v1/mcp}"
CLAUDE=${CLAUDE:-claude}

# 先自检密钥：失败就别唤醒 agent（whoami 在 MCP 同一个前缀下）。
WHOAMI_URL="${KEEL_MCP_URL%/mcp}/agent/whoami"
if ! curl -fsS -m 20 -H "Authorization: Bearer $KEEL_AGENT_KEY" "$WHOAMI_URL" >/dev/null; then
  echo "接入密钥自检失败（$WHOAMI_URL），今天不巡店" >&2
  exit 1
fi

CFG=$(mktemp)
trap 'rm -f "$CFG"' EXIT
python3 - "$KEEL_MCP_URL" "$KEEL_AGENT_KEY" > "$CFG" <<'PY'
import json, sys
url, key = sys.argv[1], sys.argv[2]
print(json.dumps({"mcpServers": {"keel": {"type": "http", "url": url, "headers": {"Authorization": "Bearer " + key}}}}))
PY
chmod 600 "$CFG"

TODAY=$(date +%Y-%m-%d)
PROMPT="今天是 ${TODAY}。按 skills/巡店日报.md 做今天的巡店：补货部分按 skills/补货.md。只用 keel 这个 MCP 服务的工具，最后用 post_brief 写简报。"

# 只放行 keel 的 MCP 工具与读本目录的手册：agent 不需要、也不该有 shell 与改文件的能力。
"$CLAUDE" -p "$PROMPT" \
  --mcp-config "$CFG" --strict-mcp-config \
  --allowedTools "mcp__keel__*" "Read" \
  --output-format text
