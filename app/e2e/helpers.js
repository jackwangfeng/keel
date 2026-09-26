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

// 从测试进程直接发 GET（不经过 App）。用 http 模块而不是 fetch：jest 27 的测试环境里
// 没有全局 fetch。返回 { status, body }，body 是解析过的 JSON（解析不了就是原文）。
function httpGet(url) {
  const mod = url.startsWith('https:') ? require('https') : require('http')
  return new Promise((resolve, reject) => {
    mod.get(url, (res) => {
      let raw = ''
      res.setEncoding('utf8')
      res.on('data', (c) => { raw += c })
      res.on('end', () => {
        let body = raw
        try { body = JSON.parse(raw) } catch (e) { /* 不是 JSON 就给原文 */ }
        resolve({ status: res.statusCode, body })
      })
    }).on('error', reject)
  })
}

module.exports = { waitFor, httpGet }
