// 商品分类：首页那一排 chip 和服务端的能力要一致。
//
//   · 服务端没有 GET /categories（今天的远端就是 404）→ 分类栏必须不出现，首页照常；
//   · 服务端有 → 分类栏出现，点一个分类，它变成选中、标题跟着变、列表按它重新拉。
//
// 先问服务端再断言，而不是二选一地写死：服务端补上这条路由那天，这条用例不用改，
// 自动从「藏起来了吗」切到「真的能筛吗」。
const { waitFor, httpGet } = require('./helpers')

// 直接从测试进程问服务端，不经过 App：Android 的自动化运行时不支持
// 经 callUniMethod 调 uni.request（"uni.request not exists"，实测；iOS 支持）。
// 问的是打包时注入进 App 的同一个地址 —— 跑用例时 KEEL_API_BASE 要和打测试包时一致。
async function serverCategories() {
  const apiBase = process.env.KEEL_API_BASE
  if (!apiBase) throw new Error('跑这条用例要设 KEEL_API_BASE（和打测试包时同一个值）')
  const res = await httpGet(apiBase + '/categories')
  return res.status === 200 ? res.body : null
}

describe('商品分类', () => {
  it('分类栏与服务端的能力一致；有分类时能切换', async () => {
    const home = await program.reLaunch('/pages/products/list')
    await waitFor(home, '.tile-title', (t) => t.length > 0)
    const tree = await serverCategories()
    const chips = await home.$$('.cat-chip')

    if (tree === null) {
      expect(chips.length).toBe(0)
      return
    }

    // 「全部」+ 每个根分类一个。
    expect(chips.length).toBe(tree.length + 1)
    const target = tree[0]
    await chips[1].tap()
    await waitFor(home, '.t-section', (t) => t === target.name)
    const on = await home.$$('.cat-chip-on')
    expect(on.length).toBe(1)
    expect(await (await on[0].$('.cat-text-on')).text()).toBe(target.name)

    // 回到「全部」。
    await chips[0].tap()
    await waitFor(home, '.t-section', (t) => t === '全部商品')
  })
})
