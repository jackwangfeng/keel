import { Shop } from "@element-plus/icons-vue";
import type { AdminSection } from "../section.ts";

// 商家管理：列表（含停用）、开店、改名、停用启用、切过去管理。
// 只对平台级会话出现在菜单里；服务端对商家级会话回 403 platform-only。
const section: AdminSection = {
    key: "merchants",
    title: "商家管理",
    icon: Shop,
    order: 70,
    platformOnly: true,
    routes: [
        {
            path: "merchants",
            name: "merchants",
            component: () => import("../../views/MerchantListView.vue"),
            meta: { title: "商家管理", menu: true },
        },
    ],
};

export default section;
