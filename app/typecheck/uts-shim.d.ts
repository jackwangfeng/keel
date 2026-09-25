// UTS 运行时里、但 TypeScript 里没有的那几样东西。
//
// ## 这个文件的边界，以及为什么它不是「第二份真相源」
//
// 它声明的全部是 **DCloud 的 API 形状**（uni.request、UTSJSONObject、
// `JSON.parse<T>`），一个 Keel 契约字段都没有。契约字段只有一个来源：
// app/src/api/schema.uts，而那份是从 docs/电商系统-OpenAPI.yaml 生成的。
// 所以这里写错了，最坏是闸门对 uni API 的用法判断不准；它**不可能**让
// 「契约改了而客户端没跟上」蒙混过关。
//
// 为什么不直接拿 @dcloudio/uni-app-x 那份类型：那份要 500 多个 npm 包才装得起来，
// 而 scripts/check-all.sh 的纪律是全程零 node_modules（只用 npx 拉一个钉死版本的
// tsc）。真的用 DCloud 自己的编译器去编，是另一道闸门（scripts/check_app_build.py），
// 它跑在 CI 的独立 job 里。两道各管一段，见 app/README.md。

// .uvue 单文件组件。tsc 不解析 SFC，这里只是让 import 不报「找不到模块」——
// 模板里的类型由 DCloud 自己的编译器管（scripts/check_app_build.py）。
declare module '*.uvue' {
  const component: any
  export default component
}

/** uni-app x 的全局：创建应用实例。 */
declare function createSSRApp(root: any): any

type UniRequestSuccess<T> = {
  data: T | null
  statusCode: number
  header: any
  cookies: string[]
}

type UniRequestFail = {
  errMsg: string
  errCode: number
}

type UniRequestOptions<T> = {
  url: string
  method?: string | null
  data?: any | null
  header?: UTSJSONObject | null
  timeout?: number | null
  dataType?: string | null
  responseType?: string | null
  success?: ((res: UniRequestSuccess<T>) => void) | null
  fail?: ((err: UniRequestFail) => void) | null
  complete?: (() => void) | null
}

declare const uni: {
  request<T>(options: UniRequestOptions<T>): void
  getStorageSync(key: string): any
  setStorageSync(key: string, value: any): void
  removeStorageSync(key: string): void
}
