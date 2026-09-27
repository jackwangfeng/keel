# Flutter 买家端（第一阶段）设计

日期：2026-09-27
状态：已与用户逐段确认，待审文档

## 1. 目的与范围

**目的**：用 Flutter 再做一套买家端，作为今后主要维护的客户端。配合正在开发的
mp-flutter（把 Flutter 工程编成微信小程序），一套代码覆盖 Android / iOS / Web / 微信小程序
（flutter_ohos 另可覆盖鸿蒙）。

**已确认的决定**

| 问题 | 决定 |
|---|---|
| 第一阶段范围 | 购物主链路：首页 / 分类 / 搜索 → 详情 → 登录 → 购物车 → 结算（地址、运费、券、活动）→ 下单 → 沙箱支付 → 订单列表 / 详情。跑通并能经 mp-flutter 编成小程序即第一阶段完成 |
| 第二阶段 | 领券中心 / 我的优惠券、售后（申请、详情、寄回物流、凭证上传）、消息中心、个人资料 |
| 现有 uni-app x 买家端（`app/`） | **冻结**：服务端的新接口只接到 Flutter；uni-app x 只修 bug、保持现有 e2e 能跑。Flutter 覆盖全部功能后再决定是否删除 |
| 视觉 | **沿用 uni-app x 那套**：暖米色底、深咖主色、卡片式布局、首字色块占位。色板与字号转成 `ThemeData` 与设计常量 |
| 契约与请求层 | **自写生成器**从 OpenAPI 生成 Dart 类型 + 手写薄请求层 + 闸门比对（与 Go / TS / UTS 三端同一做法） |

**不做（第一阶段）**：第二阶段那几块；Web 首屏包体优化；鸿蒙构建；真机 e2e（真机只在发版前跑）。

## 2. 整体结构

```
flutter_app/
  pubspec.yaml          依赖：http、shared_preferences、go_router；dev：flutter_test、integration_test、flutter_lints
  lib/
    main.dart           启动：读配置、恢复会话、MaterialApp.router
    config.dart         服务地址（编译期 --dart-define=KEEL_API_BASE；Web 默认相对路径 /api/v1）
    theme.dart          色板 / 字号 / 圆角（照 app/src/App.uvue）
    router.dart         go_router 路由表与底部 tab 外壳
    api/
      schema.g.dart     ★ 从契约生成，入库，闸门比对，请勿手改
      client.dart       请求层
      session.dart      会话（ChangeNotifier，持久化到 shared_preferences）
      store.dart        当前门店
      cart_count.dart   购物车件数（驱动角标）
      view.dart         契约类型 → 页面行模型
    widgets/            商品卡、价格、券行、底栏、空态、错误卡……
    pages/              每个页面一个文件
  test/                 单元测试
  integration_test/     端到端（Web，无头 Chrome）
  README.md
scripts/gen_dart_schema.py        生成器
scripts/check_dart_contract.py    契约闸门
scripts/check_flutter_pages.py    页面闸门：pages/ 与 widgets/ 下不许 import schema.g.dart
```

Makefile 新增：`flutter-generate`、`flutter-check`（analyze + 两道闸门）、`flutter-test`、
`flutter-e2e-web`、`flutter-build`（web / apk / ios --no-codesign）、`flutter-build-mp`。

**分层规矩**（与 uni-app x 相同）：契约字段只在 `api/` 里读写；页面只碰 `view.dart` 给出的行模型。
契约一改，报错集中在 `api/` 这一层，页面不会悄悄读错字段。

**SDK**：官方 stable（`~/development/flutter`，3.41.9）。PATH 上默认的是 flutter_ohos，
Makefile 显式用 `FLUTTER ?= $(HOME)/development/flutter/bin/flutter`，可覆盖。

## 3. 契约与请求层

### 3.1 生成器 `gen_dart_schema.py`

- 输入 `docs/电商系统-OpenAPI.yaml`。组件 schema 全量生成；接口侧（query 参数、内联请求 / 响应体）
  只生成清单里的操作。清单与 `gen_uts_schema.py` 共用（抽成两个脚本都 import 的模块）。
- 每个类型一个不可变类：字段 `final`，`fromJson(Map<String, dynamic>)` / `toJson()`。
- 映射：

| 契约 | Dart |
|---|---|
| integer / Money | `int`（金额单位「分」） |
| number | `double`（`num` 转换） |
| string（含 date-time） | `String`（时间在 view 层解析） |
| boolean | `bool` |
| array | `List<T>` |
| `$ref` | 被引用的类型 |
| `allOf` | 展平成一个类 |
| `additionalProperties: {type: string}` | `Map<String, String>` |
| `type: [X, 'null']` | `X?`，且 `toJson` 显式写 `null` |
| 非 required | `X?`，`toJson` 为 null 时**不写**这个键 |
| enum | 基础类型（与 UTS 相同，不生成 Dart enum：服务端加枚举值时旧客户端不崩） |
| 内联对象 | 提升成具名类型，命名规则与 UTS 一致（如 `ProblemErrorsItem`） |

- 清单里的操作在契约中消失 → 生成器当场失败（与 UTS 同）。
- `check_dart_contract.py`：重新生成到临时目录，与入库的 `schema.g.dart` 逐字比对；接入 `scripts/check-all.sh`。

### 3.2 请求层 `client.dart`

- 唯一出口 `send<T>(method, path, {query, body, idempotencyKey, decode})`：
  - 带 `Accept`、`Authorization`（有会话时）、`Idempotency-Key`（传了才带）；
  - 2xx：按 `decode` 解析；`Idempotency-Replayed: true` 一并返回；
  - 非 2xx：统一成 `ApiFailure { status, problem, message, retryAfter, fieldErrors, notImplemented }`，
    `message` 优先用 Problem 的 `detail`，否则 `title`。
- **401 且本机有会话、且路径不是 `/auth/`**：单飞刷新（进行中的其他请求排队），刷完用**原来的幂等键**
  重放一次；刷新失败或重放仍 401 → 清会话、跳登录页，登录成功后回到原页面。
- **幂等键**由页面生成并持有：同一次操作重试复用，内容变了换键（与 uni-app x 同）。
- **资源地址**：服务端给的相对路径（`/api/v1/uploads/..`）补上服务地址的 origin；服务地址本身是相对的（Web 同源）时原样用。
- 公开的「发了就不管」接口（搜索回传 `/search/events`）单独一个方法：不带令牌、错误一律吞掉。

### 3.3 平台差异

- **Web**：服务端不发 CORS 头，只能同源。服务地址默认相对路径 `/api/v1`；开发与 e2e 用本地反代
  （复用 `app/e2e/h5-serve.js` 的思路：托管 `build/web`，`/api` 反代到 `KEEL_API_BASE`，Host 换成服务端的）。
- **小程序（mp-flutter）**：没有页面 origin，服务地址必须是绝对地址（真机要求合法 HTTPS 域名，开发者工具可 `urlCheck:false`）。
  构建时 `--dart-define=KEEL_API_BASE=...` 注入。只用 mp-flutter 能透明接管的 `http` 与 `shared_preferences`；不依赖 Cookie。
- **Android / iOS**：绝对地址，同样经 `--dart-define` 注入；iOS 允许 http（演示栈）沿用现有 ATS 例外的做法。

## 4. 第一阶段页面与状态

底部 4 个 tab：首页 / 购物车 / 订单 / 我的。

| 页面 | 内容 |
|---|---|
| 首页 | 配送门店行、搜索入口、分类标签（根分类，拉不到就不显示）、商品卡网格（真图 / 首字占位、活动标签、价格与「起」、「＋」：单规格直接加购、多规格底部浮层选规格数量） |
| 搜索 | 搜索框、结果列表（完整价格区间、缺货标记并压暗）、搜索回传：结果点击 `click`，来自搜索的商品加购 `add_cart`、下单 `order`；`trace_id` 缺席时整批不报 |
| 商品详情 | 图片、价格（限时特价时显示特价并划线门店价）、规格选择、加入购物车（成功提示可点去结算）、立即购买、底栏购物车按钮带角标 |
| 登录 | 手机号 + 密码；登录后回到来源页 |
| 购物车 | 勾选 / 全选、改数量（超库存显示服务端原因）、不能买的行写原因且不能勾选、合计用 `selected_total_cents`、预估运费（没有地址时不显示）、凑单提示（服务端 `message` 原样）、去结算 |
| 结算 | 地址（默认地址；没有默认让用户选；一条都没有引导新建）、商品行、选券（首次试算后自动选最省的一张；之后以用户选择为准，含「不使用」）、金额拆行：商品 / 运费（带说明）/ 活动优惠 / 优惠券 / 运费抵扣 / 应付、提交 |
| 地址簿 | 列表（默认排第一）、新建（幂等键）、编辑（PUT 不切默认，切默认走专用接口）、设默认、删除；422 按 `errors[].field` 标红 |
| 订单列表 | 状态与售后状态组合显示、待支付可取消 |
| 订单详情 | 状态说明（已发货按 `auto_confirm_at`）、金额明细、沙箱支付（发起后底栏变「模拟支付完成（沙箱）」）、取消、确认收货（都是点两下确认） |
| 我的 | 登录状态、订单与地址入口、服务地址设置、退出 |

**错误处理规矩**（照搬现有、与服务端对齐过的）：

- 金额只显示服务端算好的数，客户端不复算运费、优惠、应付。
- 结算：
  - `coupon-not-applicable` 显示原因、展开券列表让用户换，不自动退回原价；
  - `region-not-deliverable` 把原因标在对应商品上，`province_unknown` 引导补全地址；
  - `promotion-limit-exceeded` 提示减数量；`promotion-sold-out` 作废试算、按现价重算、要用户重新确认；
  - `price-changed` 重算后要用户重新确认；
  - 幂等键 `in-flight` 按 `Retry-After` 退避重试；`reused` 让用户选「作为新订单提交」。
- 下单成功弹提示后跳订单详情；重放（`Idempotency-Replayed`）不跳，写明「之前已提交过」。

**全局状态**只有三个 `ChangeNotifier`：会话、当前门店（只解析一次：定位 → 解析门店 → 回落默认店）、
购物车件数。其余状态留在各页面。

## 5. 测试与验收

**三层测试**

1. **单元测试**（`flutter test`）：生成类型与真实响应样本往返无损；view 层的金额 / 本地时间 / 图片 origin；
   client 的错误映射、401 单飞续期与重放、幂等键复用（假 HTTP）。
2. **端到端**（integration_test，`flutter drive -d web-server --headless` + chromedriver，Web 版对接演示栈）：
   用 e2e 专用买家（账号口令读 `~/.config/keel/e2e.env`，与 uni-app x 的 e2e 相同）；按 widget `Key` 找元素。
   从 uni-app x 的用例移植第一阶段涉及的：冒烟、分类、搜索（含回传）、首页加购、购物车、地址、结算与运费、
   营销活动与包邮券、下单付款、取消 / 确认收货。后台前置状态沿用「造单 → 请服务端代做」。
3. **编译闸门**（每轮验收都过）：`flutter analyze`、契约闸门、页面闸门、`flutter build web / apk / ios --no-codesign`、
   经 mp-flutter 编小程序。真机只在发版前跑。

**交付顺序**（每步可单独验收、提交）

1. 骨架：工程、主题、生成器与闸门、请求层、会话、登录、首页读商品；打通「契约 → 页面 → 测试 → 小程序编译」。
2. 浏览：分类、搜索（含回传）、商品详情、「＋」加购。
3. 购物车、地址簿。
4. 结算、下单、沙箱支付、订单列表 / 详情。
5. 收尾：e2e 移植齐、小程序开发者工具里走一遍主链路、README。

**第一阶段完成的标准**：上面的 e2e 在无头 Web 上全过；编译闸门全过；小程序在开发者工具里能走完
「浏览 → 加购 → 结算 → 下单 → 沙箱支付」。

## 6. 风险

- **mp-flutter 仍在开发**：第 1 步一编出小程序就在开发者工具里跑。发现 mp-flutter 的问题，记下来交给那边，不在 App 里绕开。
- **Flutter Web 用 canvas 渲染**：没有 DOM，uni-app x 那套 automator 用不上，e2e 改用 integration_test；两套用例第一阶段并存。
- **Web 首屏包体偏大**：只影响开发体验，第一阶段不优化。
