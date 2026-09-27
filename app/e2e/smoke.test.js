// 冒烟：自动化链路通了，首页能从服务端拉到商品。
//
// 等商品出现，而不是 sleep 固定秒数：首页要先定门店再拉列表，定位被拒 / 拿不到时要等
// store.uts 里那 5 秒超时才回落默认店 —— 原来固定等 3 秒，在那条路径上必红。
const { waitFor, waitData, httpGet, apiBase } = require('./helpers')

describe('首页', () => {
  it('显示服务端的商品', async () => {
    const page = await program.reLaunch('/pages/products/list')
    const first = await waitFor(page, '.pc-title', (t) => t.length > 0)
    expect((await first.text()).length).toBeGreaterThan(0)
  })

  it('服务端给了图的商品，卡片显示成图而不是单字占位', async () => {
    const withImg = (await httpGet(apiBase() + '/products?page_size=50')).body.items.filter((p) => p.image_url)
    if (withImg.length === 0) return   // 服务端没配图（老版本 / 没跑图片种子）：不适用
    const page = await program.currentPage()
    const rows = await waitData(page, 'rows', (r) => r.length > 0)
    const row = rows.find((r) => r.id === withImg[0].id) || rows.find((r) => withImg.some((p) => p.id === r.id))
    expect(row.cover.imageUrl).not.toBe('')
    expect(row.cover.imageUrl.endsWith(withImg.find((p) => p.id === row.id).image_url)).toBe(true)
    expect((await page.$$('.cover-img')).length).toBeGreaterThan(0)
  })
})
