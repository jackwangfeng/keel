#!/usr/bin/env bash
# 演示栈**唯一**的部署入口。
#
# 为什么要单独一个脚本：演示栈要叠 5 个 compose 文件、20 多个环境变量，
# 散在 ~/.local/share/keel-eshop/demo-env.sh 里。凭记忆手敲一遍 export 必然漏——
# 2026-10-06 就漏过一次：用只叠 2 个 compose 的简化命令重建，丢了
# compose.split.yaml / compose.s3.yaml / compose.demo-split.yaml 和一批变量，
# 结果地图瓦片、S3 商品图、渠道层三处同时坏掉，而 api / console 全是 200。
#
# 所以规则只有一条：**任何时候都不手写启动命令，一律走这个脚本。**
# 要改配置改 demo-env.sh，不要改这里。
#
# 用法：
#   demo-up.sh              启动（按需构建）
#   demo-up.sh --build      重新构建镜像后启动
#   demo-up.sh --pull       拉 ghcr 镜像后启动（走 compose.release.yaml）
#   demo-up.sh --down       停掉
#   demo-up.sh --config     只打印解析后的 compose 配置，不启动
#   demo-up.sh --env        只打印这份 env 里生效的关键变量（排查用）
set -euo pipefail

PROJECT_DIR="${KEEL_PROJECT_DIR:-$HOME/work/keel}"
DEMO_ENV="${KEEL_DEMO_ENV:-$HOME/.local/share/keel-eshop/demo-env.sh}"

if [ ! -r "$DEMO_ENV" ]; then
    echo "找不到演示栈环境文件：$DEMO_ENV" >&2
    exit 1
fi
if [ ! -f "$PROJECT_DIR/compose.yaml" ]; then
    echo "找不到 compose.yaml：$PROJECT_DIR" >&2
    exit 1
fi

cd "$PROJECT_DIR"

# 环境变量全部来自这一个文件，compose 文件清单也在里面（DC 数组）。
# shellcheck source=/dev/null
. "$DEMO_ENV"

if [ "${KEEL_GOPROXY_EXPORT:-}" = "" ]; then
    # proxy.golang.org 在国内不通，构建会卡在 Dockerfile 的 go mod download。
    # 演示栈每次构建都用到，所以在这里强制，不依赖调用者的 shell。
    export GOPROXY="${GOPROXY:-https://goproxy.cn,direct}"
fi

mode="up"
extra=()
for arg in "$@"; do
    case "$arg" in
        --build)  extra+=(--build) ;;
        --pull)   extra+=(--pull) ;;
        --down)   mode="down" ;;
        --config) mode="config" ;;
        --env)    mode="env" ;;
        -h|--help)
            sed -n '2,25p' "$0"
            exit 0
            ;;
        *)
            echo "未知参数：$arg（见 --help）" >&2
            exit 2
            ;;
    esac
done

case "$mode" in
    env)
        # 故意不打印密钥本身，只报「设没设」和长度。排查时真正要的是"少了哪一项"。
        for v in COMPOSE_PROJECT_NAME KEEL_HTTP_PORT KEEL_CONSOLE_PORT \
                 KEEL_AUTH_SECRET KEEL_INTERNAL_SECRET KEEL_LOGIN_LOCK_EXEMPT_PHONES \
                 KEEL_GEO_PROVIDER KEEL_GEO_KEY \
                 KEEL_MAP_TILES KEEL_TILE_PROXY KEEL_TIANDITU_KEY \
                 KEEL_SYSTEMONE_ENDPOINT KEEL_SYSTEMONE_TOKEN \
                 KEEL_DTM_TOKEN KEEL_S3_ACCESS_KEY KEEL_S3_SECRET_KEY \
                 KEEL_CHANNELS KEEL_CHANNEL_DEMO; do
            val="${!v:-}"
            if [ -z "$val" ]; then
                printf '  %-28s 未设置\n' "$v"
            elif [[ "$v" == *SECRET || "$v" == *KEY || "$v" == *TOKEN || "$v" == *PASSWORD ]]; then
                printf '  %-28s 已设置（%d 字符）\n' "$v" "${#val}"
            else
                printf '  %-28s %s\n' "$v" "$val"
            fi
        done
        # DC 数组里是完整命令词（docker / compose / -f / 文件名...），共 12 个词，
        # 但**只数 -f 后面的文件名**才是 compose 文件数。数 ${#DC[@]} 会得到 12，
        # 报成「叠了 12 个 compose 文件」是错的（真实是 5 个），别再这么写。
        nfiles=0
        for ((i = 0; i < ${#DC[@]}; i++)); do
            [ "${DC[i]}" = "-f" ] && nfiles=$((nfiles + 1))
        done
        printf '\ncompose 文件（%d 个，DC 数组共 %d 个词）：\n' "$nfiles" "${#DC[@]}"
        for ((i = 0; i < ${#DC[@]}; i++)); do
            [ "${DC[i]}" = "-f" ] && printf '  %s\n' "${DC[i + 1]:-<未指定>}"
        done
        exit 0
        ;;
    config)
        "${DC[@]}" config "${extra[@]}"
        exit 0
        ;;
    down)
        "${DC[@]}" down
        exit 0
        ;;
    up)
        "${DC[@]}" up -d "${extra[@]}"
        ;;
esac

# 起完顺手做一次基线验收。健康不等于对——2026-10-06 那次三个功能坏掉时
# api 和 console 全是 200，是靠"图片能不能读出来"才发现的。
if [ -x scripts/verify-demo.sh ]; then
    echo
    exec scripts/verify-demo.sh
fi