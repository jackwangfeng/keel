import { createApp } from "vue";
import ElementPlus from "element-plus";
import zhCn from "element-plus/es/locale/lang/zh-cn";
import "element-plus/dist/index.css";
import "./styles.css";

import App from "./App.vue";
import { router } from "./router/index.ts";
import { setUnauthorizedHandler } from "./api/client.ts";

// 401 的统一去向：清掉会话、回登录页、带上来处。
// 放在这里而不是 client.ts 里，是因为「去哪」是路由的事，
// 而 client.ts 不该知道有路由这回事。
setUnauthorizedHandler(() => {
    const current = router.currentRoute.value;
    if (current.name === "login") return;
    void router.push({ name: "login", query: { redirect: current.fullPath } });
});

createApp(App)
    // 全量引入 Element Plus，不做按需。理由：后台一共十来个页面，
    // 产物差几百 KB，而按需引入要多一个 unplugin 与一份自动生成的
    // components.d.ts —— 那是又一个会和代码分叉的生成物。
    .use(ElementPlus, { locale: zhCn })
    .use(router)
    .mount("#app");
