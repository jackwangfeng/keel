#!/usr/bin/env bash
# CI 用本机的 Go 工具链，不用 actions/setup-go。检查版本够不够，并锁住「不许自动下载」。
#
# ## 为什么不用 actions/setup-go
#
# 实测（v0.1.0 那一轮）：自建 runner（lenserver，在国内）上，setup-go 这一步
# **每个 job 都要吃掉 6～7 分钟**，比整个全量测试还慢：
#
#   CI 闸门        Run  setup-go  405 s   （开头：找工具链 + 下载依赖缓存）
#   CI Go 测试     Run  setup-go  405 s
#   发布 verify    Post setup-go  386 s   （结尾：把依赖缓存上传回 GitHub）
#
# 两个 405 秒一模一样，像是下载超时之后才落到别的路上。它慢在两件事：
#   1. 它不认本机 /usr/local/go 里已经装好的 Go，要在自己的工具缓存里找，
#      找不到就从 Go 的官方源下载一份 —— 那个源在国内很慢。
#   2. 它默认开着依赖缓存：开头从 GitHub 的缓存服务下载一个几百 MB 的包，
#      结尾再上传回去。**在自建 runner 上这纯属负收益**：同一台机器，
#      GOMODCACHE 本来就一直留在盘上，GOPROXY 配的是国内的 goproxy.cn。
#
# 托管 runner 每次都是新虚拟机，setup-go 是对的；自建 runner 不是。
#
# ## 这个脚本做什么
#
#   · 本机没有 go → 失败，并说清要在 runner 上装什么版本。
#   · 本机 go 低于 go.mod（以及 tools/go.mod）的 go 指令 → 失败。**不自动下载**：
#     GOTOOLCHAIN=local 写进后续所有步骤的环境，Go 不会因为版本不够而悄悄去拉
#     一个新工具链 —— 那正是上面那 405 秒的来路之一，而且失败时报错不指向真因。
#   · 版本从 go.mod 读，不写字面量：tools/ 里钉住的 sqlc 与 goose 都声明 go 1.26，
#     下限由它们传染而来（docker/Dockerfile 与 CONTRIBUTING 都记了这件事）。
#     写死一个数字，将来 go.mod 提上去而这里忘了跟，报出来的会是一句和真因无关的错。
set -euo pipefail

cd "$(dirname "$0")/.."

if ! command -v go >/dev/null 2>&1; then
    echo "::error::runner 上没有 go。CI 用本机工具链（见本脚本文件头），请在 runner 上装 Go $(awk '/^go /{print $2}' go.mod) 或更高版本。"
    exit 1
fi

have=$(go env GOVERSION | sed 's/^go//')
for mod in go.mod tools/go.mod; do
    want=$(awk '/^go /{print $2; exit}' "$mod")
    lowest=$(printf '%s\n%s\n' "$want" "$have" | sort -V | head -1)
    if [ "$lowest" != "$want" ]; then
        echo "::error::runner 上的 Go 是 $have，而 $mod 要求 $want。请升级 runner 上的 Go —— 这里不会自动下载（GOTOOLCHAIN=local）。"
        exit 1
    fi
done

# 让后续所有步骤都不自动下载工具链。不在 Actions 里跑（本地手动执行）时没有这个文件，跳过。
if [ -n "${GITHUB_ENV:-}" ]; then
    echo "GOTOOLCHAIN=local" >> "$GITHUB_ENV"
fi

echo "Go $have（本机 $(command -v go)），满足 go.mod $(awk '/^go /{print $2; exit}' go.mod)"
echo "GOMODCACHE=$(go env GOMODCACHE)"
echo "GOPROXY=$(go env GOPROXY)"
