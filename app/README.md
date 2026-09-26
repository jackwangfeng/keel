# Keel 买家端（uni-app x）

用 [uni-app x](https://doc.dcloud.net.cn/uni-app-x/) 写的买家端：UTS 编译成原生
Kotlin / Swift，不走 webview；同一份代码也编 H5 与小程序。

- 页面流：商品列表 / 搜索 → 商品详情 →（加入购物车 → 购物车）→ 下单（选地址 → 试算 → 提交）
  → 我的订单 → 订单详情 → 发起支付。tabBar：首页 / 购物车 / 订单 / 我的。
- 首页有一排分类 chip（`GET /categories` 的根分类，点了按 `category_id` 重拉列表）。拉不到分类
  （接口挂了）就整排不显示，首页照常。
- 搜索走 `POST /search`（语义 + 关键词混合检索）。一期不翻页；服务端按相关度把召回到的都排出来、
  不设阈值，所以不相关的商品会排在后面而不是消失。`in_stock_only` 默认 false：缺货商品也返回、
  排在最后，搜索页给它挂「缺货」标并压暗。
- 搜索效果回传（`POST /search/events`，公开接口、不带令牌和幂等键）：搜索页把 `trace_id` 和这批结果
  存在一起；**从搜索结果点进去的**商品回传 `click`，之后这件商品被加购回传 `add_cart`、被下单回传
  `order`（`src/api/search-trace.uts` 记 productId → traceId，只在内存里，最近一次为准）。
  `trace_id` 缺席时这批结果什么都不报。回传发了就不管：所有错误忽略、不重试、不打断业务。
- 收货地址：下单页用 `GET /addresses` 的默认地址（排第一且 `is_default`）；没有默认就让用户选，
  一条都没有就引导去新建。地址簿页：新建（`POST`，带幂等键）、编辑（`PUT`，**不**切默认）、
  删除、设默认（`PUT …/default`，专用接口）。422 按 `Problem.errors[].field` 标红对应输入框。
  删掉默认地址后服务端不自动补一个默认，页面照实提示。
- 购物车：每条请求都带与商品页 / 下单页同一个 `store_id`。不能买的行（`off_shelf` /
  `not_sold_in_store` / `out_of_stock` / `insufficient_stock`）列出原因、不能勾选、不进合计。
  合计用服务端的 `selected_total_cents`；去结算时结算页按同一家店重读购物车，把能买且勾选的行
  送 `/orders/preview`，下单成功后把这几行从车里删掉。
- 订单后半程：待支付的单能取消（详情页与列表都有，「点两下」防误触），已发货的单能确认收货。
  申请售后：选哪几行、各退几件（可退 = 购买 − 已退 − 在途）、类型（仅退款 / 退货退款，**不给默认值**）、
  原因；不传金额，每行实退多少看售后单里服务端算好的 `items[].amount_cents`。售后详情：30「退款中」
  写成「正在原路退回」而不是失败；50「已拒绝」显示 `reject_reason` 并可重新申请；10 / 20 可撤回。
  「我的」→ 售后 / 退款列表。未发货的单只能申请仅退款（服务端约束），「退货退款」置灰并写明原因。
- 售后凭证：申请页从相册选图（`uni.chooseImage`，只开相册，免相机权限），逐张 `POST /uploads`
  （multipart，purpose=3，带令牌与幂等键），返回的 `/api/v1/uploads/{id}` 原样放进 `evidence_urls`。
  读凭证要带令牌（本人 302 到限时地址，别人 403），`<image>` 带不了头，所以售后详情先
  `uni.downloadFile` 带 Authorization 取成本地文件再显示。
- 退货寄回物流：退货退款且 20 待买家退货时，售后详情里选承运商、填运单号
  （`POST /refunds/{no}/return-shipment`），20 期间可以改；填完仍是 20，商家确认收到才到 30。
- 营销活动：列表 / 搜索 / 详情显示活动标签（服务端的 `label` 原样）；详情与购物车里限时特价按特价显示、
  划线门店价。结算页优惠拆成活动优惠 / 优惠券 / 运费抵扣三行，下面列服务端给的活动说明（`message` 原样，
  「已减 20 元，再买 161 元可减 50 元」），不自己算凑单差额；购物车显示已减金额与凑单说明。
  409 `promotion-limit-exceeded` 提示减数量；`promotion-sold-out` 作废试算、自动按现价重算一次，要用户重新确认，
  不自动重下。订单详情列出命中活动的快照，优惠写成「优惠合计」（订单上没有单独的券抵扣额，不做减法去推）。
- 运费：结算页直接显示服务端的 `freight_cents`（不自己算），旁边一句来自 `freight.groups` 的说明
  （「满¥99 包邮」「已满¥99 包邮」「首件¥15，续件¥5」），用了包邮券时多一行运费抵扣，应付是 `payable_cents`。
  422 `region-not-deliverable` 按 `undeliverable_items` 逐行标在商品上；`province_unknown` 时给「去补全地址」。
  购物车的接口都带 `address_id`（结算页传选中的地址，购物车页不传即默认地址），显示预估运费与送不到的行；
  **没有地址时服务端不给 `freight`，页面不显示运费，不写成「包邮」**。订单详情显示运费、运费抵扣与说明（老订单没有快照就不显示说明）。
- 消息中心：「我的」→ 消息（`GET /me/notifications`），标题 / 正文原样显示（模板在服务端，不按 kind 拼），
  点一条标已读（`POST …/read`，不带幂等键）并按 `target` 跳订单详情或售后详情，「全部标为已读」。
  「我的」tab 挂未读角标（`src/api/badge.uts`，进首页 / 我的页时查 `unread-count`，标已读后用响应里的新未读数）。
  只有站内，微信订阅消息 / 短信没接。
- 自动确认收货：已发货的订单详情按服务端的 `auto_confirm_at` 写「MM-DD HH:mm 未确认将自动确认收货」
  （老服务端没有这个字段时退回「发货 7 天」）。待买家退货且没填物流时按 `return_deadline_at` 提示寄回截止，
  逾期售后自动关闭。
- 时间显示：接口时间一律是 UTC 的 RFC3339，`shortTime` / `day` 先解析成时刻、再按设备本地时区显示
  （早先直接截字符串，北京时间的买家看到的全早 8 小时，2c9247a 修）。
- 个人资料：`GET/PATCH /me`（昵称、性别），第三方账号列表与解绑（409 `last-credential` 提示先设密码）。
  绑微信、换绑手机号服务端回 501，页面写「暂未开通」，不放必失败的按钮。
- 优惠券：「我的」→ 领券中心（`GET /coupon-templates`，领券 `POST …/claim` 带幂等键，同一张模板在
  页面活着期间复用同一个键）/ 我的优惠券（`GET /coupons`，未使用 / 使用中 / 已使用 / 已过期四个 tab，
  「使用中」是锁在待支付订单上的）。结算页用试算响应里的 `applicable_coupons`：第一次试算后自动选
  最省的那张再算一次，之后以用户的选择为准（包括「不使用」）。券用不了（409 `coupon-not-applicable`）
  时显示服务端给的原因并展开列表让用户换，**不**自动退回原价。列表上「省多少」用
  `applicable_discount_cents`，不是券面额。订单详情显示用的哪张券、优惠多少。

---

## 为什么是 `app/` 而不是 `client/`，以及为什么不放进 `web/`

**不放进 `web/`**：那个目录是浏览器侧 SDK 的地盘，而且 `make schema-check` 的编译范围
就是 `web/src`（`scripts/check_ts_scope.py` 还会断言那个范围覆盖到每一个文件）。
把 `.uts` / `.uvue` 混进去，要么被那道闸门拒收，要么逼着它放宽范围——后者等于把
一道已经在防「假绿」的闸门重新变松。

**叫 `app/` 不叫 `client/`**：`client/` 太泛，README 的 Clients 一节里 admin 控制台
同样是 client，将来它进来时这个名字会打架。而 uni-app x 的立身之本就是原生 App
（H5 与小程序是顺带的目标），`app/` 说的就是这件事。

---

## 跑起来

需要后端起着：

```bash
# 仓库根目录
export KEEL_HTTP_PORT=18080        # 宿主机 8080 常被占
docker compose up -d --build
./scripts/smoke.sh
```

然后：

```bash
make app-install        # 装依赖（不要直接 npm ci，理由见下）
make app-build-h5       # 编 H5，产物在 app/dist/build/h5
```

H5 形态**必须同源访问**：服务端一个 CORS 头都不发（实测 `OPTIONS /api/v1/products`
回 405），浏览器里跨源根本打不通。所以默认的服务地址是相对路径 `/api/v1`，
由一个反向代理把 `/api` 转给 Keel。开发期那段代理写在 `vite.config.js` 里
（读同一个 `KEEL_HTTP_PORT`）。

原生 App 没有 origin 这回事，要在 App 的「服务地址」页填一个绝对地址。

---

## 两项实测结论

下面两节是这个目录存在之前先做的调研，结论是实测出来的，不是查文档抄的。
实测环境：Ubuntu 24.04 / Node 24.10.0 / npm 11.6.1 /
`@dcloudio/*` 的 `3.0.0-5020620260917001` 那条线（对应 HBuilderX 5.2.6）。

### 一、UTS 吃不下现有的 TS 契约产物，另生成一份

**结论：`web/src/api/schema.d.ts` 与 `web/src/api/client.mts` 在 UTS 里都用不了。**

`client.mts` 那一整套类型体操（`Slot<K, Raw>`、`PathsWith<M>`、`MethodsOf<P>`、
`ResponseBodyOf<P, M>`）建立在**映射类型 + 条件类型 + `keyof` 索引**之上。
UTS 的类型系统要能落到 Kotlin 与 Swift，那边没有对应物：一个
`export type Foo = { ... }` 在 Kotlin 侧是一个 `open class Foo(...) : UTSObject()`
（可以直接在 `app/dist/build/app-android/.uniappx/android/src/index.kt` 里看到），
而「对 `paths` 做索引再分配到联合上」没有任何东西可以生成成。

`schema.d.ts` 本身（不带 SDK）的核心是一个巨大的 `paths` 接口 + 索引签名，
同样落不到名义类型上。

所以走的是这个仓库已经用了三次的老路：**同一份契约，另生成一份产物，产物入库，
闸门比对**。生成器是 `scripts/gen_uts_schema.py`（Python + PyYAML，
和 `scripts/check_openapi.py` 同一套依赖，没有引入新的 npm 生成器要钉版本），
产物是 `app/src/api/schema.uts`。

映射规则与理由写在生成器的文件头。几条值得单独说的：

| 契约 | UTS | 为什么 |
|---|---|---|
| `allOf` | 展平成一个对象类型 | UTS 没有交叉类型，Kotlin 那边也没有 |
| `additionalProperties` | `UTSJSONObject` | 名义类型系统里没有索引签名 |
| `enum: [a, b]`（字符串） | `string` | 字面量联合 UTS 支持，但契约里那几个 enum 服务端并不强校验，收窄了反而会让合法响应解析失败 |
| 非 required | `field?: T` | UTS 下是 `T \| null`，tsc 下是 `T \| undefined` —— 两边都成立，但函数签名要写成**可选参数**才同时兼容，见 `view.uts` 里 `yuan()` 的注释 |

**展平 `allOf` 有一个必须记住的后果**：`OrderDetail` 在契约里是
`allOf: [Order, {...}]`，展平之后它和 `Order` 是两个没有继承关系的类型。
TS 的结构化类型下 `orderRow(orderDetail)` 能过，UTS 落到 Kotlin 之后不能过。
`view.uts` 里 `orderDetailView` 因此重新取了一遍字段而不是复用 `orderRow`。

#### 不漂移靠什么

两道闸门，缺一不可：

1. **`scripts/check_uts_contract.py`**（在 `scripts/check-all.sh` 里）
   把契约重新生成到临时目录再 diff，对工作区只读。挡的是
   「契约改了却没重生成」与「有人手改了生成文件」。
2. **`scripts/check_app_types.py`**（同样在 `check-all.sh` 里）
   用 `tsc --strict` 编译 `app/src` 下全部 `.uts`。挡的是
   「重生成了，但客户端还在读那个已经改了名的字段」。

只有第一道，客户端可以一直读一个不存在的字段；只有第二道，谁手改一下生成文件
就能把红色抹掉。

**变异验证**（把契约里 `ProductSummary.min_price_cents` 改名成 `min_price_cents_v2`，
不动客户端一行代码）：

```
$ python3 scripts/check_uts_contract.py ; echo exit=$?
FAIL: app/src/api/schema.uts 与契约重生成的结果不一致。
      契约改了却没重生成，或者有人手改了生成文件。请跑 `make generate-uts` 并一起提交：
-  min_price_cents : Money
+  min_price_cents_v2? : Money
exit=1

$ make generate-uts && python3 scripts/check_app_types.py typescript@5.9.2 ; echo exit=$?
app/src/api/view.uts(69,17): error TS2551: Property 'min_price_cents' does not exist on type 'ProductSummary'. Did you mean 'min_price_cents_v2'?
app/src/api/view.uts(130,23): error TS2551: Property 'min_price_cents' does not exist on type 'ProductDetail'. Did you mean 'min_price_cents_v2'?
exit=2
```

同一个变异下，**DCloud 自己的 UTS 编译器是绿的**（见下一节最后一段）。
这就是为什么契约漂移的判据放在 tsc 那一条，而不是「反正真编译器会编一遍」。

#### tsc 这道闸门**盖不到**哪里

`.uvue` 里的模板表达式。tsc 不解析 SFC。客户端为此把契约字段的读取全部收进
`app/src/api/view.uts`，模板只碰那里定义的 `XxxRow` 类型——但这是一条**靠人守的
约定，不是闸门**。有人在模板里直接写 `{{ item.min_price_cents }}`，没有任何东西
会拦住他。

### 二、CLI 能构建到什么程度

**能：H5 与「UTS → Kotlin」，都不需要 HBuilderX。不能：apk / ipa。**

| 目标 | CLI | 产物 | 类型检查 |
|---|---|---|---|
| H5（web） | ✅ `uni build --platform h5` | `dist/build/h5/*.html/js/css`，直接能发 | ✅ 跑 tsc |
| Android | ✅ `uni build --platform app-android` | `dist/build/app-android/.uniappx/android/src/**/*.kt` —— **Kotlin 源码** | ❌ 不跑 |
| Android apk | ✅ `make app-apk`（离线 SDK + Gradle，见下文「本地打 apk」） | `dist/keel-buyer-<版本>.apk` | 经 app-android 那一格 |
| iOS | ✅ `make app-ios`（见下文「本地打 iOS 包」） | `dist/ios-device/KeelBuyer.app` | 经 check_app_build |
| 微信小程序 | ✅ `make app-build-mp-weixin` | `dist/build/mp-weixin`，微信开发者工具直接打开 | 经 check_app_build |

「Android 到 Kotlin 为止」这件事值得说清楚：`uni build --platform app-android`
在一台没有 HBuilderX、没有 Android SDK、没有 JDK 的机器上跑得通，它输出的是
每个页面一个 `.kt`：

```kotlin
open class ProductSummary (
    @JsonNotNull open var id: Number,
    @JsonNotNull open var title: String,
    open var subtitle: String? = null,
    ...
    @JsonNotNull open var min_price_cents: Money,
) : UTSObject()
```

从这堆 Kotlin 到一个能装的 apk，需要 HBuilderX（本地打包）或 DCloud 云打包 ——
那一步 CLI 做不到，也没有对应的 npm 包。**所以 CI 里跑的就是上面这张表里
打 ✅ 的两格，不多不少**（`.github/workflows/ci.yml` 的 `client` job）。

#### 路上踩到的五个坑（都是实测，修法都在仓库里）

1. **`npx degit dcloudio/uni-preset-vue#uni-app-x` 用不了** ——
   那个仓库现在没有 `uni-app-x` 分支（只有 alpha / vite / vite-ts / vue3 / master，
   `template/` 下是 `default`、`default-ts`、`common`、`common-ts`）。
   项目只能自己搭，`package.json` 的那一套包和版本是逐个试出来的。

2. **CLI 不会自己挂 uni 插件** —— `node_modules/.bin/uni` 只是带着一堆 `UNI_*`
   环境变量去跑 vite；`vite.config.js` 里必须有 `uni()`。没有它，报错是
   「.uvue 里有非法 JS 语法」，那句话不指向真因。

3. **npm 11 会跳过 UTS 的原生编译器** ——
   `@dcloudio/uts-linux-x64-gnu` 的 package.json 写 `"libc": ["gnu"]`，
   而 npm 11.6.1 报的本机 libc 是 `glibc`，于是判成「本平台不支持」直接跳过
   （可选依赖，不报错）。`--force` 与 `--libc=gnu` 都不行。
   少了它，`uni build` 死在 `Cannot find module '@dcloudio/uts-linux-x64-gnu'`。
   修法在 `app/scripts/install-deps.sh`：装完自己核对，缺了就 `npm pack` 取
   tarball 解开（版本取自 `@dcloudio/uts` 自己的 optionalDependencies）。

4. **UTS 的语言内建类型不在任何 npm 包里** —— `UTSJSONObject`、
   带类型参数的 `JSON.parse<T>()`，HBuilderX 是通过一个只有 IDE 里才存在的虚拟模块
   喂给 tsc 的。CLI 下它们解析不到，构建会刷出一片 `Cannot find name 'UTSJSONObject'`
   与 `Expected 0 type arguments, but got 1`（不影响产物，只影响类型检查）。
   补声明在 `src/uts-builtin.d.ts`。

5. **H5 的 main 会被字符串替换** —— `@dcloudio/uni-h5-vite` 把 main 里
   **第一次**出现的 `createSSRApp` 替换成 `createVueApp as createSSRApp`。
   写成 uni-app x 文档里那种「`createSSRApp` 是全局、不用 import」的形式，
   替换会落到 `const app = createSSRApp(App)` 上，编出
   `main.uts:4:43 - error TS1005: ',' expected.` ——一个源码里根本不存在的列。
   所以 `src/main.uts` 必须写成 `import { createSSRApp } from 'vue'`。
   替换后引用的 `createVueApp` 在 vue 的类型里没有，补在 `src/vue-shim.d.ts`
   （**必须是模块增强**；写成全局 `declare module 'vue'` 会整个顶掉 vue 的类型声明，
   实测代价是 104 条诊断）。

#### 类型检查：`uni build` 自己**不会**因为类型错误失败

这条最要紧。本机实测：在一个 `.uvue` 里写 `const s: string = 1`，输出里出现

```
warning: Type 'number' is not assignable to type 'string'.
at pages/index/index.uvue:8:12
DONE  Build complete.
```

**退出码 0。** 直接把 `uni build` 接到 CI 上等于接了一个永远不会红的闸门 ——
比没有更糟，它还会让人放心。`scripts/check_app_build.py` 因此解析输出，
把任何 `warning:` / `error TS` 都判成失败。

而且它的诊断有洞：**访问一个不存在的属性不报**。实测在一个 `type` 上写
`p.totally_bogus_field`，`uni build --platform h5` 照样 exit 0。
契约改名产生的正是这一类错误——所以那道闸门在 `check_app_types.py`（tsc）那边。

#### `uni dev --platform h5` 在本机起不来

开发服务器把入口 `/src/main.uts` **原样**发出去（content-type 为空、没经过
transform），Chrome 拒绝执行 module script，页面白屏。没有继续深究，
因为验证与 CI 用的都是构建产物。要看效果就 `make app-build-h5`，
再用任意一个「静态目录 + `/api` 反代」的服务器发出去。

---

## 本地打 apk

```bash
KEEL_API_BASE=http://192.168.0.110:18099/api/v1 make app-apk
# -> app/dist/keel-buyer-0.1.0.apk
```

需要 JDK 17 和 Android SDK（`platforms;android-36`、`build-tools;36.0.0`）。
Homebrew 装法：`brew install --cask android-commandlinetools`，再用 `sdkmanager` 装上面两项。
离线 SDK（80MB）第一次跑时自动下载到 `native-android/.uni-sdk/`，钉了版本与 sha256。

**不走 HBuilderX。** DCloud 的正规本地打包要先在 HBuilderX 里「生成本地打包App资源」，
但实测 HBuilderX 5.26 用 CLI 导入这个项目就崩（最小副本也崩）。离线 SDK 的 Demo 工程
说明了资源的真实形状：页面就是 `uniappx/src/main/java/` 下的 `.kt`，和 npm 版
`uni build --platform app-android` 的产物是同一种东西。所以 `build-apk.sh` 直接把
`dist/build/app-android/` 拷进 `native-android/`，再交给 Gradle。

几件值得知道的事：

- **默认服务地址在编译期注入**（`KEEL_API_BASE`），源码里没有写死任何地址，见
  `src/api/native-default.uts`：`vite.config.js` 把它放进 `define` 的 `process.env.KEEL_API_BASE`，
  DCloud 编译器会同时交给 UTS → Kotlin（Android）与 JS（iOS、H5）两条路；H5 永远是空串。
  （最早的做法是在拷进原生工程的 .kt 上做文本替换 —— iOS 的产物是压缩过的 JS，常量已被
  折叠成 `"/api/v1"`，那条路走不通，才找到了这个两端通用的正规入口。）
- **aar 只挑用得到的**（`native-android/settings.gradle` 的 `uniAars`），不是 SDK 里的
  全部 135 个。编译器在 `manifest.json` 的 `app-android.distribute.modules` 里列出代码
  实际用到的 uni 模块，脚本会核对每一个都在清单里——漏一个，apk 照样打得出来，
  只会在调用那个 API 时在真机上崩。
- **`uni.request` 的 `data` 在 Android 上不收契约类型的对象。** 传 `LoginRequest` 这类
  typed 对象，Android 直接走 fail：`errCode 600008 the data parameter type is invalid`；
  H5 上一切正常，于是症状是「GET 全通、所有 POST 全挂」。`client.uts` 的 `send()` 因此
  先 `JSON.stringify` 再交出去。真机（小米 / Android 15）上实测过登录 → 试算 → 下单 →
  支付 → 沙箱入账整条链路。
- **`make app-build-android` 绿 ≠ Kotlin 编得过。** 它只跑到「UTS → Kotlin 源码」为止。
  实测在 `uni.request` 的 `fail` 回调里引用外层函数的参数，UTS 编译器是绿的，Kotlin
  编译报 `Unresolved reference`。真正的 Kotlin 编译只在 `make app-apk` 里发生。
- **release 包里 `console.log` 进不了 logcat**（走的是调试服务器，那个模块没打进包）。
  真机排错时把信息显示在页面上，比如网络失败的提示后面带着 `［errCode errMsg］`。
- **签名是 debug 证书**，能装能测，不能上架。正式证书还没有。
- **明文 HTTP 是开着的**（`usesCleartextTraffic`），因为开发期地址是局域网 http。上线换
  HTTPS 后要关。

## 微信小程序

```bash
KEEL_API_BASE=http://192.168.0.110:18099/api/v1 make app-build-mp-weixin
# -> app/dist/build/mp-weixin，用微信开发者工具打开
/Applications/wechatwebdevtools.app/Contents/MacOS/cli open --project "$PWD/app/dist/build/mp-weixin"
```

- uni-app x 从 4.41 起支持微信小程序（我们是 5.26）；支付宝小程序标的是 5.31 起，其余小程序平台不支持
  （以编译器自带的 API 类型声明为准）。
- AppID 在 `src/manifest.json` 的 `mp-weixin.appid`。**必须是小程序类型的 AppID**：拿一个小游戏的 AppID
  开发者工具会进「小游戏模式」、模拟器黑屏、日志里 `gameLaunch error`，连空白小程序都跑不起来（实测）。
- 接口地址：小程序和原生 App 一样没有页面 origin，`KEEL_API_BASE` 编进包里（`vite.config.js`）。
  局域网 HTTP 地址只能在开发者工具里跑（`setting.urlCheck: false`）；真机预览与上线要 HTTPS 域名、
  在小程序后台配成 request 合法域名。
- 定位：`manifest.json` 里声明了 `scope.userLocation` 与 `requiredPrivateInfos: ["getLocation"]`，
  首次进首页会弹授权框。拒绝时回落默认门店，和 App 同一条路径。
- 实测（开发者工具 2.02 模拟器，`miniprogram-automator` 驱动，定位 mock 成失败）：首页回落默认门店、
  20 件商品；登录；结算页带出默认地址并试算出应付；购物车；搜索。这些是一次性的手动验证，
  **没有**做成 `app/e2e` 那样的用例。
- 还没有的：微信登录（服务端绑微信回 501，现在用手机号 + 密码）、微信支付（沙箱那套是 App 的）。

## 本地打 iOS 包

```bash
KEEL_IOS_TEAM=2Q89DQSSH6 KEEL_API_BASE=http://192.168.0.110:18099/api/v1 make app-ios
# -> app/dist/ios-device/KeelBuyer.app（加 KEEL_IOS_INSTALL=1 顺手装到 USB 连着的 iPhone）
```

需要 Xcode 与 XcodeGen（`brew install xcodegen`）。离线 SDK（866MB）第一次跑时自动下载，
只取需要的五个 xcframework 放到 `native-ios/.uni-sdk/`。

**iOS 版也是原生界面，只是页面逻辑的跑法和 Android 不同**：UTS 编译成 JS
（`app-service.js`），跑在系统的 JavaScriptCore 里，界面由 SDK 的原生渲染运行时画
（自带 flexbox 布局引擎，输入框是 UITextView）—— 和 React Native 一个路数。所以 iOS 不编译
我们的代码，`uni build --platform app-ios` 的产物直接作为资源放进 `native-ios`。

几件实测出来的事：

- **`uni.request` 在 iOS 上不理 `dataType: 'text'`**，JSON 响应照样被解析成对象。`client.uts`
  的 `responseText()` 把它序列化回文本再按类型解析；否则所有请求都报「响应体解析失败」。
  这条是经自动化在真机上读出 `typeof res.data === 'object'` 定位的。
- **SDK 的场景代理会按名字加载宿主的 `Main.storyboard`**（文档没写）。没有这个文件启动即崩，
  所以 `native-ios` 里有一个只含空页面的 `Main.storyboard`。
- **没有模拟器版本**：`DCloudUTSExtAPI` 的模拟器切片只有 x86_64，iOS 26 模拟器不收 x86_64。
- **签名是 Automatic**：Xcode 没登录也行，只要本机有该团队覆盖这台设备的描述文件；手动签名
  反而拒绝 Xcode 管理的通配描述文件。
- `.xcodeproj` 由 XcodeGen 从 `native-ios/project.yml` 生成，不入库；`Info.plist` 用
  `INFOPLIST_FILE` 引用（XcodeGen 的 `info:` 会重新生成它，把手写的键抹掉）。

## 这一版真的跑通了什么

对着 `docker compose up -d --build` 起来的真后端（`KEEL_HTTP_PORT=18080`），
H5 构建产物 + 一个把 `/api` 反代给 Keel 的静态服务器，用无头 Chrome 走了一遍。
**没有任何 mock**：每一行都是真请求真响应。

| 页面 / 动作 | 接口 | 结果 |
|---|---|---|
| 商品列表 | `GET /products` | ✅ 种子商品、价格区间、总数（M3 把种子从 3 件加到 5 件，多的两件是给检索用的「字面不像、意思相近」对照） |
| 商品详情 | `GET /products/{product_id}` | ✅ 标题 / 副标题 / SKU（`MUG-2` ¥49.00，库存 43） |
| 登录 | `POST /auth/login` | ✅「登录成功：示例买家」 |
| 退出登录 | `POST /auth/logout` | ✅ |
| 下单试算 | `POST /orders/preview` | ✅ 应付 ¥49.00 |
| 提交订单 | `POST /orders` + `Idempotency-Key` | ✅ 返回 order_no |
| 幂等重放 | 同键同体再提交 | ✅ 201 + `Idempotency-Replayed: true`，页面说「这是上次那单，本次没有真的执行任何业务动作」 |
| 同键异体 | 改数量、重新试算后再提交 | ✅ 422 `idempotency-key-reused`，页面给「换一个新键」的按钮 |
| 换键重下 | 换键 + 提交 | ✅ 新的 order_no |
| 订单详情 | `GET /orders/{order_no}` | ✅ 状态、收货快照、明细、支付记录 |
| 发起支付 | `POST /orders/{order_no}/payments` | ✅ 沙箱调起，页面用黄框显式标注「沙箱支付，没有真实资金流动」 |
| 沙箱入账 | `POST /webhooks/payments/{channel}` | ✅ 把服务端签好名的 `payload.settle` 原样投回去，订单 `待支付 → 已支付`、`已付 ¥98.00`、支付记录出现「微信 / 成功」 |
| 我的订单 | `GET /orders` | ✅ 分页列表，状态与售后状态组合渲染 |
| 换服务地址 | — | ✅ 本地令牌当场清掉 |
| 搜索 | `POST /search` | ✅ 首页入口 → 搜索页，按相关度排序；Android / iPhone 真机 e2e 覆盖（按首页一件商品的标题搜，它排第一，点进去是它的详情） |
| 收货地址 | `GET/POST/PUT/DELETE /addresses…`、`PUT …/default` | ✅ 小米真机 e2e（`address.test.js`）：列表与服务端一致、默认排第一；错的表单按 `errors[].field` 标红两项，改对后新建成功；结算页自动用默认地址 |
| 购物车 | `GET /cart`、`POST /cart/items`、`PATCH /cart/items/{id}`、`POST …/batch-delete` | ✅ 小米真机 e2e（`cart.test.js`）：加购 → 调数量，合计与服务端 `selected_total_cents` 一致 → 去结算，试算商品金额 = 购物车合计 → 下单后这行从车里删掉。调大超库存时页面显示服务端 409 的原因（调试时实测） |
| 售后凭证 / 寄回物流 | `POST /uploads`、`GET /uploads/{id}`、`POST /refunds/{no}/return-shipment` | ✅ iPhone 真机 e2e（`aftersale.test.js`，这轮小米锁屏）：带凭证的售后单在详情里带令牌读出凭证图；待买家退货填物流（顺丰）→ 服务端一致、仍是 20 → 改成京东 → 服务端跟着变；未发货的单申请页不能选退货退款。**App 内从相册选图并上传没有自动化**（相册选择器无法由自动化驱动），凭证由测试进程上传 |
| 营销活动 / 包邮券 | `promotion_tags`、`promo_price_cents`、试算与购物车的 `promotions` / `promotion_discount_cents`、409 `promotion-limit-exceeded`、包邮券（CouponType 4） | ✅ H5 无头 e2e（`promotion.test.js`）：标签、特价划线、限购 3 件被拒、满 199 减 20 与凑单说明、包邮券在 99 以下自动选上抵 8 元、已包邮的单不出现包邮券、选着包邮券加到满 99 时显示「包邮券抵不了钱」并展开券列表；购物车划线与已减金额。**原生渲染留待发版前真机验证**（这轮 iPhone 状态不稳，promotion 在 iPhone 上的结果不可信，见提交说明） |
| 运费 | `/orders/preview` 的 `freight_cents` / `freight`、422 `region-not-deliverable`、`Cart.freight`（`address_id`） | ✅ iPhone 真机 e2e（`freight.test.js`）：杭州 99 以下 8 元「满¥99 包邮」、以上「已满¥99 包邮」0 元；新疆 15 元「首件¥15，续件¥5」；香港送不到、原因标在商品上；省份写错引导补全地址；购物车按默认地址显示预估运费。包邮券没有 e2e（演示买家没有包邮券） |
| 消息中心 | `GET /me/notifications`、`unread-count`、`POST …/{id}/read`、`read-all` | ✅ H5 无头 e2e（`notifications.test.js`，这轮小米、iPhone 都锁屏）：付款后出现「支付成功」、标题正文原样、点进去是那笔订单且服务端标成已读；全部已读后未读 0。售后驳回的通知点进去是那张售后单（后台代审产生 refund_rejected）。**tab 角标是原生 API，H5 覆盖不到；Android / iOS 只验证了编译** |
| 搜索效果回传 | `POST /search` 的 `trace_id`、`POST /search/events` | ✅ iPhone 真机 e2e（`searchtrace.test.js`，这轮小米锁屏）：结果带 trace_id；点进商品后测试进程对同一对 trace_id + 商品重发 click 得 204（服务端认这对）；加购、购物车结算、下单照常成功。服务端是否记下三条事件由服务端按 trace_id 查日志核对 |
| 取消 / 售后 | `POST /orders/{no}/cancel`、`POST /orders/{no}/refunds`、`GET /refunds/{no}`、`POST /refunds/{no}/cancel` | ✅ 小米真机 e2e（`aftersale.test.js`）：待支付单取消 → 已关闭；已支付单申请仅退款 → 待审核（页面金额 = 服务端算的 `amount_cents`）→ 撤回 → 已取消。驳回（显示 `reject_reason`、可重新申请）/ 同意仅退款（已退款，整单全退的订单走到 60 已退款、详情显示已退金额）/ 发货后确认收货 → 已完成：前置状态由服务端的后台会话代做，单号经 `KEEL_E2E_*` 环境变量交给用例 |
| 个人资料 | `GET/PATCH /me`、`GET /me/identities` | ✅ 小米真机 e2e（`profile.test.js`）：回显昵称与脱敏手机号，改昵称后服务端是新值。解绑 / `last-credential` 没有自动化覆盖（演示买家没有第三方身份） |
| 优惠券 | `GET /coupon-templates`、`POST /coupon-templates/{id}/claim`、`GET /coupons`、`POST /orders/preview` 的 `applicable_coupons` / `user_coupon_id` | ✅ 小米真机 e2e（`coupon.test.js`）：领「9 折」→ 我的优惠券四个 tab → 结算页自动用券、切「不使用」优惠归零。iOS 只验证了编译 |

### 只写了、没跑通的

- **降级分支**。四条读接口的 `notImplemented` 分支（页面说「这条接口打过去是
  {404/405}，后端还没接上」）在后端补齐**之前**逐条验过；补齐之后它们不再触发。
  代码留着：接口会挂，而「这条没接上」和「这件东西不存在」对用户是两回事。
- **`image_url` / `OrderItem.refunding_qty`**。契约声明了，服务端目前从不填
  （商品图的列与退款域的表都还没建）。客户端按可选处理，但没有「它有值」
  这条路径的实证。
- **`in_stock`**：`POST /search` 的每一条结果都带它，`in_stock_only` 默认 false（缺货的也返回，
  排在最后）。

### 完全没有验证的

多商家形态（`compose.multi.yaml` + Host 选店）。微信小程序只在开发者工具的模拟器里跑过（见「微信小程序」），
没有真机预览、没有上线。

### 沙箱支付这件事，请不要误读截图

`POST /orders/{order_no}/payments` 在这一版是**沙箱**：Keel 没有对接任何真实支付
渠道，没有任何真实资金流动。服务端在 `payload` 里自报 `"sandbox": true` 与一句
中文警告，并且刻意**不出现** `prepay_id` / `paySign` / `nonceStr` / `appId` ——
那几个键一在，客户端就会真去调 `wx.requestPayment`。客户端这边把这句警告
用一个黄框显式摆在页面上：演示截图不应该看起来像接了真微信支付。

沙箱给的不是捷径：`payload.settle` 是一份用这家店真实密钥签好名的规范回调报文，
客户端把它原样投到 `/api/v1/webhooks/payments/{channel}`，走的是生产那条完整的
入账路径（验签、金额校验、`uk_payments_channel_txn` 幂等、订单状态机）。
客户端在这条链路上一行业务逻辑都没有。

---

## 目录

```
app/
  package.json          钉死 @dcloudio 的 5.2.6 那条线；package-lock.json 入库
  vite.config.js        uni() 插件 + /api 反代（H5 只能同源，服务端没有 CORS）
  tsconfig.json         给 uni 自带的 UTS/tsc 检查用
  index.html            H5 入口
  design/tabbar/        tabBar 图标的 SVG 源（COLOR/FILL/INNER 占位符）
  design/app-icon.svg   Android 启动图标的 SVG 源
  native-android/       Android 原生壳（Gradle 工程），见「本地打 apk」
  scripts/
    install-deps.sh     npm ci + 绕开 npm 对 uts 原生 binding 的 libc 误判
    render-icons.sh     用无头 Chrome 把上面的 SVG 渲染成 tabBar 与启动图标 PNG（产物入库）
    build-apk.sh        本地打 apk，见「本地打 apk」
  typecheck/            只给 scripts/check_app_types.py 用，不参与真构建
    tsconfig.json
    uts-shim.d.ts       uni.request / 存储 / *.uvue 的最小声明
    vue.d.ts            'vue' 的桩
  src/
    uts-builtin.d.ts    UTSJSONObject、JSON.parse<T>（真构建与闸门共用同一份）
    vue-shim.d.ts       给 vue 补 createVueApp（模块增强）
    api/
      schema.uts        ★ 从契约生成，入库，闸门比对。**请勿手改**
      client.uts        HTTP 层，形状全部来自 schema.uts
      config.uts        服务地址（= 租户）、会话、本机订单号
      idempotency.uts   幂等键
      view.uts          契约类型 -> 页面的 Row 类型（契约字段读取都收在这里）
    App.uvue            设计系统：色板与原子类（原生端没有 CSS 变量，改色只改这里）
    static/tabbar/      tabBar 图标 PNG（由 render-icons.sh 生成）
    pages/…             18 个页面；首页 / 购物车 / 订单 / 我的 四个是 tabBar 页
```

---

## 两条容易搞错的设计

**租户由 Host 决定，不是请求头。** `internal/tenant/resolver.go` 写着「刻意不支持
用请求头指定租户：公开接口没有鉴权，那等于让调用方自己声明它是哪家店」。
这个客户端因此**不发任何** `X-Merchant` 之类的头，唯一能配的是 base URL。
换 base URL 时本地令牌当场作废——服务端刻意让 A 店的令牌在 B 店被拒，
留着它只会让用户撞上一个他解释不了的 401。

**幂等键进下单页生成一次，之后所有提交复用它。** 每次点击重新生成等于把幂等
整个废掉：用户手抖点两下就是两笔订单。改了订单内容会撞上 422
（服务端在保护你），页面为此给一个「换一个新键」的按钮，而不是偷偷换掉——
偷偷换掉就等于替用户决定「这是一笔新订单」。
