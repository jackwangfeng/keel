// 电子围栏的几何：坐标序、闭合、坐标系。**这个文件里的每一条规则都有测试**
// （geo.test.ts，`make admin-test`，已接进 check-all.sh）。
//
// ## 坐标系：库里是 WGS-84，而且只能是 WGS-84
//
// 围栏落库成 `GEOGRAPHY(POLYGON, 4326)`，4326 就是 WGS-84。买家端按 `wgs84`
// 取定位（app/src/api/store.uts 文件头）。国内地图给的不是它：
//
//   · 高德 / 腾讯：GCJ-02（「火星坐标」），在 WGS-84 上加了一个非线性偏移
//   · 百度：BD-09，在 GCJ-02 上再偏一次
//
// 城区偏一两百到五六百米（geo.test.ts 里实测北京约 600 米），足够把买家判进
// 错的门店，而且**不报错**：偏过的多边形照样合法，ST_Intersects 照样返回
// 一个答案，只是答案是错的。
//
// 所以后台的地图是 Leaflet + OpenStreetMap 瓦片：OSM 本身就是 WGS-84，
// 在地图上点出来的顶点**原样**就是要存的值，编辑器那条路上没有任何换算。
// 换算只出现在一个地方：「粘贴坐标」入口里，运营**显式**声明「我这批坐标
// 是从高德 / 百度抄的」时，才把它换回 WGS-84。默认值是 WGS-84，不猜。
//
// ## 坐标序：GeoJSON 是 [经度, 纬度]，Leaflet 是 (纬度, 经度)
//
// 两个方向相反，写反了不会报错——得到的是地球另一侧的一个合法多边形。
// 全后台只有 toPosition / toLatLng 这两个函数做这件事，别处一律经过它们。
//
// 这个文件**不依赖任何运行时模块**（只有 import type），于是测试可以用
// `node --test` 直接跑 .ts（Node 24 的类型剥离），不需要再装一个测试框架。

import type { GeoPolygon } from "./client.ts";

/** GeoJSON 的一个位置：**[经度, 纬度]**。 */
export type Position = [lng: number, lat: number];

/** Leaflet 那一侧的形状。 */
export interface LatLngLike {
    lat: number;
    lng: number;
}

/** Leaflet (lat, lng) → GeoJSON [lng, lat]。全后台唯一的换序点之一。 */
export function toPosition(p: LatLngLike): Position {
    return [p.lng, p.lat];
}

/** GeoJSON [lng, lat] → Leaflet (lat, lng)。全后台唯一的换序点之二。 */
export function toLatLng(pos: Position): LatLngLike {
    return { lat: pos[1], lng: pos[0] };
}

function samePoint(a: Position, b: Position): boolean {
    return a[0] === b[0] && a[1] === b[1];
}

/** 闭合一个环：首尾不同就把首点补到尾上。已闭合的原样返回（不重复补）。 */
export function closeRing(ring: readonly Position[]): Position[] {
    const out = ring.map((p): Position => [p[0], p[1]]);
    const first = out[0];
    const last = out[out.length - 1];
    if (first !== undefined && last !== undefined && !samePoint(first, last)) out.push([first[0], first[1]]);
    return out;
}

/** 环的顶点（去掉闭合用的那个重复尾点）。给编辑器回显用。 */
export function openRing(ring: readonly Position[]): Position[] {
    const out = ring.map((p): Position => [p[0], p[1]]);
    const first = out[0];
    const last = out[out.length - 1];
    if (out.length > 1 && first !== undefined && last !== undefined && samePoint(first, last)) out.pop();
    return out;
}

/**
 * 顶点 → 契约的 GeoPolygon。
 *
 * 这里**只做形状上的事**（闭合、至少 3 个不同顶点、坐标在范围内），
 * 不做自交判断：自交由服务端的 ST_IsValid 判，它的 ST_IsValidReason
 * 比前端能写的任何一句都准。前端再实现一遍，两边的判据迟早分叉。
 */
export function polygonFromVertices(vertices: readonly Position[]): GeoPolygon {
    const problem = rangeProblem(vertices);
    if (problem !== null) throw new Error(problem);
    const distinct = new Set(vertices.map((p) => `${p[0]},${p[1]}`));
    if (distinct.size < 3) throw new Error(`至少要 3 个不同的顶点，现在是 ${distinct.size} 个`);
    return { type: "Polygon", coordinates: [closeRing(vertices)] };
}

/** 契约的 GeoPolygon → 外环顶点（不含闭合点）。 */
export function verticesFromPolygon(polygon: GeoPolygon | null | undefined): Position[] {
    const outer = polygon?.coordinates[0];
    if (outer === undefined) return [];
    return openRing(outer.map((p): Position => [p[0] ?? NaN, p[1] ?? NaN]));
}

/** 经度 ∈ [-180,180]、纬度 ∈ [-90,90]。越界时返回一句人话，否则 null。 */
export function rangeProblem(points: readonly Position[]): string | null {
    for (let i = 0; i < points.length; i += 1) {
        const p = points[i];
        if (p === undefined) continue;
        const [lng, lat] = p;
        if (!Number.isFinite(lng) || !Number.isFinite(lat)) return `第 ${i + 1} 个点不是数字`;
        if (lat < -90 || lat > 90) {
            return `第 ${i + 1} 个点的纬度 ${lat} 超出 [-90, 90]。GeoJSON 的顺序是 [经度, 纬度]——多半是写反了`;
        }
        if (lng < -180 || lng > 180) return `第 ${i + 1} 个点的经度 ${lng} 超出 [-180, 180]`;
    }
    return null;
}

/**
 * 在中国境内使用时，这批点是不是**看起来**把经纬度写反了。
 *
 * 纬度写反往往不会越界（北京 [39.9, 116.4] 写成 [经, 纬] 就是经度 39.9、
 * 纬度 116.4——越界，能抓到；但广州 [23.1, 113.3] 反过来是纬度 113.3，也越界；
 * 真正抓不到的是两个值都 ≤ 90 的地方）。所以范围校验之外再加一条只报警
 * 不拒绝的启发：全部点的「经度」落在中国纬度带、「纬度」落在中国经度带。
 */
export function looksSwappedForChina(points: readonly Position[]): boolean {
    if (points.length === 0) return false;
    return points.every(([lng, lat]) => lng >= 3 && lng <= 54 && lat >= 73 && lat <= 135);
}

// ---------------------------------------------------------------------------
// 国内坐标系 → WGS-84（只在「粘贴坐标」入口、且运营显式选择时使用）
// ---------------------------------------------------------------------------
//
// GCJ-02 的正变换是公开的那套算法（Krasovsky 1940 椭球参数）。它**没有**
// 解析逆变换，这里用迭代法：猜一个 WGS 点，正变换过去，按差值修正，
// 几轮之后误差 < 1e-7 度（约 1 厘米）。对围栏来说远远够了。

const GCJ_A = 6378245.0;
const GCJ_EE = 0.00669342162296594323;

function outOfChina(lng: number, lat: number): boolean {
    // GCJ-02 只在中国境内加偏移；境外 GCJ = WGS。粗框，与业界通行实现一致。
    return lng < 72.004 || lng > 137.8347 || lat < 0.8293 || lat > 55.8271;
}

function transformLat(x: number, y: number): number {
    let r = -100.0 + 2.0 * x + 3.0 * y + 0.2 * y * y + 0.1 * x * y + 0.2 * Math.sqrt(Math.abs(x));
    r += ((20.0 * Math.sin(6.0 * x * Math.PI) + 20.0 * Math.sin(2.0 * x * Math.PI)) * 2.0) / 3.0;
    r += ((20.0 * Math.sin(y * Math.PI) + 40.0 * Math.sin((y / 3.0) * Math.PI)) * 2.0) / 3.0;
    r += ((160.0 * Math.sin((y / 12.0) * Math.PI) + 320 * Math.sin((y * Math.PI) / 30.0)) * 2.0) / 3.0;
    return r;
}

function transformLng(x: number, y: number): number {
    let r = 300.0 + x + 2.0 * y + 0.1 * x * x + 0.1 * x * y + 0.1 * Math.sqrt(Math.abs(x));
    r += ((20.0 * Math.sin(6.0 * x * Math.PI) + 20.0 * Math.sin(2.0 * x * Math.PI)) * 2.0) / 3.0;
    r += ((20.0 * Math.sin(x * Math.PI) + 40.0 * Math.sin((x / 3.0) * Math.PI)) * 2.0) / 3.0;
    r += ((150.0 * Math.sin((x / 12.0) * Math.PI) + 300.0 * Math.sin((x / 30.0) * Math.PI)) * 2.0) / 3.0;
    return r;
}

/** WGS-84 → GCJ-02。只在测试与逆变换的迭代里用；后台从不往库里写 GCJ-02。 */
export function wgs84ToGcj02([lng, lat]: Position): Position {
    if (outOfChina(lng, lat)) return [lng, lat];
    let dLat = transformLat(lng - 105.0, lat - 35.0);
    let dLng = transformLng(lng - 105.0, lat - 35.0);
    const radLat = (lat / 180.0) * Math.PI;
    let magic = Math.sin(radLat);
    magic = 1 - GCJ_EE * magic * magic;
    const sqrtMagic = Math.sqrt(magic);
    dLat = (dLat * 180.0) / (((GCJ_A * (1 - GCJ_EE)) / (magic * sqrtMagic)) * Math.PI);
    dLng = (dLng * 180.0) / ((GCJ_A / sqrtMagic) * Math.cos(radLat) * Math.PI);
    return [lng + dLng, lat + dLat];
}

/** GCJ-02 → WGS-84，迭代逆变换。 */
export function gcj02ToWgs84(gcj: Position): Position {
    if (outOfChina(gcj[0], gcj[1])) return [gcj[0], gcj[1]];
    let guess: Position = [gcj[0], gcj[1]];
    for (let i = 0; i < 30; i += 1) {
        const fwd = wgs84ToGcj02(guess);
        const dLng = fwd[0] - gcj[0];
        const dLat = fwd[1] - gcj[1];
        guess = [guess[0] - dLng, guess[1] - dLat];
        if (Math.abs(dLng) < 1e-9 && Math.abs(dLat) < 1e-9) break;
    }
    return guess;
}

const X_PI = (Math.PI * 3000.0) / 180.0;

/** BD-09 → GCJ-02（百度公开的那套换算）。 */
export function bd09ToGcj02([lng, lat]: Position): Position {
    const x = lng - 0.0065;
    const y = lat - 0.006;
    const z = Math.sqrt(x * x + y * y) - 0.00002 * Math.sin(y * X_PI);
    const theta = Math.atan2(y, x) - 0.000003 * Math.cos(x * X_PI);
    return [z * Math.cos(theta), z * Math.sin(theta)];
}

/** GCJ-02 → BD-09。只给测试用（验证 BD-09 那条路径的往返）。 */
export function gcj02ToBd09([lng, lat]: Position): Position {
    const z = Math.sqrt(lng * lng + lat * lat) + 0.00002 * Math.sin(lat * X_PI);
    const theta = Math.atan2(lat, lng) + 0.000003 * Math.cos(lng * X_PI);
    return [z * Math.cos(theta) + 0.0065, z * Math.sin(theta) + 0.006];
}

export type CoordSystem = "wgs84" | "gcj02" | "bd09";

/** 任一坐标系 → WGS-84。wgs84 原样返回。 */
export function toWgs84(pos: Position, from: CoordSystem): Position {
    switch (from) {
        case "wgs84":
            return [pos[0], pos[1]];
        case "gcj02":
            return gcj02ToWgs84(pos);
        case "bd09":
            return gcj02ToWgs84(bd09ToGcj02(pos));
    }
}

/** 两点之间的大圆距离（米）。给「这次换算挪了多远」的提示用。 */
export function distanceMeters(a: Position, b: Position): number {
    const R = 6371008.8;
    const toRad = (d: number): number => (d * Math.PI) / 180;
    const dLat = toRad(b[1] - a[1]);
    const dLng = toRad(b[0] - a[0]);
    const h = Math.sin(dLat / 2) ** 2 + Math.cos(toRad(a[1])) * Math.cos(toRad(b[1])) * Math.sin(dLng / 2) ** 2;
    return 2 * R * Math.asin(Math.sqrt(h));
}

// ---------------------------------------------------------------------------
// 「粘贴 GeoJSON / 坐标」入口的解析
// ---------------------------------------------------------------------------

export type ParseResult =
    | { ok: true; polygon: GeoPolygon; warnings: string[]; shiftedMeters: number }
    | { ok: false; error: string };

function isNumberPair(v: unknown): v is [number, number, ...number[]] {
    return Array.isArray(v) && v.length >= 2 && typeof v[0] === "number" && typeof v[1] === "number";
}

/** 从一个已解析的 JSON 值里取出多边形的环。接受 Polygon / Feature / 单要素 FeatureCollection / 裸坐标数组。 */
function ringsFromJson(value: unknown): Position[][] | string {
    if (Array.isArray(value)) {
        if (value.length > 0 && value.every(isNumberPair)) {
            return [value.map((p): Position => [p[0], p[1]])];
        }
        if (value.length > 0 && value.every((r) => Array.isArray(r) && r.every(isNumberPair))) {
            return (value as [number, number][][]).map((r) => r.map((p): Position => [p[0], p[1]]));
        }
        return "坐标数组的形状不对：要么是 [[经,纬], ...]，要么是 [[[经,纬], ...]]";
    }
    if (typeof value !== "object" || value === null) return "不是 GeoJSON";
    const obj = value as { type?: unknown; geometry?: unknown; features?: unknown; coordinates?: unknown };
    if (obj.type === "Feature") return ringsFromJson(obj.geometry);
    if (obj.type === "FeatureCollection") {
        const features = Array.isArray(obj.features) ? obj.features : [];
        if (features.length !== 1) return `FeatureCollection 里要恰好一个要素，现在是 ${features.length} 个`;
        return ringsFromJson(features[0]);
    }
    if (obj.type === "MultiPolygon") return "契约只收 Polygon（一家店一个环），不收 MultiPolygon";
    if (obj.type !== "Polygon") return `几何类型是 ${String(obj.type)}，要的是 Polygon`;
    return ringsFromJson(obj.coordinates);
}

/** 纯文本：每行一个点，`经度,纬度` 或 `经度 纬度`。 */
function ringsFromLines(text: string): Position[][] | string {
    const ring: Position[] = [];
    const lines = text.split(/\r?\n/).map((l) => l.trim()).filter((l) => l !== "" && !l.startsWith("#"));
    for (let i = 0; i < lines.length; i += 1) {
        const parts = (lines[i] ?? "").split(/[\s,，;；]+/).filter((s) => s !== "");
        const lng = Number(parts[0]);
        const lat = Number(parts[1]);
        if (parts.length < 2 || !Number.isFinite(lng) || !Number.isFinite(lat)) {
            return `第 ${i + 1} 行「${lines[i] ?? ""}」读不出两个数`;
        }
        ring.push([lng, lat]);
    }
    return [ring];
}

/**
 * 解析运营粘贴进来的东西，换成 WGS-84 的 GeoPolygon。
 *
 * 失败时返回一句**说清楚哪里错**的话，不抛异常：这是给人看的。
 * 自交不在这里判（见 polygonFromVertices 的注释），交给服务端。
 */
export function parseFenceInput(text: string, from: CoordSystem): ParseResult {
    const trimmed = text.trim();
    if (trimmed === "") return { ok: false, error: "什么都没粘贴" };

    let rings: Position[][] | string;
    if (trimmed.startsWith("{") || trimmed.startsWith("[")) {
        let parsed: unknown;
        try {
            parsed = JSON.parse(trimmed) as unknown;
        } catch (e) {
            return { ok: false, error: `JSON 解析失败：${e instanceof Error ? e.message : String(e)}` };
        }
        rings = ringsFromJson(parsed);
    } else {
        rings = ringsFromLines(trimmed);
    }
    if (typeof rings === "string") return { ok: false, error: rings };

    const warnings: string[] = [];
    const all = rings.flat();
    const range = rangeProblem(all);
    if (range !== null) return { ok: false, error: range };
    if (looksSwappedForChina(all)) {
        warnings.push("这批点的「经度」都在 3~54、「纬度」都在 73~135，看起来像把经纬度写反了（GeoJSON 是 [经度, 纬度]）。");
    }

    let shiftedMeters = 0;
    const converted = rings.map((ring) =>
        ring.map((p) => {
            const w = toWgs84(p, from);
            shiftedMeters = Math.max(shiftedMeters, distanceMeters(p, w));
            return w;
        }),
    );

    const outer = openRing(converted[0] ?? []);
    const distinct = new Set(outer.map((p) => `${p[0]},${p[1]}`));
    if (distinct.size < 3) return { ok: false, error: `外环至少要 3 个不同的顶点，现在是 ${distinct.size} 个` };

    return {
        ok: true,
        polygon: { type: "Polygon", coordinates: converted.map((r) => closeRing(r)) },
        warnings,
        shiftedMeters,
    };
}
