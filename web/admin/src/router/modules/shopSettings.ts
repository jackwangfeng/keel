import { Setting } from "@element-plus/icons-vue";
import type { AdminSection } from "../section.ts";

// 店铺设置（数据模型 §2 shop_preferences）：时区、自动确认收货天数、退货寄回时限、客服电话。
// 只有管理员看得见（auth/permissions.ts 的 sectionVisible）；服务端对别的角色 403 role-forbidden。
const section: AdminSection = {
    key: "shop-settings",
    title: "店铺设置",
    icon: Setting,
    order: 65,
    routes: [
        {
            path: "shop-settings",
            name: "shop-settings",
            component: () => import("../../views/ShopSettingsView.vue"),
            meta: { title: "店铺设置", menu: true },
        },
    ],
};

export default section;
