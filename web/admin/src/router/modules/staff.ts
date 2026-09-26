import { User } from "@element-plus/icons-vue";
import type { AdminSection } from "../section.ts";

const section: AdminSection = {
    key: "staff",
    title: "员工",
    icon: User,
    order: 60,
    routes: [
        {
            path: "staff",
            name: "staff",
            component: () => import("../../views/StaffListView.vue"),
            meta: { title: "员工", menu: true },
        },
    ],
};

export default section;
