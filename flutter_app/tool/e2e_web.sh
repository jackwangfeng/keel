#!/usr/bin/env bash
# Flutter 买家端 Web 无头 e2e：按 KEEL_API_BASE 生成 web_dev_config.yaml（/api 反代到服务端，Web 只能同源），
# 取与本机 Chrome 同版本的 chromedriver，flutter drive -d web-server --headless 跑 integration_test/。
set -euo pipefail
cd "$(dirname "$0")/.."
: "${KEEL_API_BASE:?要设 KEEL_API_BASE（例如 http://192.168.0.110:18099/api/v1）}"
FLUTTER=${FLUTTER:-$HOME/development/flutter/bin/flutter}
ORIGIN=$(python3 -c "import sys,urllib.parse as u;p=u.urlparse(sys.argv[1]);print(f'{p.scheme}://{p.netloc}')" "$KEEL_API_BASE")
cat > web_dev_config.yaml <<EOF
server:
  proxy:
    - target: "$ORIGIN/"
      prefix: "/api/"
EOF

# e2e 账号：环境变量优先，没有就读 ~/.config/keel/e2e.env（不进仓库，与 app/ 的 e2e 同一个文件）。
ENV_FILE=${KEEL_E2E_ENV:-$HOME/.config/keel/e2e.env}
if [ -z "${KEEL_E2E_PHONE:-}" ] && [ -f "$ENV_FILE" ]; then set -a; . "$ENV_FILE"; set +a; fi
DEFINES=()
[ -n "${KEEL_E2E_PHONE:-}" ] && DEFINES+=(--dart-define=KEEL_E2E_PHONE="$KEEL_E2E_PHONE")
[ -n "${KEEL_E2E_PASSWORD:-}" ] && DEFINES+=(--dart-define=KEEL_E2E_PASSWORD="$KEEL_E2E_PASSWORD")

# chromedriver：与本机 Chrome 同一个主版本，缓存在 .tools/（gitignore）。
CHROME="/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"
VER=$("$CHROME" --version | awk '{print $3}')
DRIVER=".tools/chromedriver-$VER/chromedriver"
if [ ! -x "$DRIVER" ]; then
  mkdir -p .tools
  npx -y @puppeteer/browsers install "chromedriver@$VER" --path "$PWD/.tools/pb" >/dev/null
  mkdir -p ".tools/chromedriver-$VER"
  cp "$(find .tools/pb -name chromedriver -type f | head -1)" "$DRIVER"
fi
"$DRIVER" --port=4444 >/dev/null 2>&1 &
DRV=$!
trap 'kill $DRV 2>/dev/null || true' EXIT
sleep 1

for f in integration_test/*_test.dart; do
  "$FLUTTER" drive --driver=test_driver/integration_test.dart --target="$f" \
    -d web-server --headless --browser-name=chrome "${DEFINES[@]}"
done
