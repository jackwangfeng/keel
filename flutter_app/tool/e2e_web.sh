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
# 不定位：用例按默认店的商品与价格写（本机的真实位置会被围栏解析到别的店）。
DEFINES=(--dart-define=KEEL_LOCATE=off)
[ -n "${KEEL_E2E_PHONE:-}" ] && DEFINES+=(--dart-define=KEEL_E2E_PHONE="$KEEL_E2E_PHONE")
[ -n "${KEEL_E2E_PASSWORD:-}" ] && DEFINES+=(--dart-define=KEEL_E2E_PASSWORD="$KEEL_E2E_PASSWORD")
# 后台前置状态的单号（请服务端会话代做后传进来）：没设的那几条用例跳过。
for v in KEEL_E2E_ONLY KEEL_E2E_REJECTED_REFUND KEEL_E2E_REFUNDED_REFUND KEEL_E2E_RETURN_REFUND KEEL_E2E_SHIPPED_ORDER; do
  [ -n "${!v:-}" ] && DEFINES+=(--dart-define="$v=${!v}")
done

# chromedriver：与本机 Chrome 同一个主版本，缓存在 .tools/（gitignore）。
# 本机 Chrome：默认 macOS 的位置；Linux 上传 CHROME=/usr/bin/google-chrome（flutter drive 也读 CHROME_EXECUTABLE）。
# 按手机屏跑（默认 iPhone 14/15 的 390×844@3）：flutter drive 默认是 1600×1024 的桌面窗口，窄屏才有的问题
# （按钮在屏幕外、价格行溢出、键盘挡住提示）在桌面宽度下全都测不出来 —— 2026-09 真机 e2e 一次抓到四个。
VIEWPORT=${KEEL_E2E_VIEWPORT:-390x844@3}
CHROME=${CHROME:-"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"}
export CHROME_EXECUTABLE=${CHROME_EXECUTABLE:-$CHROME}
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
    -d web-server --headless --browser-name=chrome --browser-dimension="$VIEWPORT" "${DEFINES[@]}"
done
