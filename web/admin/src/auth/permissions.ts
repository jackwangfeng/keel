// 界面上的按角色显示 / 置灰。**只是体验，真正的拦截在服务端**
// （internal/service/authz.go；契约 StaffRole 的描述里那张矩阵）。
//
// 所以这里的每一个判断都宁可放宽也不收紧：界面放行了而服务端拒了，用户看到的是
// 服务端原样的 403 Problem（ProblemAlert 会把 title / detail / type 摊开），
// 那是一句真话；反过来，界面把一个其实有权的人挡在按钮外面，他连被拒的理由都
// 看不到，只会以为功能坏了。
//
// 全部判据收在这一个文件里，页面只调 `can.xxx()`，不在模板里自己比 role ——
// 菜单布局与各页面另有人在改，把角色逻辑散进去的话，每次合并都要逐行对。
//
// 身份来自登录时的 StaffSession.staff，并在每次整页加载时用 GET /admin/me 刷新
// 一次（refreshIdentity）：管辖范围可能在这 7 天的会话里被人改过，而服务端
// 每个请求都从库里重读，界面至少要在刷新之后跟上。

import { currentSession, keel, setSession, type AdminStore, type Staff, type StaffRole } from "../api/client.ts";

export const ROLE = { admin: 1, operator: 2, regionManager: 3, storeManager: 4 } as const;

/** 角色的显示名。契约 StaffRole：1 管理员 · 2 操作员 · 3 大区管理员 · 4 门店管理员。 */
export const ROLE_TEXT: Record<StaffRole, string> = {
    1: "管理员",
    2: "操作员",
    3: "大区管理员",
    4: "门店管理员",
};

/** 置灰按钮上的提示。说清楚「为什么点不动」，免得像是坏了。 */
export const NO_PERMISSION = "你的角色或管辖范围不包括这个操作（服务端同样会拒绝）";

function me(): Staff | null {
    return currentSession()?.staff ?? null;
}

function role(): number {
    return me()?.role ?? 0;
}

/** 对当前这家店是不是全店范围：管理员与操作员（含平台级）。 */
export function merchantWide(): boolean {
    return role() === ROLE.admin || role() === ROLE.operator;
}

export function isPlatform(): boolean {
    return me()?.merchant_id === null;
}

function managesRegion(regionId: number): boolean {
    return role() === ROLE.regionManager && (me()?.region_ids ?? []).includes(regionId);
}

type StoreRef = Pick<AdminStore, "id" | "region_id">;

export const can = {
    /** 商品、SKU、基准价、类目、上传。 */
    editCatalog: (): boolean => merchantWide(),
    /** 建大区：大区管理员也不行（建出来的大区不在他的范围里）。 */
    createRegion: (): boolean => merchantWide(),
    /** 改 / 删大区、大区价、大区上下架。 */
    manageRegion: (regionId: number): boolean => merchantWide() || managesRegion(regionId),
    /** 建门店（在这个大区里）。 */
    createStoreIn: (regionId: number): boolean => merchantWide() || managesRegion(regionId),
    /** 门店本身的改 / 删 / 围栏。 */
    manageStore: (s: StoreRef): boolean => merchantWide() || managesRegion(s.region_id),
    /** 门店价、门店上下架、门店库存。 */
    operateStore: (s: StoreRef): boolean =>
        merchantWide() ||
        managesRegion(s.region_id) ||
        (role() === ROLE.storeManager && (me()?.store_ids ?? []).includes(s.id)),
    /** 设默认门店：只有管理员。 */
    setDefaultStore: (): boolean => role() === ROLE.admin,
    /** 加 / 改员工。 */
    manageStaff: (): boolean => role() === ROLE.admin || role() === ROLE.regionManager,
    /** 改这个员工：大区管理员只能改门店管理员；谁都不能改自己的角色（那一格在表单里单独锁）。 */
    editStaff: (target: Staff): boolean =>
        role() === ROLE.admin || (role() === ROLE.regionManager && target.role === ROLE.storeManager),
    /**
     * 给这个员工重签一次性登录 token。服务端的判据与「改这个员工」是同一个
     * （authorizeStaffWrite），所以这里也跟 editStaff 同一个判断；停用的人另外
     * 置灰（服务端 409 staff-disabled），那一条在页面上单独说原因。
     */
    reissueLoginToken: (target: Staff): boolean => can.editStaff(target),
    /** 大区管理员能不能动大区列表里的这一行（只读的时候仍然能点进详情）。 */
    seeRegionsSection: (): boolean => role() !== ROLE.storeManager,
    seeStaffSection: (): boolean => role() !== ROLE.storeManager,
};

/** 当前身份能分配哪些角色。平台级只能加平台级的管理员 / 操作员。 */
export function assignableRoles(): StaffRole[] {
    if (role() === ROLE.admin) return isPlatform() ? [1, 2] : [1, 2, 3, 4];
    if (role() === ROLE.regionManager) return [4];
    return [];
}

/** 菜单分区是否对当前角色显示。key 与 src/router/modules 里各分区的 key 一致。 */
export function sectionVisible(key: string): boolean {
    switch (key) {
        case "regions":
            return can.seeRegionsSection();
        case "staff":
            return can.seeStaffSection();
        default:
            return true;
    }
}

/** 角色的一句话说明，挂在页头「你是谁」旁边。 */
export function roleLabel(): string {
    const s = me();
    if (s === null) return "";
    const text = ROLE_TEXT[s.role] ?? `角色 ${s.role}`;
    if (s.merchant_id === null) return `平台级${text}`;
    if (s.role === ROLE.regionManager) return `${text}（管 ${s.region_ids.length} 个大区）`;
    if (s.role === ROLE.storeManager) return `${text}（管 ${s.store_ids.length} 家门店）`;
    return `商家级${text}`;
}

let refreshed = false;

/**
 * 每次整页加载时刷新一次身份（角色与管辖范围）。失败不拦路：拿不到就沿用
 * 登录时那一份 —— 界面判断本来就只是体验，服务端每个请求都会重新判。
 */
export async function refreshIdentity(): Promise<void> {
    if (refreshed) return;
    const s = currentSession();
    if (s === null) return;
    refreshed = true;
    try {
        const staff = await keel.get("/admin/me", {});
        setSession({ ...s, staff });
    } catch {
        // 401 由 client 的统一出口处理（清会话、回登录页）；别的错误不影响使用。
    }
}
