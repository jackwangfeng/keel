import { createRouter, createWebHistory, type RouteRecordRaw } from "vue-router";
import { currentSession } from "../api/client.ts";
import { sections } from "./modules/index.ts";
import { refreshIdentity } from "../auth/permissions.ts";

const routes: RouteRecordRaw[] = [
    {
        path: "/login",
        name: "login",
        component: () => import("../views/LoginView.vue"),
        meta: { title: "登录", anonymous: true },
    },
    {
        path: "/",
        component: () => import("../layouts/MainLayout.vue"),
        children: [
            // 首页是经营概览（src/router/modules/overview.ts）。
            { path: "", redirect: { name: "overview" } },
            // 各分区的路由在这里汇总。加一块不需要动这个文件。
            ...sections.flatMap((s) => s.routes),
        ],
    },
    {
        path: "/:pathMatch(.*)*",
        name: "not-found",
        component: () => import("../views/NotFoundView.vue"),
        meta: { title: "页面不存在" },
    },
];

export const router = createRouter({
    // createWebHistory 而不是 hash：nginx 那边已经配了 SPA 回落
    // （docker/admin-nginx.conf 的 try_files）。hash 路由不需要那一行，
    // 代价是每个 URL 里多一个 # —— 而这个后台是要发给人看的。
    history: createWebHistory(import.meta.env.BASE_URL),
    routes,
});

router.beforeEach(async (to) => {
    if (to.meta.anonymous === true) return true;
    if (currentSession() !== null) {
        // 角色与管辖范围可能在会话期间被改过：整页加载后的第一次导航刷新一次
        // （src/auth/permissions.ts）。之后的导航不再请求。
        await refreshIdentity();
        return true;
    }
    // 带上来处：登录之后回到他本来要去的地方，而不是一律甩回首页。
    return { name: "login", query: { redirect: to.fullPath } };
});

router.afterEach((to) => {
    const title = to.meta.title;
    document.title = title === undefined ? "Keel 商家后台" : `${title} · Keel 商家后台`;
});
