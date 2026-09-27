// 冒烟：自动化链路通了，首页能从服务端拉到商品。
//
// 等商品出现，而不是 sleep 固定秒数：首页要先定门店再拉列表，定位被拒 / 拿不到时要等
// store.uts 里那 5 秒超时才回落默认店 —— 原来固定等 3 秒，在那条路径上必红。
const { waitFor } = require('./helpers')

describe('首页', () => {
  it('显示服务端的商品', async () => {
    const page = await program.reLaunch('/pages/products/list')
    const first = await waitFor(page, '.pc-title', (t) => t.length > 0)
    expect((await first.text()).length).toBeGreaterThan(0)
  })
})
