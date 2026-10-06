#!/usr/bin/env bash
# 上线前自查。每一项都对应一个**已经有人踩过**的故障，输出写成「症状 → 原因」的形式，
# 因为多数项的报错信息（连不上、401、全站 404）都不指向真正的原因。
#
# 退出码 0 = 可以起栈；非 0 = 有阻断项。

set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ENV_FILE="$ROOT/.env.prod"

fail=0
ok()   { printf '  \033[32mok\033[0m    %s\n' "$*"; }
warn() { printf '  \033[33m注意\033[0m  %s\n' "$*"; }
bad()  { printf '  \033[31m缺\033[0m    %s\n' "$*"; fail=1; }

echo "== 1. Docker =="
if ! command -v docker > /dev/null 2>&1; then
	bad "没装 docker"
elif ! docker info > /dev/null 2>&1; then
	bad "docker 装了但 daemon 连不上（权限不足，或者服务没起）"
else
	ok "docker 可用（$(docker version -f '{{.Server.Version}}' 2> /dev/null)）"
fi

echo "== 2. .env.prod =="
if [ ! -f "$ENV_FILE" ]; then
	bad "没有 .env.prod —— 先跑 make init"
	echo
	echo "还缺的话现在什么都查不了，先 make init。"
	exit 1
fi
# shellcheck disable=SC1090
set -a; . "$ENV_FILE"; set +a
ok ".env.prod 存在（权限 $(stat -c '%a' "$ENV_FILE")）"

echo "== 3. 必填项 =="
for k in KEEL_ADMIN_PASSWORD KEEL_APP_PASSWORD KEEL_AUTH_SECRET KEEL_DEFAULT_MERCHANT KEEL_MERCHANT_NAME; do
	if [ -z "${!k:-}" ]; then
		bad "$k 是空的"
	fi
done

# 演示值还没改，是最常见的一种：compose 一路绿灯，起来之后就是个演示店。
for k in KEEL_ADMIN_PASSWORD KEEL_APP_PASSWORD; do
	case "${!k:-}" in
		keel|keel_app|postgres|password|changeme)
			bad "$k 还是默认值（${!k}）—— 这正是公开在仓库里的那个"
			;;
	esac
done

if [ -n "${KEEL_DEFAULT_MERCHANT:-}" ] && ! printf '%s' "$KEEL_DEFAULT_MERCHANT" | grep -qE '^[a-z0-9][a-z0-9_-]*$'; then
	bad "KEEL_DEFAULT_MERCHANT='$KEEL_DEFAULT_MERCHANT' 不是小写字母数字 —— 它会直接当 URL 片段用"
fi

if [ "$(printf '%s' "${KEEL_AUTH_SECRET:-}" | wc -c)" -lt 32 ]; then
	bad "KEEL_AUTH_SECRET 短于 32 字符（HMAC 密钥至少要 32 字节，用 openssl rand -hex 32）"
fi

echo "== 4. GOPROXY 连通性 =="
# 为什么要单独查：Dockerfile 第 4 步是 go mod download，走的就是 GOPROXY。
# 官方 proxy.golang.org 在很多网络里连不上，症状是构建失败在那一层，
# 而错误只有一行 dial i/o timeout —— 不知道的人会以为是 Dockerfile 写错了。
if [ -n "${KEEL_IMAGE_TAG:-}" ]; then
	ok "设了 KEEL_IMAGE_TAG=$KEEL_IMAGE_TAG，走预构建镜像，不用构建时拉 module（跳过这项）"
	# 实测坑：包本身是公开的（匿名 REST 能读 manifest 和 blob），但 docker daemon
	# 不会发匿名 token，不 docker login 就是
	#   denied: denied（而且它报的是 Head ... denied，不提登录这件事，很容易被
	# 当成权限或网络问题去找错方向）。所以这里替客户先试一次。
	img="ghcr.io/jackwangfeng/keel:$KEEL_IMAGE_TAG"
	if timeout 25 docker manifest inspect "$img" > /dev/null 2>&1; then
		ok "$img 可拉取"
	else
		bad "拉不到 $img —— make release-up 会报 denied: denied"
		if grep -qs ghcr.io "${DOCKER_CONFIG:-$HOME/.docker}/config.json"; then
			echo "        已登录 ghcr.io，所以不是登录问题：这个 tag 可能不存在"
			echo "        已发布的 tag 见 https://github.com/jackwangfeng/keel/packages"
		else
			echo "        没登录 ghcr.io。登录："
			echo "          echo \"\$GITHUB_TOKEN\" | docker login ghcr.io -u <账号> --password-stdin"
			echo "        令牌要带 packages:read（gh auth token 拿到的 oauth 令牌没有这个 scope）"
			echo "        或者清空 KEEL_IMAGE_TAG 改走本地构建（慢，但不需登录）"
		fi
	fi
else
	proxy="${GOPROXY:-https://proxy.golang.org,direct}"
	host="$(printf '%s' "$proxy" | cut -d, -f1 | cut -d/ -f3)"
	if curl -s -o /dev/null -m 8 "https://${host}/" 2> /dev/null; then
		ok "$host 可达"
	else
		bad "$host 不可达 —— 构建会在 go mod download 那一步失败"
		echo "        国内网络填 GOPROXY=https://goproxy.cn,direct（.env 里改）"
		echo "        或者设 KEEL_IMAGE_TAG 走预构建镜像，见 compose.release.yaml"
	fi
fi

echo "== 5. Compose 解析 =="
if docker compose --env-file "$ENV_FILE" -f "$ROOT/compose.yaml" -f "$ROOT/compose.prod.yaml" config > /dev/null 2> /tmp/keel-doctor.$$; then
	ok "compose.prod.yaml 解析通过"
else
	bad "compose 解析失败："
	sed 's/^/        /' /tmp/keel-doctor.$$
fi
rm -f /tmp/keel-doctor.$$

echo "== 6. 端口 =="
for v in KEEL_HTTP_PORT:app KEEL_CONSOLE_PORT:console; do
	var="${v%%:*}"; who="${v##*:}"
	port="${!var:-}"
	[ -z "$port" ] && continue
	if ss -ltn 2> /dev/null | grep -qE "[:.]${port}[[:space:]]"; then
		bad "端口 $port 已被占用（$who 要用），改 .env 里的 $var"
	else
		ok "$who 端口 $port 空闲"
	fi
done

echo "== 7. 存量数据的两个坑 =="
# 数据卷在不在，决定下面两条是「现在是问题」还是「将来会咬你」。
if docker volume ls --format '{{.Name}}' 2> /dev/null | grep -qE '(^|_)keel_pgdata'; then
	warn "检测到已存在的 pgdata 卷 —— KEEL_ADMIN_PASSWORD 现在改已经**不生效**了。"
	warn "  PostgreSQL 只在 initdb 时读一次那个值。要换口令得执行："
	warn "    docker compose -p keel exec postgres psql -U keel -c \"ALTER ROLE keel PASSWORD '新口令'\""
	warn "  改完同步改 .env，否则 migrate 会报「密码错误」而真因是卷还是老的。"
else
	ok "还没有数据卷，KEEL_ADMIN_PASSWORD 会按 .env 的值生效"
fi

warn "KEEL_APP_PASSWORD 只在 keel_app 角色**首次创建**时生效（db/migrations/00003 的论证）。"
warn "  以后要轮换：ALTER ROLE keel_app PASSWORD '...'，再同步改 .env.prod。"

echo
# 演示栈会不会被生产口令污染。裸 docker compose up 会自动读项目根的 .env，
# 而生产口令只该在 .env.prod 里——项目根有 .env 的话，演示栈的 migrate 会拿
# 生产口令去连演示库，报 password authentication failed，而真因是两个栈
# 曾经共用 .env 这一个文件名。2026-10-06 真踩过：演示库整个 schema 被清空重建过。
if [ -e "$ROOT/.env" ]; then
	warn "项目根有 .env —— 裸 docker compose up（演示栈）会自动读它。"
	echo "        生产口令只该放 .env.prod（make init 生成的就是那个）。"
	echo "        症状：演示栈 migrate 报 password authentication failed，"
	echo "        而真因是它拿到了生产口令去连演示库。删掉 .env 或改名成 .env.prod。"
fi

if [ "$fail" -eq 0 ]; then
	echo "没有阻断项。make prod-up 起栈；起完用 make prod-logs 看 app 打出来的 bootstrap token。"
else
	echo "有阻断项，先改完再起栈。上面每条都写了对应的故障症状。"
fi
exit "$fail"
