// 订单后半程：取消订单、申请售后 → 撤回，以及要后台员工配合的三条
// （驳回显示理由、仅退款同意后到账、发货后确认收货）。
//
// 「一笔某状态的单」由测试进程直接造（helpers.placeOrder：下单、付款、投递沙箱回调），
// 用例只在 App 里走被测的那一步。下单本身由 checkout.test.js 在 App 里走过。
//
// 后台那三条要 KEEL_E2E_STAFF_TOKEN（演示栈上的 e2e 操作员会话，只从环境变量读）；
// 没设就跳过，买家侧的用例照跑。
const { randomUUID } = require('crypto')
const { waitFor, waitEl, httpGet, httpRequest, apiBase, serverToken, loginInApp, placeOrder, staffToken, staffPost } = require('./helpers')

const withStaff = staffToken() ? it : it.skip

async function tapTwice(page, selector) {
  // 取消 / 确认收货 / 撤回都是「点两下」：第一下变成「再点一次确认」。
  await (await waitEl(page, selector)).tap()
  await waitFor(page, selector, (t) => t.includes('再点一次'))
  await (await page.$(selector)).tap()
}

async function buyerRefund(token, orderNo, refundType) {
  const d = (await httpGet(apiBase() + '/orders/' + orderNo, token)).body
  const it0 = d.items[0]
  const r = await httpRequest('POST', apiBase() + '/orders/' + orderNo + '/refunds',
    { items: [{ order_item_id: it0.id, quantity: it0.quantity }], refund_type: refundType, reason_code: 1 },
    token, { 'Idempotency-Key': randomUUID() })
  if (r.status !== 201) throw new Error('申请售后失败：' + r.status + ' ' + JSON.stringify(r.body))
  return r.body
}

describe('订单后半程', () => {
  let token = ''

  beforeAll(async () => {
    await loginInApp()
    token = await serverToken()
  })

  it('待支付的单可以取消，取消后是已关闭', async () => {
    const orderNo = await placeOrder(token)
    const page = await program.navigateTo('/pages/order/detail?order_no=' + orderNo)
    await waitFor(page, '.t-display', (t) => t === '待支付')
    await tapTwice(page, '.cancel-btn')
    await waitFor(page, '.t-display', (t) => t === '已关闭')
    expect((await httpGet(apiBase() + '/orders/' + orderNo, token)).body.status).toBe(90)
  })

  it('已支付的单申请仅退款 → 待审核，金额由服务端算；撤回后是已取消', async () => {
    const orderNo = await placeOrder(token, { pay: true })
    const detail = await program.navigateTo('/pages/order/detail?order_no=' + orderNo)
    await waitFor(detail, '.t-display', (t) => t === '已支付')
    await (await waitEl(detail, '.refund-btn')).tap()

    let page = await program.currentPage()
    for (let i = 0; page.path !== 'pages/refund/apply' && i < 30; i++) { await detail.waitFor(300); page = await program.currentPage() }
    expect(page.path).toBe('pages/refund/apply')
    await waitEl(page, '.submit-btn')
    // 只有一行时默认全选了可退件数；类型与原因必须用户选，没选时按钮是灰的。
    expect(await page.data('refundType')).toBe(0)
    await (await page.$$('.type-opt'))[0].tap()      // 仅退款
    await (await page.$$('.reason-opt'))[0].tap()    // 不想要了
    await (await page.$('.submit-btn')).tap()

    // 提交后 redirectTo 售后详情。
    let rd = await program.currentPage()
    for (let i = 0; rd.path !== 'pages/refund/detail' && i < 50; i++) { await page.waitFor(300); rd = await program.currentPage() }
    expect(rd.path).toBe('pages/refund/detail')
    await waitFor(rd, '.t-display', (t) => t === '待审核')
    const refundNo = await rd.data('refundNo')
    const server = (await httpGet(apiBase() + '/refunds/' + refundNo, token)).body
    expect(server.status).toBe(10)
    expect(server.refund_type).toBe(1)
    // 页面显示的退款金额就是服务端算的那个。
    const shown = await (await rd.$('.refund-amount')).text()
    expect(shown).toBe('¥' + (server.amount_cents / 100).toFixed(2))

    await tapTwice(rd, '.cancel-refund-btn')
    await waitFor(rd, '.t-display', (t) => t === '已取消')
    expect((await httpGet(apiBase() + '/refunds/' + refundNo, token)).body.status).toBe(60)
  })

  withStaff('商家驳回：显示驳回理由，可以重新申请', async () => {
    const orderNo = await placeOrder(token, { pay: true })
    const r = await buyerRefund(token, orderNo, 1)
    await staffPost('/admin/refunds/' + r.refund_no + '/audit', { action: 'reject', reject_reason: 'e2e 驳回' })
    const page = await program.navigateTo('/pages/refund/detail?refund_no=' + r.refund_no)
    await waitFor(page, '.t-display', (t) => t === '已拒绝')
    await waitFor(page, '.reject', (t) => t.includes('e2e 驳回'))
    expect(await page.$('.reapply-btn')).not.toBeNull()
  })

  withStaff('商家同意仅退款：沙箱下直接到已退款，订单详情显示已退金额', async () => {
    const orderNo = await placeOrder(token, { pay: true })
    const r = await buyerRefund(token, orderNo, 1)
    await staffPost('/admin/refunds/' + r.refund_no + '/audit', { action: 'approve' })
    const page = await program.navigateTo('/pages/refund/detail?refund_no=' + r.refund_no)
    // 30 退款中 → 40 已退款：沙箱回调是异步的，页面 onShow 读到哪个都算对，重进一次读终态。
    let ok = false
    for (let i = 0; i < 20 && !ok; i++) {
      const s = (await httpGet(apiBase() + '/refunds/' + r.refund_no, token)).body.status
      ok = s === 40
      if (!ok) await page.waitFor(500)
    }
    expect(ok).toBe(true)
    const again = await program.redirectTo('/pages/refund/detail?refund_no=' + r.refund_no)
    await waitFor(again, '.t-display', (t) => t === '已退款')
    const od = await program.navigateTo('/pages/order/detail?order_no=' + orderNo)
    await waitFor(od, '.t-display', (t) => t.length > 0)
    const view = await od.data('view')
    expect(view.refundedText).toBe('¥' + (r.amount_cents / 100).toFixed(2))
  })

  withStaff('已发货的单确认收货后是已完成', async () => {
    const orderNo = await placeOrder(token, { pay: true })
    await staffPost('/admin/orders/' + orderNo + '/shipments', { carrier_code: 'SF', tracking_no: 'E2E' + Date.now() })
    const page = await program.navigateTo('/pages/order/detail?order_no=' + orderNo)
    await waitFor(page, '.t-display', (t) => t === '已发货')
    await tapTwice(page, '.confirm-btn')
    await waitFor(page, '.t-display', (t) => t === '已完成')
    expect((await httpGet(apiBase() + '/orders/' + orderNo, token)).body.status).toBe(40)
  })
})
