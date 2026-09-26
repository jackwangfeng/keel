// 元 ↔ 分的换算。**只有字符串解析，没有一次浮点运算。**
//
// 契约里金额一律是整数分（`Money`）。界面让运营按「元」输入，但 `Number("0.29") * 100`
// 是 28.999999999999996 —— 四舍五入救得回这一个，救不回所有的，而券面金额错一分
// 就是每一单都错一分。所以这里把「12.3」当成字符串拆成整数部分与小数部分，
// 各自按整数算。
//
// 刻意零 import：`make admin-test` 用 `node --test` 直接跑这个文件（Node 的类型剥离），
// 没有 node_modules 也能跑，与 geo.ts 同一个约定。

const YUAN_RE = /^(\d+)(?:\.(\d{0,2}))?$/;

/**
 * 「12」「12.3」「12.34」「0.05」→ 分。空串、负数、超过两位小数、非数字、
 * 超出安全整数范围 → null（调用方据此提示，而不是悄悄截断或四舍五入）。
 */
export function yuanToCents(input: string): number | null {
    const s = input.trim().replace(/^¥/, "");
    const m = YUAN_RE.exec(s);
    if (m === null) return null;
    const whole = m[1] ?? "0";
    const frac = (m[2] ?? "").padEnd(2, "0");
    // 整数部分先做长度闸门：超过 13 位时乘 100 之后就出了 2^53 的安全范围。
    const trimmed = whole.replace(/^0+(?=\d)/, "");
    if (trimmed.length > 13) return null;
    const cents = Number(trimmed) * 100 + Number(frac);
    return Number.isSafeInteger(cents) ? cents : null;
}

/** 分 → 表单里回显的「12.34」（不带 ¥，不带千分位）。负数与非整数不接受。 */
export function centsToYuanInput(cents: number): string {
    if (!Number.isSafeInteger(cents) || cents < 0) return "";
    const whole = Math.floor(cents / 100);
    const frac = cents % 100;
    return frac === 0 ? String(whole) : `${whole}.${String(frac).padStart(2, "0")}`;
}

/** 千分比折扣率 → 「8.5 折」。850 → "8.5 折"，800 → "8 折"，995 → "9.95 折"。 */
export function rateLabel(rate: number): string {
    const zhe = rateToZheInput(rate);
    return zhe === "" ? "—" : `${zhe} 折`;
}

/**
 * 「8.5」（折）→ 850（千分比）。按字符串解析，理由同 yuanToCents。
 * 只收 (0, 10) 之间、至多两位小数的值；越界返回 null。
 */
export function zheToRate(input: string): number | null {
    const m = /^(\d)(?:\.(\d{0,2}))?$/.exec(input.trim());
    if (m === null) return null;
    const rate = Number(m[1]) * 100 + Number((m[2] ?? "").padEnd(2, "0"));
    return rate >= 1 && rate <= 999 ? rate : null;
}

/** 850 → "8.5"（表单回显）。 */
export function rateToZheInput(rate: number): string {
    if (!Number.isInteger(rate) || rate <= 0 || rate >= 1000) return "";
    const whole = Math.floor(rate / 100);
    const frac = rate % 100;
    if (frac === 0) return String(whole);
    return `${whole}.${String(frac).padStart(2, "0").replace(/0$/, "")}`;
}
