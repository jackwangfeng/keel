#!/usr/bin/env bash
# 多商家形态栈**唯一**的入口。
#
# 为什么单独一个脚本：`compose.multi.yaml` 从 M1 起就在仓库里，但没有任何东西跑过它 ——
# CI 的 e2e job 只起默认的单商家形态（`.github/workflows/ci.yml` 的注释自己写着
# 「多商家形态……e2e 只跑默认的单商家」；2026-10-07 之前 Makefile 里也没有 target），演示站与生产栈
# 都必填 `KEEL_DEFAULT_MERCHANT`。于是「开店 / 切店 / 停用」这条租户管理的路径
# 从来没有端到端的证据，只有单测。脚本 + `make multi-up` + CI 一步是为了把它接上。
#
# 它**必须**在起完之后跑一次验收：`docker compose up -d` 只等到容器启动，
# 应用随后因 `tenant.Preflight` 不过而退出，它照样返回 0（同一个坑写在 ci.yml 里）。
#
# 用法：
#   multi-up.sh              启动（复用已有镜像）+ 验收（scripts/multi-verify.sh）
#   multi-up.sh --no-verify  只启动，不验收
#   multi-up.sh --build      强制重新构建镜像后再启动
#   multi-up.sh --down       停掉，数据卷保留
#   multi-up.sh --wipe       停掉并删数据卷（拒绝碰演示栈的项目名）
#   multi-up.sh --config     只打印合成后的 compose 配置
#   multi-up.sh --logs       跟 app 日志（bootstrap token 在里面）
set -euo pipefail

cd "$(dirname "$0")/.."

# 项目名单独一个：容器名与卷名都由它派生。用默认的 `keel` 会怎样 ——
# compose.multi.yaml 只把**卷**加了 `_multi` 后缀，容器名仍是 keel-app-1 那几个，
# 和单商家开发栈同名。所以在同一个项目名下叠这个文件，实际是把人家正在跑的
# 那一栈原地换成了多商家形态，而不是并存两套。
export COMPOSE_PROJECT_NAME="${COMPOSE_PROJECT_NAME:-keelmulti}"

# 8080 / 8081 在这台开发机上都被占着（8080 = 另一个容器的 80）。
# 撞端口的症状很难看：compose 报 address already in use，但换一个端口手抖只改了
# 一边，于是验收打的是别的服务。所以端口只在这里定义一次，compose 与 verify 读同一个变量。
export KEEL_HTTP_PORT="${KEEL_HTTP_PORT:-28185}"
export KEEL_CONSOLE_PORT="${KEEL_CONSOLE_PORT:-28186}"

# smoke.sh 与 verify 都按这个 Host 打；多商家形态下不带 Host 解析不出任何店。
export KEEL_SMOKE_HOST="${KEEL_SMOKE_HOST:-shop-a.example.com}"

# proxy.golang.org 在本机与国内网络不通（dial i/o timeout，构建卡在 go mod download）。
# 与 scripts/demo-up.sh 同一个处理，理由见 PROGRESS.md 的「四个坑」③。
export GOPROXY="${GOPROXY:-https://goproxy.cn,direct}"

DC=(docker compose -p "$COMPOSE_PROJECT_NAME" -f compose.yaml -f compose.multi.yaml)

# 端口被占就先停下，别等到 compose 报 address already in use。
# 那条报错的指向性很差：它说的是端口，而人会先去查应用。
check_port() {
    if ss -ltn "sport = :$1" 2>/dev/null | tail -n +2 | grep -q .; then
        echo "宿主机端口 $1 已被占用（COMPOSE_PROJECT_NAME=$COMPOSE_PROJECT_NAME）。" >&2
        echo "换一个：KEEL_HTTP_PORT=xxxxx KEEL_CONSOLE_PORT=xxxxx $0" >&2
        exit 1
    fi
}

mode="up"
build=""
verify=1
for arg in "$@"; do
    case "$arg" in
        --build)     build="--build" ;;
        --no-verify) verify=0 ;;
        --down)      mode="down" ;;
        --wipe)      mode="wipe" ;;
        --config)    mode="config" ;;
        --logs)      mode="logs" ;;
        -h|--help)   sed -n '2,26p' "$0"; exit 0 ;;
        *)           echo "未知参数：$arg（见 --help）" >&2; exit 2 ;;
    esac
done

case "$mode" in
    config)
        exec "${DC[@]}" config
        ;;
    logs)
        exec "${DC[@]}" logs -f app
        ;;
    down)
        exec "${DC[@]}" down
        ;;
    wipe)
        # -v 会删数据卷，而删错的代价是整库没了（2026-10-06 演示库被清过一次，
        # 教训是「只在结构上不可能发生的位置放破坏性快捷方式」）。这一栈的卷由
        # 项目名派生，所以唯一的防线是：项目名一看就属于别的栈就拒绝。
        case "$COMPOSE_PROJECT_NAME" in
            keelmulti*|keelci*) ;;
            *)
                echo "拒绝删卷：COMPOSE_PROJECT_NAME=$COMPOSE_PROJECT_NAME 不是多商家测试栈的项目名。" >&2
                echo "这个脚本的 --wipe 只删自己建的卷（${COMPOSE_PROJECT_NAME}_*_multi）。" >&2
                exit 1
                ;;
        esac
        exec "${DC[@]}" down -v --remove-orphans
        ;;
    up)
        check_port "$KEEL_HTTP_PORT"
        check_port "$KEEL_CONSOLE_PORT"
        "${DC[@]}" up -d ${build:+--build}
        ;;
esac

if [ "$verify" = "1" ]; then
    echo
    exec ./scripts/multi-verify.sh
fi
