// 营销活动与包邮券。演示店种子：全场满 199 减 20 / 满 399 减 50；挂耳咖啡（商品 19，规格 21）
// 限时特价 49.9、每人限购 2 件。包邮券是模板 3（每人可领 100 张，可反复跑）。
//
// 断言的都是「页面显示的 = 服务端给的」：金额、标签、说明文案都来自服务端，客户端不复算。
// 这里写死了种子里的商品与模板 id —— 它们是服务端专为演示 / e2e 种的（见服务端的种子说明），
// 种子变了这条用例要跟着改，而不是让它自己去猜哪件商品在做活动。
const { randomUUID } = require('crypto')
const { waitFor, waitData, httpGet, httpRequest, apiBase, serverToken, loginInApp } = require('./helpers')

const GUAER = { productId: 19, skuId: 21 }      // 限时特价 49.9（门店价 69），限购 2
const MILL = { productId: 22, skuId: 25 }       // 238 元：命中满 199 减 20，且满 99 包邮
const CHEAP = { productId: 23, skuId: 26 }      // 55.8 元：运费 8 元
const FREE_SHIP_TEMPLATE = 3
const cents = (t) => Math.round(parseFloat(t.replace(/[-¥]/g, '')) * 100)

async function checkout(sku) {
  return program.navigateTo('/pages/order/create?sku_id=' + sku.skuId + '&product_id=' + sku.productId)
}

describe('营销活动与包邮券', () => {
  let token = ''

  beforeAll(async () => {
    await loginInApp()
    token = await serverToken()
    await httpRequest('DELETE', apiBase() + '/cart', null, token)
  })

  afterAll(async () => {
    await httpRequest('DELETE', apiBase() + '/cart', null, token)
  })

  it('首页与详情显示活动标签；详情按特价显示并划线门店价', async () => {
    const home = await program.reLaunch('/pages/products/list')
    const rows = await waitData(home, 'rows', (r) => r.some((x) => x.id === GUAER.productId))
    const row = rows.find((x) => x.id === GUAER.productId)
    expect(row.promoTags.some((t) => t.includes('限时特价'))).toBe(true)

    const detail = await program.navigateTo('/pages/products/detail?id=' + GUAER.productId)
    await waitData(detail, 'skuId', (v) => v === GUAER.skuId)
    await waitFor(detail, '.t-price-l', (t) => t === '¥49.90')
    await waitFor(detail, '.strike', (t) => t === '¥69.00')
  })

  it('限时特价：结算按特价算并显示说明；买 3 件超过限购，提示减数量', async () => {
    const page = await checkout(GUAER)
    const pv = await waitData(page, 'pv', (p) => p != null)
    expect(pv.goodsAmountText).toBe('¥49.90')
    expect(pv.promotionNotes.some((n) => n.includes('限时特价'))).toBe(true)
    // 手上有包邮券时 1 件（49.9 + 8 运费）会自动选上它；加到 2 件满 99 包邮，那张券就用不上了 ——
    // 页面照设计报「本单运费为 0，包邮券抵不了钱」并让用户换券（实测）。这条测的是限购，先不用券。
    await page.setData({ couponId: 0, couponAuto: false })

    await (await page.$$('.step'))[1].tap()   // +
    await waitData(page, 'pv', (p) => p != null && p.goodsAmountText === '¥99.80')
    await (await page.$$('.step'))[1].tap()   // + → 3 件
    await waitFor(page, '.result-err', (t) => t.includes('限购'))
    expect((await page.data('pv')) == null).toBe(true)
  })

  it('满 199 减 20：活动优惠一行、凑单说明，应付 = 商品 + 运费 − 优惠', async () => {
    const page = await checkout(MILL)
    const pv = await waitData(page, 'pv', (p) => p != null)
    expect(pv.promotionDiscountText).toBe('-¥20.00')
    expect(pv.promotionNotes.some((n) => n.includes('已减 20 元'))).toBe(true)
    expect(pv.payableCents).toBe(cents(pv.goodsAmountText) + cents(pv.freightText) - cents(pv.discountText))
  })

  it('包邮券：99 元以下的单自动选上、抵掉 8 元运费；已包邮的单里不出现', async () => {
    const claim = await httpRequest('POST', apiBase() + '/coupon-templates/' + FREE_SHIP_TEMPLATE + '/claim', null, token,
      { 'Idempotency-Key': randomUUID() })
    expect(claim.status).toBe(201)

    const cheap = await checkout(CHEAP)
    const pv = await waitData(cheap, 'pv', (p) => p != null && p.couponId > 0)
    const chosen = pv.coupons.find((c) => c.id === pv.couponId)
    expect(chosen.name).toContain('包邮')
    expect(pv.freightText).toBe('¥8.00')
    expect(pv.freightDiscountText).toBe('-¥8.00')
    expect(pv.payableCents).toBe(cents(pv.goodsAmountText))

    const dear = await checkout(MILL)
    const pv2 = await waitData(dear, 'pv', (p) => p != null)
    expect(pv2.freightText).toBe('¥0.00')
    expect(pv2.coupons.some((c) => c.name.includes('包邮'))).toBe(false)
  })

  it('包邮券选着时数量加到满 99 包邮：显示「包邮券抵不了钱」并展开券列表，不悄悄换掉', async () => {
    // 上一条已经领过包邮券。挂耳 1 件（49.9 + 8 运费）会自动选上它，加到 2 件（99.8）就包邮了。
    const page = await checkout(GUAER)
    const pv = await waitData(page, 'pv', (p) => p != null && p.couponId > 0)
    expect(pv.coupons.find((c) => c.id === pv.couponId).name).toContain('包邮')
    await (await page.$$('.step'))[1].tap()
    await waitFor(page, '.result-err', (t) => t.includes('包邮券'))
    expect(await page.data('couponOpen')).toBe(true)
    expect((await page.data('pv')) == null).toBe(true)
  })

  it('购物车：特价行划线门店价；满减显示已减金额与凑单说明', async () => {
    const store = (await httpGet(apiBase() + '/products?page_size=1')).body.store.store_id
    for (const sku of [GUAER, MILL]) {
      const r = await httpRequest('POST', apiBase() + '/cart/items?store_id=' + store, { sku_id: sku.skuId, quantity: 1 }, token,
        { 'Idempotency-Key': randomUUID() })
      expect(r.status).toBe(200)
    }
    const cart = await program.switchTab('/pages/cart/index')
    const rows = await waitData(cart, 'rows', (r) => r.length === 2)
    const g = rows.find((x) => x.skuId === GUAER.skuId)
    expect(g.priceText).toBe('¥49.90')
    expect(g.listPriceText).toBe('¥69.00')
    await waitData(cart, 'promotionDiscountText', (t) => t === '-¥20.00')
    expect((await cart.data('promotionNotes')).length).toBeGreaterThan(0)
  })
})
