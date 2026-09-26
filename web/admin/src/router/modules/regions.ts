import { MapLocation } from "@element-plus/icons-vue";
import type { AdminSection } from "../section.ts";

// 大区排在门店前面：门店必须属于一个大区（stores.region_id NOT NULL），
// 操作顺序就是先建大区、再建门店。
const section: AdminSection = {
    key: "regions",
    title: "大区",
    icon: MapLocation,
    order: 40,
    routes: [
        {
            path: "regions",
            name: "regions",
            component: () => import("../../views/RegionListView.vue"),
            meta: { title: "大区", menu: true },
        },
        {
            path: "regions/:regionId(\\d+)",
            name: "region-detail",
            component: () => import("../../views/RegionDetailView.vue"),
            meta: { title: "大区商品与定价" },
            props: true,
        },
    ],
};

export default section;
