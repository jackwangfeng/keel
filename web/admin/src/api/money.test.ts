// 元 ↔ 分换算的单元测试。`node --test src/api/money.test.ts` 直接跑，不要 node_modules。
import { test } from "node:test";
import assert from "node:assert/strict";
import { centsToYuanInput, rateToZheInput, yuanToCents, zheToRate } from "./money.ts";

test("元转分：没有浮点误差", () => {
    // 这几个值用 Number(x) * 100 都会得出非整数。
    assert.equal(yuanToCents("0.29"), 29);
    assert.equal(yuanToCents("0.57"), 57);
    assert.equal(yuanToCents("1.13"), 113);
    assert.equal(yuanToCents("19.99"), 1999);
    assert.equal(yuanToCents("4.35"), 435);
});

test("元转分：各种合法写法", () => {
    assert.equal(yuanToCents("100"), 10000);
    assert.equal(yuanToCents("100."), 10000);
    assert.equal(yuanToCents("12.3"), 1230);
    assert.equal(yuanToCents(" 0.05 "), 5);
    assert.equal(yuanToCents("¥20"), 2000);
    assert.equal(yuanToCents("0"), 0);
    assert.equal(yuanToCents("007.10"), 710);
});

test("元转分：不合法的一律 null，不截断不四舍五入", () => {
    for (const bad of ["", "abc", "-1", "1.234", "1,000", "1e3", ".5", "12.3.4", "99999999999999"]) {
        assert.equal(yuanToCents(bad), null, bad);
    }
});

test("分转元回显，与元转分互逆", () => {
    assert.equal(centsToYuanInput(10000), "100");
    assert.equal(centsToYuanInput(1230), "12.30");
    assert.equal(centsToYuanInput(5), "0.05");
    assert.equal(centsToYuanInput(-1), "");
    for (const c of [0, 1, 29, 57, 113, 1999, 123456789]) {
        assert.equal(yuanToCents(centsToYuanInput(c)), c);
    }
});

test("折 ↔ 千分比", () => {
    assert.equal(zheToRate("8.5"), 850);
    assert.equal(zheToRate("8"), 800);
    assert.equal(zheToRate("9.95"), 995);
    assert.equal(zheToRate("0.1"), 10);
    assert.equal(zheToRate("0"), null);
    assert.equal(zheToRate("10"), null);
    assert.equal(zheToRate("8.555"), null);
    assert.equal(rateToZheInput(850), "8.5");
    assert.equal(rateToZheInput(800), "8");
    assert.equal(rateToZheInput(995), "9.95");
    assert.equal(rateToZheInput(10), "0.1");
    for (const r of [1, 10, 99, 500, 850, 999]) assert.equal(zheToRate(rateToZheInput(r)), r);
});

test("折扣标签", async () => {
    const { rateLabel } = await import("./money.ts");
    assert.equal(rateLabel(850), "8.5 折");
    assert.equal(rateLabel(995), "9.95 折");
    assert.equal(rateLabel(0), "—");
});
