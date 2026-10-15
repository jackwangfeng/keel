#!/usr/bin/env bash
# 构建之前，先用 `docker pull` 把各 Dockerfile 的基础镜像拉到本地。
#
# 为什么要这一步（2026-10-16 一晚上踩了三次）：/etc/docker/daemon.json 配了好几个镜像源，
# `docker pull` 遇到一个源回 5xx 会依次换下一个；但 BuildKit 构建时解析 `FROM` 只认第一个源，
# 第一个回 502 就整个构建失败，不往后换。演示站部署、CI 的 e2e、发版流水线都因此失败过。
# 先 pull 一遍，本地就有了这些镜像，构建时直接用本地的。
#
# 镜像清单从 Dockerfile 的 FROM 里现取，不手写：Dockerfile 换了基础镜像，这里自动跟上。
# 跳过 scratch、跳过引用前面构建阶段名的 FROM（如 FROM build）、跳过带变量的（如 FROM ${BASE}）。
#
#   scripts/prepull-base-images.sh            拉 docker/ 下全部 Dockerfile 的基础镜像
#   scripts/prepull-base-images.sh <文件>...   只拉这几个 Dockerfile 的
#
# 单个镜像 3 次都拉不下来时只报警告、不中止：本地可能已经有旧的一份，构建仍可能成功，
# 真不行由构建自己报错 —— 这一步是加固，不该自己成为新的失败点。
set -uo pipefail
cd "$(dirname "$0")/.."

files=("$@")
if [ ${#files[@]} -eq 0 ]; then
    mapfile -t files < <(find docker -name 'Dockerfile*' -type f | sort)
fi

images=$(awk '
    toupper($1) == "FROM" {
        img = ""
        for (i = 2; i <= NF; i++) { if ($i !~ /^--/) { img = $i; break } }
        for (i = 1; i <= NF; i++) if (toupper($i) == "AS" && i < NF) stage[$(i + 1)] = 1
        if (img != "" && img != "scratch" && img !~ /\$/ && !(img in stage)) print img
    }' "${files[@]}" | sort -u)

for img in $images; do
    ok=0
    for attempt in 1 2 3; do
        if docker pull -q "$img" >/dev/null 2>&1; then
            ok=1
            break
        fi
        sleep $((attempt * 5))
    done
    if [ "$ok" -eq 1 ]; then
        echo "预拉基础镜像 $img"
    else
        echo "::warning::预拉基础镜像 $img 失败（试了 3 次），构建时若本地没有它会再失败" >&2
    fi
done
