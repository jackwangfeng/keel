// 订单后半程：取消订单、申请售后 → 撤回，以及要后台员工配合的三条
// （驳回显示理由、仅退款同意后到账、发货后确认收货）。
//
// 「一笔某状态的单」由测试进程直接造（helpers.placeOrder：下单、付款、投递沙箱回调），
// 用例只在 App 里走被测的那一步。下单本身由 checkout.test.js 在 App 里走过。
//
// 后台那三条的前置状态（已发货 / 已驳回 / 已退款）要后台员工来做。两种给法：
//   · KEEL_E2E_STAFF_TOKEN：用例自己造单、自己调后台接口；
//   · 现成的单号：KEEL_E2E_SHIPPED_ORDER（已发货的单）、KEEL_E2E_REJECTED_REFUND（已驳回的售后单）、
//     KEEL_E2E_REFUNDED_REFUND（已同意的仅退款售后单）—— 由服务端那边的后台会话代做好再交过来。
// 都没有就跳过，买家侧的用例照跑。
const { randomUUID } = require('crypto')
const { waitFor, waitEl, httpGet, httpRequest, apiBase, serverToken, loginInApp, placeOrder, staffToken, staffPost, uploadEvidenceFromTest } = require('./helpers')

const env = process.env

// 服务端时间（UTC 的 RFC3339）按本机时区出「MM-DD HH:mm」—— 和 App 的 shortTime 同一口径。
// H5 无头跑时 Chrome 的时区跟随本机，测试进程也是本机，所以两边一致；不写死北京时间的钟点。
function localShort(iso) {
  const d = new Date(iso)
  const p = (n) => String(n).padStart(2, '0')
  return p(d.getMonth() + 1) + '-' + p(d.getDate()) + ' ' + p(d.getHours()) + ':' + p(d.getMinutes())
}
const withStaff = (fixture) => (staffToken() || env[fixture] ? it : it.skip)
// 只能由后台交单号的那几条（没有 staff token 的自动造数路径）。
const withFixture = (fixture) => (env[fixture] ? it : it.skip)

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
    // 未发货：服务端只收仅退款，「退货退款」置灰、点了不生效。
    await waitFor(page, '.type-note', (t) => t.includes('只能申请仅退款'))
    expect(await page.data('returnAllowed')).toBe(false)
    await (await page.$$('.type-opt'))[1].tap()
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

  it('带凭证的售后单：凭证图带令牌读出来显示', async () => {
    // 相册选图没法自动化：凭证由测试进程上传（POST /uploads，purpose=3），售后单也由测试进程建，
    // App 里验证的是 GET /uploads/{id} 要带令牌读、<image> 带不了头这一段 —— downloadFile 之后显示。
    const orderNo = await placeOrder(token, { pay: true })
    const up = await uploadEvidenceFromTest(token)
    const d = (await httpGet(apiBase() + '/orders/' + orderNo, token)).body
    const r = await httpRequest('POST', apiBase() + '/orders/' + orderNo + '/refunds',
      { items: [{ order_item_id: d.items[0].id, quantity: 1 }], refund_type: 1, reason_code: 3, evidence_urls: [up.url] },
      token, { 'Idempotency-Key': randomUUID() })
    expect(r.status).toBe(201)
    const page = await program.navigateTo('/pages/refund/detail?refund_no=' + r.body.refund_no)
    await waitFor(page, '.t-display', (t) => t === '待审核')
    const deadline = Date.now() + 15000
    let imgs = []
    while (imgs.length === 0 && Date.now() < deadline) {
      imgs = await page.$$('.evidence-img')
      if (imgs.length === 0) await page.waitFor(300)
    }
    expect(imgs.length).toBe(1)
    expect(await page.data('evidenceError')).toBe('')
    // 收尾：撤回，免得演示库里堆待审核的单。
    await httpRequest('POST', apiBase() + '/refunds/' + r.body.refund_no + '/cancel', null, token, { 'Idempotency-Key': randomUUID() })
  })

  withFixture('KEEL_E2E_RETURN_REFUND')('待买家退货：填寄回物流，再改一次，状态仍是待买家退货', async () => {
    // 前置：一张退货退款单被商家同意（10→20）。只能由后台做，单号经环境变量交来（staff token 路径没实现这条）。
    const refundNo = env.KEEL_E2E_RETURN_REFUND
    const page = await program.navigateTo('/pages/refund/detail?refund_no=' + refundNo)
    await waitFor(page, '.t-display', (t) => t === '待买家退货')
    await waitEl(page, '.return-btn')
    // 还没填物流时提示寄回截止时间（return_deadline_at，本地时区）。
    const before = (await httpGet(apiBase() + '/refunds/' + refundNo, token)).body
    expect(before.return_deadline_at).toBeTruthy()
    expect(before.return_shipment == null).toBe(true)   // 这条要一张还没填过物流的单
    await waitFor(page, '.return-deadline', (t) => t.includes('请在 ' + localShort(before.return_deadline_at) + ' 前寄回'))
    const no1 = 'E2E' + Date.now()
    await (await page.$$('.carrier-opt'))[0].tap()   // 顺丰 sf
    await (await page.$('.f-tracking')).input(no1)
    await (await page.$('.return-btn')).tap()
    await waitFor(page, '.t-ok', (t) => t.includes('物流信息已提交'))
    let server = (await httpGet(apiBase() + '/refunds/' + refundNo, token)).body
    expect(server.status).toBe(20)
    expect(server.return_shipment.carrier_code).toBe('sf')
    expect(server.return_shipment.tracking_no).toBe(no1)
    await waitFor(page, '.return-filled', (t) => t.includes(no1))

    const no2 = no1 + 'B'
    await (await page.$$('.carrier-opt'))[1].tap()   // 京东 jd
    await (await page.$('.f-tracking')).input(no2)
    await (await page.$('.return-btn')).tap()
    await waitFor(page, '.return-filled', (t) => t.includes(no2))
    server = (await httpGet(apiBase() + '/refunds/' + refundNo, token)).body
    expect(server.status).toBe(20)
    expect(server.return_shipment.carrier_code).toBe('jd')
    expect(server.return_shipment.tracking_no).toBe(no2)
  })

  withStaff('KEEL_E2E_REJECTED_REFUND')('商家驳回：显示驳回理由，可以重新申请', async () => {
    let refundNo = env.KEEL_E2E_REJECTED_REFUND
    if (staffToken()) {
      const orderNo = await placeOrder(token, { pay: true })
      const r = await buyerRefund(token, orderNo, 1)
      await staffPost('/admin/refunds/' + r.refund_no + '/audit', { action: 'reject', reject_reason: 'e2e 驳回' })
      refundNo = r.refund_no
    }
    const server = (await httpGet(apiBase() + '/refunds/' + refundNo, token)).body
    expect(server.status).toBe(50)
    const page = await program.navigateTo('/pages/refund/detail?refund_no=' + refundNo)
    await waitFor(page, '.t-display', (t) => t === '已拒绝')
    await waitFor(page, '.reject', (t) => t.includes(server.reject_reason))
    expect(await page.$('.reapply-btn')).not.toBeNull()
  })

  withStaff('KEEL_E2E_REFUNDED_REFUND')('商家同意仅退款：沙箱下直接到已退款，订单详情显示已退金额', async () => {
    let r
    if (staffToken()) {
      const orderNo = await placeOrder(token, { pay: true })
      r = await buyerRefund(token, orderNo, 1)
      await staffPost('/admin/refunds/' + r.refund_no + '/audit', { action: 'approve' })
    } else {
      r = (await httpGet(apiBase() + '/refunds/' + env.KEEL_E2E_REFUNDED_REFUND, token)).body
    }
    const orderNo = r.order_no
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
    // 未发货的整单全退，订单按状态机走到 60（20→50→60）；部分退款的话 status 不变。
    const orderStatus = (await httpGet(apiBase() + '/orders/' + orderNo, token)).body.status
    if (orderStatus === 60) await waitFor(od, '.t-display', (t) => t === '已退款')
    const view = await od.data('view')
    expect(view.refundedText).toBe('¥' + (r.amount_cents / 100).toFixed(2))
  })

  withStaff('KEEL_E2E_SHIPPED_ORDER')('已发货的单确认收货后是已完成', async () => {
    let orderNo = env.KEEL_E2E_SHIPPED_ORDER
    if (staffToken()) {
      orderNo = await placeOrder(token, { pay: true })
      await staffPost('/admin/orders/' + orderNo + '/shipments', { carrier_code: 'SF', tracking_no: 'E2E' + Date.now() })
    }
    const page = await program.navigateTo('/pages/order/detail?order_no=' + orderNo)
    await waitFor(page, '.t-display', (t) => t === '已发货')
    // 自动确认收货的时间照服务端的 auto_confirm_at 显示（本地时区），不再按 7 天估。
    const autoAt = (await httpGet(apiBase() + '/orders/' + orderNo, token)).body.auto_confirm_at
    expect(autoAt).toBeTruthy()
    await waitFor(page, '.t-sub', (t) => t.includes(localShort(autoAt) + ' 未确认将自动确认收货'))
    await tapTwice(page, '.confirm-btn')
    await waitFor(page, '.t-display', (t) => t === '已完成')
    expect((await httpGet(apiBase() + '/orders/' + orderNo, token)).body.status).toBe(40)
  })
})
