import { RefreshLeft } from "@element-plus/icons-vue";
import type { AdminSection } from "../section.ts";

// 售后：退款单列表与详情（审核、确认收到退货）。紧挨着订单（30）之后、优惠券（35）之前。
const section: AdminSection = {
    key: "refunds",
    title: "售后",
    icon: RefreshLeft,
    order: 32,
    routes: [
        {
            path: "refunds",
            name: "refunds",
            component: () => import("../../views/refunds/RefundListView.vue"),
            meta: { title: "售后", menu: true },
        },
    ],
};

export default section;
