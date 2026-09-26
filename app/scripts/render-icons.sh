#!/usr/bin/env bash
# 渲染两类位图图标，产物都入库，只有改了图标才需要重跑：
#   · design/tabbar/*.svg -> src/static/tabbar/*.png，tabBar 要的 81x81（普通 + 选中两套）
#   · design/app-icon.svg -> native-android/app/src/main/res/mipmap-*/ic_launcher.png
#                         -> native-ios/KeelBuyer/Assets.xcassets/AppIcon.appiconset（1024，直角不透明）
#
# tabBar 的 iconPath 与 Android 启动图标都只认位图，而这台机器上不一定有 rsvg/ImageMagick，
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

# Android 启动图标。Chrome 无头模式的窗口有最小宽度（约 500px），比 192 小的尺寸
# 截出来会被裁，所以一律截 512 再用 sips（macOS 自带）缩。
RES=native-android/app/src/main/res
sed -e "s/SIZE/512/g" design/app-icon.svg > "$tmp/app-icon.svg"
shot "$tmp/app-icon.svg" "$tmp/app-icon.png" 512
for pair in mdpi:48 hdpi:72 xhdpi:96 xxhdpi:144 xxxhdpi:192; do
    d=${pair%%:*}; px=${pair##*:}
    mkdir -p "$RES/mipmap-$d"
    sips -z "$px" "$px" "$tmp/app-icon.png" --out "$RES/mipmap-$d/ic_launcher.png" >/dev/null
    echo "==> $RES/mipmap-$d/ic_launcher.png ($px)"
done

# iOS 启动图标：一张 1024x1024。系统自己切圆角，而且不允许透明通道（App Store 会拒），
# 所以把圆角半径改成 0 再截图，然后用 sips 转成不带 alpha 的 JPEG 再转回 PNG。
IOS=native-ios/KeelBuyer/Assets.xcassets/AppIcon.appiconset
mkdir -p "$IOS"
sed -e "s/SIZE/1024/g" -e 's/rx="44"/rx="0"/' design/app-icon.svg > "$tmp/ios-icon.svg"
shot "$tmp/ios-icon.svg" "$tmp/ios-icon.png" 1024
sips -s format jpeg "$tmp/ios-icon.png" --out "$tmp/ios-icon.jpg" >/dev/null
sips -s format png "$tmp/ios-icon.jpg" --out "$IOS/icon-1024.png" >/dev/null
cat > "$IOS/Contents.json" <<'JSON'
{
  "images" : [
    { "filename" : "icon-1024.png", "idiom" : "universal", "platform" : "ios", "size" : "1024x1024" }
  ],
  "info" : { "author" : "xcode", "version" : 1 }
}
JSON
echo "==> $IOS/icon-1024.png (1024)"
