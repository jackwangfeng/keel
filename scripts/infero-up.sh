#!/usr/bin/env bash
# 在**宿主机**上起 infero（自研推理引擎），供 compose 里的 app 与
# `make test-engine` 使用。
#
# ## 为什么是宿主机进程，不是 compose 里的一个服务
#
# 本来打算给它一个 compose 服务、用 deploy.resources.reservations.devices
# 把 GPU 透进容器。实测下来放弃了，理由是显存账而不是技术障碍：
#
#   - 这里起的是 infero 的 GPU 版（CUDA / Metal；它 2026-09-26 起也有 CPU 后端，但单线程、单条约 5 秒，
#     Keel 今天用不上，见 docs/电商系统-总体架构.md §1「那个缺口」），而一块卡上
#     **同时只装得下一份**。本机这块 RTX A4000（16 GiB）此刻还分给另外两个
#     项目（约 9 GiB），infero 自己要 4.8 GiB —— 容器化不会让它变小，
#     只会在切换时多一次停机。
#   - 代价是一个 2–3 GB 的 CUDA 基础镜像 + GPU 透传配置，换来的只有
#     「一条命令起全栈」。
#   - infero 也不是自带镜像的：它的二进制动态 dlopen 一套 CUDA 用户态库
#     （cuBLAS 等），那套库在本机是 infero 仓库里 vendor/cuda/lib 的一个
#     软链，指向某个 Python 环境的 nvidia-*-cu13 wheel。要塞进容器，
#     得连那套库一起挂 —— 比直接跑宿主机进程更绕。
#
# **这件事的代价是真的，写在 README「还没在盒子里的」那一节**：
# `docker compose up` 不再能一条命令起全栈，语义检索要人先跑这个脚本。
# 不跑的话栈照样起得来，只是 /search 走纯关键词降级路径（§8），不报错。
#
# ## 用法
#
#     ./scripts/infero-up.sh            # 起在 0.0.0.0:18081
#     KEEL_INFERO_PORT=18099 ./scripts/infero-up.sh
#
# 起完之后：
#
#     KEEL_EMBED_ENDPOINT=http://127.0.0.1:18081 make test-engine
#     KEEL_EMBED_ENDPOINT=http://host.docker.internal:18081 docker compose \
#         -f compose.yaml -f compose.infero.yaml up -d
set -euo pipefail

# infero 的 checkout。二进制取 $KEEL_INFERO_HOME/target/release/infero，
# 那是 `cargo build --release` 的产物 —— 这个仓库不 vendor 它，也不建它。
KEEL_INFERO_HOME="${KEEL_INFERO_HOME:-$HOME/work/infero}"

# Qwen3-Embedding-0.6B 的 checkpoint **目录**（safetensors 布局，HF 上游原样）。
#
# 注意是目录不是文件：infero 的 --help 写着「Path to a GGUF model file」，
# 那句话已经过期 —— crates/server/src/main.rs 里是
# `if std::path::Path::new(&args.model).is_dir()` 走 safetensors、否则走 GGUF。
# （顺带：Qwen3-Embedding-0.6B 官方那份 GGUF 本机实测 infero 加载不了，
# 报 `d_head 128 * n_heads 16 != d_model 1024`，所以只能用 safetensors 这份。）
#
# 为什么是这个模型：它原生 1024 维，正好对上
# product_text_vectors.embedding 的 vector(1024) —— 换引擎不用改 DDL、
# 不用重算全库，那是这次切换里唯一真正贵的一步，而它被免掉了。
KEEL_INFERO_MODEL="${KEEL_INFERO_MODEL:-/mnt/data/tuili-models/Qwen3-Embedding-0.6B}"

KEEL_INFERO_PORT="${KEEL_INFERO_PORT:-18081}"

# 监听 0.0.0.0 而不是 127.0.0.1：compose 里的 app 容器要经
# host.docker.internal 打进来，绑在 loopback 上它看不见。
KEEL_INFERO_HOST="${KEEL_INFERO_HOST:-0.0.0.0}"

# --max-seqs 决定 embedding 的**批上限**，别随手调小。
#
# 实测 infero 的 embedding 批受它约束，上限是 2 × max-seqs：--max-seqs 8 起的
# 进程送 32 条就回 500「32 sequences want logits, the limit is 16」。
# 而 Keel 索引侧一批就是 inference.DefaultBatchSize = 64 条
# （语义检索层 §10「索引侧批大小 32–64」），所以这里至少要 32。
# internal/inference 的「正好 64 条要能算完」那条断言守的就是这件事。
KEEL_INFERO_MAX_SEQS="${KEEL_INFERO_MAX_SEQS:-32}"

# --ctx 是单条序列的长度上限。商品文本（search.EmbedContent 拼出来的那段）
# 远短于此；调大只会让 KV 池白占显存。
KEEL_INFERO_CTX="${KEEL_INFERO_CTX:-2048}"

bin="$KEEL_INFERO_HOME/target/release/infero"

if [ ! -x "$bin" ]; then
    echo "找不到 infero 二进制：$bin" >&2
    echo "infero 不在这个仓库里，也不由这个仓库构建。先 clone 并 cargo build --release，" >&2
    echo "或者用 KEEL_INFERO_HOME 指到你的 checkout。" >&2
    exit 1
fi
if [ ! -d "$KEEL_INFERO_MODEL" ]; then
    echo "找不到 checkpoint 目录：$KEEL_INFERO_MODEL" >&2
    echo "要 Qwen/Qwen3-Embedding-0.6B 的 safetensors 目录（不是 GGUF 文件）。" >&2
    echo "用 KEEL_INFERO_MODEL 指到它。" >&2
    exit 1
fi
if ! command -v nvidia-smi >/dev/null 2>&1; then
    echo "这台机器上没有 nvidia-smi，这个脚本起的是 infero 的 GPU 版。" >&2
    echo "infero 的 CPU 后端（--features cpu）数值一致但今天单线程、单条查询约 5 秒，Keel 用不上；" >&2
    echo "无 GPU 的部署目前只能不配 KEEL_EMBED_ENDPOINT，让 /search 走纯关键词降级路径（§8）。" >&2
    exit 1
fi

# 同时只装得下一份。已经有一个在跑就直说，不要起第二个 —— 第二个必然 OOM，
# 而 OOM 的现场（CUDA out of memory）指向的是新起的那个，不是真凶。
#
# 用 pgrep -x 按**进程名**精确匹配，不要用 `pgrep -f "release/infero --model"`：
# -f 匹配整条命令行，于是任何一个命令行里出现过这个字符串的进程都会命中——
# 包括启动这个脚本的那个 shell 自己。实测过，症状是脚本第一次跑就报
# 「已经有一个在跑了」并指着自己的父进程。
if pgrep -x infero >/dev/null 2>&1; then
    echo "已经有一个 infero 在跑了：" >&2
    pgrep -ax infero >&2
    echo "这块卡同时只装得下一份。要换配置先把它停掉。" >&2
    exit 1
fi

echo "infero  model=$KEEL_INFERO_MODEL  listen=$KEEL_INFERO_HOST:$KEEL_INFERO_PORT" >&2
exec "$bin" \
    --model "$KEEL_INFERO_MODEL" \
    --host "$KEEL_INFERO_HOST:$KEEL_INFERO_PORT" \
    --ctx "$KEEL_INFERO_CTX" \
    --max-seqs "$KEEL_INFERO_MAX_SEQS"
