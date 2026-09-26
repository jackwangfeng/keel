// 后台与 Keel 之间的全部通信，都从这里出去。
//
// ## 这个文件里没有一个手写的请求 / 响应类型
//
// 全部来自 `web/src/api/schema.d.ts` —— `make generate` 从
// `docs/电商系统-OpenAPI.yaml` 生成并入库的那 176 KB。契约里把
// `min_price_cents` 改个名字，重新生成之后，读它的每一个 .vue 都会在
// `make admin-type-check` 下当场变红。验证这条真的成立的步骤写在
// web/admin/README.md 的「变异验证」一节 —— 那不是一句口号，是跑过的。
//
// ## 也没有重新实现一个客户端
//
// 直接用 `web/src/api/client.mts`：那份 SDK 的 49 条路径、query / path / body
// 三个槽的必填性、Problem 与非 Problem 的分流，全部由契约推导，已经有
// `make schema-check` 守着。后台再写一份「差不多的」，就是第二个会分叉的
// 真相源，而且分叉的那一份不会有任何闸门抓到。
//
// SDK 缺的两件事在这里补，两件都补在**它自己留的扩展点**上，不改它：
//
//   1. **动态的 Authorization 头**。SDK 的 headers 在构造时固定，而后台 token
//      会随登录 / 登出变。用 `options.fetch` 包一层注入 —— 那个参数存在的
//      理由就是这个。
//   2. **multipart 上传**。`POST /admin/uploads` 的请求体是 multipart/form-data，
//      SDK 的 body 槽只认 application/json（契约里 multipart 的那条路径，
//      它推导出来的 body 槽是 `{ body?: never }`）。所以上传单独一个函数，
//      但**响应类型仍然从契约取**（`ResponseBodyOf<"/admin/uploads", "post">`）。

import {
    KeelClient,
    ProblemError,
    UnexpectedResponseError,
    isProblem,
    type Problem,
    type ResponseBodyOf,
} from "@contract/client.mts";
import type { components } from "@contract/schema.js";
import { merchantScopeHeaders } from "./merchantScope.ts";

export { KeelError, ProblemError, UnexpectedResponseError, isProblem } from "@contract/client.mts";
export type { Problem } from "@contract/client.mts";

// ---------------------------------------------------------------------------
// 契约类型的对外别名。后台各页面只从这里取类型，不去碰 schema.d.ts 的索引语法。
// ---------------------------------------------------------------------------

type S = components["schemas"];

export type Staff = S["Staff"];
export type StaffSession = S["StaffSession"];
export type StaffRole = S["StaffRole"];
export type Merchant = S["Merchant"];
export type AdminProduct = S["AdminProduct"];
export type AdminProductDetail = S["AdminProductDetail"];
export type AdminSku = S["AdminSku"];
export type AdminInventory = S["AdminInventory"];
export type AdminCategory = S["AdminCategory"];
export type ProductImage = S["ProductImage"];
export type Upload = S["Upload"];
export type FieldError = S["FieldError"];
export type InventoryConflict = S["InventoryConflict"];
export type ProductCreateRequest = S["ProductCreateRequest"];
export type ProductUpdateRequest = S["ProductUpdateRequest"];
export type SkuCreateRequest = S["SkuCreateRequest"];
export type SkuUpdateRequest = S["SkuUpdateRequest"];
export type CategoryCreateRequest = S["CategoryCreateRequest"];
export type CategoryUpdateRequest = S["CategoryUpdateRequest"];
export type InventorySetRequest = S["InventorySetRequest"];
export type MerchantCreateRequest = S["MerchantCreateRequest"];
export type StaffCreateRequest = S["StaffCreateRequest"];

// 多门店 + 大区（契约 Store tag）。
export type AdminRegion = S["AdminRegion"];
export type AdminStore = S["AdminStore"];
export type AdminStoreList = S["AdminStoreList"];
export type GeoPolygon = S["GeoPolygon"];
export type ScopedProductListing = S["ScopedProductListing"];
export type ScopedSkuPrice = S["ScopedSkuPrice"];
export type RegionCreateRequest = S["RegionCreateRequest"];
export type RegionUpdateRequest = S["RegionUpdateRequest"];
export type StoreCreateRequest = S["StoreCreateRequest"];
export type StoreUpdateRequest = S["StoreUpdateRequest"];
export type MerchantUpdateRequest = S["MerchantUpdateRequest"];

/** `GET /admin/products` 的响应体（PageMeta 三个字段 + items）。 */
export type AdminProductPage = ResponseBodyOf<"/admin/products", "get">;
/** `GET /admin/staff` 的响应体。 */
export type StaffPage = ResponseBodyOf<"/admin/staff", "get">;
/** `GET /admin/regions` 的响应体。 */
export type RegionPage = ResponseBodyOf<"/admin/regions", "get">;
/** 门店 / 大区维度的商品列表（两条路径同一个形状）。 */
export type ScopedProductPage = ResponseBodyOf<"/admin/stores/{store_id}/products", "get">;
/** `GET /admin/stores/{store_id}/inventories` 的响应体。 */
export type StoreInventoryPage = ResponseBodyOf<"/admin/stores/{store_id}/inventories", "get">;
/** `GET /admin/merchants` 的响应体（含 single_merchant_mode）。 */
export type MerchantList = ResponseBodyOf<"/admin/merchants", "get">;
/** `POST /admin/uploads` 的响应体。 */
export type UploadResponse = ResponseBodyOf<"/admin/uploads", "post">;

// ---------------------------------------------------------------------------
// 会话 token
// ---------------------------------------------------------------------------

const STORAGE_KEY = "keel.admin.session";

/**
 * 会话放 sessionStorage，不放 localStorage。
 *
 * 契约里这串 token 的描述是「后台 token 能读全部订单与客户手机号、能改价、
 * 能发起退款——它和支付密钥是同一个量级的东西」。localStorage 会让它在这台
 * 机器上活满 7 天，任何一次 XSS 都能把它整个取走；sessionStorage 关掉标签页
 * 就没了，刷新（F5）还在。
 *
 * 更好的形态是服务端下发 HttpOnly Cookie，但契约把 token 放在响应体里并要求
 * `Authorization: Bearer`（`StaffSession.token`），前端**没有**把它藏进
 * HttpOnly 的办法 —— 这是契约层面的事，不是界面能自己解决的。写在这里，
 * 免得读者以为没人想过。
 */
let session: StaffSession | null = readStoredSession();

function readStoredSession(): StaffSession | null {
    try {
        const raw = globalThis.sessionStorage.getItem(STORAGE_KEY);
        if (raw === null) return null;
        const parsed = JSON.parse(raw) as StaffSession;
        // 过期的会话不如没有：留着它只会让第一次请求以 401 失败，
        // 而用户看到的是一个已登录的界面突然报错。
        if (Date.parse(parsed.expire_at) <= Date.now()) return null;
        return parsed;
    } catch {
        return null;
    }
}

export function currentSession(): StaffSession | null {
    return session;
}

export function setSession(next: StaffSession | null): void {
    session = next;
    try {
        if (next === null) globalThis.sessionStorage.removeItem(STORAGE_KEY);
        else globalThis.sessionStorage.setItem(STORAGE_KEY, JSON.stringify(next));
    } catch {
        // 隐私模式下 sessionStorage 会抛。内存里那一份仍然有效，
        // 代价只是刷新页面要重新登录 —— 比整个后台打不开好。
    }
}

// ---------------------------------------------------------------------------
// 客户端实例
// ---------------------------------------------------------------------------

/**
 * 服务地址。默认 `/api/v1` —— 同源，由 nginx 反代到 app（docker/admin-nginx.conf）。
 * `npm run dev` 时由 vite.config.ts 的 proxy 转发，形状一致。
 */
export const API_BASE = import.meta.env.VITE_KEEL_API_BASE ?? "/api/v1";

/** 401 的统一出口。由 main.ts 注册成「清会话 + 跳登录页」。 */
let onUnauthorized: (() => void) | null = null;

export function setUnauthorizedHandler(fn: () => void): void {
    onUnauthorized = fn;
}

function authHeaders(): Record<string, string> {
    if (session === null) return {};
    // X-Keel-Merchant 只会出现在平台级会话上（api/merchantScope.ts）。
    return { Authorization: `Bearer ${session.token}`, ...merchantScopeHeaders(session) };
}

function handleUnauthorized(status: number): void {
    // 只对 401 动手。403（不是平台级管理员）说明会话是好的，只是这个人
    // 干不了这件事 —— 把他踢下线会让「权限不够」表现成「登录失效」，
    // 那是一条指向错误方向的症状。
    if (status !== 401) return;
    setSession(null);
    onUnauthorized?.();
}

export const keel = new KeelClient({
    baseUrl: API_BASE,
    fetch: async (input, init) => {
        const headers = new Headers(init?.headers);
        for (const [k, v] of Object.entries(authHeaders())) headers.set(k, v);
        const response = await globalThis.fetch(input, { ...init, headers });
        handleUnauthorized(response.status);
        return response;
    },
});

// ---------------------------------------------------------------------------
// multipart 上传
// ---------------------------------------------------------------------------

/**
 * `POST /admin/uploads`。
 *
 * 不走 SDK 的原因写在文件头：它的 body 槽只认 application/json。
 * 错误处理刻意与 SDK 一致（同样抛 ProblemError / UnexpectedResponseError），
 * 否则调用方要为这一条接口写第二套 catch。
 *
 * 契约里的三条限制在这里**不重复实现**：10 MB、三种 content_type 由服务端
 * 回 413 / 415，界面把那两个 Problem 的 title 原样显示出来。前端再抄一份
 * 阈值，改契约时就会有一处忘了改，而那一处会把合法的上传挡在本地。
 */
export async function uploadProductImage(file: File, idempotencyKey: string): Promise<UploadResponse> {
    const url = `${API_BASE.replace(/\/+$/, "")}/admin/uploads`;
    const form = new FormData();
    form.append("file", file);

    const headers = new Headers({
        Accept: "application/json, application/problem+json",
        "Idempotency-Key": idempotencyKey,
    });
    for (const [k, v] of Object.entries(authHeaders())) headers.set(k, v);
    // Content-Type 不设：必须让浏览器自己带上 multipart 的 boundary。

    const response = await globalThis.fetch(url, { method: "POST", body: form, headers });
    handleUnauthorized(response.status);
    const text = await response.text();

    if (!response.ok) {
        let parsed: unknown;
        try {
            parsed = JSON.parse(text) as unknown;
        } catch {
            throw new UnexpectedResponseError(url, response.status, response.headers.get("content-type"), text);
        }
        if (!isProblem(parsed)) {
            throw new UnexpectedResponseError(url, response.status, response.headers.get("content-type"), text);
        }
        throw new ProblemError(url, response.status, parsed);
    }
    return JSON.parse(text) as UploadResponse;
}

// ---------------------------------------------------------------------------
// Problem 的两个后台专用判据
// ---------------------------------------------------------------------------

/** 契约里那些 `type` 的前缀。写成常量免得每处拼一遍字符串。 */
const P = "https://keel.dev/problems/";

export const ProblemType = {
    inventoryPrecondition: `${P}inventory-precondition-failed`,
    complianceRejected: `${P}compliance-rejected`,
    complianceUnavailable: `${P}compliance-unavailable`,
    idempotencyInFlight: `${P}idempotency-key-in-flight`,
    idempotencyKeyReused: `${P}idempotency-key-reused`,
    notImplemented: `${P}not-implemented`,
    // 门店 / 大区那一组。每一种该怎么处理写在 internal/problem/problem.go 那段注释里，
    // 界面上的处理在 api/errors.ts 的 problemHint。
    regionCodeConflict: `${P}region-code-conflict`,
    regionHasStores: `${P}region-has-stores`,
    storeCodeConflict: `${P}store-code-conflict`,
    defaultStoreConflict: `${P}default-store-conflict`,
    storeFenceRequired: `${P}store-fence-required`,
    storeUnavailable: `${P}store-unavailable`,
    storeAmbiguous: `${P}store-ambiguous`,
    invalidFence: `${P}invalid-fence`,
    skuNotSoldInStore: `${P}sku-not-sold-in-store`,
    // 商家管理与平台级租户切换。
    tenantSwitchForbidden: `${P}tenant-switch-forbidden`,
    unknownMerchant: `${P}unknown-merchant`,
    singleMerchantMode: `${P}single-merchant-mode`,
    platformOnly: `${P}platform-only`,
} as const;

/** 这个错误是不是某个 type 的 Problem。 */
export function isProblemType(err: unknown, type: string): err is ProblemError {
    return err instanceof ProblemError && err.problem.type === type;
}

/**
 * 库存 CAS 冲突。
 *
 * 契约把这个 409 的响应体单独定义成 `InventoryConflict`（= Problem + current），
 * 所以这里的收窄结果是**契约类型**，不是一个手写的 `{current: {...}}`。
 * `current` 里少一个字段、改一个名字，用它的那一行会红。
 */
export function asInventoryConflict(err: unknown): InventoryConflict | null {
    if (!(err instanceof ProblemError)) return null;
    if (err.problem.type !== ProblemType.inventoryPrecondition) return null;
    const candidate = err.problem as Problem & { current?: unknown };
    const current = candidate.current;
    if (typeof current !== "object" || current === null) return null;
    if (typeof (current as { available_qty?: unknown }).available_qty !== "number") return null;
    return candidate as InventoryConflict;
}

/** 合规拒绝（422，errors[] 带 offset / length）。 */
export function asComplianceRejection(err: unknown): Problem | null {
    if (!(err instanceof ProblemError)) return null;
    return err.problem.type === ProblemType.complianceRejected ? err.problem : null;
}
