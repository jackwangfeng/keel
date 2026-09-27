// H5 无头测试用的静态服务器：托管 dist/build/h5，并把 /api 反代到 KEEL_API_BASE 那台服务端。
//
// 为什么要反代：服务端不发 CORS 头，H5 只能同源访问（src/api/config.uts 的默认地址是相对路径 /api/v1）。
// Host 头换成服务端自己的地址 —— 租户按 Host 定，这样和原生 App 访问的是同一家店。
const http = require('http')
const https = require('https')
const fs = require('fs')
const path = require('path')

const PORT = Number(process.env.KEEL_E2E_H5_PORT || 5199)
const ROOT = path.resolve(__dirname, '..', 'dist', 'build', 'h5')
const base = process.env.KEEL_API_BASE
if (!base) throw new Error('要设 KEEL_API_BASE（例如 http://192.168.0.110:18099/api/v1）')
const target = new URL(base)

const TYPES = { '.html': 'text/html; charset=utf-8', '.js': 'text/javascript', '.css': 'text/css', '.png': 'image/png', '.svg': 'image/svg+xml', '.json': 'application/json', '.ico': 'image/x-icon', '.woff2': 'font/woff2', '.ttf': 'font/ttf' }

http.createServer((req, res) => {
  if (req.url.startsWith('/api/')) {
    // 目标可以是 http（局域网演示栈）也可以是 https（公网演示站）。
    const mod = target.protocol === 'https:' ? https : http
    const up = mod.request({ host: target.hostname, port: target.port || (target.protocol === 'https:' ? 443 : 80),
      method: req.method, path: req.url, servername: target.hostname,
      headers: { ...req.headers, host: target.host } }, (r) => {
      res.writeHead(r.statusCode, r.headers)
      r.pipe(res)
    })
    up.on('error', (e) => { res.writeHead(502); res.end(String(e)) })
    req.pipe(up)
    return
  }
  let file = path.join(ROOT, decodeURIComponent(req.url.split('?')[0]))
  if (!file.startsWith(ROOT) || !fs.existsSync(file) || fs.statSync(file).isDirectory()) file = path.join(ROOT, 'index.html')
  res.writeHead(200, { 'Content-Type': TYPES[path.extname(file)] || 'application/octet-stream' })
  fs.createReadStream(file).pipe(res)
}).listen(PORT, '127.0.0.1', () => console.log(`[h5-serve] http://127.0.0.1:${PORT}/ -> ${target.origin}`))
