// 订单与售后页的界面规则：状态怎么显示、哪一单亮哪个按钮、表单怎么变成请求体、
// 筛选怎么变成 query。**全部是纯函数**，由 `make admin-test` 用 `node --test` 直接跑。
//
// 刻意只有 `import type`（运行时被类型剥离抹掉）加一个同样零依赖的 ./money.ts：
// 与 geo.ts / money.ts 同一个约定，这一步不需要 node_modules。
//
// ## 这里的判断只是体验，服务端才是权威
//
// 「10 待审核才能审」「20 已支付才能发货」这些规则服务端都有（409 refund-status-not-auditable、
// order-status-not-shippable），界面按同样的规则亮按钮，只是为了不让人点一个注定失败的按钮。
// 列表是几秒前读的，状态可能已经变了 —— 那时服务端的 409 会原样摊开（ProblemAlert），
// 界面刷新即可。所以这里宁可跟着状态走，也不自作主张多藏按钮：例如订单有在途售后时
// 发货按钮**仍然亮着**（部分退款不阻断发货，契约原话），只多挂一句提醒。
//
// ## 金额不在这里算
//
// 每一行的退款金额由服务端按优惠分摊算好（契约 RefundItem.amount_cents），界面只展示；
// 唯一让人填的是退货退款审核时裁定的运费，且只做「元 → 分」的字符串换算（money.ts）。

import type { components, paths } from "@contract/schema.js";
import { yuanToCents } from "./money.ts";

type S = components["schemas"];
type OrderStatus = S["OrderStatus"];
type OrderRefundStatus = S["OrderRefundStatus"];
type RefundStatus = S["RefundStatus"];
type RefundType = S["RefundType"];
type RefundReasonCode = S["RefundReasonCode"];
type TagType = "info" | "success" | "warning" | "danger" | "primary";

/** 契约 OrderStatus 的显示名与标签色。`Record<OrderStatus, …>` 让契约多一个值时这里编译不过。 */
export const ORDER_STATUS: Record<OrderStatus, { text: string; tag: TagType }> = {
    10: { text: "待支付", tag: "info" },
    20: { text: "已支付 · 待发货", tag: "warning" },
    30: { text: "已发货", tag: "primary" },
    40: { text: "已完成", tag: "success" },
    50: { text: "退款中（整单）", tag: "danger" },
    60: { text: "已退款", tag: "info" },
    90: { text: "已关闭", tag: "info" },
};

/** 资金维度（契约 OrderRefundStatus）。0 不显示标签。 */
export const ORDER_REFUND_STATUS: Record<OrderRefundStatus, { text: string; tag: TagType }> = {
    0: { text: "", tag: "info" },
    1: { text: "售后中", tag: "danger" },
    2: { text: "部分退款", tag: "warning" },
    3: { text: "全额退款", tag: "info" },
};

export const REFUND_STATUS: Record<RefundStatus, { text: string; tag: TagType }> = {
    10: { text: "待审核", tag: "warning" },
    20: { text: "待买家退货", tag: "primary" },
    30: { text: "退款中", tag: "primary" },
    40: { text: "已退款", tag: "success" },
    50: { text: "已拒绝", tag: "danger" },
    60: { text: "已取消", tag: "info" },
};

export const REFUND_TYPE: Record<RefundType, string> = { 1: "仅退款", 2: "退货退款" };

export const REFUND_REASON: Record<RefundReasonCode, string> = {
    1: "不想要了",
    2: "少发漏发",
    3: "商品损坏",
    4: "描述不符",
    5: "其他",
};

/** 常用承运商。契约只要一个字符串标识（sf / jd / yto …），这里给下拉的候选，也允许手填。 */
export const CARRIERS: { code: string; name: string }[] = [
    { code: "sf", name: "顺丰" },
    { code: "jd", name: "京东物流" },
    { code: "yto", name: "圆通" },
    { code: "zto", name: "中通" },
    { code: "sto", name: "申通" },
    { code: "yd", name: "韵达" },
    { code: "ems", name: "EMS" },
];

export function carrierName(code: string): string {
    return CARRIERS.find((c) => c.code === code)?.name ?? code;
}

/** 一个按钮在界面上的样子：出不出现、能不能点、点不动时挂什么提示。 */
export interface ActionState {
    visible: boolean;
    enabled: boolean;
    /** 点不动时的理由；能点时是给人看的提醒（可为空串）。 */
    hint: string;
}

const hidden: ActionState = { visible: false, enabled: false, hint: "" };

/** 置灰按钮上的话，与 auth/permissions.ts 的 NO_PERMISSION 同一句（这里不 import，保持零依赖）。 */
export const NO_PERMISSION_HINT = "你的角色或管辖范围不包括这个操作（服务端同样会拒绝）";

/**
 * 订单上的「发货」按钮。
 *
 * 只有 20 已支付出现。50 退款中（整单退款申请在途）不出现发货，而是提示先处理退款单 ——
 * 服务端对它回 409 order-has-pending-full-refund。有在途的部分退款时照常可发，只挂提醒。
 */
export function shipAction(
    order: Pick<S["AdminOrderSummary"], "status" | "has_open_refund">,
    canOperate: boolean,
): ActionState {
    if (order.status !== 20) return hidden;
    if (!canOperate) return { visible: true, enabled: false, hint: NO_PERMISSION_HINT };
    return {
        visible: true,
        enabled: true,
        hint: order.has_open_refund ? "这一单有进行中的售后（部分退款不阻断发货），发货前先看一眼退款单" : "",
    };
}

/** 退款单上的三个动作：同意、驳回（10 待审核），确认收到退货（20 待买家退货）。 */
export interface RefundActions {
    approve: ActionState;
    reject: ActionState;
    receive: ActionState;
    /** 同意时能不能填运费：只有退货退款（契约：freight_cents 只对 refund_type=2 生效）。 */
    freightEditable: boolean;
}

export function refundActions(
    refund: Pick<S["AdminRefund"], "status" | "refund_type">,
    canOperate: boolean,
): RefundActions {
    const denied: ActionState = { visible: true, enabled: false, hint: NO_PERMISSION_HINT };
    const ok: ActionState = { visible: true, enabled: true, hint: "" };
    const pending = refund.status === 10;
    const awaiting = refund.status === 20;
    return {
        approve: pending ? (canOperate ? ok : denied) : hidden,
        reject: pending ? (canOperate ? ok : denied) : hidden,
        receive: awaiting ? (canOperate ? ok : denied) : hidden,
        freightEditable: pending && refund.refund_type === 2,
    };
}

/** 发货表单 → 请求体。与服务端同一条规则（非空、各不超过 64 个字），错了返回一句话。 */
export function buildShipment(
    carrier: string,
    trackingNo: string,
): { body: S["ShipmentCreateRequest"] } | { error: string } {
    const c = carrier.trim();
    const t = trackingNo.trim();
    if (c === "") return { error: "请选择或填写承运商" };
    if (t === "") return { error: "请填写运单号" };
    if ([...c].length > 64 || [...t].length > 64) return { error: "承运商与运单号都不能超过 64 个字" };
    return { body: { carrier_code: c, tracking_no: t } };
}

type AuditBody = NonNullable<paths["/admin/refunds/{refund_no}/audit"]["post"]["requestBody"]>["content"]["application/json"];

/**
 * 审核表单 → 请求体。
 *
 * - 驳回必须有理由（≤ 200 字，契约 maxLength）。
 * - 同意时的运费只对退货退款有意义；仅退款的运费按规则算，**请求体里不带**
 *   （带一个不同的值服务端回 422）。运费留空 = 保持申请时的值（省略字段）。
 * - 运费按「元」填，经 yuanToCents 换成分，不做浮点乘法。
 */
export function buildAudit(
    action: "approve" | "reject",
    refundType: RefundType,
    input: { rejectReason?: string; freightYuan?: string },
): { body: AuditBody } | { error: string } {
    if (action === "reject") {
        const reason = (input.rejectReason ?? "").trim();
        if (reason === "") return { error: "驳回必须填写理由，买家会看到它" };
        if ([...reason].length > 200) return { error: "驳回理由不能超过 200 个字" };
        return { body: { action: "reject", reject_reason: reason } };
    }
    const raw = (input.freightYuan ?? "").trim();
    if (refundType !== 2 || raw === "") return { body: { action: "approve" } };
    const cents = yuanToCents(raw);
    if (cents === null) return { error: "运费请填金额（元），最多两位小数，例如 12 或 8.50" };
    return { body: { action: "approve", freight_cents: cents } };
}

/** 订单页的筛选表单（界面状态）。日期是 el-date-picker 的 value-format="YYYY-MM-DD"。 */
export interface OrderFilterForm {
    status?: OrderStatus;
    storeId?: number;
    dateRange?: [string, string] | null;
    orderNo?: string;
    phone?: string;
}

export interface RefundFilterForm {
    status?: RefundStatus;
    storeId?: number;
    dateRange?: [string, string] | null;
}

type OrderQuery = NonNullable<paths["/admin/orders"]["get"]["parameters"]["query"]>;
type RefundQuery = NonNullable<paths["/admin/refunds"]["get"]["parameters"]["query"]>;

/**
 * 「YYYY-MM-DD 到 YYYY-MM-DD」（按**本地**日历，两端都含）→ 契约的半开区间
 * [created_from, created_to)：起始日本地零点、结束日**次日**本地零点，都转成 RFC3339（UTC）。
 *
 * 次日零点而不是 23:59:59：契约的上界不含，23:59:59.500 下的单用后者会漏掉。
 */
export function dayRange(range: [string, string] | null | undefined): { created_from?: string; created_to?: string } {
    if (range === null || range === undefined) return {};
    const from = localMidnight(range[0], 0);
    const to = localMidnight(range[1], 1);
    if (from === null || to === null) return {};
    return { created_from: from, created_to: to };
}

function localMidnight(day: string, plusDays: number): string | null {
    const m = /^(\d{4})-(\d{2})-(\d{2})$/.exec(day);
    if (m === null) return null;
    const d = new Date(Number(m[1]), Number(m[2]) - 1, Number(m[3]) + plusDays, 0, 0, 0, 0);
    return Number.isNaN(d.getTime()) ? null : d.toISOString();
}

function textOrUndefined(s: string | undefined): string | undefined {
    const v = (s ?? "").trim();
    return v === "" ? undefined : v;
}

/** 订单筛选表单 → GET /admin/orders 的 query。没填的字段不出现（契约里全部是可选的，不传即不筛）。 */
export function orderQuery(f: OrderFilterForm, page: number, pageSize: number): OrderQuery {
    const q: OrderQuery = { page, page_size: pageSize, ...dayRange(f.dateRange) };
    if (f.status !== undefined) q.status = f.status;
    if (f.storeId !== undefined) q.store_id = f.storeId;
    const no = textOrUndefined(f.orderNo);
    if (no !== undefined) q.order_no = no;
    const phone = textOrUndefined(f.phone);
    if (phone !== undefined) q.phone = phone;
    return q;
}

export function refundQuery(f: RefundFilterForm, page: number, pageSize: number): RefundQuery {
    const q: RefundQuery = { page, page_size: pageSize, ...dayRange(f.dateRange) };
    if (f.status !== undefined) q.status = f.status;
    if (f.storeId !== undefined) q.store_id = f.storeId;
    return q;
}

/** 订单行「还可退」：quantity - refunded_qty - refunding_qty（契约 OrderItem.refunding_qty 的定义式）。 */
export function refundableQty(item: Pick<S["OrderItem"], "quantity" | "refunded_qty" | "refunding_qty">): number {
    return Math.max(0, item.quantity - (item.refunded_qty ?? 0) - (item.refunding_qty ?? 0));
}
