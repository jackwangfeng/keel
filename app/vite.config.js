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

// 自动化测试构建（`make app-apk-e2e`）时把官方的自动化运行时接进 App。
//
// `uni build --auto-port` 只会在生成的 main() 里插一句 `initAutomator()`，
// 而这个函数的实现（@dcloudio/uni-app-uts/lib/automator/android/*.uts）npm 编译器
// 刻意不输出 —— HBuilderX 的真机运行流程自己把它编进基座里。我们不走 HBuilderX，
// 所以 build-apk.sh 把那份源码拷到 src/automator-runtime/（gitignore），
// 这里给 main.uts 补上 import，让它进入编译图、和 App 同包输出，那句调用就有了着落。
// 普通构建里 UNI_AUTOMATOR_WS_ENDPOINT 不存在，这个插件什么都不做。
const path = require('path')
const fs = require('fs')
const automatorRuntime = {
  name: 'keel-automator-runtime',
  enforce: 'pre',
  transform(code, id) {
    if (!process.env.UNI_AUTOMATOR_WS_ENDPOINT) return
    const file = id.split('?')[0].replace(/\\/g, '/')
    if (!file.endsWith('/src/main.uts')) return
    if (!fs.existsSync(path.join(__dirname, 'src/automator-runtime/index.uts'))) {
      throw new Error('自动化构建缺少 src/automator-runtime/：请用 app/scripts/build-apk.sh --e2e 构建')
    }
    return "import { initAutomator } from './automator-runtime/index.uts'\n" + code
  },
}

module.exports = {
  plugins: [automatorRuntime, uni()],
  server: {
    proxy: {
      '/api': {
        target: target,
        changeOrigin: false,
      },
    },
  },
}
