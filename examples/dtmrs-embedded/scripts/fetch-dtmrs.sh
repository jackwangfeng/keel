#!/usr/bin/env bash
# 取回 dtmrs 并构建出 C ABI 动态库，产物落到 lib/ 与 include/。
#
# 这两个目录都不入库：.so 是平台相关的构建产物，而头文件必须与 .so 同版本，
# 分开管理迟早对不上。
set -euo pipefail

cd "$(dirname "$0")/.."

REPO="${DTMRS_REPO:-https://github.com/jackwangfeng/dtmrs}"
# 钉死版本。上游改了 C ABI 而这里悄悄跟着变，是最难查的一类问题。
REF="${DTMRS_REF:-8259c6a}"
SRC="${DTMRS_SRC:-$PWD/.dtmrs-src}"

command -v cargo >/dev/null || {
    echo "需要 Rust 工具链（1.82+）。dtmrs 是 Rust 实现的，" >&2
    echo "本例子通过 cgo 嵌入它，绕不开这一步。" >&2
    exit 1
}

if [ ! -d "$SRC/.git" ]; then
    echo "==> clone $REPO"
    git clone "$REPO" "$SRC"
fi
echo "==> checkout $REF"
git -C "$SRC" fetch --all --quiet || true
git -C "$SRC" checkout --quiet "$REF"

echo "==> cargo build --release -p dtmrs-ffi"
cargo build --release --manifest-path "$SRC/Cargo.toml" -p dtmrs-ffi

mkdir -p lib include
for f in libdtmrs.so libdtmrs.a libdtmrs.dylib; do
    [ -f "$SRC/target/release/$f" ] && cp "$SRC/target/release/$f" lib/
done
cp "$SRC/crates/dtmrs-ffi/dtmrs.h" include/

echo
echo "==> 完成"
ls -la lib include
