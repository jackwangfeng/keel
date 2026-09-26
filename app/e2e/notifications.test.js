// 消息中心：付款后出现「支付成功」通知 → 消息页看到它（标题、正文原样）→ 点进去是那笔订单、
// 服务端标成已读 → 再来一条，「全部标为已读」后未读是 0。
//
// 通知由服务端在状态变化时产生；付款由测试进程造（helpers.placeOrder 投递沙箱回调）。
// 每跑一次下两笔已付款的单。
const { randomUUID } = require('crypto')
const { waitFor, waitData, waitEl, httpGet, httpRequest, apiBase, serverToken, loginInApp, placeOrder } = require('./helpers')

async function notifications(token) {
  const r = await httpGet(apiBase() + '/me/notifications?page_size=50', token)
  if (r.status !== 200) throw new Error('GET /me/notifications 失败：' + r.status)
  return r.body
}

// 等某笔订单的「支付成功」通知出现（服务端写通知在支付回调里，稍有延迟）。
async function waitPaidNote(token, orderNo) {
  for (let i = 0; i < 40; i++) {
    const n = (await notifications(token)).items.find((x) => x.kind === 'order_paid' && x.target.order_no === orderNo)
    if (n) return n
    await new Promise((r) => setTimeout(r, 300))
  }
  throw new Error('付款后没等到「支付成功」通知：' + orderNo)
}

describe('消息中心', () => {
  let token = ''

  beforeAll(async () => {
    await loginInApp()
    token = await serverToken()
    await httpRequest('POST', apiBase() + '/me/notifications/read-all', null, token)
  })

  it('付款后消息页出现「支付成功」，点进去是那笔订单，并标成已读', async () => {
    const orderNo = await placeOrder(token, { pay: true })
    const note = await waitPaidNote(token, orderNo)
    expect(note.read_at).toBeNull()

    const page = await program.navigateTo('/pages/me/notifications')
    const rows = await waitData(page, 'rows', (r) => r.some((x) => x.id === note.id))
    const idx = rows.findIndex((x) => x.id === note.id)
    // 标题、正文原样显示（服务端渲染好的，不按 kind 拼）。
    expect(rows[idx].title).toBe(note.title)
    expect(rows[idx].body).toBe(note.body)
    expect(rows[idx].unread).toBe(true)
    expect(await page.data('unread')).toBeGreaterThanOrEqual(1)

    await (await page.$$('.note'))[idx].tap()
    let cur = await program.currentPage()
    for (let i = 0; cur.path !== 'pages/order/detail' && i < 30; i++) { await page.waitFor(300); cur = await program.currentPage() }
    expect(cur.path).toBe('pages/order/detail')
    expect(await cur.data('orderNo')).toBe(orderNo)

    let readAt = null
    for (let i = 0; i < 30 && !readAt; i++) {
      readAt = (await notifications(token)).items.find((x) => x.id === note.id).read_at
      if (!readAt) await cur.waitFor(300)
    }
    expect(readAt).not.toBeNull()
  })

  it('「全部标为已读」后未读是 0', async () => {
    const orderNo = await placeOrder(token, { pay: true })
    await waitPaidNote(token, orderNo)
    const page = await program.navigateTo('/pages/me/notifications')
    await waitData(page, 'unread', (n) => n >= 1)
    await (await waitEl(page, '.read-all')).tap()
    await waitData(page, 'unread', (n) => n === 0)
    const rows = await page.data('rows')
    expect(rows.every((x) => !x.unread)).toBe(true)
    expect((await httpGet(apiBase() + '/me/notifications/unread-count', token)).body.unread_count).toBe(0)
  })
})
