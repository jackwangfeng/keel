// 下单主链路：登录 → 试算 → 下单 → 发起支付 → 投递沙箱回调 → 已支付。
//
// 打的是 apk 里编进去的真实服务端（KEEL_API_BASE），每跑一次会真的建一笔订单并走沙箱入账。
// 沙箱没有真实资金流动（见 app/README.md「沙箱支付」）。
const { waitFor, pickSku, waitOrderPlaced } = require('./helpers')

describe('下单主链路', () => {
  let orderNo = ''

  beforeAll(async () => {
    // 从未登录开始：清掉本机会话与服务地址覆盖，用 apk 里注入的默认地址。
    await program.callUniMethod('clearStorageSync')
  })

  it('登录', async () => {
    const page = await program.reLaunch('/pages/auth/login')
    await page.waitFor(1000)
    await (await page.$('.btn')).tap()
    await waitFor(page, '.msg', (t) => t.includes('登录成功'))
  })

  it('试算出应付金额并下单', async () => {
    const sku = await pickSku(1)
    const page = await program.navigateTo('/pages/order/create?sku_id=' + sku.skuId + '&product_id=' + sku.productId)
    // 登录后进页面会自动试算；底部栏的应付金额从 — 变成具体金额。
    // 选择器只写单个类：原生端的选择器引擎不认后代选择器（`.bar .t-price-l` 返回 null，实测）。
    // 这一页上 .t-price-l 只有底部栏那一个。
    await waitFor(page, '.t-price-l', (t) => t.startsWith('¥'))
    await (await page.$('.bar-btn')).tap()
    // 新下的单直接跳到订单详情去付款（不再停在确认页、没有「查看订单」）。
    orderNo = (await waitOrderPlaced(page)).orderNo
    expect(orderNo).toMatch(/^\d{14}/)
  })

  it('支付并入账后订单变成已支付', async () => {
    // 下单后已经在这笔的订单详情上。沙箱下：点底栏「立即支付」发起支付，底栏按钮随即变成
    // 「模拟支付完成（沙箱）」，再点它投递回调。页面下方的沙箱说明（没有真实资金流动）照样要在。
    const page = await program.currentPage()
    expect(page.path).toBe('pages/order/detail')
    expect(await page.data('orderNo')).toBe(orderNo)
    await waitFor(page, '.t-display', (t) => t === '待支付')
    await (await page.$('.bar-btn')).tap()
    await waitFor(page, '.notice-title', (t) => t.includes('沙箱支付'))
    await waitFor(page, '.bar-btn', (t) => t.includes('模拟支付完成（沙箱）'))
    await (await page.$('.bar-btn')).tap()
    await waitFor(page, '.t-display', (t) => t === '已支付')
  })
})
