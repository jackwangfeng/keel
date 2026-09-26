// 搜索：拿首页上一件真实商品的标题去搜，它应该排第一；点进去是它的详情。
//
// 不写死关键词：服务端的商品数据会变（种子是咖啡杯，远端测试机上现在是服装），
// 写死「连衣裙」的话换一家店这条就红得莫名其妙。
const { waitFor } = require('./helpers')

describe('搜索', () => {
  let title = ''

  beforeAll(async () => {
    const home = await program.reLaunch('/pages/products/list')
    const first = await waitFor(home, '.tile-title', (t) => t.length > 0)
    title = await first.text()
  })

  it('首页的搜索框进入搜索页', async () => {
    const home = await program.currentPage()
    await (await home.$('.search')).tap()
    await home.waitFor(1000)
    const page = await program.currentPage()
    expect(page.path).toBe('pages/search/index')
  })

  it('按商品标题搜，这件商品排第一，点进去是它的详情', async () => {
    const page = await program.currentPage()
    await (await page.$('.box-input')).input(title)
    await (await page.$('.go')).tap()
    await waitFor(page, '.count', (t) => t.includes('共'))
    const hits = await page.$$('.hit-title')
    expect(hits.length).toBeGreaterThan(0)
    expect(await hits[0].text()).toBe(title)

    await (await page.$('.hit')).tap()
    await page.waitFor(1000)
    const detail = await program.currentPage()
    expect(detail.path).toBe('pages/products/detail')
    await waitFor(detail, '.name', (t) => t === title)
  })
})
