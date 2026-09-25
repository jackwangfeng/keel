// 'vue' 的桩。app/typecheck/tsconfig.json 把 vue 这个模块名映到这里。
//
// 客户端只从 vue 里取一个 createSSRApp（main.uts）。把真的 vue 类型拉进来要
// node_modules，而这道闸门的全部价值就在于**零 node_modules**：
// 它跟 make schema-check 一样，只 npx 拉一个钉死版本的 tsc。
// 真的按 vue 的类型编一遍是另一道闸门（scripts/check_app_build.py）。
export declare function createSSRApp(rootComponent: any, rootProps?: any): any
