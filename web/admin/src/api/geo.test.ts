/// <reference types="node" />
// 围栏几何的测试。`make admin-test`（node --test，Node 24 直接跑 .ts），已接进 check-all.sh。
//
// 这些测试守的是「偏了不会报错」的那一类错：坐标序写反、坐标系没换、
// 环没闭合。每一种在服务端都会得到一个**合法**的多边形，只是位置错了。

import { test } from "node:test";
import assert from "node:assert/strict";
import {
    bd09ToGcj02,
    closeRing,
    distanceMeters,
    gcj02ToBd09,
    gcj02ToWgs84,
    looksSwappedForChina,
    openRing,
    parseFenceInput,
    polygonFromVertices,
    rangeProblem,
    toLatLng,
    toPosition,
    toWgs84,
    verticesFromPolygon,
    wgs84ToGcj02,
    type Position,
} from "./geo.ts";

const BEIJING: Position = [116.3914, 39.9042]; // 天安门附近，WGS-84
const SHANGHAI: Position = [121.4737, 31.2304];
const GUANGZHOU: Position = [113.2644, 23.1291];
const LONDON: Position = [-0.1276, 51.5072];

test("Leaflet (lat,lng) 与 GeoJSON [lng,lat] 的换序：两个方向互逆，且确实换了序", () => {
    const pos = toPosition({ lat: 39.9, lng: 116.4 });
    assert.deepEqual(pos, [116.4, 39.9]);
    assert.deepEqual(toLatLng(pos), { lat: 39.9, lng: 116.4 });
});

test("closeRing 补上闭合点，已闭合的不重复补；openRing 反过来", () => {
    const ring: Position[] = [[0, 0], [1, 0], [1, 1]];
    const closed = closeRing(ring);
    assert.equal(closed.length, 4);
    assert.deepEqual(closed[3], [0, 0]);
    assert.equal(closeRing(closed).length, 4);
    assert.deepEqual(openRing(closed), ring);
});

test("polygonFromVertices：闭合、至少 3 个不同顶点、越界拒绝", () => {
    const poly = polygonFromVertices([[116.3, 39.85], [116.5, 39.85], [116.5, 39.95]]);
    assert.equal(poly.type, "Polygon");
    assert.equal(poly.coordinates[0]?.length, 4);
    assert.throws(() => polygonFromVertices([[116.3, 39.85], [116.3, 39.85], [116.5, 39.95]]), /至少要 3 个/);
    assert.throws(() => polygonFromVertices([[39.9, 116.4], [39.95, 116.4], [39.95, 116.5]]), /纬度 116.4 超出/);
    assert.deepEqual(verticesFromPolygon(poly), [[116.3, 39.85], [116.5, 39.85], [116.5, 39.95]]);
});

test("rangeProblem 抓得住越界的纬度，并说出「多半是写反了」", () => {
    assert.equal(rangeProblem([[116.4, 39.9]]), null);
    assert.match(rangeProblem([[39.9, 116.4]]) ?? "", /写反/);
});

test("looksSwappedForChina：两个值都不越界的写反也能提示", () => {
    assert.equal(looksSwappedForChina([BEIJING, SHANGHAI]), false);
    assert.equal(looksSwappedForChina([[39.9, 116.4], [31.2, 121.5]]), true);
});

test("GCJ-02 在国内城区的偏移是一两百到上千米的量级——这就是不换算会把买家判错店的距离", () => {
    for (const city of [BEIJING, SHANGHAI, GUANGZHOU]) {
        const shift = distanceMeters(city, wgs84ToGcj02(city));
        assert.ok(shift > 100 && shift < 1000, `偏移 ${shift.toFixed(0)} 米不在预期量级`);
    }
    // 方向：北京一带 GCJ-02 相对 WGS-84 往东偏约 0.006 度、往北偏约 0.0014 度。
    const g = wgs84ToGcj02(BEIJING);
    assert.ok(g[0] - BEIJING[0] > 0.004 && g[0] - BEIJING[0] < 0.008, `经度偏移 ${g[0] - BEIJING[0]}`);
    assert.ok(g[1] - BEIJING[1] > 0.0005 && g[1] - BEIJING[1] < 0.0025, `纬度偏移 ${g[1] - BEIJING[1]}`);
});

test("GCJ-02 → WGS-84 的迭代逆变换：往返误差小于 1 厘米", () => {
    for (const city of [BEIJING, SHANGHAI, GUANGZHOU]) {
        const back = gcj02ToWgs84(wgs84ToGcj02(city));
        assert.ok(distanceMeters(city, back) < 0.01, `往返误差 ${distanceMeters(city, back)} 米`);
    }
});

test("境外不加偏移：GCJ-02 = WGS-84", () => {
    assert.deepEqual(wgs84ToGcj02(LONDON), LONDON);
    assert.deepEqual(gcj02ToWgs84(LONDON), LONDON);
});

test("BD-09 → WGS-84：往返误差小于 10 厘米，且比 GCJ-02 偏得更多", () => {
    // BD-09 公开的那对换算本身就不是严格互逆的（实测往返约 5 厘米），
    // 所以这里的阈值比 GCJ-02 那条宽。对一个围栏来说 5 厘米无关紧要；
    // 要紧的是下面那条：不换算会偏一公里以上。
    const gcj = wgs84ToGcj02(BEIJING);
    const bd = gcj02ToBd09(gcj);
    assert.ok(distanceMeters(bd09ToGcj02(bd), gcj) < 0.1);
    assert.ok(distanceMeters(toWgs84(bd, "bd09"), BEIJING) < 0.1);
    assert.ok(distanceMeters(BEIJING, bd) > distanceMeters(BEIJING, gcj));
});

test("toWgs84(·, wgs84) 原样返回——默认不换算，不猜", () => {
    assert.deepEqual(toWgs84(BEIJING, "wgs84"), BEIJING);
});

test("parseFenceInput：GeoJSON Polygon / Feature / FeatureCollection / 裸数组 / 逐行文本都认", () => {
    const ring = [[116.3, 39.85], [116.5, 39.85], [116.5, 39.95], [116.3, 39.95], [116.3, 39.85]];
    const inputs = [
        JSON.stringify({ type: "Polygon", coordinates: [ring] }),
        JSON.stringify({ type: "Feature", properties: {}, geometry: { type: "Polygon", coordinates: [ring] } }),
        JSON.stringify({ type: "FeatureCollection", features: [{ type: "Feature", geometry: { type: "Polygon", coordinates: [ring] } }] }),
        JSON.stringify(ring),
        "116.3,39.85\n116.5 39.85\n116.5，39.95\n# 注释行\n116.3,39.95",
    ];
    for (const text of inputs) {
        const r = parseFenceInput(text, "wgs84");
        assert.ok(r.ok, r.ok ? "" : r.error);
        if (!r.ok) continue;
        assert.equal(r.polygon.coordinates[0]?.length, 5, text);
        assert.deepEqual(r.polygon.coordinates[0]?.[0], [116.3, 39.85]);
        assert.equal(r.shiftedMeters, 0);
    }
});

test("parseFenceInput：拒绝的都说得出为什么", () => {
    const bad: [string, RegExp][] = [
        ["", /什么都没粘贴/],
        ["{not json", /JSON 解析失败/],
        [JSON.stringify({ type: "MultiPolygon", coordinates: [] }), /MultiPolygon/],
        [JSON.stringify({ type: "Point", coordinates: [1, 2] }), /Polygon/],
        ["116.3,39.85\n116.5", /第 2 行/],
        ["39.9,116.4\n39.95,116.4\n39.95,116.5", /写反/],
        ["116.3,39.85\n116.5,39.85", /至少要 3 个/],
    ];
    for (const [text, re] of bad) {
        const r = parseFenceInput(text, "wgs84");
        assert.equal(r.ok, false, text);
        if (!r.ok) assert.match(r.error, re, text);
    }
});

test("parseFenceInput：声明是 GCJ-02 时换回 WGS-84，并报出挪了多少米", () => {
    const wgsRing: Position[] = [BEIJING, [116.40, 39.9042], [116.40, 39.91]];
    const gcjText = wgsRing.map((p) => wgs84ToGcj02(p).join(",")).join("\n");
    const r = parseFenceInput(gcjText, "gcj02");
    assert.ok(r.ok);
    if (!r.ok) return;
    assert.ok(r.shiftedMeters > 100, `挪了 ${r.shiftedMeters} 米`);
    const got = r.polygon.coordinates[0]?.[0] as Position;
    assert.ok(distanceMeters(got, BEIJING) < 0.01);
});

test("parseFenceInput：中国境内两值都不越界的写反，给警告但不拒绝", () => {
    // 喀什一带经度 < 90，写反之后纬度仍 ≤ 90，范围校验抓不到，只能靠启发提示。
    const swapped = "39.47,75.99\n39.50,75.99\n39.50,76.02";
    const r = parseFenceInput(swapped, "wgs84");
    assert.ok(r.ok);
    if (r.ok) assert.match(r.warnings.join(""), /写反/);
    // 正着写的同一块地方不该误报。
    const straight = parseFenceInput("75.99,39.47\n75.99,39.50\n76.02,39.50", "wgs84");
    assert.ok(straight.ok);
    if (straight.ok) assert.equal(straight.warnings.length, 0);
});
