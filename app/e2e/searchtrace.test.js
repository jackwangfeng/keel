// 搜索效果回传：搜索 → 从结果点进商品 → 加购 → 购物车结算 → 下单。
//
// 回传是「发了就不管」，App 里看不到结果；这条用例断言的是：
//   · 这批结果带着 trace_id（页面存下了）；
//   · 用例从测试进程对同一个 trace_id + 商品重发一遍 click，服务端回 204 ——
//     证明页面存下的 trace_id 与商品 id 是服务端认的那一对（不认的话是 404 / 422）；
//   · 回传不打断业务：加购、下单照常成功。
// 服务端有没有真的记下 click / add_cart / order 三条，用例看不到 —— 打印出 trace_id，
// 请服务端那边按它查日志核对。
//
// 每跑一次会下一笔不付款的单。
const { waitFor, waitData, waitEl, pickSku, httpGet, httpRequest, apiBase, serverToken, loginInApp } = require('./helpers')

describe('搜索效果回传', () => {
  let token = ''
  let traceId = ''
  let productId = 0

  beforeAll(async () => {
    await loginInApp()
    token = await serverToken()
    await httpRequest('DELETE', apiBase() + '/cart', null, token)
  })

  it('搜索结果带 trace_id；点进商品，服务端认这对 trace_id + 商品', async () => {
    const sku = await pickSku(5)
    const title = (await httpGet(apiBase() + '/products/' + sku.productId)).body.title
    const page = await program.navigateTo('/pages/search/index')
    await (await waitEl(page, '.box-input')).input(title)
    await (await page.$('.go')).tap()
    await waitFor(page, '.count', (t) => t.includes('共'))
    traceId = await page.data('traceId')
    // 服务端写搜索日志失败时这批结果没有 trace_id（契约如此，页面这时存空串、不回传）。
    // 实测偶发过两次（2026-09-26 14:55 / 14:57 UTC）：服务端核对是那台机器当时整体卡顿，
    // 日志写入超过 200ms 上限被放弃（设计上宁可丢一行日志也不拖慢搜索）。所以重搜一次；
    // 两次都没有才算失败 —— 客户端「从来不存 trace_id」这种回归照样抓得住。
    if (!/^[0-9a-f]{32}$/.test(traceId)) {
      console.log('searchtrace: 第一次搜索没有 trace_id，重搜一次（' + new Date().toISOString() + '）')
      await (await page.$('.go')).tap()
      await page.waitFor(2000)
      await waitFor(page, '.count', (t) => t.includes('共'))
      traceId = await page.data('traceId')
    }
    expect(traceId).toMatch(/^[0-9a-f]{32}$/)
    const rows = await page.data('rows')
    const idx = rows.findIndex((r) => r.id === sku.productId)
    expect(idx).toBeGreaterThanOrEqual(0)
    productId = sku.productId
    await (await page.$$('.hit'))[idx].tap()

    const again = await httpRequest('POST', apiBase() + '/search/events', { trace_id: traceId, event: 'click', product_id: productId })
    expect(again.status).toBe(204)
    console.log('searchtrace: trace_id=' + traceId + ' product_id=' + productId)
  })

  it('加购与下单照常成功（回传 add_cart / order 在后台发出）', async () => {
    let detail = await program.currentPage()
    for (let i = 0; detail.path !== 'pages/products/detail' && i < 30; i++) { await detail.waitFor(300); detail = await program.currentPage() }
    await waitData(detail, 'storeId', (v) => v > 0)
    // 等详情真的渲染出来再点：H5 下 navigateTo 有切页动画，动画没走完就按坐标点，会点空（实测偶发：
    // 页面数据都齐了，加购请求却没发出去）。点了 5 秒没反应再点一次 —— 多加一件不影响这条的断言。
    await waitFor(detail, '.name', (t) => t.length > 0)
    await (await waitEl(detail, '.cart-btn')).tap()
    await waitFor(detail, '.t-ok', (t) => t.includes('已加入购物车'), 5000).catch(async () => {
      await (await detail.$('.cart-btn')).tap()
      await waitFor(detail, '.t-ok', (t) => t.includes('已加入购物车'))
    })

    const cart = await program.switchTab('/pages/cart/index')
    await waitData(cart, 'rows', (r) => r.length === 1 && r[0].productId === productId)
    await (await cart.$('.bar-btn')).tap()
    let page = await program.currentPage()
    for (let i = 0; page.path !== 'pages/order/create' && i < 30; i++) { await cart.waitFor(300); page = await program.currentPage() }
    await waitFor(page, '.t-price-l', (t) => t.startsWith('¥'))
    await (await page.$('.bar-btn')).tap()
    await waitFor(page, '.result-ok', (t) => t.includes('下单成功'))
    console.log('searchtrace: order_no=' + (await page.data('orderNo')))
  })
})
