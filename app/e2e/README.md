# 真机自动化测试

用 DCloud 官方的 [uni-automator](https://uniapp.dcloud.net.cn/worktile/auto/quick-start.html)
（jest + 选择器 API）驱动 USB 连着的 Android 手机上**我们自己打的 apk**。
不需要 HBuilderX。

```bash
KEEL_API_BASE=http://192.168.0.110:18099/api/v1 make app-apk-e2e   # 打测试包（改了 App 代码才需要）
make app-e2e                                                      # 装包、启动、跑用例
```

前置条件：手机开了 USB 调试并已授权（`adb devices` 显示 `device`）；其余同 `make app-apk`。
只改用例不用重新打包。

## 写用例

每个 `*.test.js` 里有全局的 `program`（官方 API）：

```js
const page = await program.navigateTo('/pages/order/create?sku_id=1')
await (await page.$('.bar-btn')).tap()
const orderNo = await page.data('orderNo')
```

两条实测出来的规矩：

- **选择器只写单个类**（`.t-price-l`）。原生端不认后代选择器，`.bar .t-price-l` 返回 null。
- **等条件，不要 sleep**。接口有网络延迟，用 `helpers.js` 的 `waitFor(page, selector, predicate)`。

`checkout.test.js` 每跑一次会在服务端真的建一笔订单并走沙箱入账（没有真实资金流动）。

## 它是怎么接上的

官方流程是 HBuilderX 编译 → 装 HBuilderX 的标准基座（`io.dcloud.uniappx`）→ 用
HBuilderX 插件里的 `pushResources.js` 把产物推进基座 → 基座连回电脑。我们换掉了其中
两段，协议与测试 API 全用官方的：

**App 端**：`uni build --auto-port` 只会在生成的 `main()` 里插一句 `initAutomator()`，
而它的实现（`@dcloudio/uni-app-uts/lib/automator/android/*.uts`）npm 编译器刻意不输出。
`build-apk.sh --e2e` 把那份 UTS 源码临时拷到 `src/automator-runtime/`，`vite.config.js`
里的 `keel-automator-runtime` 插件给 `main.uts` 补上 import，于是它和 App 一起编进 apk。
地址编成 `ws://127.0.0.1:9520`（`KEEL_E2E_PORT` 可改），测试时 `adb reverse` 转回电脑，
不依赖同一网段。自动化运行时额外用到几个 uni 模块（websocket、modal 等），已经在
`native-android/settings.gradle` 的清单里。

**电脑端**：`automator.config.js` 给启动器传一个自定义 puppet —— 基于官方
`@dcloudio/uni-app-plus/lib/uni.automator.js`，只换掉 `devtools.create`（不推资源，改成
`adb reverse` + 装包 + 启动）和 `shouldCompile`（返回 false，不让它自己再编一遍）。

版本：`@dcloudio/uni-automator` 与编译器同一版（`3.0.0-5020620260917001`）；`jest 27.0.4`、
`jest-environment-node 27.5.1` 是它的 peer 依赖钉的版本；`adbkit 2.11.1` 照 HBuilderX
测试插件用的版本。

脚本带 `--forceExit`：连接是在 uni-automator 的测试环境里建的，teardown 之后 jest 仍不会
自己退出（官方 HBuilderX 插件也是这么跑的）。退出码照常反映用例结果。
