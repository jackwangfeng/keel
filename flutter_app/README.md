# Keel 买家端（Flutter）

同一套代码：Android / iOS / Web / 微信小程序（经 mp-flutter）。uni-app x 那套（`app/`）已冻结，新功能只接这里。

## 跑起来

```bash
make flutter-get
make flutter-generate      # 契约改了之后：重新生成 lib/api/schema.g.dart 并一起提交
make flutter-analyze flutter-test
KEEL_API_BASE=http://192.168.0.110:18099/api/v1 make flutter-e2e-web   # Web 无头 e2e
KEEL_API_BASE=http://192.168.0.110:18099/api/v1 make flutter-build     # Web / apk / iOS 编译
KEEL_API_BASE=https://eshop.zzss.fun/api/v1 make flutter-build-mp      # 微信小程序
```

e2e 账号读 `~/.config/keel/e2e.env`（`KEEL_E2E_PHONE` / `KEEL_E2E_PASSWORD`，与 `app/` 的 e2e 同一个文件，不进仓库）。
Android 编译找 SDK 的顺序与 `app/scripts/build-apk.sh` 相同：`ANDROID_HOME` → `~/Library/Android/sdk` → Homebrew 的 android-commandlinetools；JDK 17。

## 规矩

- 契约字段只在 `lib/api/` 读写；`lib/pages/`、`lib/widgets/` 不许 import `schema.g.dart`（`scripts/check_flutter_pages.py`）。
- `schema.g.dart` 由 `scripts/gen_dart_schema.py` 生成，`scripts/check_dart_contract.py` 比对，请勿手改。
- 服务地址：原生与小程序编译期 `--dart-define=KEEL_API_BASE=...` 注入；Web 用相对路径 `/api/v1` 同源（开发 / e2e 走 `web_dev_config.yaml` 反代）。
- 依赖只用 mp-flutter 能透明接管的（`http`、`shared_preferences`）；不依赖 Cookie；不用原生插件。
- 登录页压在外壳上（push，成功 pop），不要 `go('/login')`：会在转场中重建外壳（Duplicate GlobalKey）。
- 日常验收：analyze、两道闸门、单测、Web 无头 e2e、各平台编译、mp-flutter 编译；真机只在发版前跑。
