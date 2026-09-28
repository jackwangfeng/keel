import { test } from "node:test";
import assert from "node:assert/strict";
import { PAYMENT_RETURN_REASON, PAYMENT_RETURN_STATUS, isStuck } from "./paymentReturnRules.ts";

test("多收款退回：原因与状态的文字、提交失败判定", () => {
    assert.equal(PAYMENT_RETURN_REASON[1], "重复支付");
    assert.equal(PAYMENT_RETURN_STATUS[40].text, "已退回");
    assert.equal(isStuck({ status: 10, last_error: "没配密钥" }), true);
    assert.equal(isStuck({ status: 10 }), false);
    assert.equal(isStuck({ status: 40, last_error: "x" }), false);
});
