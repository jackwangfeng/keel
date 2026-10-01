# Keel 买家端（Flutter）

同一套代码：Android / iOS / Web / 微信小程序（经 mp-flutter）。更早的 uni-app x 客户端已于 2026-09-30 下线，新功能都在这里接。代码注释里提到的 uni-app x 的文件（view.uts、App.uvue …）和样式名（.btn、.card …）都能在 tag `uniapp-final` 里找到。

## 跑起来

```bash
make flutter-get
make flutter-generate      # 契约改了之后：重新生成 lib/api/schema.g.dart 并一起提交
make flutter-analyze flutter-test
KEEL_API_BASE=http://192.168.0.110:18099/api/v1 make flutter-e2e-web   # Web 无头 e2e
KEEL_API_BASE=http://192.168.0.110:18099/api/v1 make flutter-build     # Web / apk / iOS 编译
KEEL_API_BASE=https://eshop.zzss.fun/api/v1 make flutter-build-mp      # 微信小程序
```

e2e 账号读 `~/.config/keel/e2e.env`（`KEEL_E2E_PHONE` / `KEEL_E2E_PASSWORD`，不进仓库）。
Android 编译找 SDK 的顺序：`ANDROID_HOME` → `~/Library/Android/sdk` → Homebrew 的 android-commandlinetools；JDK 17。

## 页面（第一阶段）

底部 4 个 tab：首页 / 购物车 / 订单 / 我的。

- 首页：门店行、搜索入口、分类、商品网格、「＋」原地加购（单规格直接加，多规格底部浮层）
- 搜索：按相关度排序，缺货的标出并压暗；效果回传（click / add_cart / order，trace_id 缺席整批不报）
- 商品详情：图片、特价划线、规格、加入购物车（「去结算 ›」）、立即购买、底栏购物车角标
- 购物车：勾选 / 全选、改数量（超库存显示服务端原因）、不能买的行写原因、预估运费、凑单提示、管理删除
- 结算：默认地址 / 选地址 / 新建地址，自动选最省的券（只一次）、金额拆行、各种问题照实说（送不到、限购、名额抢完、价格变了、券用不了、同键异体）
- 订单列表 / 详情：状态 + 售后状态、取消 / 确认收货（点两下）、沙箱支付（「模拟支付完成（沙箱）」）
- 地址簿：列表、新建（幂等键）、编辑（PUT 不切默认，切默认走专用接口）、设默认、删除；422 按字段标红

## 页面（第二阶段）

- 我的：头像与昵称、个人资料、消息（未读角标，也挂在「我的」tab 上）、收货地址、我的订单、售后 / 退款、我的优惠券、领券中心、服务地址、退出
- 领券中心 / 我的优惠券（未使用 / 使用中 / 已使用 / 已过期）
- 消息中心：分页、点一条标已读并跳订单 / 售后、全部已读
- 个人资料：昵称、性别（只带改了的字段）、第三方账号解绑（最后一个登录方式照实说）
- 服务地址：运行时换店（存本机；换了就退出上一家的登录）
- 售后：申请（选行与件数、仅退款 / 退货退款、原因、凭证图最多 9 张）、列表、详情（驳回理由与重新申请、寄回物流填 / 改、凭证图带令牌读、撤回）；订单详情里的售后区与「申请售后」

凭证选图：小程序走 `mp_flutter_wechat`（`wx.chooseMedia` → `wx.uploadFile`，临时文件路径直接上传）；Web / 原生走 `image_picker` 读字节再 multipart。

## e2e（Web 无头）

`integration_test/all_test.dart` 一个入口跑全部（`flutter drive` 每个 target 都要重编一次 App）。用例用 `e2e(...)` 包一层：
Web 上 drive 只报「有 N 个异常」，失败信息里会带第一个异常的原文。覆盖：冒烟、分类、搜索、首页加购、详情加购、购物车、
地址簿、下单付款、购物车结算、取消订单、运费（杭州 / 新疆 / 香港 / 省份写错 / 购物车预估）、限时特价与限购、满减、包邮券、
领券与我的优惠券、结算自动用券、消息中心（点进订单、全部已读）、售后申请与撤回、凭证图读取。
要后台前置状态的几条（驳回、已退款、待买家退货、已发货）读 `KEEL_E2E_REJECTED_REFUND` / `KEEL_E2E_REFUNDED_REFUND` /
`KEEL_E2E_RETURN_REFUND` / `KEEL_E2E_SHIPPED_ORDER`（请服务端会话造好单再传），没设就跳过。

## 小程序冒烟

```bash
KEEL_API_BASE=http://192.168.0.110:18099/api/v1 make flutter-build-mp
cd flutter_app/tool && npm install && cd -
NODE_PATH=flutter_app/tool/node_modules node flutter_app/tool/mp_walk.js   # 截图在 build/mp-shots/
```

开发者工具是和 mp-flutter 会话共用的：用之前先跟它打招呼，用完说一声。

## 安全：Web 端的登录令牌存在 localStorage

**现状**：`lib/api/session.dart` 把 access token、refresh token 和昵称放进 `shared_preferences`。各平台落到不同的地方：

| 平台 | 实际存储 | 风险 |
|---|---|---|
| Web（H5） | 浏览器 `localStorage` | **页面上任何能执行的脚本都读得到**。一旦有 XSS（比如注入的第三方脚本、被污染的依赖），令牌可以被整个读走，拿到别处冒用，直到它过期或被轮换 |
| 小程序 | `wx.setStorageSync`（mp-flutter 替换） | 只在小程序沙箱里，外部脚本读不到 |
| iOS / 安卓 App | 系统的偏好存储（NSUserDefaults / SharedPreferences） | 在 App 沙箱里；越狱 / root 设备上可被读取 |

每次请求由 `lib/api/client.dart` 带 `Authorization: Bearer <access token>`。401 时用 refresh token 换新的一对，服务端每次刷新都轮换 refresh token（旧的立即作废），并发的 401 只刷新一次。

**为什么现在这样取舍**

- 四个端共用一套鉴权代码：原生 App 和小程序没有浏览器 cookie，只能用 Bearer 令牌；Web 用同一套，代码路径最少、行为一致，测试也只要一套。
- 演示站是同源部署（页面和 `/api/v1` 同一个域名），不加载第三方脚本，页面上的文字都由 Flutter 画在画布上，不会把服务端数据当 HTML 插进页面。这降低了 XSS 的入口，但**不等于没有风险**。
- access token 有效期短（登录返回 `expires_in`），被偷走后能用的窗口有限；refresh token 一旦被轮换，旧的就失效了。

**以后换成 httpOnly cookie 要改的地方**（只改 Web，App 和小程序继续用 Bearer）

1. **服务端**（不在本目录）：登录 / 刷新接口在 Web 请求时改用 `Set-Cookie` 下发令牌，设 `HttpOnly; Secure; SameSite=Strict; Path=/api`；鉴权中间件同时接受 cookie 和 `Authorization`；刷新接口从 cookie 里读 refresh token；退出接口清掉 cookie。
2. **防 CSRF**：cookie 会被浏览器自动带上，写接口要加防护，比如校验 `SameSite`，再加一个自定义请求头（如 `X-Requested-With`）或双提交 token，服务端拒绝没有它的请求。
3. **`lib/api/session.dart`**：Web 上不再保存 access / refresh token（`kIsWeb` 分支），只记一个「已登录」标记和昵称；`loggedIn` 改为依据这个标记，或者启动时调一次 `GET /me` 判断。
4. **`lib/api/client.dart`**：Web 上不再设置 `Authorization` 请求头；同源请求浏览器会自动带 cookie。`_refresh()` 在 Web 上调刷新接口时不带 body，401 后的单飞重试逻辑保留。
5. **凭证图**（`lib/widgets/evidence_image.dart` 通过 `ApiClient.bytesOf` 带令牌取图）：Web 上改为直接按地址加载，由 cookie 鉴权。
6. **退出登录**：Web 上必须调服务端的退出接口，才能清掉 httpOnly cookie，前端自己删不掉。
7. **测试**：`test/client_test.dart`、`test/order_test.dart`、`test/review_fixes_test.dart` 里断言 `Authorization` 请求头的用例要按平台分开；Web e2e（`tool/e2e_web.sh`）里测试进程自己调接口用的是 Bearer，不受影响。

另外，原生 App 如果要更进一步，可以把令牌从偏好存储换到系统钥匙串（如 `flutter_secure_storage`），改动只在 `session.dart`。

## 规矩

- 契约字段只在 `lib/api/` 读写；`lib/pages/`、`lib/widgets/` 不许 import `schema.g.dart`（`scripts/check_flutter_pages.py`）。
- `schema.g.dart` 由 `scripts/gen_dart_schema.py` 生成，`scripts/check_dart_contract.py` 比对，请勿手改。
- 门店：先定位（最多等 5 秒，WGS-84）按围栏解析就近门店，拿不到回落默认店；`--dart-define=KEEL_LOCATE=off` 不定位（Web e2e 用它，用例按默认店写）。
- 地图选点（选择送货地址 / 地址编辑页的「在地图上选点」）：小程序用微信自带的 `wx.chooseLocation`（GCJ-02 转 WGS-84 再 reverse 补全）；App / Web 在服务端 `GET /geo/map` 开了底图时才给入口（一次会话问一次），打开 `lib/pages/map_picker_page.dart`——`flutter_map` 叠服务端代理的瓦片（`/geo/tiles/{layer}/{z}/{x}/{y}`，坐标 CGCS2000≈WGS-84，不做 GCJ 转换），中心图钉、停下 400ms 后 reverse 显示当前位置，确定返回省市区齐全的地点。`flutter_map` 在小程序包里会编进去（约 +120 KB dart），但入口不走它。
- 服务地址：原生与小程序编译期 `--dart-define=KEEL_API_BASE=...` 注入；Web 用相对路径 `/api/v1` 同源（开发 / e2e 走 `web_dev_config.yaml` 反代）。
- 依赖只用 mp-flutter 能透明接管的（`http`、`shared_preferences`）；不依赖 Cookie；不用原生插件。
- 登录页压在外壳上（push，成功 pop），不要 `go('/login')`：会在转场中重建外壳（Duplicate GlobalKey）。
- 日常验收：analyze、两道闸门、单测、Web 无头 e2e、各平台编译、mp-flutter 编译；真机只在发版前跑。
