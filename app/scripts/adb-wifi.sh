#!/usr/bin/env bash
# 把 USB 连着的 Android 手机切到无线调试（adb TCP 模式），之后拔线也能装包、跑 e2e。
#
#   make app-adb-wifi        # 插着线跑一次
#
# 手机重启后 TCP 模式会失效，插线再跑一次即可。手机与电脑要在同一网段。
# 同一台手机同时插着线时，adb devices 里会有两条；e2e 的 automator.config.js 优先用无线那条。
set -euo pipefail

ADB=${ADB:-}
if [ -z "$ADB" ]; then
    for p in "${ANDROID_HOME:-}/platform-tools/adb" "$HOME/Library/Android/sdk/platform-tools/adb" \
             /opt/homebrew/share/android-commandlinetools/platform-tools/adb; do
        [ -x "$p" ] && ADB=$p && break
    done
fi
[ -n "$ADB" ] || { echo "FAIL: 找不到 adb" >&2; exit 1; }

usb=$("$ADB" devices | awk 'NR>1 && $2=="device" && $1 !~ /:[0-9]+$/ {print $1; exit}')
[ -n "$usb" ] || { echo "FAIL: 没有 USB 连着的 Android 设备（先插线、授权 USB 调试）" >&2; exit 1; }

ip=$("$ADB" -s "$usb" shell ip -f inet addr show wlan0 2>/dev/null | awk '/inet /{sub(/\/.*/,"",$2); print $2; exit}')
[ -n "$ip" ] || { echo "FAIL: 手机没连 Wi-Fi（取不到 wlan0 的地址）" >&2; exit 1; }

echo "==> ${usb} 切到 TCP 模式，Wi-Fi 地址 ${ip}"
"$ADB" -s "$usb" tcpip 5555 >/dev/null
sleep 2
"$ADB" connect "${ip}:5555"
"$ADB" -s "${ip}:5555" get-state >/dev/null && echo "==> 可以拔线了：之后 make app-e2e 走 ${ip}:5555"
