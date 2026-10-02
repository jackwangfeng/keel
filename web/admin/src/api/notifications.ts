// 顶栏铃铛（后台待办提醒，契约 GET /admin/notifications）的界面规则。
// **全部是纯函数**，由 `make admin-test` 用 `node --test` 直接跑，与 orderRules.ts 同一个约定：
// 只有 `import type`，运行时零依赖。
//
// ## 文案不在这里拼
//
// 标题与正文由服务端渲染好（契约 Notification.title / body 的描述原话：原样展示）。
// 这里只决定两件事：点了跳到哪一页、角标怎么写。按 kind 自己拼文案的话，
// 服务端改一次措辞，界面上就是两种说法。

import type { components } from "@contract/schema.js";

type S = components["schemas"];
export type Notification = S["Notification"];
export type NotificationKind = S["NotificationKind"];

/** 跳转目标：vue-router 的 location（只用 path + query 两样，免得这里依赖 vue-router）。 */
export interface NotificationLocation {
    path: string;
    query: Record<string, string>;
}

/**
 * 点了一条提醒跳到哪：订单 → 订单页并打开那一单（`?order_no=`，订单页已支持）；
 * 售后 → 售后页并打开那一张（`?refund_no=`）；库存 → 那家门店详情的库存页签；
 * 渠道订单 → 渠道页的订单视图、按那家门店筛（`?view=orders&store_id=`，等接单的渠道单还没有 keel 订单号）。
 * 定位字段缺了（不该发生：契约说对应 type 的字段一定非空）时返回 null，界面只标已读不跳。
 */
export function notificationLocation(n: Pick<Notification, "target">): NotificationLocation | null {
    const t = n.target;
    switch (t.type) {
        case "order":
            return t.order_no === null ? null : { path: "/orders", query: { order_no: t.order_no } };
        case "refund":
            return t.refund_no === null ? null : { path: "/refunds", query: { refund_no: t.refund_no } };
        case "inventory":
            return t.store_id === null ? null : { path: `/stores/${t.store_id}`, query: { tab: "inventory" } };
        case "channel_orders":
            return t.store_id === null ? null : { path: "/channels", query: { view: "orders", store_id: String(t.store_id) } };
    }
}

/** 角标文字：0 不显示，超过 99 显示「99+」。 */
export function badgeText(unread: number): string {
    if (unread <= 0) return "";
    return unread > 99 ? "99+" : String(unread);
}

/**
 * 每种提醒的标签色。`Record<NotificationKind, …>` 让契约多一种时这里编译不过 ——
 * 后台只会收到商家那四种，买家那几种也写上，是为了让这张表跟着枚举走而不是挑着写。
 */
export const NOTIFICATION_TAG: Record<NotificationKind, "warning" | "danger" | "primary" | "info" | "success"> = {
    merchant_order_paid: "warning",
    merchant_refund_requested: "danger",
    merchant_return_shipped: "primary",
    merchant_inventory_low: "danger",
    merchant_channel_order_exception: "danger",
    merchant_channel_order_pending: "warning",
    order_paid: "info",
    order_shipped: "info",
    order_auto_confirm_soon: "info",
    order_finished: "success",
    order_timeout_closed: "info",
    refund_approved: "success",
    refund_rejected: "danger",
    refund_succeeded: "success",
    refund_return_expired: "info",
};

/** 轮询未读数的间隔（毫秒）。铃铛是提醒不是 IM，30 秒足够，也不给服务端添负担。 */
export const UNREAD_POLL_MS = 30_000;

/** 下拉列表一次取多少条。 */
export const DROPDOWN_PAGE_SIZE = 20;
