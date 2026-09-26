import { OfficeBuilding } from "@element-plus/icons-vue";
import type { AdminSection } from "../section.ts";

// 门店。**位置留着，页面是一句实话。**
//
// 多门店那条线（6 张表 + 23 条接口）在另一条分支上，还没合进 main，
// 也就还没进 docs/电商系统-OpenAPI.yaml、还没进 web/src/api/schema.d.ts。
// 而这个后台的第一条纪律是「界面里不许手写任何一个请求 / 响应类型」——
// 契约里没有的东西，这里就一个字段都写不出来，那正是这条纪律该有的样子。
//
// 合进来之后：把下面 component 换成真的列表页，路由与菜单不用动。
const section: AdminSection = {
    key: "stores",
    title: "门店",
    icon: OfficeBuilding,
    order: 40,
    routes: [
        {
            path: "stores",
            name: "stores",
            component: () => import("../../views/PlaceholderView.vue"),
            meta: { title: "门店", menu: true },
            props: {
                title: "门店这一块还没接上",
                reason: "多门店 + 大区正在另一条分支上落地（6 张表 + 23 条接口），契约还没合进 main。契约里没有的接口，这个后台就不画——手写一份请求体类型能让页面看起来能用，但它和服务端之间从此只剩人的注意力在维系。",
                upcoming: ["门店列表与开关店", "门店库存（与商品 SKU 的库存分开）", "门店与大区的归属关系"],
            },
        },
    ],
};

export default section;
