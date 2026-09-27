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

import { ProblemError, ProblemType, UnexpectedResponseError, type FieldError } from "./client.ts";

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
    /** 「接下来该做什么」。按 type 给，不按状态码给——见 problemHint。 */
    hint: string;
}

/**
 * 按 Problem 的 type 说「接下来该做什么」。
 *
 * **为什么按 type 不按状态码**：同一个 409 在门店这一组里有三对意思相反的
 * 情况（internal/problem/problem.go 那段注释）——「换个编号重试会成功」
 * 和「在这条接口上重试永远不会成功」都是 409。只看状态码的界面会教人
 * 对着后一种一直点重试。
 *
 * 这里只写**动作**，不重复服务端的 title：title 说的是「发生了什么」，
 * 那是服务端的话；这里补的是界面知道而服务端不知道的那一半——
 * 「在这个后台里，下一步点哪儿」。
 */
export function problemHint(type: string): string {
    switch (type) {
        case ProblemType.storeCodeConflict:
        case ProblemType.regionCodeConflict:
            return "编号在本店内已被占用。换一个编号再提交就会成功。";
        case ProblemType.defaultStoreConflict:
            return "已经有一家默认门店了。在这里重试永远不会成功——先按「不设为默认」建出来，再到门店列表里对它点「设为默认」（那条接口会在同一个事务里先清旧再置新）。";
        case ProblemType.storeFenceRequired:
            return "这家店不是默认门店。清空它的围栏会让它永远接不到单（不被任何坐标命中，也不是回落目标）。要么别清，要么先把它设成默认门店。";
        case ProblemType.invalidFence:
            return "多边形本身画错了。上面那句是 PostGIS 给的原话（ST_IsValidReason），方括号里是出错位置的 [经度 纬度]，地图上已用红圈标出。常见原因：边交叉（自交）、点太少。";
        case ProblemType.storeLocationRequired:
            return "门店必须有坐标，而「门店在不在围栏内」没有坐标就判不了。先到「基本信息」在地图上选门店位置并保存，再回来画围栏。";
        case ProblemType.storeOutsideFence:
            return "门店自己的位置不在围栏内。要么把围栏画大一些、把门店圈进去（橙色圆点是门店），要么到「基本信息」把门店位置挪到围栏里。";
        case ProblemType.storeUnavailable:
            return "这家门店已停业或已删除，不能作为回落目标。先把它改回营业，或换一家设为默认。";
        case ProblemType.storeAmbiguous:
            return "这家商家不止一家门店，「这个 SKU 的库存」没有唯一答案，服务端不替你猜。重试没有用——到门店维度改（门店 → 某家店 → 库存）。";
        case ProblemType.skuNotSoldInStore:
            return "这家店（或它所在的大区）不卖这件商品。重试没有用，得换一家店，或者先在门店 / 大区的商品页把它上架。";
        case ProblemType.regionHasStores:
            return "这个大区下面还有门店。先把门店挪到别的大区或删掉，再删大区。";
        case ProblemType.tenantSwitchForbidden:
            return "商家级账号不能切换商家。这是后台的一个 bug（商家级会话不该带 X-Keel-Merchant 头），请刷新或重新登录。";
        case ProblemType.unknownMerchant:
            return "顶栏选中的那家店不存在了（可能已被删除）。服务端没有回落到别的店——在顶栏重新选一家，或切回「按当前域名」。";
        case ProblemType.singleMerchantMode:
            return "这是单商家部署：再开一家店（或启用另一家）会让它下次重启时启动自检失败。要开多家店，先切到多商家部署：清空 KEEL_DEFAULT_MERCHANT、配置 KEEL_BASE_DOMAIN。";
        case ProblemType.platformOnly:
            return "这件事只有平台级操作员（开店、停用启用要平台级管理员）能做。";
        case ProblemType.roleForbidden:
            return "你的角色做不了这件事。重试不会成功——请找商家管理员来做，或者请他调整你的角色。";
        case ProblemType.outOfScope:
            return "这一个不在你的管辖范围里。重试不会成功——请找管那个大区 / 门店的人，或者请商家管理员调整你的管辖范围。";
        case ProblemType.staffForbidden:
            return "只有管理员（以及只管门店管理员的大区管理员）能管员工。";
        case ProblemType.inventoryPrecondition:
            return "库存在你读到它之后被改过。用服务端回来的当前值刷新后重试就会成功。";
        case ProblemType.orderStatusNotShippable:
            return "这一单已经不是「已支付」了（可能刚被别人发过货，或买家申请了整单退款）。刷新订单看它现在的状态。";
        case ProblemType.orderHasPendingFullRefund:
            return "买家申请了整单退款，还没处理。先到「售后」里审那张退款单：驳回之后订单回到已支付，才能发货。";
        case ProblemType.trackingNoDuplicated:
            return "这个承运商的这个运单号已经登记过了。核对一下是不是录重了。";
        case ProblemType.refundStatusNotAuditable:
            return "这张退款单已经不是「待审核」了（可能刚被别人审过，或买家撤回了）。刷新看它现在的状态。";
        case ProblemType.refundStatusNotReceivable:
            return "这张退款单已经不是「待买家退货」了。刷新看它现在的状态。";
        case ProblemType.refundFreightExceeded:
            return "退运费超过了订单实收运费（扣掉别的退款单已占的部分）。改小一点，或者留空保持申请时的值。";
        case ProblemType.importFileTooLarge:
            return "单个导入文件不超过 5 MB。把表格拆成几个文件分批导入。";
        case ProblemType.importUnsupportedFormat:
            return "只认 xlsx 与 csv。老式 .xls 或加了密码的工作簿请在 Excel 里另存为不加密的 .xlsx。";
        case ProblemType.importFileInvalid:
            return "整份文件不成立，上面逐条列了原因（缺列、超过 2000 行、表头合并……）。改好文件再传；从「下载模板」开始最省事。";
        case ProblemType.importNothingToImport:
            return "没有一件商品能导入：每件要么有红色的错误行，要么还没选类目。回到预检结果改完再确认。";
        default:
            return "";
    }
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
            hint: problemHint(err.problem.type),
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
            hint: "",
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
        hint: "",
    };
}

/**
 * PostGIS 的 ST_IsValidReason 形如 `Self-intersection[116.4 39.9]`——方括号里是
 * 出错位置的 [经度 纬度]。取出来给地图标一个红圈。取不到就返回 null，
 * 不猜：detail 的措辞是服务端的，这里只是顺手利用它。
 */
export function fenceErrorPoint(detail: string): [number, number] | null {
    const m = /\[\s*(-?\d+(?:\.\d+)?)\s+(-?\d+(?:\.\d+)?)\s*\]/.exec(detail);
    if (m === null) return null;
    const lng = Number(m[1]);
    const lat = Number(m[2]);
    return Number.isFinite(lng) && Number.isFinite(lat) ? [lng, lat] : null;
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
