// 顶栏铃铛的界面规则。`node --test src/api/notifications.test.ts` 直接跑，不要 node_modules。
//
// 守的是「点下去跳错了地方」：订单提醒跳到售后页、库存提醒丢了门店、定位缺失时跳到一个空页面。
import { test } from "node:test";
import assert from "node:assert/strict";
import { badgeText, notificationLocation, type Notification } from "./notifications.ts";

type Target = Notification["target"];

function target(t: Partial<Target> & Pick<Target, "type">): Pick<Notification, "target"> {
    return { target: { order_no: null, refund_no: null, store_id: null, sku_id: null, ...t } };
}

test("订单提醒跳订单页并打开那一单", () => {
    assert.deepEqual(notificationLocation(target({ type: "order", order_no: "O123" })), {
        path: "/orders",
        query: { order_no: "O123" },
    });
});

test("售后提醒跳售后页并打开那一张（即使同时带着订单号）", () => {
    assert.deepEqual(notificationLocation(target({ type: "refund", refund_no: "R9", order_no: "O1" })), {
        path: "/refunds",
        query: { refund_no: "R9" },
    });
});

test("库存提醒跳那家门店的库存页签", () => {
    assert.deepEqual(notificationLocation(target({ type: "inventory", store_id: 7, sku_id: 3 })), {
        path: "/stores/7",
        query: { tab: "inventory" },
    });
});

test("定位字段缺了就不跳（只标已读），而不是跳到一个空页面", () => {
    assert.equal(notificationLocation(target({ type: "order" })), null);
    assert.equal(notificationLocation(target({ type: "refund", order_no: "O1" })), null);
    assert.equal(notificationLocation(target({ type: "inventory", sku_id: 3 })), null);
});

test("角标：0 不显示，超过 99 写 99+", () => {
    assert.equal(badgeText(0), "");
    assert.equal(badgeText(-1), "");
    assert.equal(badgeText(5), "5");
    assert.equal(badgeText(99), "99");
    assert.equal(badgeText(100), "99+");
});
