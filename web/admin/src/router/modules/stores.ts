import { OfficeBuilding } from "@element-plus/icons-vue";
import type { AdminSection } from "../section.ts";

const section: AdminSection = {
    key: "stores",
    title: "门店",
    icon: OfficeBuilding,
    order: 50,
    routes: [
        {
            path: "stores",
            name: "stores",
            component: () => import("../../views/StoreListView.vue"),
            meta: { title: "门店", menu: true },
        },
        {
            path: "stores/:storeId(\\d+)",
            name: "store-detail",
            component: () => import("../../views/StoreDetailView.vue"),
            meta: { title: "门店" },
            props: true,
        },
    ],
};

export default section;
