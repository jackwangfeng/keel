// Node 全局的**最小**声明。
//
// 为什么不用 @types/node：Makefile 里那条决定（「不凭空引入一棵没人维护的 npm
// 依赖树」）对类型包同样成立 —— @types/node 会把整个 Node 标准库的类型拉进来，
// 而这里只用到 process 的三个成员。手写这十行的代价是：Node 改了签名这里不会知道；
// 收益是 `make schema-check` 到今天为止仍然零 node_modules。
//
// 只声明真的用到的东西。多声明一个不用的成员，就是多一处没人验证过的断言。
//
// 注意这是个**全局**声明文件：它不能出现任何 import / export，
// 否则整个文件会变成一个模块，declare 出来的东西就不再是全局的了。

declare const process: {
    /** 环境变量。值可能不存在 —— 这里如实写成可选，不写成 string。 */
    readonly env: Readonly<Record<string, string | undefined>>;
    /** 进程退出码。脚本用它表达成败，而不是直接 exit —— 直接 exit 会截断还没冲出去的 stdout。 */
    exitCode: number | undefined;
};
