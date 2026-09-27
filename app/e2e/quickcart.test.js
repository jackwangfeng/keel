// 首页卡片「＋」原地加购：单规格直接加；多规格弹规格浮层（.sheet / .chip / .confirm）选完再加。
// 全程不离开首页。结果以服务端的购物车为准。
const { waitData, waitEl, httpGet, httpRequest, apiBase, serverToken, loginInApp } = require('./helpers')

const SINGLE = { productId: 23, skuId: 26 }   // 冷萃咖啡液：一个规格
const MULTI = { productId: 19, skuIds: [21, 22] } // 挂耳咖啡：两个规格

async function serverCart(token) {
  const store = (await httpGet(apiBase() + '/products?page_size=1')).body.store.store_id
  return (await httpGet(apiBase() + '/cart?store_id=' + store, token)).body
}

async function waitCartHas(token, skuId, page) {
  for (let i = 0; i < 40; i++) {
    const c = await serverCart(token)
    if (c.items.some((x) => x.sku_id === skuId)) return c
    await page.waitFor(300)
  }
  throw new Error('购物车里没等到 sku ' + skuId)
}

// 「＋」只画在能买的卡片上，所以按「能买的卡片」数第几个，而不是 rows 下标。
async function addBtnOf(home, productId) {
  const rows = await waitData(home, 'rows', (r) => r.some((x) => x.id === productId))
  const buyable = rows.filter((x) => !x.offShelf && !x.soldOut)
  const idx = buyable.findIndex((x) => x.id === productId)
  if (idx < 0) throw new Error('商品 ' + productId + ' 在首页上不能买')
  const btns = await home.$$('.add-btn')
  return btns[idx]
}

describe('首页原地加购', () => {
  let token = ''

  beforeAll(async () => {
    await loginInApp()
    token = await serverToken()
    await httpRequest('DELETE', apiBase() + '/cart', null, token)
  })

  afterAll(async () => {
    await httpRequest('DELETE', apiBase() + '/cart', null, token)
  })

  it('单规格：点「＋」直接加进购物车，不离开首页', async () => {
    const home = await program.reLaunch('/pages/products/list')
    await (await addBtnOf(home, SINGLE.productId)).tap()
    await waitCartHas(token, SINGLE.skuId, home)
    expect((await program.currentPage()).path).toBe('pages/products/list')
  })

  it('多规格：点「＋」弹规格浮层，选第二个规格加入', async () => {
    const home = await program.currentPage()
    await (await addBtnOf(home, MULTI.productId)).tap()
    await waitEl(home, '.sheet')
    const chips = await home.$$('.chip')
    expect(chips.length).toBe(MULTI.skuIds.length)
    await chips[1].tap()
    await (await home.$('.confirm')).tap()
    await waitCartHas(token, MULTI.skuIds[1], home)
    expect((await program.currentPage()).path).toBe('pages/products/list')
  })
})
