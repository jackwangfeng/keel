#!/usr/bin/env bash
# 把 design/tabbar/*.svg 渲染成 tabBar 要的 81x81 PNG（普通 + 选中两套）。
# 产物入库，只有改了图标才需要重跑。
#
# tabBar 的 iconPath 只认位图，而这台机器上不一定有 rsvg/ImageMagick，
# 所以借无头 Chrome 截图：透明底 + 1 倍像素比，截出来就是 81x81。
# SVG 里的 COLOR / FILL / INNER 是占位符：普通态是描边的浅色，选中态是实心深咖。
set -euo pipefail

cd "$(dirname "$0")/.."
CHROME=${CHROME:-"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"}
OUT=src/static/tabbar
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
mkdir -p "$OUT"

shot() { # <svg> <out.png> <size>
    "$CHROME" --headless=new --disable-gpu --hide-scrollbars --force-device-scale-factor=1 \
        --default-background-color=00000000 --window-size="$3,$3" \
        --screenshot="$2" "file://$1" >/dev/null 2>&1
}

render() { # <src.svg> <out.png> <stroke> <fill> <inner>
    sed -e "s/COLOR/$3/g" -e "s/FILL/$4/g" -e "s/INNER/$5/g" "$1" > "$tmp/icon.svg"
    shot "$tmp/icon.svg" "$2" 81
}

for src in design/tabbar/*.svg; do
    name=$(basename "$src" .svg)
    render "$src" "$OUT/$name.png"    '#A89C90' 'none'    '#A89C90'
    render "$src" "$OUT/$name-on.png" '#3B2A20' '#3B2A20' '#FFFDF9'
    echo "==> $OUT/$name.png, $OUT/$name-on.png"
done

