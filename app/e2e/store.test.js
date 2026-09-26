// 当前门店：首页说清楚由哪家店配送，并且下单用的就是这家店（store.uts 解析一次、四处共用）。
//
// 不写死门店名：远端演示环境现在只有「默认门店」一家、没有围栏（fallback_default），
// 换一家商家、配了围栏之后名字会变。断言的是「有一家店在服务你」这件事本身。
const { waitFor } = require('./helpers')

describe('当前门店', () => {
  it('首页显示「由 xx 为你配送」', async () => {
    const home = await program.reLaunch('/pages/products/list')
    const line = await waitFor(home, '.store-line', (t) => /^由「.+」为你配送$/.test(t))
    expect(await line.text()).toMatch(/为你配送/)
  })
})
