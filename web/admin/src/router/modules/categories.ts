import { Files } from "@element-plus/icons-vue";
import type { AdminSection } from "../section.ts";

const section: AdminSection = {
    key: "categories",
    title: "类目",
    icon: Files,
    order: 20,
    routes: [
        {
            path: "categories",
            name: "categories",
            component: () => import("../../views/CategoryListView.vue"),
            meta: { title: "类目", menu: true },
        },
    ],
};

export default section;
