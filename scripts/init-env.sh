#!/usr/bin/env bash
# 生成 .env。**不覆盖已有的** —— 里面是客户的库口令与签名密钥，被静默重置掉的话
# 症状是「重启后全体买家被登出」或者更坏的「连上了，但用的是旧密钥」。
#
# 三个密钥项自动生成随机值，剩下两项（商家 code 与店名）留空等客户填 ——
# 那两个没有唯一正确答案，替他编一个反而会让人以为可以就这么上线。

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ENV_FILE="$ROOT/.env.prod"
EXAMPLE="$ROOT/.env.prod.example"

[ -f "$EXAMPLE" ] || { echo "找不到 .env.prod.example，先确认仓库是完整的" >&2; exit 1; }

if [ -e "$ENV_FILE" ]; then
	echo ".env.prod 已经存在，不动它。要重新生成请先自己备份或删掉。"
	exit 0
fi

# 十六进制而不是 base64：口令会经过 compose 的 ${...} 插值与 .env 解析，
# '/' '+' '=' 在那条路上各有各的麻烦，而 hex 只有 0-9a-f，一层编码都不用想。
gen() { openssl rand -hex "$1"; }

ADMIN_PW="$(gen 16)"
APP_PW="$(gen 16)"
AUTH_SECRET="$(gen 32)"

sed -e "s|^KEEL_ADMIN_PASSWORD=.*|KEEL_ADMIN_PASSWORD=$ADMIN_PW|" \
    -e "s|^KEEL_APP_PASSWORD=.*|KEEL_APP_PASSWORD=$APP_PW|" \
    -e "s|^KEEL_AUTH_SECRET=.*|KEEL_AUTH_SECRET=$AUTH_SECRET|" \
    "$EXAMPLE" > "$ENV_FILE"

# .env.prod 里有库口令和 HMAC 密钥，权限收紧。umask 挡不住显式 chmod 之后的窄权限。
chmod 600 "$ENV_FILE"

cat <<EOF
已生成 .env.prod（权限 600），三个密钥项已填随机值。

还需要你填两项，它们决定「这家店是谁」：

  KEEL_DEFAULT_MERCHANT=   商家 code，小写字母数字，例如 nanshan
  KEEL_MERCHANT_NAME=      店名，例如 南山便利店

改完跑 \`make doctor\` 自查，再 \`make prod-up\` 起栈。
EOF
