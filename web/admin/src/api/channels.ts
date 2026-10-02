// 渠道管理页的取数。对 client.ts 的薄封装：类型全从契约来（client.ts 的别名），请求都走 `keel`。

import {
    ProblemError,
    UnexpectedResponseError,
    keel,
    onSessionChange,
    type ChannelListing,
    type ChannelOrderDetail,
    type ChannelOrderPage,
} from "./client.ts";
import { channelsAvailable } from "./channelRules.ts";
import { sectionVisible } from "../auth/permissions.ts";

/**
 * 探一次 `GET /admin/channel-kinds`，把结果折成 `{status}` 交给 channelsAvailable。
 * 渠道层关着（KEEL_CHANNELS 未开）时路由没注册，回 404 —— 菜单不出现，也不弹错。
 */
async function probeChannelKinds(): Promise<{ status: number }> {
    try {
        await keel.get("/admin/channel-kinds", {});
        return { status: 200 };
    } catch (err) {
        if (err instanceof ProblemError || err instanceof UnexpectedResponseError) return { status: err.status };
        throw err;
    }
}

/**
 * 「渠道」分区的 available：只缓存可信的结果（200 / 404，见 channelRules.channelProbeOutcome）。
 * 401/403/5xx/网络失败都不缓存，下次再探——不然重新登录之后（只 router.push，整个
 * 模块不会重新加载）这个缓存会一直按上一次的失败结果回答，菜单永远不出现。
 * 会话变化（登录 / 登出 / 刷新身份，见 client.ts 的 onSessionChange）时也清一次：
 * 换了人，角色可能跟着变，旧结果不该再信。
 */
let cached: Promise<boolean> | null = null;
onSessionChange(() => {
    cached = null;
});

export function channelSectionAvailable(): Promise<boolean> {
    // 当前角色本来就看不见「渠道」分区（服务端 requireMerchantWide）：不用为一个
    // 注定不显示的菜单项打请求。
    if (!sectionVisible("channels")) return Promise.resolve(false);
    cached ??= (async () => {
        const { available, cacheable } = await channelsAvailable(probeChannelKinds);
        if (!cacheable) cached = null;
        return available;
    })();
    return cached;
}

/** 推送状态一页。契约的 listings 只回 items（没有 total），满一页就认为可能还有下一页。 */
export const LISTING_PAGE_SIZE = 50;

export async function listListings(
    bindingId: number,
    opts: { storeId: number | null; errorsOnly: boolean; page: number },
): Promise<{ items: ChannelListing[]; hasMore: boolean }> {
    const res = await keel.request("get", "/admin/channel-bindings/{binding_id}/listings", {
        path: { binding_id: bindingId },
        query: {
            page: opts.page,
            page_size: LISTING_PAGE_SIZE,
            ...(opts.storeId === null ? {} : { store_id: opts.storeId }),
            ...(opts.errorsOnly ? { errors_only: true } : {}),
        },
    });
    return { items: res.items, hasMore: res.items.length >= LISTING_PAGE_SIZE };
}

/** 渠道订单一页（GET /admin/channel-orders）。null 的筛选不带。 */
export const CHANNEL_ORDER_PAGE_SIZE = 20;

export async function listChannelOrders(opts: {
    bindingId: number | null;
    storeId: number | null;
    status: number | null;
    exceptionOnly: boolean;
    page: number;
    pageSize?: number;
}): Promise<ChannelOrderPage> {
    return keel.request("get", "/admin/channel-orders", {
        query: {
            page: opts.page,
            page_size: opts.pageSize ?? CHANNEL_ORDER_PAGE_SIZE,
            ...(opts.bindingId === null ? {} : { binding_id: opts.bindingId }),
            ...(opts.storeId === null ? {} : { store_id: opts.storeId }),
            ...(opts.status === null ? {} : { status: opts.status as ChannelOrderDetail["status"] }),
            ...(opts.exceptionOnly ? { exception_only: true } : {}),
        },
    });
}

export function getChannelOrder(id: number): Promise<ChannelOrderDetail> {
    return keel.request("get", "/admin/channel-orders/{channel_order_id}", { path: { channel_order_id: id } });
}

/** 重试 / 接单 / 拒单：都回处理之后的渠道单。 */
export function channelOrderAction(id: number, action: "retry" | "accept" | "reject", reason?: string): Promise<ChannelOrderDetail> {
    const path = { channel_order_id: id };
    switch (action) {
        case "retry":
            return keel.request("post", "/admin/channel-orders/{channel_order_id}/retry", { path });
        case "accept":
            return keel.request("post", "/admin/channel-orders/{channel_order_id}/accept", { path });
        case "reject":
            return keel.request("post", "/admin/channel-orders/{channel_order_id}/reject", {
                path,
                body: reason === undefined || reason === "" ? {} : { reason },
            });
    }
}

/** 同意 / 拒绝平台申请：回申请所在的渠道单。 */
export function decideChannelOrderRequest(requestId: number, agree: boolean): Promise<ChannelOrderDetail> {
    return keel.request("post", "/admin/channel-order-requests/{request_id}/decision", {
        path: { request_id: requestId },
        body: { agree },
    });
}
