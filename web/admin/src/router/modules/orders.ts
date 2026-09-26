import { Tickets } from "@element-plus/icons-vue";
import type { AdminSection } from "../section.ts";

const section: AdminSection = {
    key: "orders",
    title: "订单",
    icon: Tickets,
    order: 30,
    routes: [
        {
            path: "orders",
            name: "orders",
            component: () => import("../../views/OrderOpsView.vue"),
            meta: { title: "订单", menu: true },
        },
    ],
};

export default section;
