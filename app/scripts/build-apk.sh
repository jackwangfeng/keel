#!/usr/bin/env bash
# 本地打 Android apk：不用 HBuilderX，不用云打包，不上传任何东西。
#
#   KEEL_API_BASE=http://192.168.0.110:18099/api/v1 ./app/scripts/build-apk.sh
#
# 产物：app/dist/keel-buyer-<versionName>.apk
#
#   ./app/scripts/build-apk.sh --e2e      # 自动化测试包，见 app/e2e/README 与下文
#
# 产物：app/dist/keel-buyer-<versionName>-e2e.apk。它多带一份官方的自动化运行时，
# 启动后连 ws://127.0.0.1:${KEEL_E2E_PORT:-9520}（测试时由 adb reverse 转回本机）。
#
# ## 流程
#
#   1. 离线 SDK：没有就下载到 native-android/.uni-sdk/（钉版本 + sha256）。
#   2. `uni build --platform app-android`（经 scripts/check_app_build.py，类型/样式诊断即失败），
#      得到 dist/build/app-android/：Kotlin 源码 + manifest.json + static/。
#   3. 核对 manifest.json 里编译器声明用到的 uni 模块，全都在 settings.gradle 的 aar 清单里。
#   4. 拷进 native-android/uniappx：Kotlin -> src/main/java，其余 -> assets/apps/<appid>/www。
#   5. 注入 KEEL_API_BASE（见 src/api/native-default.uts），Gradle assembleRelease。
#
# ## 需要什么
#
#   JDK 17、Android SDK（platforms;android-36 + build-tools）。ANDROID_HOME 没设时
#   依次找 ~/Library/Android/sdk 与 Homebrew 的 android-commandlinetools。
#
# ## 为什么是这条路
#
# DCloud 的正规路线是 HBuilderX「发行 → 原生App-本地打包 → 生成本地打包App资源」，
# 再把资源拷进离线 SDK 的工程。实测 HBuilderX 5.26 用 CLI 导入这个项目会直接崩溃
# （连一份不含 node_modules 的最小副本也崩），而离线 SDK 自带的 Demo 工程里，
# 页面就是 uniappx/src/main/java/ 下的 .kt —— 和 npm 版 `uni build` 的产物同一种东西。
# 所以这里跳过 HBuilderX，直接喂 npm 编译器的产物。
set -euo pipefail

E2E=0
[ "${1:-}" = "--e2e" ] && E2E=1
E2E_PORT=${KEEL_E2E_PORT:-9520}

cd "$(dirname "$0")/.."
APP=$(pwd)
ROOT=$(cd .. && pwd)
NATIVE=$APP/native-android

# ---- 离线 SDK 的版本必须与 npm 编译器（package.json 里 @dcloudio/* 那条线）同一版 ----
SDK_VERSION=5.26
SDK_URL="https://web-ext-storage.dcloud.net.cn/uni-app-x/sdk/Android/Android-uni-app-x-SDK@15075-${SDK_VERSION}.zip"
SDK_SHA256=3a97adb960eb753cc872983fd830e382cf7df7537b764e34b4d9220779a0af8b
SDK_DIR=$NATIVE/.uni-sdk

die() { echo "FAIL: $*" >&2; exit 1; }

# 退出时要删的临时路径，统一在一个 trap 里（多个 trap 会互相覆盖）。
CLEANUP=""
trap 'for p in $CLEANUP; do rm -rf "$p"; done' EXIT

# ---- 工具链 ----
if [ -z "${JAVA_HOME:-}" ]; then
    JAVA_HOME=$(/usr/libexec/java_home -v 17 2>/dev/null) || die "找不到 JDK 17"
fi
export JAVA_HOME
if [ -z "${ANDROID_HOME:-}" ]; then
    for d in "$HOME/Library/Android/sdk" /opt/homebrew/share/android-commandlinetools; do
        [ -d "$d/platforms" ] && ANDROID_HOME=$d && break
    done
fi
[ -n "${ANDROID_HOME:-}" ] || die "找不到 Android SDK，请设置 ANDROID_HOME"
export ANDROID_HOME

# ---- 1. 离线 SDK ----
if [ ! -f "$SDK_DIR/.version" ] || [ "$(cat "$SDK_DIR/.version")" != "$SDK_SHA256" ]; then
    echo "==> 下载 uni-app x 离线 SDK ${SDK_VERSION}（约 80MB）"
    tmp=$(mktemp -d)
    CLEANUP="$CLEANUP $tmp"
    curl -fSL --progress-bar -o "$tmp/sdk.zip" "$SDK_URL"
    actual=$(shasum -a 256 "$tmp/sdk.zip" | awk '{print $1}')
    [ "$actual" = "$SDK_SHA256" ] || die "SDK 校验和不符：期望 ${SDK_SHA256}，实际 $actual"
    unzip -q "$tmp/sdk.zip" -d "$tmp/x"
    rm -rf "$SDK_DIR" && mkdir -p "$SDK_DIR"
    inner=$(find "$tmp/x" -maxdepth 1 -type d -name 'Android-uni-app-x-SDK@*' | head -1)
    cp -R "$inner/SDK" "$inner/plugins" "$SDK_DIR/"
    echo "$SDK_SHA256" > "$SDK_DIR/.version"
fi

# ---- 2. 编译 ----
AUTO_ARGS=()
if [ "$E2E" = 1 ]; then
    # 官方自动化运行时是 UTS 源码，npm 编译器不输出它（见 vite.config.js 的
    # keel-automator-runtime）。临时拷进 src/，构建完就删：它满是 @ts-expect-error，
    # 留在 src/ 里会让 scripts/check_app_types.py 那道 tsc 闸门红掉。
    RUNTIME=$APP/src/automator-runtime
    rm -rf "$RUNTIME"
    cp -R "$APP/node_modules/@dcloudio/uni-app-uts/lib/automator/android" "$RUNTIME"
    CLEANUP="$CLEANUP $RUNTIME"
    # 127.0.0.1 而不是局域网 IP：测试时 adb reverse 把手机的这个端口转回本机，
    # 不依赖手机与电脑同网段，也不用开防火墙。
    AUTO_ARGS=(--auto-host 127.0.0.1 --auto-port "$E2E_PORT")
fi
echo "==> uni build --platform app-android ${AUTO_ARGS[*]:-}"
python3 "$ROOT/scripts/check_app_build.py" app-android ${AUTO_ARGS[@]+"${AUTO_ARGS[@]}"}
OUT=$APP/dist/build/app-android
MANIFEST=$OUT/manifest.json
[ -f "$OUT/.uniappx/android/src/index.kt" ] || die "编译产物里没有 index.kt：$OUT"

read -r APPID VNAME VCODE COMPILER <<<"$(python3 -c '
import json,sys
m=json.load(open(sys.argv[1]))
print(m["id"], m["version"]["name"], m["version"]["code"], m.get("uni-app-x",{}).get("compilerVersion",""))
' "$MANIFEST")"
[ "$COMPILER" = "$SDK_VERSION" ] || die "编译器版本 $COMPILER 与离线 SDK $SDK_VERSION 不一致：两边必须同一版"

# ---- 3. 模块清单核对 ----
# 编译器把代码用到的 uni API 模块写在 app-android.distribute.modules 里。
# settings.gradle 的 aar 清单漏一个，apk 照样打得出来，但调用那个 API 时才在真机上崩。
python3 - "$MANIFEST" "$NATIVE/settings.gradle" <<'EOF'
import json, re, sys
mods = json.load(open(sys.argv[1])).get('app-android', {}).get('distribute', {}).get('modules', {})
gradle = open(sys.argv[2]).read()
listed = set(re.findall(r"'([A-Za-z0-9_.-]+)'", gradle[gradle.index('gradle.ext.uniAars'):]))
missing = sorted(m for m in mods if m not in listed)
if missing:
    print('FAIL: 代码用到了这些 uni 模块，但 native-android/settings.gradle 的 uniAars 里没有：')
    for m in missing:
        print('  -', m)
    sys.exit(1)
print('==> 模块清单核对通过：' + ', '.join(sorted(mods)))
EOF

# ---- 4. 拷进原生工程 ----
JAVA_DIR=$NATIVE/uniappx/src/main/java
WWW=$NATIVE/uniappx/src/main/assets/apps/$APPID/www
rm -rf "$JAVA_DIR" "$NATIVE/uniappx/src/main/assets"
mkdir -p "$JAVA_DIR" "$WWW"
cp -R "$OUT/.uniappx/android/src/." "$JAVA_DIR/"
(cd "$OUT" && tar cf - --exclude .uniappx .) | (cd "$WWW" && tar xf -)

# ---- 5. 注入默认服务地址 ----
NEEDLE='val NATIVE_DEFAULT_BASE_URL: String = ""'
count=$(grep -rhF "$NEEDLE" "$JAVA_DIR" | wc -l | tr -d ' ')
[ "$count" = "1" ] || die "在 Kotlin 产物里找到 $count 处 \`$NEEDLE\`（应恰好 1 处）。src/api/native-default.uts 的写法变了？"
if [ -n "${KEEL_API_BASE:-}" ]; then
    case "$KEEL_API_BASE" in
        http://*|https://*) ;;
        *) die "KEEL_API_BASE 必须是 http(s):// 开头的绝对地址，拿到的是：$KEEL_API_BASE" ;;
    esac
    file=$(grep -rlF "$NEEDLE" "$JAVA_DIR")
    python3 - "$file" "$NEEDLE" "$KEEL_API_BASE" <<'EOF'
import json, sys
path, needle, url = sys.argv[1:4]
s = open(path, encoding='utf-8').read()
open(path, 'w', encoding='utf-8').write(
    s.replace(needle, 'val NATIVE_DEFAULT_BASE_URL: String = ' + json.dumps(url), 1))
EOF
    echo "==> 默认服务地址：$KEEL_API_BASE"
else
    echo "==> 没设 KEEL_API_BASE：App 装好后要先在「我的 → 服务地址」里填地址"
fi

# ---- 6. Gradle ----
echo "==> gradle assembleRelease（appid=$APPID version=$VNAME($VCODE)）"
(cd "$NATIVE" && ./gradlew --console=plain -q assembleRelease \
    -PuniAppId="$APPID" -PversionName="$VNAME" -PversionCode="$VCODE")

APK_SRC=$NATIVE/app/build/outputs/apk/release/app-release.apk
[ -f "$APK_SRC" ] || die "Gradle 说成功了，但没有 $APK_SRC"
SUFFIX=""
[ "$E2E" = 1 ] && SUFFIX="-e2e"
APK=$APP/dist/keel-buyer-$VNAME$SUFFIX.apk
cp "$APK_SRC" "$APK"
echo "==> ${APK}（$(du -h "$APK" | awk '{print $1}')）"
