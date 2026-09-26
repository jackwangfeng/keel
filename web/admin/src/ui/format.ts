// 显示用的格式化。**只在显示层做，不在数据层做。**
//
// 金额在契约里是 `Money = number`，单位是**分**（integer cents）。界面上显示
// 元，但存进 ref 的、发回服务端的，始终是分 —— 中途换单位是这类系统里
// 最贵的一类 bug：一次四舍五入的误差会变成一笔真的钱。

/** 分 → 「¥12.34」。 */
export function yuan(cents: number): string {
    const sign = cents < 0 ? "-" : "";
    const abs = Math.abs(cents);
    return `${sign}¥${Math.floor(abs / 100)}.${String(abs % 100).padStart(2, "0")}`;
}

/** 价格区间。一个 SKU 都没有时两端都是 0，服务端现算的。 */
export function priceRange(min: number, max: number): string {
    return min === max ? yuan(min) : `${yuan(min)} ~ ${yuan(max)}`;
}

/** ISO 时间 → 本地可读。空值给一个明确的「—」，不要给空白。 */
export function datetime(value: string | null | undefined): string {
    if (value === null || value === undefined || value === "") return "—";
    const t = new Date(value);
    if (Number.isNaN(t.getTime())) return value;
    return t.toLocaleString("zh-CN", { hour12: false });
}

/** `products.status`：0 草稿 / 1 上架 / 2 下架（数据模型 §3，契约里不另发明）。 */
export const PRODUCT_STATUS: Record<0 | 1 | 2, { text: string; tag: "info" | "success" | "warning" }> = {
    0: { text: "草稿", tag: "info" },
    1: { text: "上架", tag: "success" },
    2: { text: "下架", tag: "warning" },
};

/** `skus.status`：0 停售 / 1 在售。 */
export const SKU_STATUS: Record<0 | 1, { text: string; tag: "info" | "success" }> = {
    0: { text: "停售", tag: "info" },
    1: { text: "在售", tag: "success" },
};

/** `categories.status`：0 停用 / 1 启用。 */
export const CATEGORY_STATUS: Record<0 | 1, { text: string; tag: "info" | "success" }> = {
    0: { text: "停用", tag: "info" },
    1: { text: "启用", tag: "success" },
};

/** `staff.role`：契约里 StaffRole 是 1 | 2。 */
export const STAFF_ROLE: Record<1 | 2, string> = { 1: "管理员", 2: "操作员" };

/** `staff.status`：1 正常 2 停用。 */
export const STAFF_STATUS: Record<1 | 2, { text: string; tag: "success" | "info" }> = {
    1: { text: "正常", tag: "success" },
    2: { text: "停用", tag: "info" },
};
