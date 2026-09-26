# Keel 买家端（uni-app x）

用 [uni-app x](https://doc.dcloud.net.cn/uni-app-x/) 写的买家端：UTS 编译成原生
Kotlin / Swift，不走 webview；同一份代码也编 H5 与小程序。

- 页面流：商品列表 → 商品详情 → 登录 → 下单（试算 → 提交）→ 我的订单 → 订单详情 → 发起支付
- 搜索框是占位的。`GET /search` 是 M3 的东西，后端还没有。

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
| iOS | 未验证 | — | — |
| 微信小程序 | 未验证（缺 `@dcloudio/uni-mp-weixin` 一系包，没继续装） | — | — |

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

- **默认服务地址是打包时注入的**（`KEEL_API_BASE`），源码里永远是空串，见
  `src/api/native-default.uts`。vite 的 `define` 试过，只替换 JS 产物，Kotlin 里留下原样
  的标识符，所以脚本在拷进原生工程的 `.kt` 上做替换，并断言恰好替换一处。
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

## 这一版真的跑通了什么

对着 `docker compose up -d --build` 起来的真后端（`KEEL_HTTP_PORT=18080`），
H5 构建产物 + 一个把 `/api` 反代给 Keel 的静态服务器，用无头 Chrome 走了一遍。
**没有任何 mock**：每一行都是真请求真响应。

| 页面 / 动作 | 接口 | 结果 |
|---|---|---|
| 商品列表 | `GET /products` | ✅ 三件种子商品、价格区间、`共 3 件` |
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
| 搜索 | `GET /search` | ⏸ M3 才有，搜索框是禁用的占位 |

### 只写了、没跑通的

- **收货地址**。契约里有 `GET /addresses`，后端没有，所以下单页的 address_id 是
  手填的（种子买家名下是 1），页面上写明了这件事。
- **降级分支**。四条读接口的 `notImplemented` 分支（页面说「这条接口打过去是
  {404/405}，后端还没接上」）在后端补齐**之前**逐条验过；补齐之后它们不再触发。
  代码留着：接口会挂，而「这条没接上」和「这件东西不存在」对用户是两回事。
- **`in_stock` / `image_url` / `OrderItem.refunding_qty`**。契约声明了，服务端
  目前从不填。客户端按可选处理（`in_stock` 缺席时不说「无货」），但没有
  「它有值」这条路径的实证。

### 完全没有验证的

原生 App 形态（编到 Kotlin 为止，**没有 apk，没有在设备上跑过**）、iOS、
微信小程序、多商家形态（`compose.multi.yaml` + Host 选店）。

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
    pages/…             8 个页面；首页 / 订单 / 我的 三个是 tabBar 页
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
