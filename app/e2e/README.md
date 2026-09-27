# 自动化测试

**日常验收一律跑 H5 无头全量**（`make app-e2e-h5`，一分多钟、能进 CI、能重复跑、失败有日志）；
Android / iOS / 小程序只验证编译。**真机只在发版前跑一次**：真机要解锁、要有人在场，无人值守会卡住，
慢且结果不稳定（2026-09-27 实测 iPhone 半锁屏时全量大面积 Connection closed）。原生才有的东西
（tab 角标、相册选择器、原生渲染）在提交说明里记「留待发版前真机验证」。

用 DCloud 官方的 [uni-automator](https://uniapp.dcloud.net.cn/worktile/auto/quick-start.html)
（jest + 选择器 API）驱动 USB 连着的手机上**我们自己打的包**，Android 与 iOS 同一套用例。
不需要 HBuilderX。

```bash
# Android
KEEL_API_BASE=http://192.168.0.110:18099/api/v1 make app-apk-e2e   # 打测试包（改了 App 代码才需要）
make app-e2e                                                      # 装包、启动、跑用例

# iOS
KEEL_IOS_TEAM=2Q89DQSSH6 KEEL_API_BASE=http://192.168.0.110:18099/api/v1 make app-ios-e2e
make app-e2e-ios

# H5 无头（真机都锁屏时的兜底，一轮不到一分钟）
KEEL_API_BASE=http://192.168.0.110:18099/api/v1 make app-e2e-h5
```

H5 无头：`scripts/e2e-h5.sh` 带自动化运行时编 H5（`--auto-host 127.0.0.1 --auto-port 9520`），
`e2e/h5-serve.js` 托管产物并把 `/api` 反代到 `KEEL_API_BASE`（H5 只能同源；Host 换成服务端的，和 App
访问同一家店），官方 H5 puppet 用 playwright 以 `channel: 'chrome'` 起本机 Chrome（不下载 playwright
自带的浏览器）。`KEEL_E2E_H5_HEADED=1` 可以看着跑。**它只证明 JS / H5 那一层**：Android 的 Kotlin 产物
与 iOS 原生那层测不到，那两端要么上真机，要么在报告里写明「只验证了编译」。

前置条件：Android 开了 USB 调试并已授权（`adb devices` 显示 `device`），想拔线的话插着线跑一次
`make app-adb-wifi`（切到无线调试；手机重启后要重跑）；iPhone 已配对
（`xcrun devicectl list devices` 显示 `paired`）、**不锁屏**（锁屏时系统拒绝远程启动 App，
测试期间把「自动锁定」设成「永不」），并且和电脑在同一网段（见下）。iPhone 配对过一次之后
Xcode 就能经 Wi-Fi 连它（`devicectl list devices` 里 transport 是 `localNetwork`），不用插线。
同一台 Android 手机插着线又开了无线时 `adb devices` 里有两条，用例优先用无线那条，
`ANDROID_SERIAL` 可指定。只改用例不用重新打包。

## 写用例

每个 `*.test.js` 里有全局的 `program`（官方 API）：

```js
const page = await program.navigateTo('/pages/order/create?sku_id=1')
await (await page.$('.bar-btn')).tap()
const orderNo = await page.data('orderNo')
```

实测出来的规矩：

- **选择器只写单个类**（`.t-price-l`）。原生端不认后代选择器，`.bar .t-price-l` 返回 null。
- **等条件，不要 sleep**。接口有网络延迟，用 `helpers.js` 的 `waitFor(page, selector, predicate)`、
  `waitData(page, key, predicate)`、`waitEl(page, selector)`。刚 `navigateTo` 过去的页面 500ms 内
  `$` 可能还是 null（实测）。
- **表单用 `el.input(v)` 填，不要 `page.setData` 整个替换对象字段**。页面 data 里的对象在 Android 上是
  具名类（如 `AddressForm`），拿普通对象整个换掉它，页面当场渲染不出来、之后 `$` 全返回 null（实测）。
  字符串 / 数字字段 `setData` 没问题。
- **不要写死 sku_id**。演示库的库存是真扣的，结算用例每跑一次付掉一件；用 `pickSku(minQty)` 现挑一个
  库存够的规格（问的是不带 store_id 的默认门店，与 App 拿不到定位时回落的同一家）。

**用哪个买家**：默认是演示买家（13800000000，登录页预填的那个）。演示栈对公众开放后公众访客也用它，
用例会清空购物车、改昵称、把通知标已读，两边互相干扰 —— 可以用 `KEEL_E2E_PHONE` / `KEEL_E2E_PASSWORD`
指定一个专供 e2e 的买家（名下要有一条杭州的默认地址：运费用例按它断言 8 元）。演示库随时可能被重置，
用例不依赖上一轮留下的数据，每轮自己造。

下单成功后 App 会弹「下单成功」并在约 0.5 秒后 redirectTo 订单详情（重放不跳）：用例用 `helpers.waitOrderPlaced`
等页面到订单详情再取订单号，不要在确认页上等「下单成功」那行字（会和跳转赛跑）。

`checkout.test.js` 每跑一次会在服务端真的建一笔订单并走沙箱入账（没有真实资金流动）。

**跑用例时 `KEEL_API_BASE` 要和打测试包时一致**：`category.test.js` 从测试进程直接问服务端
支不支持 `GET /categories`，问的就是这个地址。它先问、再按答案断言：服务端没有这条路由
（今天的远端）→ 分类栏必须不出现；有 → 分类栏出现、点一个分类能切换。服务端补上路由那天
这条用例不用改。本地想走「有分类」那条，可以起一个只替换 `/api/v1/categories`、其余转发
的小代理，把测试包的 `KEEL_API_BASE` 指过去。

`store.test.js`：首页显示「由「xx」为你配送」。两个平台上门店解析走的都是「定位拿不到 →
回落默认店」那条路径：iOS 不能远程授予定位；Android 上试过 `pm grant` 预授，但 MIUI
带「仅本次允许」标记、照样弹自己的权限框，靠 adb 走不通（实测），所以没留这段。
真拿坐标解析那条路径目前没有自动化覆盖。

`address.test.js`：新建的是**非默认**地址（名字 `e2e 收件人`），前后都从测试进程删掉，不动种子里的
默认地址 —— 删了默认服务端不会补回，结算用例就没地址了。`cart.test.js`：开始前 `DELETE /cart` 清空；
每跑一次下一笔**不付款**的单（可能顺带锁住演示买家手上的券，待支付单超时关闭后退回）。
`profile.test.js`：改完昵称在 afterAll 里改回原值。

`aftersale.test.js`：一笔「某状态的单」由测试进程用 `helpers.placeOrder` 造（下单、发起支付、把
服务端签好的沙箱回调原样投回去），App 里只走被测的那一步。驳回 / 同意到账 / 发货后确认收货三条的前置状态
要后台员工来做，两种给法：设 `KEEL_E2E_STAFF_TOKEN`（用例自己调后台接口；**只放环境变量、不进仓库**），
或者先用 `placeOrder` 造好单、请服务端那边的后台会话代为发货 / 驳回 / 同意，再把单号交给
`KEEL_E2E_SHIPPED_ORDER` / `KEEL_E2E_REJECTED_REFUND` / `KEEL_E2E_REFUNDED_REFUND`。都没有时这三条
`skip`，买家侧照跑。「待买家退货填物流」只收单号：`KEEL_E2E_RETURN_REFUND`（一张已同意到 20 的退货退款单；
退货退款要先发货，所以是两轮：请后台发货 → 买家申请退货退款 → 请后台同意）。凭证图由测试进程
`uploadEvidenceFromTest` 上传（multipart、purpose=3），App 内的相册选图不在自动化范围里。

`coupon.test.js`：演示买家的券状态跑一次变一次（「9 折」第一次领是 201，之后是 409 每人限领；
结算用例会把自动选上的券真的用掉）。所以它断言的是**终态**：领完按钮是「已领取」、这张券在
我的优惠券某个 tab 里；结算页那一步先从测试进程问服务端手上有没有未使用的券（`httpPost` 登录拿
token、`httpGet` 带 token），有才断言「自动用上了最省的那张」，没有就断言「不带券照常算钱」。
想走完整的「有券」路径，请服务端重置演示库。

Android 的自动化运行时不支持经 `program.callUniMethod('request', …)` 调 `uni.request`
（"uni.request not exists"）；要从用例里问服务端，用 `helpers.js` 的 `httpGet` / `httpPost`
（jest 27 的测试环境里没有全局 `fetch`）。

## 它是怎么接上的

官方流程是 HBuilderX 编译 → 装 HBuilderX 的标准基座（`io.dcloud.uniappx`）→ 用
HBuilderX 插件里的 `pushResources.js` 把产物推进基座 → 基座连回电脑。我们换掉了其中
两段，协议与测试 API 全用官方的：

**App 端**：`uni build --auto-port` 时，`@dcloudio/uni-automator` 自带的 uni 插件往 main 里
import 官方的自动化运行时。
- iOS：产物是 JS，那份运行时照常进包，什么都不用做。
- Android：运行时是 UTS 源码（`@dcloudio/uni-app-uts/lib/automator/android`），而 Android
  编译器对 uni-app-uts 下的文件不输出，Kotlin 编译会报 `Unresolved reference 'initAutomator'`。
  `build-apk.sh --e2e` 把源码临时拷到 `src/automator-runtime/`，`vite.config.js` 的
  `keel-automator-runtime` 插件用 `resolveId` 把那句 import 重定向到拷贝上。不能自己再补一句
  import：两句同名导入会让 UTS 打包器 panic（实测）。

连接：Android 编成 `ws://127.0.0.1:9520`，`adb reverse` 转回电脑；iPhone 不能把端口反向转回
电脑（usbmuxd 只支持电脑连手机），编的是电脑的局域网 IP（默认 en0，`KEEL_E2E_HOST` 可改）。
端口 `KEEL_E2E_PORT` 可改。自动化运行时在 Android 上额外用到几个 uni 模块（websocket、modal
等），已经在 `native-android/settings.gradle` 的清单里。

**电脑端**：`automator.config.js` 给启动器传一个自定义 puppet —— 基于官方
`@dcloudio/uni-app-plus/lib/uni.automator.js`，只换掉 `devtools.create`（不推资源，Android 改成
`adb reverse` + 装包 + 启动，iOS 改成 `devicectl` 装包 + 启动）和 `shouldCompile`（返回 false，
不让它自己再编一遍）。iOS 还换掉了 `validate`：官方的那个面向模拟器 / HBuilderX 推资源。

iOS 的自动化运行时没实现 `App.callFunction`，所以 `program.evaluate()` 在 iOS 上不能用；
要在 App 里探查什么，用 `program.callUniMethod('request', {...})` 这类 uni API 调用（它把
success 回调的结果原样带回来）。

版本：`playwright 1.63.0`（H5 无头，只用它的库，浏览器用本机 Chrome）；`@dcloudio/uni-automator` 与编译器同一版（`3.0.0-5020620260917001`）；`jest 27.0.4`、
`jest-environment-node 27.5.1` 是它的 peer 依赖钉的版本；`adbkit 2.11.1` 照 HBuilderX
测试插件用的版本。

脚本带 `--forceExit`：连接是在 uni-automator 的测试环境里建的，teardown 之后 jest 仍不会
自己退出（官方 HBuilderX 插件也是这么跑的）。退出码照常反映用例结果。
