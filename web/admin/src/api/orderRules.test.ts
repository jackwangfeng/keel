// 订单与售后页的界面规则。`node --test src/api/orderRules.test.ts` 直接跑，不要 node_modules。
//
// 守的是「点下去才发现」那一类：按钮亮在不该亮的状态上、仅退款的审核把运费带进请求体
// （服务端 422）、驳回没带理由、日期范围差一天。服务端是权威，但这些错在界面上是
// 可以提前挡住的。
import { test } from "node:test";
import assert from "node:assert/strict";
import {
    ORDER_STATUS,
    adminUploadPath,
    REFUND_STATUS,
    buildAudit,
    buildShipment,
    dayRange,
    orderQuery,
    refundActions,
    refundQuery,
    refundableQty,
    shipAction,
} from "./orderRules.ts";

test("发货按钮只在 20 已支付出现；没权限时置灰而不是藏起来", () => {
    for (const status of [10, 30, 40, 50, 60, 90] as const) {
        assert.equal(shipAction({ status, has_open_refund: false }, true).visible, false, `status ${status}`);
    }
    const ok = shipAction({ status: 20, has_open_refund: false }, true);
    assert.deepEqual([ok.visible, ok.enabled, ok.hint], [true, true, ""]);

    const denied = shipAction({ status: 20, has_open_refund: false }, false);
    assert.equal(denied.visible, true);
    assert.equal(denied.enabled, false);
    assert.match(denied.hint, /服务端同样会拒绝/);
});

test("有在途售后的已支付订单：发货仍可点（部分退款不阻断），但挂一句提醒", () => {
    const s = shipAction({ status: 20, has_open_refund: true }, true);
    assert.equal(s.enabled, true);
    assert.match(s.hint, /售后/);
});

test("退款单：10 只能审（同意 / 驳回），20 只能确认收到退货，其余状态一个按钮都没有", () => {
    const pending = refundActions({ status: 10, refund_type: 1 }, true);
    assert.deepEqual(
        [pending.approve.enabled, pending.reject.enabled, pending.receive.visible, pending.freightEditable],
        [true, true, false, false],
    );

    const pendingReturn = refundActions({ status: 10, refund_type: 2 }, true);
    assert.equal(pendingReturn.freightEditable, true, "退货退款审核时可以裁定运费");

    const awaiting = refundActions({ status: 20, refund_type: 2 }, true);
    assert.deepEqual(
        [awaiting.approve.visible, awaiting.reject.visible, awaiting.receive.enabled, awaiting.freightEditable],
        [false, false, true, false],
    );

    for (const status of [30, 40, 50, 60] as const) {
        const a = refundActions({ status, refund_type: 2 }, true);
        assert.deepEqual([a.approve.visible, a.reject.visible, a.receive.visible], [false, false, false], `status ${status}`);
    }

    const denied = refundActions({ status: 10, refund_type: 2 }, false);
    assert.deepEqual([denied.approve.visible, denied.approve.enabled, denied.reject.enabled], [true, false, false]);
});

test("审核请求体：驳回必须带理由；同意时运费只随退货退款带上，按元换成分", () => {
    assert.deepEqual(buildAudit("reject", 1, { rejectReason: "  " }), { error: "驳回必须填写理由，买家会看到它" });
    assert.deepEqual(buildAudit("reject", 1, { rejectReason: " 凭证不清晰 " }), {
        body: { action: "reject", reject_reason: "凭证不清晰" },
    });
    assert.ok("error" in buildAudit("reject", 1, { rejectReason: "字".repeat(201) }));

    // 仅退款：运费按规则算，就算表单里有值也不带（带一个不同的值服务端回 422）。
    assert.deepEqual(buildAudit("approve", 1, { freightYuan: "10" }), { body: { action: "approve" } });
    // 退货退款：留空 = 保持申请时的值（省略字段）。
    assert.deepEqual(buildAudit("approve", 2, { freightYuan: "" }), { body: { action: "approve" } });
    // 没有浮点误差：0.29 元是 29 分。
    assert.deepEqual(buildAudit("approve", 2, { freightYuan: "0.29" }), {
        body: { action: "approve", freight_cents: 29 },
    });
    assert.deepEqual(buildAudit("approve", 2, { freightYuan: "12" }), {
        body: { action: "approve", freight_cents: 1200 },
    });
    for (const bad of ["-1", "1.234", "abc"]) {
        assert.ok("error" in buildAudit("approve", 2, { freightYuan: bad }), bad);
    }
});

test("发货请求体：两个字段都要填，去掉首尾空白", () => {
    assert.deepEqual(buildShipment(" sf ", " SF123 "), { body: { carrier_code: "sf", tracking_no: "SF123" } });
    assert.ok("error" in buildShipment("", "SF1"));
    assert.ok("error" in buildShipment("sf", "  "));
    assert.ok("error" in buildShipment("sf", "x".repeat(65)));
});

test("日期范围 → 半开区间：结束日的次日本地零点，不是 23:59:59", () => {
    const r = dayRange(["2026-01-15", "2026-01-15"]);
    assert.equal(r.created_from, new Date(2026, 0, 15).toISOString());
    assert.equal(r.created_to, new Date(2026, 0, 16).toISOString());
    // 跨月
    assert.equal(dayRange(["2026-01-31", "2026-01-31"]).created_to, new Date(2026, 1, 1).toISOString());
    assert.deepEqual(dayRange(null), {});
    assert.deepEqual(dayRange(["2026-1-5", "2026-01-06"]), {});
});

test("筛选 → query：没填的字段不出现，单号与手机号去掉空白", () => {
    assert.deepEqual(orderQuery({}, 1, 20), { page: 1, page_size: 20 });
    const q = orderQuery({ status: 20, storeId: 7, orderNo: "  NO1 ", phone: " ", dateRange: null }, 2, 50);
    assert.deepEqual(q, { page: 2, page_size: 50, status: 20, store_id: 7, order_no: "NO1" });
    assert.deepEqual(refundQuery({ status: 10 }, 1, 20), { page: 1, page_size: 20, status: 10 });
});

test("还可退件数 = 购买 − 已退 − 在途，不为负", () => {
    assert.equal(refundableQty({ quantity: 3, refunded_qty: 1, refunding_qty: 1 }), 1);
    assert.equal(refundableQty({ quantity: 2 }), 2);
    assert.equal(refundableQty({ quantity: 1, refunded_qty: 1, refunding_qty: 1 }), 0);
});

test("状态表覆盖契约的全部取值（Record 的键就是枚举）", () => {
    assert.deepEqual(Object.keys(ORDER_STATUS).map(Number), [10, 20, 30, 40, 50, 60, 90]);
    assert.deepEqual(Object.keys(REFUND_STATUS).map(Number), [10, 20, 30, 40, 50, 60]);
});

test("退款凭证地址 → 后台读文件的路径；形状不对的原样放过（null）", () => {
    assert.equal(adminUploadPath("/api/v1/uploads/42"), "/admin/uploads/42");
    for (const bad of ["https://example.com/x.png", "/api/v1/uploads/42?x=1", "/api/v1/uploads/0", "/api/v1/uploads/", ""]) {
        assert.equal(adminUploadPath(bad), null, bad);
    }
});
