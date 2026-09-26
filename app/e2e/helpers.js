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

// 测试进程自己的登录（演示买家），拿 token 从服务端核对 / 布置状态。App 里的会话是另一份。
const DEMO_PHONE = '13800000000'
const DEMO_PASSWORD = 'keel-demo-2026'
async function serverToken() {
  const res = await httpPost(apiBase() + '/auth/login', { phone: DEMO_PHONE, password: DEMO_PASSWORD })
  if (res.status !== 200) throw new Error('测试进程登录失败：' + res.status + ' ' + JSON.stringify(res.body))
  return res.body.access_token
}

// App 里从未登录开始登一次（登录页默认填的就是演示买家）。
async function loginInApp() {
  await program.callUniMethod('clearStorageSync')
  const page = await program.reLaunch('/pages/auth/login')
  await page.waitFor(1000)
  await (await page.$('.btn')).tap()
  await waitFor(page, '.msg', (t) => t.includes('登录成功'))
}

// 轮询页面 data 直到条件成立。接口有延迟，data 是异步填上的。
async function waitData(page, key, predicate, timeout = 15000) {
  const start = Date.now()
  let last
  while (Date.now() - start < timeout) {
    last = await page.data(key)
    if (predicate(last)) return last
    await page.waitFor(300)
  }
  throw new Error(`等 data.${key} 超时（${timeout}ms），最后看到的：${JSON.stringify(last)}`)
}

// 等一个元素出现（页面刚 navigateTo 过去时还没渲染完，$ 会返回 null）。
async function waitEl(page, selector, timeout = 15000) {
  const start = Date.now()
  while (Date.now() - start < timeout) {
    const el = await page.$(selector)
    if (el) return el
    await page.waitFor(300)
  }
  throw new Error(`等元素 ${selector} 超时（${timeout}ms）`)
}

// 挑一个库存够的规格来下单 / 加购，而不是写死 sku_id=1：演示库的库存是真扣的，
// 结算用例每跑一次付掉一件，写死的那个迟早卖光（实测 sku 1 已经只剩 1 件）。
// 不带 store_id 问：和 App 在拿不到定位时回落的是同一家默认门店。
// 只看每件商品「第一个有货的规格」—— 商品详情页默认选的就是它。
async function pickSku(minQty = 5) {
  const list = (await httpGet(apiBase() + '/products?page_size=50')).body
  let best = null
  for (const p of list.items || []) {
    if (p.status !== 1) continue
    const d = (await httpGet(apiBase() + '/products/' + p.id)).body
    const first = (d.skus || []).find((s) => s.available_qty > 0)
    if (!first) continue
    if (!best || first.available_qty > best.qty) best = { productId: p.id, skuId: first.id, qty: first.available_qty }
  }
  if (!best || best.qty < minQty) throw new Error('演示库里没有库存 ≥ ' + minQty + ' 的规格了，请重置演示库：' + JSON.stringify(best))
  return best
}

module.exports = { waitFor, waitData, waitEl, pickSku, httpGet, httpPost, httpRequest, apiBase, serverToken, loginInApp }
