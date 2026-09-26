#!/usr/bin/env bash
# H5 无头 e2e：带自动化运行时编 H5 → 起 e2e/h5-serve.js → 本机 Chrome 无头跑 app/e2e 下同一套用例。
# 真机（小米 / iPhone）都锁屏时的兜底：只覆盖 JS / H5 那一层，Android（Kotlin）与 iOS 原生那层测不到。
set -euo pipefail
cd "$(dirname "$0")/.."
: "${KEEL_API_BASE:?要设 KEEL_API_BASE（和 App 连的是同一台服务端）}"
PORT=${KEEL_E2E_PORT:-9520}
python3 ../scripts/check_app_build.py h5 --auto-host 127.0.0.1 --auto-port "$PORT"
node e2e/h5-serve.js &
SERVER=$!
trap 'kill $SERVER 2>/dev/null || true' EXIT
for _ in $(seq 1 50); do curl -sf -o /dev/null "http://127.0.0.1:${KEEL_E2E_H5_PORT:-5199}/" && break; sleep 0.2; done
UNI_PLATFORM=h5 UNI_APP_X=true UNI_AUTOMATOR_CONFIG="$PWD/e2e/automator.config.js" \
  npx jest -c e2e/jest.config.js -i --forceExit "$@"
