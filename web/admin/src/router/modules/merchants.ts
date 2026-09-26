import { Shop } from "@element-plus/icons-vue";
import type { AdminSection } from "../section.ts";

const section: AdminSection = {
    key: "merchants",
    title: "开店",
    icon: Shop,
    order: 70,
    // 契约写明：调用者必须是平台级（merchant_id 为空）且 role = 1，否则 403。
    platformOnly: true,
    routes: [
        {
            path: "merchants",
            name: "merchants",
            component: () => import("../../views/MerchantCreateView.vue"),
            meta: { title: "开店", menu: true },
        },
    ],
};

export default section;
