// 真机上接口有网络延迟：按条件轮询，而不是 sleep 一个拍脑袋的秒数。
async function waitFor(page, selector, predicate = () => true, timeout = 15000) {
  const start = Date.now()
  let last = null
  while (Date.now() - start < timeout) {
    const el = await page.$(selector)
    if (el) {
      last = await el.text()
      if (predicate(last)) return el
    }
    await page.waitFor(300)
  }
  throw new Error(`等 ${selector} 超时（${timeout}ms），最后看到的文字：${JSON.stringify(last)}`)
}

module.exports = { waitFor }
