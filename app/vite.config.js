// uni-app x 的 CLI（node_modules/.bin/uni）**不会**自己把 uni 插件塞进 vite 配置，
// 它只是带着一堆 UNI_* 环境变量去跑 vite。没有这个文件里的 uni()，
// `uni build --platform h5` 会一路走到 vite 的 import 分析，然后报
// 「.uvue 里有非法 JS 语法」——那句话不指向真因（真因是 SFC 编译器压根没挂上）。
const uni = require('@dcloudio/vite-plugin-uni').default

// 开发服务器把 /api 转给 Keel。
//
// **这不是为了方便，是为了能跑。** 服务端一个 CORS 头都不发（实测
// `OPTIONS /api/v1/products` 回 405），所以 H5 形态只能同源访问。
// 生产上对应的是「一个反向代理后面同时挂静态页和 API」——
// 而那也正是多租户想要的形状：页面的 Host 就是店。
//
// 端口跟 compose 读同一个变量（KEEL_HTTP_PORT），不写死 8080：
// 宿主机 8080 常被占，README 的快速开始也是这么绕的。
//
// 后端不在本机时（例如局域网里的一台测试机），用 KEEL_API_TARGET 指整个 origin，
// 例如 KEEL_API_TARGET=http://192.168.0.110:18099。它优先于 KEEL_HTTP_PORT。
const port = process.env.KEEL_HTTP_PORT || '8080'
const target = process.env.KEEL_API_TARGET || 'http://127.0.0.1:' + port

// 自动化测试构建（`make app-apk-e2e`）时把官方的自动化运行时接进 Android App。
//
// `uni build --auto-port` 时，@dcloudio/uni-automator 自带的 uni 插件会往 main 里补一句
// `import { initAutomator } from '<node_modules>/@dcloudio/uni-app-uts/lib/automator/android/index.uts'`。
// 但 Android 编译器对 uni-app-uts 下的 .uts **不输出**（刻意的：HBuilderX 的真机运行流程自己把
// 它编进基座），于是 Kotlin 编译报 Unresolved reference 'initAutomator'。
//
// 所以 build-apk.sh --e2e 把那份源码拷到 src/automator-runtime/（gitignore），这里把那句 import
// **重定向**到拷贝上：拷贝不在 uni-app-uts 下，照常输出、和 App 同包编译。
// 不能自己再补一句 import —— 两句同名导入会让 UTS 打包器 panic（Multiple identifiers ...
// initAutomator，实测）。
// iOS 不需要：产物是 JS，官方插件 import 的那份照常进包。
const path = require('path')
const fs = require('fs')
const automatorRuntime = {
  name: 'keel-automator-runtime',
  enforce: 'pre',
  resolveId(source) {
    if (!process.env.UNI_AUTOMATOR_WS_ENDPOINT) return null
    if (process.env.UNI_UTS_PLATFORM !== 'app-android') return null
    if (!source.replace(/\\/g, '/').endsWith('uni-app-uts/lib/automator/android/index.uts')) return null
    const copy = path.join(__dirname, 'src/automator-runtime/index.uts')
    if (!fs.existsSync(copy)) {
      throw new Error('自动化构建缺少 src/automator-runtime/：请用 app/scripts/build-apk.sh --e2e 构建')
    }
    return copy
  },
}

// 原生 App 的默认服务地址（见 src/api/native-default.uts）。只在 app 平台注入：
// H5 必须同源访问（服务端不发 CORS 头），永远用相对路径。
const isApp = (process.env.UNI_PLATFORM || '').startsWith('app')
const nativeApiBase = isApp ? process.env.KEEL_API_BASE || '' : ''

module.exports = {
  plugins: [automatorRuntime, uni()],
  define: {
    'process.env.KEEL_API_BASE': JSON.stringify(nativeApiBase),
  },
  server: {
    proxy: {
      '/api': {
        target: target,
        changeOrigin: false,
      },
    },
  },
}
