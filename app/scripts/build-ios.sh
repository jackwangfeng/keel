#!/usr/bin/env bash
# 本地打 iOS 包：不用 HBuilderX，不用云打包。
#
#   KEEL_IOS_TEAM=XXXXXXXXXX KEEL_API_BASE=http://192.168.0.110:18099/api/v1 ./app/scripts/build-ios.sh
#   KEEL_IOS_TEAM=... KEEL_API_BASE=... ./app/scripts/build-ios.sh --e2e      # 自动化测试包
#
# --e2e：带上官方自动化运行时（@dcloudio/uni-automator 的 uni 插件在 --auto-port 时自动往
# main 里 import，iOS 产物是 JS，不像 Android 需要额外搬源码）。和 Android 不同，iPhone
# 没法把端口反向转回电脑（usbmuxd 只支持电脑连手机），所以 App 经局域网连
# ws://<电脑的局域网 IP>:9520 —— 手机与电脑要在同一网段。IP 默认取 en0，KEEL_E2E_HOST 可改。
#
# 真机签名是 Automatic：Xcode 没登录 Apple ID 也行，只要本机已有这个团队覆盖该设备的
# 描述文件（例如团队的通配描述文件 "iOS Team Provisioning Profile: *"）。实测
# 手动签名反而不行 —— 那类描述文件是 Xcode 管理的，手动签名拒绝使用它。
# 本机没有合适的描述文件时，在 Xcode 里登录该团队的 Apple ID，再加
# KEEL_IOS_ALLOW_PROVISIONING=1 让 xcodebuild 自动去生成。
#
# ## 和 Android 的区别
#
# uni-app x 在 iOS 上的页面逻辑编译成 JS（app-service.js），跑在系统的 JavaScriptCore 里，
# 界面仍是原生渲染（UIKit）。所以这里不编译我们的代码：`uni build --platform app-ios`
# 的产物直接作为资源拷进 native-ios/KeelBuyer/uni-app-x/apps/<appid>/www。
#
# ## 流程
#
#   1. 离线 SDK（866MB，钉版本 + sha256）：只取需要的五个 xcframework 与 uni-prompt.bundle，
#      放到 native-ios/.uni-sdk/。已经下载过 zip 的话用 UNI_IOS_SDK_ZIP 指过去，免得再下一遍。
#   2. `uni build --platform app-ios`（经 scripts/check_app_build.py）。KEEL_API_BASE 在这一步
#      注入（见 src/api/native-default.uts）。
#   3. 拷资源、回填 Info.plist 的 appid，XcodeGen 生成 .xcodeproj，xcodebuild。
#
# ## 为什么没有模拟器版本
#
# SDK 里 DCloudUTSExtAPI.xcframework（uni API 的实现）的模拟器切片只有 x86_64，没有
# arm64；而 iOS 26 的模拟器已经不收 x86_64 的 App（安装时报 "Failed to find matching
# arch"，实测）。要模拟器，得用 SDK 里的 ExtApiSrc 源码自己编一份 arm64 模拟器版。
set -euo pipefail

E2E=0
for arg in "$@"; do
    case "$arg" in
        --device) ;;   # 兼容：本来就只出真机包
        --e2e) E2E=1 ;;
        *) echo "未知参数：$arg" >&2; exit 2 ;;
    esac
done

cd "$(dirname "$0")/.."
APP=$(pwd)
ROOT=$(cd .. && pwd)
NATIVE=$APP/native-ios

SDK_VERSION=5.26
SDK_URL="https://web-ext-storage.dcloud.net.cn/uni-app-x/sdk/iOS/UniAppX-iOS%40${SDK_VERSION}.zip"
SDK_SHA256=70a9ea88822c6771c785e74e36ffbd232ade80974a66f6b4653f5d7d3b61c976
SDK_DIR=$NATIVE/.uni-sdk
LIBS="DCloudUniappRuntime DCloudUTSFoundation SDWebImage KSCrash"

die() { echo "FAIL: $*" >&2; exit 1; }

CLEANUP=""
trap 'for p in $CLEANUP; do rm -rf "$p"; done' EXIT

command -v xcodebuild >/dev/null || die "找不到 xcodebuild（需要 Xcode）"
command -v xcodegen >/dev/null || die "找不到 xcodegen：brew install xcodegen"

# ---- 1. 离线 SDK ----
if [ ! -f "$SDK_DIR/.version" ] || [ "$(cat "$SDK_DIR/.version")" != "$SDK_SHA256" ]; then
    tmp=$(mktemp -d)
    CLEANUP="$CLEANUP $tmp"
    zip=${UNI_IOS_SDK_ZIP:-}
    if [ -z "$zip" ]; then
        echo "==> 下载 uni-app x iOS 离线 SDK ${SDK_VERSION}（约 866MB）"
        zip=$tmp/sdk.zip
        curl -fSL --progress-bar -o "$zip" "$SDK_URL"
    fi
    actual=$(shasum -a 256 "$zip" | awk '{print $1}')
    [ "$actual" = "$SDK_SHA256" ] || die "SDK 校验和不符：期望 ${SDK_SHA256}，实际 $actual"
    inner="UniAppX-iOS@${SDK_VERSION}"
    patterns=("$inner/TemporarySampleFramework/DCloudUTSExtAPI.xcframework/*" "$inner/SDK/Resources/uni-prompt.bundle/*")
    for l in $LIBS; do patterns+=("$inner/SDK/Libs/$l.xcframework/*"); done
    echo "==> 解出 SDK 里需要的部分"
    unzip -q "$zip" "${patterns[@]}" -x '*/.DS_Store' -d "$tmp/x"
    rm -rf "$SDK_DIR" && mkdir -p "$SDK_DIR/Libs" "$SDK_DIR/Resources"
    for l in $LIBS; do cp -R "$tmp/x/$inner/SDK/Libs/$l.xcframework" "$SDK_DIR/Libs/"; done
    cp -R "$tmp/x/$inner/TemporarySampleFramework/DCloudUTSExtAPI.xcframework" "$SDK_DIR/Libs/"
    cp -R "$tmp/x/$inner/SDK/Resources/uni-prompt.bundle" "$SDK_DIR/Resources/"
    echo "$SDK_SHA256" > "$SDK_DIR/.version"
fi

# ---- 2. 编译 ----
if [ -n "${KEEL_API_BASE:-}" ]; then
    case "$KEEL_API_BASE" in
        http://*|https://*) echo "==> 默认服务地址：$KEEL_API_BASE" ;;
        *) die "KEEL_API_BASE 必须是 http(s):// 开头的绝对地址，拿到的是：$KEEL_API_BASE" ;;
    esac
    export KEEL_API_BASE
else
    echo "==> 没设 KEEL_API_BASE：App 装好后要先在「我的 → 服务地址」里填地址"
fi
AUTO_ARGS=()
if [ "$E2E" = 1 ]; then
    E2E_HOST=${KEEL_E2E_HOST:-$(ipconfig getifaddr en0 2>/dev/null || true)}
    [ -n "$E2E_HOST" ] || die "取不到电脑的局域网 IP：用 KEEL_E2E_HOST 指定"
    AUTO_ARGS=(--auto-host "$E2E_HOST" --auto-port "${KEEL_E2E_PORT:-9520}")
fi
echo "==> uni build --platform app-ios ${AUTO_ARGS[*]:-}"
python3 "$ROOT/scripts/check_app_build.py" app-ios ${AUTO_ARGS[@]+"${AUTO_ARGS[@]}"}
OUT=$APP/dist/build/app-ios
[ -f "$OUT/app-service.js" ] || die "编译产物里没有 app-service.js：$OUT"

read -r APPID VNAME VCODE COMPILER <<<"$(python3 -c '
import json,sys
m=json.load(open(sys.argv[1]))
print(m["id"], m["version"]["name"], m["version"]["code"], m.get("uni-app-x",{}).get("compilerVersion",""))
' "$OUT/manifest.json")"
[ "$COMPILER" = "$SDK_VERSION" ] || die "编译器版本 $COMPILER 与离线 SDK $SDK_VERSION 不一致：两边必须同一版"

# ---- 3. 拷资源、生成工程 ----
WWW=$NATIVE/KeelBuyer/uni-app-x/apps/$APPID/www
rm -rf "$NATIVE/KeelBuyer/uni-app-x"
mkdir -p "$WWW"
cp -R "$OUT/." "$WWW/"
/usr/libexec/PlistBuddy -c "Set :uniapp-x:appid $APPID" "$NATIVE/KeelBuyer/Info.plist"
/usr/libexec/PlistBuddy -c "Set :uniapp-x:uniRuntimeVersion $SDK_VERSION" "$NATIVE/KeelBuyer/Info.plist"

export KEEL_VERSION_NAME=$VNAME KEEL_VERSION_CODE=$VCODE KEEL_IOS_TEAM=${KEEL_IOS_TEAM:-}
(cd "$NATIVE" && xcodegen generate --quiet)

# ---- 4. xcodebuild ----
DERIVED=$NATIVE/build
[ -n "$KEEL_IOS_TEAM" ] || die "真机包要签名：用 KEEL_IOS_TEAM 指定开发者团队 ID（security find-identity -v -p codesigning 可查，证书的 OU 字段）"
echo "==> xcodebuild（真机 arm64，team=${KEEL_IOS_TEAM}）"
SIGN=()
[ "${KEEL_IOS_ALLOW_PROVISIONING:-}" = 1 ] && SIGN=(-allowProvisioningUpdates)
xcodebuild -quiet -project "$NATIVE/KeelBuyer.xcodeproj" -scheme KeelBuyer -configuration Debug \
    -sdk iphoneos -derivedDataPath "$DERIVED" "DEVELOPMENT_TEAM=$KEEL_IOS_TEAM" ${SIGN[@]+"${SIGN[@]}"} build
BUILT=$DERIVED/Build/Products/Debug-iphoneos/KeelBuyer.app
[ -d "$BUILT" ] || die "xcodebuild 说成功了，但没有 $BUILT"
DEST=$APP/dist/ios-device
[ "$E2E" = 1 ] && DEST=$DEST-e2e
rm -rf "$DEST" && mkdir -p "$DEST"
cp -R "$BUILT" "$DEST/"
echo "==> $DEST/KeelBuyer.app（$(du -sh "$DEST/KeelBuyer.app" | awk '{print $1}')）"

# 装机：USB 连着、已配对的 iPhone（KEEL_IOS_INSTALL=1 时）。
if [ "${KEEL_IOS_INSTALL:-}" = 1 ]; then
    xcrun devicectl device install app --device "${KEEL_IOS_DEVICE:-$(xcrun devicectl list devices 2>/dev/null | awk '/paired/{print $3; exit}')}" "$DEST/KeelBuyer.app" >/dev/null
    echo "==> 已装到 iPhone"
fi
