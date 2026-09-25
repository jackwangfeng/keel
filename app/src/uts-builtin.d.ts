// UTS 的语言内建类型（UTSJSONObject、带类型参数的 JSON.parse）。
//
// ## 为什么项目里要自己声明这两样
//
// 它们不在任何 npm 包里。HBuilderX 通过一个只有 IDE 里才存在的虚拟模块
// （`hbuilderx-language-services/builtin-dts/uts-types/common/index.d.ts`）把它们
// 喂给 tsc；命令行模式下那个路径解析不到，于是 `uni build` 会刷出一片
// "Cannot find name 'UTSJSONObject'" 与 "Expected 0 type arguments, but got 1"。
//
// **这不影响编译产物**：UTS 编译器自己认识这两样，Kotlin/JS 都照常生成 ——
// 受影响的只有类型检查，而类型检查恰恰是我们要拿来当闸门的东西。
// 所以这里补上声明，让 CLI 下的诊断回到「只有我们自己写错了才会报」。
//
// 这个文件里**没有一个 Keel 契约字段**。契约字段只有一个来源：
// src/api/schema.uts（从 docs/电商系统-OpenAPI.yaml 生成）。

/** UTS 的动态 JSON 对象。契约里 additionalProperties 的那些字段是它。 */
declare interface UTSJSONObject {
  [key: string]: any
}

/**
 * UTS 给 JSON.parse 加的带类型参数重载：解析失败返回 null，不抛异常。
 * 用接口合并往标准库的 JSON 上补，而不是重新 declare 一个 —— 后者会和
 * lib.es5.d.ts 撞成 duplicate identifier。
 */
interface JSON {
  parse<T>(text: string): T | null
}
