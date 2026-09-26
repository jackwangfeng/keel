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
const IOS = process.env.UNI_APP_PLATFORM === 'ios'
const IOS_APP = path.join(APP, 'dist', 'ios-device-e2e', 'KeelBuyer.app')

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
const run = (cmd, args) => execFileSync(cmd, args, { stdio: ['ignore', 'pipe', 'inherit'] }).toString()

// 选定一台 Android 设备，adb 调用与官方 launcher（adbkit，默认取列表第一台）都用它。
// 同一台手机开了无线调试又插着线时，adb devices 里是两条（USB 序列号 + ip:5555），
// 不指定的话 adb 直接报 "more than one device"。规则：ANDROID_SERIAL 优先（adb 自己也认）；
// 只有一台就用它；多台时优先无线那条（插线只是为了开无线调试，插着不该改变测的是谁）。
function androidSerial() {
  if (process.env.ANDROID_SERIAL) return process.env.ANDROID_SERIAL
  const lines = run(adbPath(), ['devices']).split('\n').slice(1)
  const online = lines.map((l) => l.trim().split(/\s+/)).filter((c) => c[1] === 'device').map((c) => c[0])
  if (online.length === 0) throw new Error('没有在线的 Android 设备：USB 连上并授权，或 adb connect <ip>:5555')
  if (online.length === 1) return online[0]
  const wireless = online.find((id) => /:\d+$/.test(id))
  const pick = wireless || online[0]
  console.log(`[e2e] 有 ${online.length} 台 Android 设备在线，用 ${pick}（ANDROID_SERIAL 可指定）`)
  return pick
}
const SERIAL = IOS ? '' : androidSerial()
const adb = (...args) => run(adbPath(), ['-s', SERIAL, ...args])

// iPhone：用 Xcode 的 devicectl（USB 连着、已配对）。KEEL_IOS_DEVICE 可指定设备，默认取第一台。
function iosDevice() {
  if (process.env.KEEL_IOS_DEVICE) return process.env.KEEL_IOS_DEVICE
  const out = path.join(require('os').tmpdir(), `keel-devices-${process.pid}.json`)
  run('xcrun', ['devicectl', 'list', 'devices', '--json-output', out])
  const devices = JSON.parse(fs.readFileSync(out, 'utf8')).result.devices
  const phone = devices.find((d) => d.hardwareProperties && d.hardwareProperties.platform === 'iOS'
    && d.connectionProperties && d.connectionProperties.pairingState === 'paired')
  if (!phone) throw new Error('没找到已配对的 iPhone：USB 连上、解锁、信任这台电脑')
  return phone.identifier
}

if (!IOS && !fs.existsSync(APK)) throw new Error(`没有自动化测试包 ${APK}：先跑 make app-apk-e2e`)
if (IOS && !fs.existsSync(IOS_APP)) throw new Error(`没有 iOS 自动化测试包 ${IOS_APP}：先跑 make app-ios-e2e`)

function launchAndroid() {
  // App 里编进去的地址是 ws://127.0.0.1:PORT；把手机上的这个端口转回电脑。
  adb('reverse', `tcp:${PORT}`, `tcp:${PORT}`)
  // 每次都装：保证测的是刚打出来的那个包，而不是手机上残留的旧包。
  adb('install', '-r', APK)
  adb('shell', 'am', 'force-stop', PKG)
  adb('shell', 'am', 'start', '-n', `${PKG}/io.dcloud.uniapp.UniAppActivity`)
  dismissPermissionDialog()
}

// 首次启动时 App 要定位（src/api/store.uts），系统弹权限框；框挡在最上面的时候，
// App 的自动化连接连不回电脑 —— "Failed to connect to runtime"，实测可稳定复现
// （清掉 App 数据、让它一启动就弹框，再跑就红）。靠 adb 预先授权或预先拒绝都挡不住：
// MIUI 有自己一层权限管理，pm grant 带「仅本次」标记、pm revoke + user-fixed 也照样弹。
// 所以启动后看一眼：最上面是权限框（各家都叫 GrantPermissionsActivity）就按返回拒掉。
// 用例因此走「定位拿不到 → 回落默认店」那条路径，和 iOS 上测的是同一条。
function dismissPermissionDialog() {
  const deadline = Date.now() + 8000
  while (Date.now() < deadline) {
    const top = adb('shell', 'dumpsys', 'activity', 'activities')
      .split('\n').find((l) => l.includes('topResumedActivity')) || ''
    if (top.includes('GrantPermissionsActivity')) {
      adb('shell', 'input', 'keyevent', 'KEYCODE_BACK')
      console.log('[e2e] 启动时弹了权限框，已按返回拒绝（走回落默认店那条路径）')
      return
    }
    if (top.includes(PKG)) return
    execFileSync('sleep', ['0.5'])
  }
}

function launchIos() {
  // iPhone 不能把端口反向转回电脑，App 里编的是电脑的局域网 IP（见 build-ios.sh --e2e）。
  const dev = iosDevice()
  run('xcrun', ['devicectl', 'device', 'install', 'app', '--device', dev, IOS_APP])
  run('xcrun', ['devicectl', 'device', 'process', 'launch', '--device', dev, '--terminate-existing', PKG])
}

const puppet = Object.assign({}, official, {
  shouldCompile: () => false,
  devtools: Object.assign({}, official.devtools, {
    // 官方要求产物目录里有 app-service.js（那是给基座推的资源）。我们装的是整包，不需要。
    required: [],
    // iOS：官方 validate 会建一个面向模拟器 / HBuilderX 推资源的 launcher，真机上用不上。
    validate: IOS ? async (options) => options : official.devtools.validate,
    async create() {
      if (IOS) launchIos()
      else launchAndroid()
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
    platform: IOS ? 'ios' : 'android',
    android: { id: SERIAL || undefined, package: PKG, appid: APPID, executablePath: APK },
    ios: { bundleId: PKG, appid: APPID, executablePath: IOS_APP },
  },
}
