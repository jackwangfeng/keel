#!/usr/bin/env bash
# 打印 docker build 要带的代理参数；没配代理就什么都不打印。
#
# 用法：docker build $(scripts/ci_build_proxy_args.sh) -t x .
#       docker compose build $(scripts/ci_build_proxy_args.sh)
#
# ## 为什么要有它
#
# 自建 runner（lenserver）在国内，镜像构建里要从 GitHub `git clone`
# （scripts/fetch-dtmrs.sh）、要拉 Go 依赖、要从 apt.postgresql.org 装扩展。
# 直连常常断（实测报 "Error in the HTTP2 framing layer"，12 秒就红了）。
# runner 的 .env 里配了 KEEL_CI_BUILD_PROXY，构建就经它走。
#
# 参数只作为 --build-arg 传进构建阶段：HTTP_PROXY 这几个是 Docker 预定义的
# ARG，不进镜像历史，跑起来的容器里也没有代理。**不写进全局的
# ~/.docker/config.json**，那样会给同机演示环境的每个容器都塞上代理。
#
# ## 为什么是一个脚本，而不是在 workflow 里各写一遍
#
# 需要它的有三处：ci.yml 的 e2e（compose 构建）、ci.yml 的 go（建 keel-postgres）、
# release.yml 的 image（建发布镜像）。第一版只在 e2e 里内联了这段，另外两处
# 没带 —— 于是打 v0.1.0 标签的那一刻，发布流水线会卡在 dtmrs 的 git clone 上。
# 三处抄三遍，下一次再加一处构建时还会漏；收成一个脚本，漏的时候至少看得见。
#
# 构建容器里 127.0.0.1 是容器自己，所以代理地址要写宿主机的局域网 IP。
set -euo pipefail

p="${KEEL_CI_BUILD_PROXY:-}"
if [ -z "$p" ]; then
    exit 0
fi
for k in HTTP_PROXY HTTPS_PROXY http_proxy https_proxy; do
    printf -- '--build-arg %s=%s ' "$k" "$p"
done
printf -- '--build-arg NO_PROXY=localhost,127.0.0.1 --build-arg no_proxy=localhost,127.0.0.1\n'
