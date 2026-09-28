import { Money } from "@element-plus/icons-vue";
import type { AdminSection } from "../section.ts";

// 多收款退回（00150）：订单不认的到账（重复支付、取消后才到、金额不符）的原路退回单。紧挨着售后（32）。
const section: AdminSection = {
    key: "payment-returns",
    title: "多收款退回",
    icon: Money,
    order: 33,
    routes: [
        {
            path: "payment-returns",
            name: "payment-returns",
            component: () => import("../../views/paymentReturns/PaymentReturnListView.vue"),
            meta: { title: "多收款退回", menu: true },
        },
    ],
};

export default section;
