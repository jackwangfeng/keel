import { Ticket } from "@element-plus/icons-vue";
import type { AdminSection } from "../section.ts";

// 券管理（数据模型 §7）。页面与对话框都在 views/coupons/ 下，一个独立模块。
const section: AdminSection = {
    key: "coupons",
    title: "优惠券",
    icon: Ticket,
    order: 35,
    routes: [
        {
            path: "coupons",
            name: "coupons",
            component: () => import("../../views/coupons/CouponListView.vue"),
            meta: { title: "优惠券", menu: true },
        },
    ],
};

export default section;
