// 个人资料：GET /me 回显，改昵称 → PATCH /me → 服务端核对 → 改回去。
const { waitFor, waitData, httpGet, httpRequest, apiBase, serverToken, loginInApp } = require('./helpers')

describe('个人资料', () => {
  let token = ''
  let original = ''

  beforeAll(async () => {
    await loginInApp()
    token = await serverToken()
    original = (await httpGet(apiBase() + '/me', token)).body.nickname
  })

  afterAll(async () => {
    // 不管用例过没过，把昵称改回去：别的用例（和人）看到的是「示例买家」。
    await httpRequest('PATCH', apiBase() + '/me', { nickname: original }, token)
  })

  it('显示服务端的昵称与脱敏手机号；改昵称后服务端是新值', async () => {
    const me = (await httpGet(apiBase() + '/me', token)).body
    const page = await program.navigateTo('/pages/me/profile')
    const view = await waitData(page, 'me', (m) => m !== null)
    expect(view.nickname).toBe(me.nickname)
    expect(view.phone).toBe(me.phone || '')

    const next = 'e2e-' + Date.now().toString().slice(-6)
    await page.setData({ nickname: next })
    await (await page.$('.save-btn')).tap()
    await waitFor(page, '.t-ok', (t) => t.includes('已保存'))
    expect((await httpGet(apiBase() + '/me', token)).body.nickname).toBe(next)
  })
})
