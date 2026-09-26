// 平台管理员「当前在管哪家店」——X-Keel-Merchant 请求头的唯一来源。
//
// 服务端的规则（internal/auth/staff_tenant.go）：这个头只对**平台级会话**生效；
// 商家级会话带了它是 403 tenant-switch-forbidden；code 不存在是 422，不回落。
// 所以前端这一侧的纪律是对称的：
//
//   · **只有平台级会话才带这个头。** 商家级员工看不到切换器，而且即使存储里
//     有一个残留的选择，这里也不会给他的请求加头——判据是会话本身
//     （staff.merchant_id === null），不是「有没有选过」。
//   · **选择绑在会话上，不跨会话。** 存的时候记下会话 token 的指纹，读的时候
//     对不上就当没有并清掉。于是登出、换人登录、会话过期重登，都不会把上一个人
//     选的店带过来——一个以为自己在管 A 店的人，不能因为上一个会话选过 B 店
//     而往 B 店里写。存 sessionStorage（和会话本身同一处），关标签页即失效。
//
// 这个文件**不 import client.ts 的运行时**（只有 import type），client.ts 反过来
// 调它：两边互相 import 运行时会成环。

import type { StaffSession } from "./client.ts";

export const MERCHANT_SWITCH_HEADER = "X-Keel-Merchant";

const KEY = "keel.admin.merchant-scope";

export interface MerchantScope {
    code: string;
    name: string;
    /** 这家店当前是不是停用的（切换器上要标出来）。 */
    disabled: boolean;
}

interface Stored extends MerchantScope {
    /** 会话 token 的指纹。不是 token 本身——没必要在第二个地方再存一份凭据。 */
    fp: string;
}

function fingerprint(token: string): string {
    // 取尾部 16 个字符：足够区分两串会话，又不把整串凭据再存一遍。
    return token.slice(-16);
}

function isPlatform(session: StaffSession | null): session is StaffSession {
    return session !== null && session.staff.merchant_id === null;
}

/** 当前会话下选中的店；没选、会话不是平台级、或者选择属于上一个会话时返回 null。 */
export function currentMerchantScope(session: StaffSession | null): MerchantScope | null {
    if (!isPlatform(session)) return null;
    try {
        const raw = globalThis.sessionStorage.getItem(KEY);
        if (raw === null) return null;
        const s = JSON.parse(raw) as Stored;
        if (s.fp !== fingerprint(session.token)) {
            globalThis.sessionStorage.removeItem(KEY);
            return null;
        }
        return { code: s.code, name: s.name, disabled: s.disabled };
    } catch {
        return null;
    }
}

export function setMerchantScope(session: StaffSession | null, scope: MerchantScope | null): void {
    try {
        if (scope === null || !isPlatform(session)) {
            globalThis.sessionStorage.removeItem(KEY);
            return;
        }
        const stored: Stored = { ...scope, fp: fingerprint(session.token) };
        globalThis.sessionStorage.setItem(KEY, JSON.stringify(stored));
    } catch {
        // 隐私模式下存不了：切换就只在这一页有效。不影响正确性——不存就不带头。
    }
}

/** 给 client.ts 的 fetch 包装用：要带的额外请求头。 */
export function merchantScopeHeaders(session: StaffSession | null): Record<string, string> {
    const scope = currentMerchantScope(session);
    return scope === null ? {} : { [MERCHANT_SWITCH_HEADER]: scope.code };
}
