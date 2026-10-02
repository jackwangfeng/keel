import { Connection } from "@element-plus/icons-vue";
import type { AdminSection } from "../section.ts";
import { channelSectionAvailable } from "../../api/channels.ts";

// 渠道（Shopify / 美团……）：商家接的渠道账号、凭据、门店映射、库存与价格规则、推送状态。
// 渠道层由 KEEL_CHANNELS 开关；关着时 GET /admin/channel-kinds 是 404，菜单整块不出现。
// 紧挨着多收款退回（33），留出间隔。
const section: AdminSection = {
    key: "channels",
    title: "渠道",
    icon: Connection,
    order: 36,
    available: channelSectionAvailable,
    routes: [
        {
            path: "channels",
            name: "channels",
            component: () => import("../../views/channels/ChannelListView.vue"),
            meta: { title: "渠道", menu: true },
        },
        {
            path: "channels/:bindingId(\\d+)",
            name: "channel-detail",
            component: () => import("../../views/channels/ChannelDetailView.vue"),
            meta: { title: "渠道账号" },
            props: true,
        },
    ],
};

export default section;
