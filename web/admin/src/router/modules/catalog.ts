import { Goods } from "@element-plus/icons-vue";
import type { AdminSection } from "../section.ts";

const section: AdminSection = {
    key: "catalog",
    title: "商品",
    icon: Goods,
    order: 10,
    routes: [
        {
            path: "products",
            name: "products",
            component: () => import("../../views/ProductListView.vue"),
            meta: { title: "商品", menu: true },
        },
        {
            // 详情页有路由没菜单：它是从列表点进来的。
            path: "products/:productId(\\d+)",
            name: "product-detail",
            component: () => import("../../views/ProductDetailView.vue"),
            meta: { title: "商品详情" },
            props: true,
        },
    ],
};

export default section;
