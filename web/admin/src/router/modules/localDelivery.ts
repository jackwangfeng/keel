import { Position } from "@element-plus/icons-vue";
import type { AdminSection } from "../section.ts";

// 同城配送模板（00111）。挨着运费模板（36）：两者都是「一行商品 / 一家门店的配送费
// 怎么算」，只是同城配送按距离分档、不按省份区分。
const section: AdminSection = {
    key: "local-delivery-templates",
    title: "同城配送模板",
    icon: Position,
    order: 37,
    routes: [
        {
            path: "local-delivery-templates",
            name: "local-delivery-templates",
            component: () => import("../../views/localDelivery/LocalDeliveryTemplateListView.vue"),
            meta: { title: "同城配送模板", menu: true },
        },
    ],
};

export default section;
