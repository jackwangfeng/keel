// 真机上接口有网络延迟：按条件轮询，而不是 sleep 一个拍脑袋的秒数。
async function waitFor(page, selector, predicate = () => true, timeout = 15000) {
  const start = Date.now()
  let last = null
  while (Date.now() - start < timeout) {
    const el = await page.$(selector)
    if (el) {
      last = await el.text()
      if (predicate(last)) return el
    }
    await page.waitFor(300)
  }
  throw new Error(`等 ${selector} 超时（${timeout}ms），最后看到的文字：${JSON.stringify(last)}`)
}

// 从测试进程直接发请求（不经过 App）。用 http 模块而不是 fetch：jest 27 的测试环境里
// 没有全局 fetch。返回 { status, body }，body 是解析过的 JSON（解析不了就是原文）。
function httpRequest(method, url, body = null, token = '') {
  const mod = url.startsWith('https:') ? require('https') : require('http')
  const headers = {}
  if (token) headers.Authorization = 'Bearer ' + token
  let payload = null
  if (body !== null) {
    payload = JSON.stringify(body)
    headers['Content-Type'] = 'application/json'
    headers['Content-Length'] = Buffer.byteLength(payload)
  }
  return new Promise((resolve, reject) => {
    const req = mod.request(url, { method, headers }, (res) => {
      let raw = ''
      res.setEncoding('utf8')
      res.on('data', (c) => { raw += c })
      res.on('end', () => {
        let parsed = raw
        try { parsed = JSON.parse(raw) } catch (e) { /* 不是 JSON 就给原文 */ }
        resolve({ status: res.statusCode, body: parsed })
      })
    })
    req.on('error', reject)
    if (payload !== null) req.write(payload)
    req.end()
  })
}

const httpGet = (url, token = '') => httpRequest('GET', url, null, token)
const httpPost = (url, body, token = '') => httpRequest('POST', url, body, token)

// 跑用例要知道 App 连的是哪个服务端：打测试包时注入的 KEEL_API_BASE。
function apiBase() {
  const b = process.env.KEEL_API_BASE
  if (!b) throw new Error('跑这条用例要设 KEEL_API_BASE（和打测试包时同一个值）')
  return b
}

module.exports = { waitFor, httpGet, httpPost, apiBase }
