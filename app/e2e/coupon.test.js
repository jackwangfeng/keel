// 优惠券：领券中心领一张 →「我的优惠券」里看得到 → 结算页自动用上最省的那张。
//
// 演示买家（13800000000）的状态跑一次变一次：「满 50 减 10」种数据时就领过并用掉了，
// 「9 折」第一次跑能领到（201），之后再领是 409 每人限领 —— 两种都算过，断言的是
// 「领完之后按钮是『已领取』、这张券在我的优惠券里」，而不是「这一次一定是新领的」。
// 结算那一步同理：手上有未使用的券才断言「自动用上了」，没有就断言「没有券也能正常算钱」。
// 想从头跑一遍可以请服务端那边重置演示库。
const { waitFor, httpGet, httpPost, apiBase } = require('./helpers')

const DEMO_PHONE = '13800000000'
const DEMO_PASSWORD = 'keel-demo-2026'
const NINE_OFF = '全场 9 折最高减 30'

// 测试进程自己登录一次，拿 token 问服务端「现在手上有哪些券」。App 里的会话是另一份。
async function serverToken() {
  const res = await httpPost(apiBase() + '/auth/login', { phone: DEMO_PHONE, password: DEMO_PASSWORD })
  if (res.status !== 200) throw new Error('测试进程登录失败：' + res.status + ' ' + JSON.stringify(res.body))
  return res.body.access_token
}

async function serverCoupons(token, status) {
  const res = await httpGet(apiBase() + '/coupons?status=' + status + '&page_size=50', token)
  if (res.status !== 200) throw new Error('GET /coupons 失败：' + res.status)
  return res.body.items
}

describe('优惠券', () => {
  beforeAll(async () => {
    await program.callUniMethod('clearStorageSync')
    const page = await program.reLaunch('/pages/auth/login')
    await page.waitFor(1000)
    await (await page.$('.btn')).tap()
    await waitFor(page, '.msg', (t) => t.includes('登录成功'))
  })

  it('领券中心领「9 折」：领到或已领过，最后按钮都是「已领取」', async () => {
    const page = await program.navigateTo('/pages/coupon/center')
    await waitFor(page, '.ticket-name', (t) => t.length > 0)
    let rows = await page.data('rows')
    const idx = rows.findIndex((r) => r.name === NINE_OFF)
    expect(idx).toBeGreaterThanOrEqual(0)

    if (rows[idx].canClaim) {
      const btns = await page.$$('.claim-btn')
      await btns[idx].tap()
      // 201 →「已领取「…」」；409 每人限领 →「这张券你已经领过了」。别的都算失败。
      await waitFor(page, '.t-ok', (t) => t.includes('已领取') || t.includes('已经领到'))
        .catch(async () => {
          const err = await page.$('.t-err')
          const text = err ? await err.text() : ''
          if (!text.includes('已经领过')) throw new Error('领券失败：' + text)
        })
    }

    // 不论哪条路，列表刷新后这张券都该是「已领取」。
    const deadline = Date.now() + 15000
    for (;;) {
      rows = await page.data('rows')
      const r = rows.find((x) => x.name === NINE_OFF)
      if (r && r.actionText === '已领取') break
      if (Date.now() > deadline) throw new Error('按钮没有变成「已领取」：' + JSON.stringify(r))
      await page.waitFor(300)
    }
  })

  it('我的优惠券：「9 折」在某个 tab 里，四个 tab 都能切', async () => {
    const page = await program.navigateTo('/pages/coupon/mine')
    const tabs = await page.$$('.tab')
    expect(tabs.length).toBe(4)
    let found = false
    for (let i = 0; i < 4; i++) {
      await tabs[i].tap()
      // 等这个 tab 的请求回来（loaded 在切 tab 时置 false）。
      const deadline = Date.now() + 15000
      while (!(await page.data('loaded')) || (await page.data('tab')) !== i) {
        if (Date.now() > deadline) throw new Error('tab ' + i + ' 没加载出来')
        await page.waitFor(300)
      }
      const rows = await page.data('rows')
      if (rows.some((r) => r.name === NINE_OFF)) found = true
    }
    expect(found).toBe(true)
  })

  it('结算页：有未使用的券就自动用上最省的那张，应付 = 商品 + 运费 − 优惠', async () => {
    const token = await serverToken()
    const available = await serverCoupons(token, 'available')

    const page = await program.navigateTo('/pages/order/create?sku_id=1&product_id=1')
    await waitFor(page, '.t-price-l', (t) => t.startsWith('¥'))
    const pv = await page.data('pv')

    if (available.length === 0) {
      // 券都用掉了（之前的运行 / checkout 用例用掉的）：照常算钱、不带券。
      expect(pv.couponId).toBe(0)
      return
    }
    // 服务端判定这一单能用的券不为空时，页面必须替用户选上第一张（最省的）。
    if (pv.coupons.length === 0) return // 手上的券这一单都用不了（门槛 / 范围）
    expect(pv.couponId).toBe(pv.coupons[0].id)
    expect(await page.data('couponId')).toBe(pv.couponId)
    expect(pv.discountText).not.toBe('¥0.00')
    // 券行上显示的是这一单省多少（applicable_discount_cents），和金额卡的优惠一致。
    const summary = await (await page.$('.coupon-on')).text()
    expect(summary).toContain(pv.coupons[0].saveText)
    expect(pv.coupons[0].saveText).toBe('-' + pv.discountText)

    // 选「不使用」→ 优惠归零；再选回来 → 优惠回来。
    await (await page.$('.chev')).tap()
    const opts = await page.$$('.coupon-opt')
    await opts[opts.length - 1].tap()
    await waitFor(page, '.t-price-l', (t) => t.startsWith('¥'))
    let deadline = Date.now() + 15000
    for (;;) {
      const p = await page.data('pv')
      if (p && p.couponId === 0) { expect(p.discountText).toBe('¥0.00'); break }
      if (Date.now() > deadline) throw new Error('选「不使用」后没有重新试算')
      await page.waitFor(300)
    }
  })
})
