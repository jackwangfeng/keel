// 全部菜单分区。**加一块 = 加一个模块文件 + 这里一行。**
//
// 刻意不用 `import.meta.glob` 自动收集：那样加一块确实只要加一个文件，
// 但「后台一共有哪几块」就从代码里消失了，而这正是读者第一眼想知道的事。
// 一行 import 的代价换一份看得见的清单，划算。

import type { AdminSection } from "../section.ts";
import agents from "./agents.ts";
import catalog from "./catalog.ts";
import categories from "./categories.ts";
import coupons from "./coupons.ts";
import freight from "./freight.ts";
import localDelivery from "./localDelivery.ts";
import orders from "./orders.ts";
import overview from "./overview.ts";
import promotions from "./promotions.ts";
import refunds from "./refunds.ts";
import regions from "./regions.ts";
import staff from "./staff.ts";
import stores from "./stores.ts";
import merchants from "./merchants.ts";
import shopSettings from "./shopSettings.ts";

export const sections: AdminSection[] = [overview, catalog, categories, coupons, promotions, freight, localDelivery, orders, refunds, regions, stores, staff, agents, shopSettings, merchants].sort(
    (a, b) => a.order - b.order,
);
