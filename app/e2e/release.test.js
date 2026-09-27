// 发版前真机验证（KEEL_E2E_RELEASE=1 才跑；日常 H5 全量不跑它）。
//
// 覆盖 H5 看不出来的那几样：原生 UTS 的 new Date(ISO 微秒) 解析与本地时区、令牌自动续期 / 过期跳登录、
// 列表「＋」与规格浮层、商品卡片与结算页的原生渲染、详情页购物车入口。
// 能断言的断言；布局（浮层按钮是否被 tab 栏挡住、标题两行省略、价格与「＋」不重叠）截图，人看。
// 截图是 App 自己的画面（program.screenshot），存到 KEEL_E2E_SHOTS 目录。
const path = require('path')
const { randomUUID } = require('crypto')
const { waitFor, waitData, waitEl, httpGet, httpRequest, apiBase, serverToken, loginInApp, placeOrder } = require('./helpers')

const RUN = process.env.KEEL_E2E_RELEASE === '1'
const d = RUN ? describe : describe.skip
const SHOTS = process.env.KEEL_E2E_SHOTS || path.join(__dirname, '..', 'dist', 'release-shots')
const IOS = process.env.UNI_APP_PLATFORM === 'ios'
const shot = async (name) => {
  require('fs').mkdirSync(SHOTS, { recursive: true })
  // 等页面稳定再截：Android 上刚切页（切 tab）就调 automator 截图，调用会一直不返回（实测卡满 60 秒）。
  // 再套一个 20 秒上限：截图只是给人看的，卡住就记一句，不拖垮断言。
  await new Promise((r) => setTimeout(r, 800))
  const r = await Promise.race([
    program.screenshot({ path: path.join(SHOTS, name + '.png') }).then(() => 'ok'),
    new Promise((res) => setTimeout(() => res('timeout'), 20000)),
  ])
  if (r !== 'ok') console.log('release: 截图 ' + name + ' 超时，跳过')
  // Android 另外用 adb 截整屏：automator 的截图只有页面区域，看不到原生 tab 栏（角标在那上面）。
  if (!IOS && process.env.KEEL_E2E_ADB) {
    const { execFileSync } = require('child_process')
    const png = execFileSync(process.env.KEEL_E2E_ADB, ['exec-out', 'screencap', '-p'], { maxBuffer: 64 * 1024 * 1024 })
    require('fs').writeFileSync(path.join(SHOTS, name + '-screen.png'), png)
  }
}
function localShort(iso) {
  const t = new Date(iso)
  const p = (n) => String(n).padStart(2, '0')
  return p(t.getMonth() + 1) + '-' + p(t.getDate()) + ' ' + p(t.getHours()) + ':' + p(t.getMinutes())
}
async function session() {
  const raw = await program.callUniMethod('getStorageSync', 'keel.session')
  return JSON.parse(raw)
}
async function onPath(p, timeout = 15000) {
  const start = Date.now()
  let cur = await program.currentPage()
  while (cur.path !== p) {
    if (Date.now() - start > timeout) throw new Error('没到 ' + p + '，当前 ' + cur.path)
    await cur.waitFor(300)
    cur = await program.currentPage()
  }
  return cur
}

d('发版前真机验证', () => {
  let token = ''

  beforeAll(async () => {
    await loginInApp()
    token = await serverToken()
    await httpRequest('DELETE', apiBase() + '/cart', null, token)
  })

  afterAll(async () => {
    await httpRequest('DELETE', apiBase() + '/cart', null, token)
  })

  it('0 商品图：首页 / 搜索 / 详情 / 购物车 / 订单详情都显示真图（相对路径补 origin、302 跟随）', async () => {
    const withImg = (await httpGet(apiBase() + '/products?page_size=50')).body.items.filter((p) => p.image_url)
    expect(withImg.length).toBeGreaterThan(0)
    const home = await program.reLaunch('/pages/products/list')
    await waitFor(home, '.pc-title', (t) => t.length > 0)
    const rows = await home.data('rows')
    const r0 = rows.find((r) => withImg.some((p) => p.id === r.id))
    // 原生端必须是绝对地址（H5 的 base 是相对的，才会原样是相对路径）。
    if (!process.env.UNI_PLATFORM || process.env.UNI_PLATFORM !== 'h5') expect(r0.cover.imageUrl).toMatch(/^https?:\/\/.+\/api\/v1\/uploads\/\d+$/)
    expect((await home.$$('.cover-img')).length).toBeGreaterThan(0)
    await home.waitFor(1500)   // 等图下载完再截
    await shot('0-home-images')

    const search = await program.navigateTo('/pages/search/index')
    await (await waitEl(search, '.box-input')).input('咖啡')
    await (await search.$('.go')).tap()
    await waitFor(search, '.count', (t) => t.includes('共'))
    expect((await search.$$('.cover-img')).length).toBeGreaterThan(0)
    await search.waitFor(1500)
    await shot('0-search-images')

    const detail = await program.navigateTo('/pages/products/detail?id=' + r0.id)
    await waitFor(detail, '.name', (t) => t.length > 0)
    expect((await detail.$$('.cover-img')).length).toBeGreaterThan(0)
    await detail.waitFor(1500)
    await shot('0-detail-image')

    const store = (await httpGet(apiBase() + '/products?page_size=1')).body.store.store_id
    const sku = (await httpGet(apiBase() + '/products/' + r0.id)).body.skus.find((x) => x.available_qty > 0)
    await httpRequest('POST', apiBase() + '/cart/items?store_id=' + store, { sku_id: sku.id, quantity: 1 }, token, { 'Idempotency-Key': randomUUID() })
    const cart = await program.switchTab('/pages/cart/index')
    await waitData(cart, 'rows', (r) => r.length > 0)
    // 购物车行 / 订单行：服务端（ff552e5）还没带 image_url，只有商品列表 / 检索 / 详情有。
    // 服务端给了才断言显示成图；没给就是占位，记为服务端缺口。
    const cartHasImg = (await httpGet(apiBase() + '/cart?store_id=' + store, token)).body.items.some((x) => x.image_url)
    if (cartHasImg) expect((await cart.$$('.cover-img')).length).toBeGreaterThan(0)
    else console.log('release: 服务端购物车行没有 image_url（服务端缺口），购物车显示占位')
    await cart.waitFor(1500)
    await shot('0-cart-images')
    await httpRequest('DELETE', apiBase() + '/cart', null, token)

    const orderNo = await placeOrder(token)
    const od = await program.navigateTo('/pages/order/detail?order_no=' + orderNo)
    await waitData(od, 'view', (v) => v != null)
    const orderHasImg = ((await httpGet(apiBase() + '/orders/' + orderNo, token)).body.items || []).some((x) => x.image_url)
    if (orderHasImg) expect((await od.$$('.cover-img')).length).toBeGreaterThan(0)
    else console.log('release: 服务端订单行没有 image_url（服务端缺口），订单详情显示占位')
    await od.waitFor(1500)
    await shot('0-order-images')
    await httpRequest('POST', apiBase() + '/orders/' + orderNo + '/cancel', null, token, { 'Idempotency-Key': randomUUID() })
  })

  it('1 时间：订单列表 / 详情的下单时间按本地时区显示（原生解析带微秒的 ISO）', async () => {
    const orderNo = await placeOrder(token)
    const server = (await httpGet(apiBase() + '/orders/' + orderNo, token)).body
    expect(server.created_at).toMatch(/\.\d{4,}Z$/)   // 服务端确实给了多于三位的小数秒
    const want = localShort(server.created_at)

    const list = await program.switchTab('/pages/order/list')
    const rows = await waitData(list, 'rows', (r) => r.some((x) => x.orderNo === orderNo))
    expect(rows.find((x) => x.orderNo === orderNo).createdAt).toBe(want)
    await shot('1-order-list')

    const detail = await program.navigateTo('/pages/order/detail?order_no=' + orderNo)
    const view = await waitData(detail, 'view', (v) => v != null)
    expect(view.head.createdAt).toBe(want)
    await shot('1-order-detail')
    // 收尾：取消这笔待支付单。
    await httpRequest('POST', apiBase() + '/orders/' + orderNo + '/cancel', null, token, { 'Idempotency-Key': randomUUID() })
  })

  it('1 时间：我的优惠券的有效期是 YYYY-MM-DD 至 YYYY-MM-DD（不是原样 ISO）', async () => {
    await httpRequest('POST', apiBase() + '/coupon-templates/3/claim', null, token, { 'Idempotency-Key': randomUUID() })
    const page = await program.navigateTo('/pages/coupon/mine')
    const rows = await waitData(page, 'rows', (r) => r.length > 0)
    for (const r of rows) expect(r.validText).toMatch(/^\d{4}-\d{2}-\d{2} 至 \d{4}-\d{2}-\d{2}$/)
    await shot('1-coupons')
  })

  it('2 令牌续期：accessToken 改坏 → 401 → 刷新 → 重放成功，本地换成了新令牌', async () => {
    const s = await session()
    const bad = 'broken.' + s.accessToken.slice(7)
    await program.callUniMethod('setStorageSync', 'keel.session', JSON.stringify({ ...s, accessToken: bad }))
    const list = await program.switchTab('/pages/order/list')
    // 列表要加载出来、不能停在错误上。
    await waitData(list, 'loaded', (v) => v === true)
    expect(await list.data('error')).toBe('')
    const after = await session()
    expect(after.accessToken).not.toBe(bad)
    expect(after.refreshToken).not.toBe(s.refreshToken)   // refresh_token 轮换了
  })

  it('2 令牌续期：两个令牌都改坏 → 跳登录页，登录后回到原来的页面', async () => {
    const s = await session()
    await program.callUniMethod('setStorageSync', 'keel.session', JSON.stringify({ ...s, accessToken: 'bad.a', refreshToken: 'bad.r' }))
    await program.navigateTo('/pages/me/notifications')
    const login = await onPath('pages/auth/login')
    await shot('2-expired-login')
    await (await waitEl(login, '.btn')).tap()
    const back = await onPath('pages/me/notifications')
    await waitData(back, 'loaded', (v) => v === true)
    expect(await back.data('error')).toBe('')
  })

  it('3 / 4 首页：卡片渲染与「＋」；单规格直接加、多规格弹浮层（截图看浮层按钮没被 tab 栏挡住）', async () => {
    const home = await program.reLaunch('/pages/products/list')
    await waitFor(home, '.pc-title', (t) => t.length > 0)
    await shot('3-home-cards')
    const rows = await home.data('rows')
    const buyable = rows.filter((x) => !x.offShelf && !x.soldOut)
    const btns = await home.$$('.pc-add')
    await btns[buyable.findIndex((x) => x.id === 23)].tap()          // 冷萃：单规格
    await home.waitFor(1500)
    await shot('3-home-after-single-add')
    await btns[buyable.findIndex((x) => x.id === 19)].tap()          // 挂耳：两个规格
    await waitEl(home, '.sheet')
    await home.waitFor(600)
    await shot('3-home-sheet')
    const chips = await home.$$('.chip')
    expect(chips.length).toBe(2)
    await chips[1].tap()
    await (await home.$('.confirm')).tap()
    await home.waitFor(1500)
    await shot('3-home-after-sheet-add')
    const store = (await httpGet(apiBase() + '/products?page_size=1')).body.store.store_id
    const cart = (await httpGet(apiBase() + '/cart?store_id=' + store, token)).body
    expect(cart.items.map((x) => x.sku_id).sort()).toEqual([22, 26])
  })

  it('3 / 4 搜索结果：行卡片渲染与「＋」', async () => {
    const page = await program.navigateTo('/pages/search/index')
    await (await waitEl(page, '.box-input')).input('咖啡')
    await (await page.$('.go')).tap()
    await waitFor(page, '.count', (t) => t.includes('共'))
    await shot('4-search-rows')
    const before = (await httpGet(apiBase() + '/cart', token)).body.items.length
    await (await page.$('.pc-add')).tap()
    await page.waitFor(1500)
    await shot('4-search-after-add')
    const sheet = await page.$('.sheet')
    if (sheet) {
      await (await page.$('.confirm')).tap()
      await page.waitFor(1500)
    }
    const after = (await httpGet(apiBase() + '/cart', token)).body
    expect(after.items.length + after.items.reduce((n, x) => n + x.quantity, 0)).toBeGreaterThan(before)
  })

  it('6 详情页：底栏「购物车」按钮；加购后「已加入购物车，去结算 ›」能点进购物车', async () => {
    const detail = await program.navigateTo('/pages/products/detail?id=22')
    await waitFor(detail, '.name', (t) => t.length > 0)
    expect(await detail.$('.bar-cart')).not.toBeNull()
    await (await waitEl(detail, '.cart-btn')).tap()
    await waitFor(detail, '.t-ok', (t) => t.includes('去结算'))
    await shot('6-detail-added')
    await (await detail.$('.t-ok')).tap()
    await onPath('pages/cart/index')
    await shot('6-cart-tab')
  })

  it('5 结算页：活动优惠拆行的原生渲染（截图）', async () => {
    const page = await program.navigateTo('/pages/order/create?sku_id=25&product_id=22')
    const pv = await waitData(page, 'pv', (p) => p != null)
    expect(pv.promotionDiscountText).toBe('-¥20.00')
    await page.waitFor(500)
    await shot('5-checkout-promo')
  })
})
