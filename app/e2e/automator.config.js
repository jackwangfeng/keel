// uni-automator 的启动配置（经 UNI_AUTOMATOR_CONFIG 读入，所以可以放函数）。
//
// ## 为什么要自定义 puppet
//
// 官方流程：HBuilderX 编译 → 装 HBuilderX 的「标准基座」（io.dcloud.uniappx）→
// 用 HBuilderX 插件里的 pushResources.js 把编译产物推进基座 → 基座连回电脑。
// 我们不走 HBuilderX（它导入这个项目就崩，见 app/README.md），测的也应该是自己打的包，
// 所以：
//
//   · 编译与打包交给 `build-apk.sh --e2e`（官方自动化运行时已编进 apk）；
//   · 启动器里只换掉 devtools.create —— 不推资源，改成 adb reverse + 装包 + 启动；
//   · 其余（协议适配器、validate 里创建的 adb launcher、截图等 App 级 API）全用官方的。
//
// shouldCompile 返回 false：launcher 就不会去 spawn `uni` 自己再编一遍。
const path = require('path')
const fs = require('fs')
const { execFileSync } = require('child_process')
const official = require('@dcloudio/uni-app-plus/lib/uni.automator.js')

const APP = path.resolve(__dirname, '..')
const PORT = Number(process.env.KEEL_E2E_PORT || 9520)
const manifest = JSON.parse(fs.readFileSync(path.join(APP, 'src/manifest.json'), 'utf8'))
const APPID = manifest.appid
const PKG = 'dev.keel.buyer' // 与 native-android/app/build.gradle 的 applicationId 一致
const APK = path.join(APP, 'dist', `keel-buyer-${manifest.versionName}-e2e.apk`)

function adbPath() {
  if (process.env.ADB) return process.env.ADB
  const home = process.env.ANDROID_HOME
  const candidates = [
    home && path.join(home, 'platform-tools/adb'),
    path.join(process.env.HOME || '', 'Library/Android/sdk/platform-tools/adb'),
    '/opt/homebrew/share/android-commandlinetools/platform-tools/adb',
  ].filter(Boolean)
  return candidates.find((p) => fs.existsSync(p)) || 'adb'
}
const ADB = adbPath()
const adb = (...args) => execFileSync(ADB, args, { stdio: ['ignore', 'pipe', 'inherit'] }).toString()

if (!fs.existsSync(APK)) {
  throw new Error(`没有自动化测试包 ${APK}：先跑 make app-apk-e2e`)
}

const puppet = Object.assign({}, official, {
  shouldCompile: () => false,
  devtools: Object.assign({}, official.devtools, {
    // 官方要求产物目录里有 app-service.js（那是给基座推的资源）。我们装的是整包，不需要。
    required: [],
    async create() {
      // App 里编进去的地址是 ws://127.0.0.1:PORT；把手机上的这个端口转回电脑。
      adb('reverse', `tcp:${PORT}`, `tcp:${PORT}`)
      // 每次都装：保证测的是刚打出来的那个包，而不是手机上残留的旧包。
      adb('install', '-r', APK)
      adb('shell', 'am', 'force-stop', PKG)
      adb('shell', 'am', 'start', '-n', `${PKG}/io.dcloud.uniapp.UniAppActivity`)
    },
  }),
})

module.exports = {
  platform: 'app-plus',
  projectPath: path.join(APP, 'src'),
  cliPath: APP,
  port: PORT,
  'app-plus': {
    puppet,
    // close 而不是默认的 disconnect：disconnect 只断连接，本机的 ws 服务与心跳定时器还开着，
    // 用例跑完 jest 不退出（实测）。close 会退出 App 并关掉服务。
    teardown: 'close',
    platform: 'android',
    android: { package: PKG, appid: APPID, executablePath: APK },
  },
}
