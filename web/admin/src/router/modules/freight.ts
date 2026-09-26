import { Van } from "@element-plus/icons-vue";
import type { AdminSection } from "../section.ts";

// 运费模板（数据模型 §7「运费模板」）。挨着优惠券（35）：两者一起决定买家实付。
const section: AdminSection = {
    key: "freight",
    title: "运费模板",
    icon: Van,
    order: 36,
    routes: [
        {
            path: "freight-templates",
            name: "freight-templates",
            component: () => import("../../views/freight/FreightTemplateListView.vue"),
            meta: { title: "运费模板", menu: true },
        },
    ],
};

export default section;
