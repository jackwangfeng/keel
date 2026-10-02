// 渠道管理页的取数。对 client.ts 的薄封装：类型全从契约来（client.ts 的别名），请求都走 `keel`。

import { ProblemError, UnexpectedResponseError, keel, type ChannelListing } from "./client.ts";
import { channelsAvailable } from "./channelRules.ts";

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

/** 「渠道」分区的 available：整页加载内只探一次。 */
let cached: Promise<boolean> | null = null;
export function channelSectionAvailable(): Promise<boolean> {
    cached ??= channelsAvailable(probeChannelKinds);
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
