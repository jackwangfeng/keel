#!/usr/bin/env bash
# 取回 dtmrs 并构建出 C ABI 动态库。
#
#   scripts/fetch-dtmrs.sh [目标目录]       默认 third_party/dtmrs
#
# 产物：<目标目录>/lib/libdtmrs.so（以及能编出来的 .a / .dylib）
#       <目标目录>/include/dtmrs.h
#
# 这两个目录都不入库：.so 是平台相关的构建产物，而头文件必须与 .so 同版本，
# 分开管理迟早对不上。
#
# **这个脚本是版本钉子的唯一一份。** 它原先在 examples/dtmrs-embedded/scripts/ 下，
# M2 把 dtmrs 接进主模块之后，主模块和例子都要同一个 .so。抄一份到根目录的话，
# 两个 REF 会在某次升级里错开，而症状是「例子绿、服务红」（或者反过来），
# 报错停在 C ABI 的某个符号上，不指向真因。所以例子的 `make deps` 调的也是这一份。
set -euo pipefail

cd "$(dirname "$0")/.."

DEST="${1:-$PWD/third_party/dtmrs}"
mkdir -p "$DEST"
DEST="$(cd "$DEST" && pwd)"

REPO="${DTMRS_REPO:-https://github.com/jackwangfeng/dtmrs}"
# 钉死版本。上游改了 C ABI 而这里悄悄跟着变，是最难查的一类问题。
REF="${DTMRS_REF:-v0.11.0}"
SRC="${DTMRS_SRC:-$DEST/.dtmrs-src}"

command -v cargo >/dev/null || {
    echo "需要 Rust 工具链（1.82+）。dtmrs 是 Rust 实现的，Keel 自 M2 起" >&2
    echo "通过 cgo 嵌入它，绕不开这一步。见 CONTRIBUTING「开发环境」。" >&2
    echo "Debian/Ubuntu: apt-get install cargo rustc；或 https://rustup.rs" >&2
    exit 1
}

# --depth 1 --branch <tag>：只取那一个 tag 的那一次提交。
#
# 全量 clone 是 870MB，而这条路径现在也跑在 docker build 里（每次镜像缓存失效
# 都要重来一遍）。浅 clone 拿到的东西一字不差 —— REF 是 tag，不是分支，
# 深度再大也只是多拉了用不到的历史。
if [ ! -d "$SRC/.git" ]; then
    echo "==> clone $REPO @ $REF（浅）"
    git clone --depth 1 --branch "$REF" "$REPO" "$SRC"
else
    # 已有的树可能是浅的，也可能是旧脚本留下的全量 clone；两种都认。
    if ! git -C "$SRC" rev-parse --verify --quiet "refs/tags/$REF^{commit}" >/dev/null; then
        echo "==> fetch tag $REF"
        git -C "$SRC" fetch --depth 1 origin "refs/tags/$REF:refs/tags/$REF"
    fi
    echo "==> checkout $REF"
    git -C "$SRC" checkout --quiet "$REF"
fi

echo "==> cargo build --release -p dtmrs-ffi"
cargo build --release --manifest-path "$SRC/Cargo.toml" -p dtmrs-ffi

mkdir -p "$DEST/lib" "$DEST/include"
for f in libdtmrs.so libdtmrs.a libdtmrs.dylib; do
    [ -f "$SRC/target/release/$f" ] && cp "$SRC/target/release/$f" "$DEST/lib/"
done
cp "$SRC/crates/dtmrs-ffi/dtmrs.h" "$DEST/include/"

echo
echo "==> 完成：$DEST"
ls -la "$DEST/lib" "$DEST/include"
