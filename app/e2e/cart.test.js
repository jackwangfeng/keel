// 购物车：商品页加购 → 购物车（与服务端同一家店的价）→ 调数量 → 去结算，
// 试算的商品金额与购物车合计逐分一致 → 下单成功后这几行从车里删掉。
//
// 每跑一次会真的下一单（不付款，待支付的单超时会被服务端关掉）。
const { waitFor, waitData, waitEl, pickSku, httpGet, httpRequest, apiBase, serverToken, loginInApp } = require('./helpers')

describe('购物车', () => {
  let token = ''

  beforeAll(async () => {
    await loginInApp()
    token = await serverToken()
    // 从空车开始：演示栈的车可能被上一次跑或手动试用留下东西。
    await httpRequest('DELETE', apiBase() + '/cart', null, token)
  })

  it('商品页加入购物车', async () => {
    // 要调到 2 件，库存至少得够 2 件；留点余量。
    const sku = await pickSku(5)
    const page = await program.navigateTo('/pages/products/detail?id=' + sku.productId)
    await waitFor(page, '.t-price-l', (t) => t.startsWith('¥'))
    await waitData(page, 'storeId', (v) => v > 0)
    await (await page.$('.cart-btn')).tap()
    await waitFor(page, '.t-ok', (t) => t.includes('已加入购物车'))
  })

  it('购物车显示这一行、勾选着，调大数量后合计跟着变', async () => {
    const page = await program.switchTab('/pages/cart/index')
    const rows = await waitData(page, 'rows', (r) => r.length === 1)
    expect(rows[0].available).toBe(true)
    expect(rows[0].selected).toBe(true)
    expect(await page.data('selectedCount')).toBe(1)
    const one = await page.data('selectedText')

    await (await page.$('.step-plus')).tap()
    await waitData(page, 'rows', (r) => r.length === 1 && r[0].quantity === 2)
    const two = await page.data('selectedText')
    // ¥a.bc -> 分。两件的合计是一件的两倍（同一家店的实时价）。
    const cents = (t) => Math.round(parseFloat(t.replace('¥', '')) * 100)
    expect(cents(two)).toBe(cents(one) * 2)
    // 和服务端按同一家店算的 selected_total_cents 一致。
    const storeId = (await page.data('storeId'))
    const server = (await httpGet(apiBase() + '/cart?store_id=' + storeId, token)).body
    expect(cents(two)).toBe(server.selected_total_cents)
  })

  it('去结算：试算的商品金额 = 购物车合计；下单后车里这行被删掉', async () => {
    const cart = await program.currentPage()
    const total = await cart.data('selectedText')
    await (await cart.$('.bar-btn')).tap()
    let page = await program.currentPage()
    const deadline0 = Date.now() + 10000
    while (page.path !== 'pages/order/create') {
      if (Date.now() > deadline0) throw new Error('没有跳到结算页，当前 ' + page.path)
      await cart.waitFor(300)
      page = await program.currentPage()
    }
    await waitFor(page, '.t-price-l', (t) => t.startsWith('¥'))
    const pv = await page.data('pv')
    expect(pv.goodsAmountText).toBe(total)
    expect((await page.data('cartRows')).length).toBe(1)

    await (await page.$('.bar-btn')).tap()
    await waitFor(page, '.result-ok', (t) => t.includes('下单成功'))
    const deadline = Date.now() + 15000
    for (;;) {
      const c = (await httpGet(apiBase() + '/cart', token)).body
      if (c.items.length === 0) break
      if (Date.now() > deadline) throw new Error('下单后购物车里的行没被删掉：' + JSON.stringify(c.items))
      await page.waitFor(500)
    }
  })
})
