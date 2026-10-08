#!/usr/bin/env bash
# 运维签发后台一次性登录 token（破损恢复入口）。
#
# 会话全过期、又没人能点「重签」时用这条。它走维护库（与 migrate 同一角色），
# **不经过 HTTP、不进 app 日志**。明文只打 stdout。
#
# 用法：
#   scripts/issue-login-token.sh -staff 1
#   scripts/issue-login-token.sh -email platform-verify@keel.invalid
#   scripts/issue-login-token.sh -email boss@x.com -merchant shop-a
#
# 指到某套 compose 栈的库（不设则用当前 shell 的 PG* / KEEL_ADMIN_*）：
#   COMPOSE_PROJECT_NAME=keelmulti scripts/issue-login-token.sh -staff 1
#   COMPOSE_PROJECT_NAME=keeldemo  scripts/issue-login-token.sh -staff 4
#
# Makefile：make issue-login STAFF=1 / make multi-login / make demo-login
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

if [ -n "${COMPOSE_PROJECT_NAME:-}" ]; then
	pg="${COMPOSE_PROJECT_NAME}-postgres-1"
	if ! docker inspect "$pg" >/dev/null 2>&1; then
		echo "找不到容器 $pg（栈没起？项目名不对？）" >&2
		exit 1
	fi
	# 容器网 IP：宿主机连得上，且不用把 5432 暴露到公网。
	ip="$(docker inspect -f '{{range.NetworkSettings.Networks}}{{.IPAddress}}{{end}}' "$pg")"
	if [ -z "$ip" ]; then
		echo "读不到 $pg 的 IP" >&2
		exit 1
	fi
	export PGHOST="$ip"
	export PGPORT=5432
	export PGDATABASE="${PGDATABASE:-keel}"
	export KEEL_ADMIN_USER="${KEEL_ADMIN_USER:-keel}"
	export KEEL_ADMIN_PASSWORD="${KEEL_ADMIN_PASSWORD:-keel}"
	echo "连 $pg（$PGHOST）维护库" >&2
fi

exec go run ./cmd/keel-admin issue-login "$@"
