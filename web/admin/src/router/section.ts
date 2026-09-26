// 左侧菜单与路由表是**同一份声明**，不是两份需要手工对齐的清单。
//
// 这个形状是为「门店」与「大区」准备的：那两块的契约还在另一条分支上
// （6 张表 + 23 条接口），合进来之后要做的事是——
//
//   1. 写 `src/router/modules/stores.ts`（照着现在那个占位模块改）；
//   2. 在 `src/router/modules/index.ts` 的数组里换掉一行。
//
// 没有第三步。菜单项、路由、页面标题都从这一个对象读，所以不存在
// 「路由加了而菜单没加」这种半截状态。

import type { Component } from "vue";
import type { RouteRecordRaw } from "vue-router";

/** 一个菜单分区 = 一组路由 + 它在左侧菜单里的样子。 */
export interface AdminSection {
    /** 菜单项的 key，也是 el-menu 的 index 归属。 */
    key: string;
    /** 菜单上显示的名字。 */
    title: string;
    /** @element-plus/icons-vue 里的组件。 */
    icon: Component;
    /** 菜单排序，小的在上。留出间隔，插新块不必重排。 */
    order: number;
    /**
     * 这个分区的路由。全部挂在主框架（MainLayout）下面，path 写相对形式。
     *
     * 进菜单的那一条在 `meta.menu` 上标 true —— 商品详情这类页面有路由
     * 但不该出现在菜单里。
     */
    routes: RouteRecordRaw[];
    /**
     * 只有平台级操作员（`staff.merchant_id` 为 null）才看得见。
     * 商家级操作员看到一个自己点不动的菜单项没有意义，而服务端那边是 403。
     */
    platformOnly?: boolean;
}

/** 路由 meta 的形状。写成 interface 而不是散装字段，免得拼错了没人发现。 */
declare module "vue-router" {
    interface RouteMeta {
        /** 页面标题（面包屑与浏览器标题）。 */
        title?: string;
        /** 是否作为菜单入口出现。 */
        menu?: boolean;
        /** 不需要登录就能访问（只有登录页）。 */
        anonymous?: boolean;
    }
}
