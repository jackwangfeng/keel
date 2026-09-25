// 给 vue 的类型补一个 `createVueApp`。
//
// @dcloudio/uni-h5-vite 的 mainJs 插件在 H5 构建时会把 main 里的 `createSSRApp`
// 改写成 `createVueApp as createSSRApp`。`createVueApp` 只存在于运行时的
// @dcloudio/uni-app-vue 里（vite 把 'vue' 别名到那儿），vue 自己的 .d.ts 里没有。
// 不补这一条，每次 H5 构建都会有一句
// "Module 'vue' has no exported member 'createVueApp'" —— 那是 DCloud 注入的代码
// 报的，不是我们写的代码报的，而闸门分不出来。
//
// **这里必须是模块增强（文件里有 import/export），不能写成 src/uts-builtin.d.ts
// 里那种全局声明。** 在一个没有 import/export 的 .d.ts 里写 `declare module 'vue'`
// 是「声明一个叫 vue 的环境模块」，它会**整个顶掉** vue 真正的类型声明 ——
// 实测代价是 104 条诊断：defineComponent 找不到了，于是每个页面的 `this.xxx`
// 全部报「属性不存在」。
import 'vue'

declare module 'vue' {
  export function createVueApp(rootComponent: any, rootProps?: any): any
}
