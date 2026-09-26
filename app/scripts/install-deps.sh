#!/usr/bin/env bash
# 装客户端依赖。
#
# ## 为什么不是一句 `npm ci`
#
# uni-app x 的 UTS 编译器是一个 Rust 写的 napi 原生模块，按平台拆成
# `@dcloudio/uts-<platform>` 六个包，由 `@dcloudio/uts` 的 optionalDependencies
# 引用。这些包的 package.json 里写的是：
#
#     "libc": ["gnu"]
#
# 而 **npm 11 报出来的本机 libc 是 `glibc`**，两个字符串不相等，于是它把这个
# 可选依赖判成「本平台不支持」直接跳过，还不报错（可选依赖本来就允许缺）。
# 实测（npm 11.6.1 / Ubuntu 24.04）：
#
#     npm error notsup Valid libc:  gnu
#     npm error notsup Actual libc: glibc
#
# 少了它之后，`uni build` 死在加载 vite.config.js 的那一刻：
#
#     Cannot find module '@dcloudio/uts-linux-x64-gnu'
#
# 这句话不指向真因 —— 读的人会去查自己的 vite 配置，而问题在 npm 的 libc 判定。
# `npm i --force` 与 `--libc=gnu` 都试过，都不行（前者「up to date」地什么都不装）。
#
# 所以这里在 npm 装完之后**自己核对一遍**：缺了就用 `npm pack` 取 tarball 解开。
# 这不是绕过校验 —— 版本号取自 @dcloudio/uts 自己的 optionalDependencies，
# 和 npm 本来要装的是同一个。
#
# 哪天 npm 或 DCloud 把这个元数据对齐了，这个脚本会发现包已经在、直接跳过。
set -euo pipefail

cd "$(dirname "$0")/.."
APP_DIR=$(pwd)

echo "==> npm install（${APP_DIR}）"
if [ -f package-lock.json ]; then
    npm ci --no-audit --no-fund
else
    npm install --no-audit --no-fund
fi

# 本机该用哪个 binding。名字规则来自 @dcloudio/uts/dist/binding.js。
binding=$(node -e '
const p = process.platform, a = process.arch
const map = {
  "linux-x64": "uts-linux-x64-gnu",
  "darwin-x64": "uts-darwin-x64",
  "darwin-arm64": "uts-darwin-arm64",
  "win32-x64": "uts-win32-x64-msvc",
  "win32-ia32": "uts-win32-ia32-msvc",
}
const k = p + "-" + a
if (!map[k]) { console.error("没有对应 " + k + " 的 uts binding"); process.exit(1) }
process.stdout.write(map[k])
')

if [ -d "node_modules/@dcloudio/$binding" ]; then
    echo "==> @dcloudio/$binding 已就位"
    exit 0
fi

version=$(node -p "require('./node_modules/@dcloudio/uts/package.json').optionalDependencies['@dcloudio/$binding']")
echo "==> npm 跳过了 @dcloudio/${binding}（libc 元数据不匹配），手工取 $version"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
npm pack "@dcloudio/$binding@$version" --pack-destination "$tmp" >/dev/null
tarball=$(ls "$tmp"/*.tgz)
mkdir -p "node_modules/@dcloudio/$binding"
tar xzf "$tarball" -C "node_modules/@dcloudio/$binding" --strip-components=1

# 真的能 require 起来才算装上。解包成功但 .node 文件跑不起来（glibc 太老之类）
# 也要在这里红，而不是等到 uni build 的时候。
node -e "require('@dcloudio/uts'); console.log('==> UTS 编译器可加载')"
