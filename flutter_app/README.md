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

## 页面（第一阶段）

底部 4 个 tab：首页 / 购物车 / 订单 / 我的。

- 首页：门店行、搜索入口、分类、商品网格、「＋」原地加购（单规格直接加，多规格底部浮层）
- 搜索：按相关度排序，缺货的标出并压暗；效果回传（click / add_cart / order，trace_id 缺席整批不报）
- 商品详情：图片、特价划线、规格、加入购物车（「去结算 ›」）、立即购买、底栏购物车角标
- 购物车：勾选 / 全选、改数量（超库存显示服务端原因）、不能买的行写原因、预估运费、凑单提示、管理删除
- 结算：默认地址 / 选地址 / 新建地址，自动选最省的券（只一次）、金额拆行、各种问题照实说（送不到、限购、名额抢完、价格变了、券用不了、同键异体）
- 订单列表 / 详情：状态 + 售后状态、取消 / 确认收货（点两下）、沙箱支付（「模拟支付完成（沙箱）」）
- 地址簿：列表、新建（幂等键）、编辑（PUT 不切默认，切默认走专用接口）、设默认、删除；422 按字段标红

## e2e（Web 无头）

`integration_test/all_test.dart` 一个入口跑全部（`flutter drive` 每个 target 都要重编一次 App）。用例用 `e2e(...)` 包一层：
Web 上 drive 只报「有 N 个异常」，失败信息里会带第一个异常的原文。覆盖：冒烟、分类、搜索、首页加购、详情加购、购物车、
地址簿、下单付款、购物车结算、取消订单、运费（杭州 / 新疆 / 香港 / 省份写错 / 购物车预估）、限时特价与限购、满减、包邮券。

## 小程序冒烟

```bash
KEEL_API_BASE=http://192.168.0.110:18099/api/v1 make flutter-build-mp
cd flutter_app/tool && npm install && cd -
NODE_PATH=flutter_app/tool/node_modules node flutter_app/tool/mp_walk.js   # 截图在 build/mp-shots/
```

开发者工具是和 mp-flutter 会话共用的：用之前先跟它打招呼，用完说一声。

## 规矩

- 契约字段只在 `lib/api/` 读写；`lib/pages/`、`lib/widgets/` 不许 import `schema.g.dart`（`scripts/check_flutter_pages.py`）。
- `schema.g.dart` 由 `scripts/gen_dart_schema.py` 生成，`scripts/check_dart_contract.py` 比对，请勿手改。
- 服务地址：原生与小程序编译期 `--dart-define=KEEL_API_BASE=...` 注入；Web 用相对路径 `/api/v1` 同源（开发 / e2e 走 `web_dev_config.yaml` 反代）。
- 依赖只用 mp-flutter 能透明接管的（`http`、`shared_preferences`）；不依赖 Cookie；不用原生插件。
- 登录页压在外壳上（push，成功 pop），不要 `go('/login')`：会在转场中重建外壳（Duplicate GlobalKey）。
- 日常验收：analyze、两道闸门、单测、Web 无头 e2e、各平台编译、mp-flutter 编译；真机只在发版前跑。
