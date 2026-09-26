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
#   1. 它要在自己的工具缓存里找指定版本，找不到就从 Go 的官方源下载一份
#      —— 那个源在国内很慢。（lenserver 的 /usr/local/go 是 1.25.6，不够用；
#      1.26.0 就在工具缓存里，是以前 setup-go 下载的。）
#   2. 它默认开着依赖缓存：开头从 GitHub 的缓存服务下载一个几百 MB 的包，
#      结尾再上传回去。**在自建 runner 上这纯属负收益**：同一台机器，
#      GOMODCACHE 本来就一直留在盘上，缺的模块经 runner .env 里的本机代理拉。
#
# 托管 runner 每次都是新虚拟机，setup-go 是对的；自建 runner 不是。
#
# ## 这个脚本做什么
#
#   · 在本机已有的几份 Go 里挑第一个版本够 go.mod（以及 tools/go.mod）的，
#     放到 PATH 最前面（候选见下方正文）。
#   · 一份够的都没有 → 失败，列出找到的版本，说清要装什么。**不自动下载**：
#     GOTOOLCHAIN=local 写进后续所有步骤的环境，Go 不会因为版本不够而悄悄去拉
#     一个新工具链 —— 那正是上面那 405 秒的来路之一，而且失败时报错不指向真因。
#   · 版本从 go.mod 读，不写字面量：tools/ 里钉住的 sqlc 与 goose 都声明 go 1.26，
#     下限由它们传染而来（docker/Dockerfile 与 CONTRIBUTING 都记了这件事）。
#     写死一个数字，将来 go.mod 提上去而这里忘了跟，报出来的会是一句和真因无关的错。
set -euo pipefail

cd "$(dirname "$0")/.."

# 要求的版本：go.mod 与 tools/go.mod 里较高的那个。
want=$(for mod in go.mod tools/go.mod; do awk '/^go /{print $2; exit}' "$mod"; done | sort -V | tail -1)

# 候选工具链，按顺序挑第一个版本够的：
#   · PATH 里的 go
#   · /usr/local/go —— runner 以系统服务身份运行，PATH 取自 actions-runner/.path，
#     不含它（登录 shell 的 profile 不会被读）
#   · runner 的工具缓存（以前 setup-go 下载的就放在这）
#   · 模块缓存里 Go 自动切换时下载过的 golang.org/toolchain
#
# 每个候选都用 GOTOOLCHAIN=local 问版本。不加的话，go 在仓库目录里会按 go.mod
# 自动切到模块缓存里的新工具链，报的是切换后的版本：lenserver 上
# /usr/local/go 实际是 1.25.6，却自称 1.26.0。第一版脚本就这样放行了，
# 后续步骤一设 GOTOOLCHAIN=local 就报 "go.mod requires go >= 1.26.0
# (running go 1.25.6)"。
candidates=()
if command -v go >/dev/null 2>&1; then candidates+=("$(command -v go)"); fi
candidates+=(/usr/local/go/bin/go)
for d in "${RUNNER_TOOL_CACHE:-/nonexistent}"/go/*/x64/bin/go; do candidates+=("$d"); done
modcache=$(GOTOOLCHAIN=local go env GOMODCACHE 2>/dev/null || echo "$HOME/go/pkg/mod")
for d in "$modcache"/golang.org/toolchain@*/bin/go; do candidates+=("$d"); done

chosen="" have="" seen=""
for g in "${candidates[@]}"; do
    [ -x "$g" ] || continue
    v=$(GOTOOLCHAIN=local "$g" env GOVERSION 2>/dev/null | sed 's/^go//') || continue
    [ -n "$v" ] || continue
    seen="$seen $v($g)"
    if [ "$(printf '%s\n%s\n' "$want" "$v" | sort -V | head -1)" = "$want" ]; then
        chosen=$g have=$v
        break
    fi
done

if [ -z "$chosen" ]; then
    echo "::error::runner 上找不到 Go $want 或更高版本（找到的：${seen:- 无}）。请在 runner 上装一份 —— 这里不会自动下载（GOTOOLCHAIN=local）。"
    exit 1
fi

bindir=$(dirname "$chosen")
export PATH="$bindir:$PATH" GOTOOLCHAIN=local
# 让后续所有步骤用同一份工具链、且不自动下载。本地手动执行时没有这两个文件，跳过。
if [ -n "${GITHUB_PATH:-}" ]; then echo "$bindir" >> "$GITHUB_PATH"; fi
if [ -n "${GITHUB_ENV:-}" ]; then echo "GOTOOLCHAIN=local" >> "$GITHUB_ENV"; fi

echo "Go $have（$chosen），满足 go.mod 要求的 $want"
echo "GOMODCACHE=$(go env GOMODCACHE)"
echo "GOPROXY=$(go env GOPROXY)"
