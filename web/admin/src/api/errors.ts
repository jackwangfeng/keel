// 把服务端的错误变成**看得懂的东西**。
//
// 后端回的是 RFC 9457 的 `application/problem+json`，`title` 已经是中文人话，
// `errors[]` 里带字段、带位置（合规拒绝时给「第几个字」）。把这些吞成一句
// 「操作失败」，等于把服务端花在错误设计上的功夫整个扔掉 —— 那正是
// internal/problem 那个包存在的理由。
//
// 这里只做「翻译成一个便于渲染的形状」，不做措辞：措辞是服务端的，
// 前端再写一套中文只会和服务端的说法分叉。前端只在服务端**说不出话**的时候
// （网络断了、响应体根本不是 Problem）自己补一句。

import { ProblemError, UnexpectedResponseError, type FieldError } from "./client.ts";

export type ErrorKind = "problem" | "unexpected" | "network";

export interface DisplayError {
    kind: ErrorKind;
    /** 大标题。problem 时就是服务端的 `title`，原样。 */
    title: string;
    detail: string;
    /** Problem 的 `type`，用于「这到底是哪一类错」。 */
    type: string;
    status: number | null;
    traceId: string;
    /** 字段级错误。合规拒绝时带 offset / length。 */
    fields: FieldError[];
    /** 非 Problem 响应的原始体（已截断），出错时贴给运维用。 */
    rawBody: string;
}

export function describeError(err: unknown): DisplayError {
    if (err instanceof ProblemError) {
        return {
            kind: "problem",
            title: err.problem.title,
            detail: err.problem.detail ?? "",
            type: err.problem.type,
            status: err.status,
            traceId: err.problem.trace_id ?? "",
            fields: err.problem.errors ?? [],
            rawBody: "",
        };
    }
    if (err instanceof UnexpectedResponseError) {
        return {
            kind: "unexpected",
            // 这句是前端自己说的，因为服务端在这条路上没说出合法的话。
            // 它和业务错误必须看起来不一样：含义是「这一跳根本没到 Keel，
            // 或者 Keel 违约了」，让人去查网关 / 代理，而不是去改表单。
            title: `服务端回了 ${err.status}，但响应体不是契约里的 Problem`,
            detail: `content-type: ${err.contentType ?? "无"}。这多半不是业务错误——请求可能没到 Keel（网关 / 反向代理），或者 Keel 违约了。`,
            type: "",
            status: err.status,
            traceId: "",
            fields: [],
            rawBody: err.bodyText,
        };
    }
    const message = err instanceof Error ? err.message : String(err);
    return {
        kind: "network",
        title: "请求没能发出去或没拿到响应",
        detail: `${message}。服务地址不通、被浏览器拦下、或者请求被取消了。`,
        type: "",
        status: null,
        traceId: "",
        fields: [],
        rawBody: "",
    };
}

// ---------------------------------------------------------------------------
// 码点下标 → UTF-16 下标
// ---------------------------------------------------------------------------
//
// 契约里 `FieldError.offset` / `length` 的单位写明是 **Unicode 码点**
// （「命中位置（Unicode 码点下标，从 0 开始）」）。JavaScript 的字符串是
// UTF-16，`str.slice(offset, offset + length)` 在有 emoji 或生僻字（U+20000 段，
// 而「蕞」这类字虽然在 BMP 内，商品标题里出现 emoji 是家常便饭）的文本上
// 会切错位置 —— 高亮框会歪，而歪掉的高亮比没有高亮更糟：它指着一个没问题
// 的字说这里违规。
//
// 所以先把字符串按码点拆开（Array.from 正是按码点迭代），再按码点下标切。

/** 按码点把文本切成 [命中前, 命中, 命中后]。越界时安全钳制。 */
export function sliceByCodePoints(text: string, offset: number, length: number): [string, string, string] {
    const points = Array.from(text);
    const start = Math.max(0, Math.min(offset, points.length));
    const end = Math.max(start, Math.min(start + Math.max(0, length), points.length));
    return [points.slice(0, start).join(""), points.slice(start, end).join(""), points.slice(end).join("")];
}

/** 文本的码点长度。给「第 N 个字」这类提示用。 */
export function codePointLength(text: string): number {
    return Array.from(text).length;
}

/** 某个字段上的字段级错误。合规拒绝时一个字段可能有多条。 */
export function fieldErrorsOf(fields: readonly FieldError[], field: string): FieldError[] {
    return fields.filter((f) => f.field === field);
}
