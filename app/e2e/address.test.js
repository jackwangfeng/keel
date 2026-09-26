// 收货地址：地址簿列表 → 新建（先交一张错的表单看标红，再交对的）→ 结算页用默认地址。
//
// 演示买家名下种子里有 1 条默认地址（id=1）。用例新建的那条是**非默认**的，跑完从测试进程
// 删掉，不动种子那条 —— 删了默认地址服务端不会自动补回，后面的结算用例就没有地址了。
const { waitFor, waitData, waitEl, pickSku, httpGet, httpRequest, apiBase, serverToken, loginInApp } = require('./helpers')

const NAME = 'e2e 收件人'

async function serverAddresses(token) {
  const res = await httpGet(apiBase() + '/addresses', token)
  if (res.status !== 200) throw new Error('GET /addresses 失败：' + res.status)
  return res.body
}

describe('收货地址', () => {
  let token = ''

  beforeAll(async () => {
    await loginInApp()
    token = await serverToken()
    // 上一次跑挂在半路留下的 e2e 地址先清掉，免得列表里越积越多。
    for (const a of await serverAddresses(token)) {
      if (a.receiver_name === NAME) await httpRequest('DELETE', apiBase() + '/addresses/' + a.id, null, token)
    }
  })

  afterAll(async () => {
    for (const a of await serverAddresses(token)) {
      if (a.receiver_name === NAME) await httpRequest('DELETE', apiBase() + '/addresses/' + a.id, null, token)
    }
  })

  it('地址簿列出服务端的地址，默认地址排第一', async () => {
    const server = await serverAddresses(token)
    const page = await program.navigateTo('/pages/address/list')
    const rows = await waitData(page, 'rows', (r) => r.length === server.length && r.length > 0)
    expect(rows[0].id).toBe(server[0].id)
    expect(rows[0].isDefault).toBe(server[0].is_default)
  })

  it('新建：字段不合法时按 errors[].field 标红，改对后保存成功', async () => {
    const page = await program.navigateTo('/pages/address/edit')
    await waitEl(page, '.save-btn')
    // 往输入框里打字，而不是 page.setData({ form: {...} })：form 在 Android 上是一个具名类
    // （AddressForm），拿普通对象整个替换它，页面当场渲染不出来（实测，之后 $ 全是 null）。
    const fill = async (values) => {
      for (const [cls, v] of Object.entries(values)) await (await page.$(cls)).input(v)
    }
    await fill({ '.f-name': '', '.f-phone': 'abc', '.f-province': '上海市', '.f-city': '上海市', '.f-district': '徐汇区', '.f-detail': '漕溪北路 1 号' })
    await (await page.$('.save-btn')).tap()
    // 服务端对这两项回 422 invalid-request，errors 里 field 是 receiver_name / phone。
    const errs = await waitData(page, 'errors', (e) => e.length > 0)
    const fields = errs.map((e) => e.field).sort()
    expect(fields).toEqual(['phone', 'receiver_name'])
    const bad = await page.$$('.field-bad')
    expect(bad.length).toBe(2)

    await fill({ '.f-name': NAME, '.f-phone': '13900000000' })
    await (await page.$('.save-btn')).tap()
    // 保存成功会 navigateBack；以服务端为准核对。
    const deadline = Date.now() + 15000
    let created = null
    while (!created) {
      created = (await serverAddresses(token)).find((a) => a.receiver_name === NAME)
      if (created) break
      if (Date.now() > deadline) throw new Error('新建的地址没出现在 GET /addresses 里')
      await page.waitFor(300)
    }
    expect(created.is_default).toBe(false)
    expect(created.phone).toBe('13900000000')
  })

  it('结算页默认用默认地址', async () => {
    const server = await serverAddresses(token)
    const def = server.find((a) => a.is_default)
    const sku = await pickSku(1)
    const page = await program.navigateTo('/pages/order/create?sku_id=' + sku.skuId + '&product_id=' + sku.productId)
    if (!def) {
      // 没有默认地址（有人把它删了）：不替用户挑，页面要他选。
      await waitData(page, 'addressLoaded', (v) => v === true)
      expect(await page.data('addressId')).toBe(0)
      return
    }
    const addr = await waitData(page, 'address', (a) => a != null)
    expect(addr.id).toBe(def.id)
    await waitFor(page, '.t-price-l', (t) => t.startsWith('¥'))
  })
})
