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
// extra：额外的请求头（Idempotency-Key 之类）。body 是字符串时原样发（沙箱回调要逐字节原样）。
function httpRequest(method, url, body = null, token = '', extra = {}) {
  const mod = url.startsWith('https:') ? require('https') : require('http')
  const headers = { ...extra }
  if (token) headers.Authorization = 'Bearer ' + token
  let payload = null
  if (body !== null) {
    payload = typeof body === 'string' ? body : JSON.stringify(body)
    if (!headers['Content-Type']) headers['Content-Type'] = 'application/json'
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

// 用例用哪个买家。默认是演示买家（登录页预填的那个）；演示栈对公众开放之后，公众访客也在用它 ——
// 用例会清空购物车、改昵称、把通知标已读，两边会互相干扰。所以可以用 KEEL_E2E_PHONE / KEEL_E2E_PASSWORD
// 指定一个专供 e2e 的买家（名下要有一条杭州的默认地址，运费用例按它断言 8 元）。
const DEMO_PHONE = process.env.KEEL_E2E_PHONE || '13800000000'
const DEMO_PASSWORD = process.env.KEEL_E2E_PASSWORD || 'keel-demo-2026'
async function serverToken() {
  const res = await httpPost(apiBase() + '/auth/login', { phone: DEMO_PHONE, password: DEMO_PASSWORD })
  if (res.status !== 200) throw new Error('测试进程登录失败：' + res.status + ' ' + JSON.stringify(res.body))
  return res.body.access_token
}

// App 里从未登录开始登一次。登录页预填的是演示买家；指定了别的账号就把两格改掉再登。
async function loginInApp() {
  await program.callUniMethod('clearStorageSync')
  const page = await program.reLaunch('/pages/auth/login')
  await page.waitFor(1000)
  if (process.env.KEEL_E2E_PHONE) {
    const inputs = await page.$$('.field-input')
    await inputs[0].input(DEMO_PHONE)
    await inputs[1].input(DEMO_PASSWORD)
  }
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

// 从测试进程下一单（演示买家、默认地址、默认门店），pay=true 时顺手付掉：
// 发起支付 → 把服务端签好的沙箱回调原样投回去 → 等订单变成已支付。
// 用例要的是「一笔某状态的单」，下单流程本身由 checkout.test.js 在 App 里走。
async function placeOrder(token, { pay = false, quantity = 1 } = {}) {
  const { randomUUID } = require('crypto')
  const sku = await pickSku(quantity + 2)
  const store = (await httpGet(apiBase() + '/products?page_size=1')).body.store
  const addrs = (await httpGet(apiBase() + '/addresses', token)).body
  const addr = addrs.find((a) => a.is_default) || addrs[0]
  if (!addr) throw new Error('演示买家名下没有地址')
  const body = { items: [{ sku_id: sku.skuId, quantity }], store_id: store.store_id, address_id: addr.id }
  const o = await httpRequest('POST', apiBase() + '/orders', body, token, { 'Idempotency-Key': randomUUID() })
  if (o.status !== 201) throw new Error('下单失败：' + o.status + ' ' + JSON.stringify(o.body))
  const orderNo = o.body.order_no
  if (!pay) return orderNo
  const p = await httpRequest('POST', apiBase() + '/orders/' + orderNo + '/payments', { channel: 'wechat' }, token,
    { 'Idempotency-Key': randomUUID() })
  if (p.status !== 201) throw new Error('发起支付失败：' + p.status + ' ' + JSON.stringify(p.body))
  const settle = p.body.payload && p.body.payload.settle
  if (!settle) throw new Error('支付响应里没有沙箱回调信封（演示栈没开沙箱？）')
  const origin = apiBase().replace(/^(https?:\/\/[^/]+).*$/, '$1')
  const w = await httpRequest(settle.method || 'POST', origin + settle.url, settle.body, '', settle.headers)
  if (w.status >= 300) throw new Error('沙箱回调失败：' + w.status + ' ' + JSON.stringify(w.body))
  for (let i = 0; i < 30; i++) {
    const d = (await httpGet(apiBase() + '/orders/' + orderNo, token)).body
    if (d.status === 20) return orderNo
    await new Promise((r) => setTimeout(r, 300))
  }
  throw new Error('付款后订单没有变成已支付：' + orderNo)
}

// 后台员工会话（发货、审核售后）。只从环境变量读，不进仓库；没设就返回空串，
// 用到它的用例自己跳过 —— 买家侧的用例不依赖它。
function staffToken() {
  return process.env.KEEL_E2E_STAFF_TOKEN || ''
}

async function staffPost(path, body) {
  const { randomUUID } = require('crypto')
  const r = await httpRequest('POST', apiBase() + path, body, staffToken(), { 'Idempotency-Key': randomUUID() })
  if (r.status >= 300) throw new Error('后台 ' + path + ' 失败：' + r.status + ' ' + JSON.stringify(r.body))
  return r.body
}

// 从测试进程上传一张售后凭证（POST /uploads，multipart，purpose=3）。App 里的相册选图没法自动化，
// 用例拿这个造「带凭证的售后单」，再在 App 里验证凭证能带令牌读出来显示。图是一张 1×1 的 PNG。
const PNG_1PX = Buffer.from('iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==', 'base64')
async function uploadEvidenceFromTest(token) {
  const { randomUUID } = require('crypto')
  const boundary = '----keel' + randomUUID().replace(/-/g, '')
  const body = Buffer.concat([
    Buffer.from('--' + boundary + '\r\nContent-Disposition: form-data; name="purpose"\r\n\r\n3\r\n'),
    Buffer.from('--' + boundary + '\r\nContent-Disposition: form-data; name="file"; filename="e2e.png"\r\nContent-Type: image/png\r\n\r\n'),
    PNG_1PX,
    Buffer.from('\r\n--' + boundary + '--\r\n'),
  ])
  const url = apiBase() + '/uploads'
  const mod = url.startsWith('https:') ? require('https') : require('http')
  return new Promise((resolve, reject) => {
    const req = mod.request(url, { method: 'POST', headers: {
      'Content-Type': 'multipart/form-data; boundary=' + boundary,
      'Content-Length': body.length,
      Authorization: 'Bearer ' + token,
      'Idempotency-Key': randomUUID(),
    } }, (res) => {
      let raw = ''
      res.setEncoding('utf8')
      res.on('data', (c) => { raw += c })
      res.on('end', () => {
        if (res.statusCode !== 201) return reject(new Error('上传凭证失败：' + res.statusCode + ' ' + raw))
        resolve(JSON.parse(raw))
      })
    })
    req.on('error', reject)
    req.write(body)
    req.end()
  })
}

module.exports = { uploadEvidenceFromTest, placeOrder, staffToken, staffPost, waitFor, waitData, waitEl, pickSku, httpGet, httpPost, httpRequest, apiBase, serverToken, loginInApp }
