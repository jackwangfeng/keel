import type { NavigationGuardWithThis } from "vue-router";
import { Connection } from "@element-plus/icons-vue";
import type { AdminSection } from "../section.ts";
import { channelSectionAvailable } from "../../api/channels.ts";

// 渠道（Shopify / 美团……）：商家接的渠道账号、凭据、门店映射、库存与价格规则、推送状态。
// 渠道层由 KEEL_CHANNELS 开关；关着时 GET /admin/channel-kinds 是 404，菜单整块不出现。
// 紧挨着多收款退回（33），留出间隔。

// 菜单会按 available 把这个分区整块藏起来，但菜单藏了不等于路由锁了——直接敲
// /channels 或 /channels/:id 照样能进来，这时才会打到后端发现 404 / 403。等探测
// 结果：不可用（开关关着，或角色看不见这个分区）就跳回首页，总比弹一个「页面
// 不存在」或一个没头没尾的接口错误更说得清楚。
const guardChannelsRoute: NavigationGuardWithThis<undefined> = async () => {
    const ok = await channelSectionAvailable();
    return ok ? true : { name: "overview" };
};

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
            beforeEnter: guardChannelsRoute,
        },
        {
            path: "channels/:bindingId(\\d+)",
            name: "channel-detail",
            component: () => import("../../views/channels/ChannelDetailView.vue"),
            meta: { title: "渠道账号" },
            props: true,
            beforeEnter: guardChannelsRoute,
        },
    ],
};

export default section;
