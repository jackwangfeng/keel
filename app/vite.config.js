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
const port = process.env.KEEL_HTTP_PORT || '8080'

module.exports = {
  plugins: [uni()],
  server: {
    proxy: {
      '/api': {
        target: 'http://127.0.0.1:' + port,
        changeOrigin: false,
      },
    },
  },
}
