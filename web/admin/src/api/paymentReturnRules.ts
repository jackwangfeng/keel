// 多收款退回单的展示规则（契约 PaymentReturn，00150）。

export const PAYMENT_RETURN_REASON: Record<1 | 2 | 3, string> = {
    1: "重复支付",
    2: "订单已取消或关闭后到账",
    3: "金额与应付不符",
};

export const PAYMENT_RETURN_STATUS: Record<10 | 30 | 40, { text: string; tag: "info" | "warning" | "success" }> = {
    10: { text: "待提交", tag: "warning" },
    30: { text: "退回中", tag: "info" },
    40: { text: "已退回", tag: "success" },
};

/** 待提交且有失败原因：提交失败、等下一轮重试 —— 列表上要让人一眼看到。 */
export function isStuck(r: { status: number; last_error?: string | null }): boolean {
    return r.status === 10 && typeof r.last_error === "string" && r.last_error !== "";
}
