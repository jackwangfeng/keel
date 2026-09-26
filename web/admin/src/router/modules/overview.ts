import { DataAnalysis } from "@element-plus/icons-vue";
import type { AdminSection } from "../section.ts";

// 经营概览是后台首页（src/router/index.ts 的 "" 重定向到这里）。四种角色都看得见：
// 数字由服务端按角色收窄（契约 StaffRole 矩阵「经营报表」那两行）。
const section: AdminSection = {
    key: "overview",
    title: "经营概览",
    icon: DataAnalysis,
    order: 0,
    routes: [
        {
            path: "overview",
            name: "overview",
            component: () => import("../../views/overview/OverviewView.vue"),
            meta: { title: "经营概览", menu: true },
        },
    ],
};

export default section;
