// 瞬时提示。**页面级的错误请用 `<ProblemAlert>`**，它留在页面上不会自己消失；
// 这里是给「点一下就完事」的动作用的（上下架、软删、改状态）。
//
// 即便是瞬时提示，也把 detail / type / 字段错误一起带上：一条 4 秒后消失的
// 「操作失败」帮不了任何人。

import { ElMessage, ElNotification } from "element-plus";
import { h } from "vue";
import { describeError } from "../api/errors.ts";

export function notifyError(err: unknown): void {
    const info = describeError(err);
    const rows = [];
    if (info.detail !== "") rows.push(h("p", { style: "margin:4px 0;white-space:pre-wrap" }, info.detail));
    for (const f of info.fields) {
        const pos = typeof f.offset === "number" ? `（第 ${f.offset + 1} 个字起，共 ${f.length ?? 0} 个字）` : "";
        rows.push(h("p", { style: "margin:2px 0;font-size:12px" }, `${f.field ?? ""} ${f.message ?? ""}${pos}`));
    }
    if (info.hint !== "") rows.push(h("p", { style: "margin:6px 0;font-weight:500" }, `下一步：${info.hint}`));
    const meta = [info.status === null ? "" : `HTTP ${info.status}`, info.type, info.traceId]
        .filter((s) => s !== "")
        .join("  ·  ");
    if (meta !== "") rows.push(h("p", { style: "margin:6px 0 0;font-size:11px;opacity:.7" }, meta));

    ElNotification({
        title: info.title,
        message: h("div", rows),
        type: info.status !== null && info.status >= 500 ? "warning" : "error",
        // 0 = 不自动关。错误要等人读完再走 —— 4 秒读不完一条带字段位置的拒绝。
        duration: 0,
        // type / trace_id 是要被复制走的，默认宽度会把它们折成三行。
        customClass: "keel-error-notification",
    });
}

export function notifyOk(message: string): void {
    ElMessage({ message, type: "success" });
}
