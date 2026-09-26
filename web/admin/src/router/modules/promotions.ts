import { Present } from "@element-plus/icons-vue";
import type { AdminSection } from "../section.ts";

// 营销活动（数据模型 §7·二）：满减、满折、限时折扣、秒杀、新人礼。页面与对话框在 views/promotions/ 下。
const section: AdminSection = {
    key: "promotions",
    title: "营销活动",
    icon: Present,
    order: 36,
    routes: [
        {
            path: "promotions",
            name: "promotions",
            component: () => import("../../views/promotions/PromotionListView.vue"),
            meta: { title: "营销活动", menu: true },
        },
    ],
};

export default section;
