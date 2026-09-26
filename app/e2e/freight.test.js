// 运费：结算页显示服务端算的运费与说明（满 99 包邮 / 偏远地区首件续件），应付 = 商品 + 运费 − 优惠；
// 送不到（港澳台）逐行标出原因；地址归不到省时引导补全地址；购物车显示按默认地址的预估运费。
//
// 演示店的模板：全国首件 8 元、续件 2 元、满 99 包邮；新疆等首件 15 元、续件 5 元、不包邮；港澳台不配送。
// 用例从测试进程建三条临时地址（新疆 / 香港 / 省份写错），跑完删掉，不动默认地址。
const { randomUUID } = require('crypto')
const { waitFor, waitData, httpGet, httpRequest, apiBase, serverToken, loginInApp } = require('./helpers')

const NAME = 'e2e 运费'
const cents = (t) => Math.round(parseFloat(t.replace('¥', '').replace('-', '')) * 100) * (t.startsWith('-') ? -1 : 1)

// 挑一便宜（< 99 元）一贵（≥ 99 元）两个有货的规格，都是商品详情默认选中的那个。
async function pickByPrice() {
  const list = (await httpGet(apiBase() + '/products?page_size=50')).body
  let cheap = null
  let dear = null
  for (const p of list.items) {
    if (p.status !== 1) continue
    const s = (await httpGet(apiBase() + '/products/' + p.id)).body.skus.find((x) => x.available_qty > 5)
    if (!s) continue
    if (!cheap && s.price_cents < 9900) cheap = { productId: p.id, skuId: s.id }
    if (!dear && s.price_cents >= 9900) dear = { productId: p.id, skuId: s.id }
  }
  if (!cheap || !dear) throw new Error('演示库里凑不齐 99 元上下各一个有货的规格')
  return { cheap, dear }
}

async function cleanup(token) {
  for (const a of (await httpGet(apiBase() + '/addresses', token)).body) {
    if (a.receiver_name === NAME) await httpRequest('DELETE', apiBase() + '/addresses/' + a.id, null, token)
  }
}

async function makeAddress(token, fields) {
  const r = await httpRequest('POST', apiBase() + '/addresses',
    { receiver_name: NAME, phone: '13900000000', detail: '1 号', ...fields }, token, { 'Idempotency-Key': randomUUID() })
  if (r.status !== 201) throw new Error('建地址失败：' + r.status + ' ' + JSON.stringify(r.body))
  return r.body.id
}

// 用某个地址打开结算页：和地址簿「选择模式」回传的是同一个存储键，结算页 onShow 时取走。
async function checkoutWith(sku, addressId) {
  if (addressId) await program.callUniMethod('setStorageSync', 'keel.pickedAddress', String(addressId))
  return program.navigateTo('/pages/order/create?sku_id=' + sku.skuId + '&product_id=' + sku.productId)
}

describe('运费', () => {
  let token = ''
  let skus = null

  beforeAll(async () => {
    await loginInApp()
    token = await serverToken()
    await cleanup(token)
    skus = await pickByPrice()
  })

  afterAll(async () => {
    await cleanup(token)
    await httpRequest('DELETE', apiBase() + '/cart', null, token)
  })

  it('默认地址（杭州）：99 元以下运费 8 元并提示满 99 包邮，应付 = 商品 + 运费 − 优惠', async () => {
    const page = await checkoutWith(skus.cheap, null)
    const pv = await waitData(page, 'pv', (p) => p !== null)
    expect(pv.freightText).toBe('¥8.00')
    expect(pv.freightNote).toBe('满¥99 包邮')
    // 演示买家没有包邮券，运费抵扣为 0：应付 = 商品 + 运费 − 优惠（这些数都是服务端给的，这里只核对它们自洽）。
    expect(pv.freightDiscountText).toBe('')
    expect(pv.payableCents).toBe(cents(pv.goodsAmountText) + cents(pv.freightText) - cents(pv.discountText))
  })

  it('默认地址：99 元以上包邮，说明写「已满¥99 包邮」', async () => {
    const page = await checkoutWith(skus.dear, null)
    const pv = await waitData(page, 'pv', (p) => p !== null)
    expect(pv.freightText).toBe('¥0.00')
    expect(pv.freightNote).toBe('已满¥99 包邮')
  })

  it('新疆地址：首件 15 元、不包邮', async () => {
    const id = await makeAddress(token, { province: '新疆维吾尔自治区', city: '乌鲁木齐市', district: '天山区', region_code: '650102' })
    const page = await checkoutWith(skus.dear, id)
    await waitData(page, 'addressId', (v) => v === id)
    const pv = await waitData(page, 'pv', (p) => p !== null)
    expect(pv.freightText).toBe('¥15.00')
    expect(pv.freightNote).toBe('首件¥15，续件¥5')
  })

  it('香港地址：送不到，逐行标出原因、提交按钮灰掉', async () => {
    const id = await makeAddress(token, { province: '香港特别行政区', city: '香港', district: '中西区', region_code: '810000' })
    const page = await checkoutWith(skus.cheap, id)
    await waitData(page, 'addressId', (v) => v === id)
    await waitFor(page, '.result-err', (t) => t.includes('送不到这个收货地址'))
    await waitFor(page, '.undeliverable', (t) => t.includes('香港') && t.includes('配送范围'))
    // 试算失败时 pv 是 null，提交按钮因此是灰的。iOS 的自动化读 data 时把 null 读成 undefined（实测），两种都算空。
    expect((await page.data('pv')) == null).toBe(true)
    expect(await page.data('provinceUnknown')).toBe(false)
  })

  it('省份写错的地址：引导补全地址', async () => {
    const id = await makeAddress(token, { province: '火星', city: 'x', district: 'y' })
    const page = await checkoutWith(skus.cheap, id)
    await waitData(page, 'addressId', (v) => v === id)
    await waitFor(page, '.result-err', (t) => t.includes('补全地址'))
    expect(await page.data('provinceUnknown')).toBe(true)
    expect(await page.$('.fix-addr')).not.toBeNull()
  })

  it('购物车按默认地址显示预估运费', async () => {
    await httpRequest('DELETE', apiBase() + '/cart', null, token)
    const store = (await httpGet(apiBase() + '/products?page_size=1')).body.store.store_id
    const add = await httpRequest('POST', apiBase() + '/cart/items?store_id=' + store, { sku_id: skus.cheap.skuId, quantity: 1 }, token,
      { 'Idempotency-Key': randomUUID() })
    expect(add.status).toBe(200)
    const cart = await program.switchTab('/pages/cart/index')
    await waitData(cart, 'rows', (r) => r.length === 1)
    await waitData(cart, 'freightText', (t) => t === '¥8.00')
    await waitFor(cart, '.cart-freight', (t) => t.includes('¥8.00') && t.includes('满¥99 包邮'))
  })
})
