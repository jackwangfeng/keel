import { MapLocation } from "@element-plus/icons-vue";
import type { AdminSection } from "../section.ts";

// 大区。理由与门店那一条一字不差，见 stores.ts。
const section: AdminSection = {
    key: "regions",
    title: "大区",
    icon: MapLocation,
    order: 50,
    routes: [
        {
            path: "regions",
            name: "regions",
            component: () => import("../../views/PlaceholderView.vue"),
            meta: { title: "大区", menu: true },
            props: {
                title: "大区这一块还没接上",
                reason: "与门店同一条分支，契约还没合进 main。",
                upcoming: ["大区树", "大区下的门店", "按大区看的经营数据"],
            },
        },
    ],
};

export default section;
