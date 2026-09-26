// 冒烟：自动化链路通了，首页能从服务端拉到商品。
describe('首页', () => {
  let page
  beforeAll(async () => {
    page = await program.reLaunch('/pages/products/list')
    await page.waitFor(3000)
  })

  it('显示服务端的商品', async () => {
    const titles = await page.$$('.tile-title')
    expect(titles.length).toBeGreaterThan(0)
    const first = await titles[0].text()
    expect(first.length).toBeGreaterThan(0)
  })
})
