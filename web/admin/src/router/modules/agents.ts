import { MagicStick } from "@element-plus/icons-vue";
import type { AdminSection } from "../section.ts";

// AI 员工（AI 经营 M9）：提案 / 简报 / 员工与密钥，三个 tab 在一个视图里
// （src/views/agents/AgentStaffView.vue），按 el-tabs 切换，不拆成三条路由——
// 与门店详情页（basic/fence/products/inventory 四个 tab）同一个做法。
const section: AdminSection = {
    key: "agents",
    title: "AI 员工",
    icon: MagicStick,
    order: 62,
    routes: [
        {
            path: "agents",
            name: "agents",
            component: () => import("../../views/agents/AgentStaffView.vue"),
            meta: { title: "AI 员工", menu: true },
        },
    ],
};

export default section;
