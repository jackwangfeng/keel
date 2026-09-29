// 地图底图配置的取值与缓存规则。`node --test src/api/mapTiles.test.ts` 直接跑，
// 不要 node_modules（文件头理由同 geo.test.ts）。
import { test } from "node:test";
import assert from "node:assert/strict";
import { loadMapConfig, resetMapConfigCacheForTest, tileLayerSpecs, type GeoMapConfig } from "./mapTiles.ts";

const enabledConfig: GeoMapConfig = {
    enabled: true,
    layers: ["tianditu-base", "tianditu-label"],
    max_zoom: 18,
    attribution: "天地图",
};

const disabledConfig: GeoMapConfig = { enabled: false, layers: [], max_zoom: 0, attribution: "" };

test("tileLayerSpecs：按 layers 顺序生成 URL 模板（从下往上叠），层名原样带上", () => {
    assert.deepEqual(tileLayerSpecs("/api/v1", enabledConfig), [
        { layer: "tianditu-base", urlTemplate: "/api/v1/geo/tiles/tianditu-base/{z}/{x}/{y}" },
        { layer: "tianditu-label", urlTemplate: "/api/v1/geo/tiles/tianditu-label/{z}/{x}/{y}" },
    ]);
});

test("tileLayerSpecs：enabled=false 时 layers 是空数组，不生成任何图层", () => {
    assert.deepEqual(tileLayerSpecs("/api/v1", disabledConfig), []);
});

test("loadMapConfig：模块级缓存，同一批调用只真正调一次 fetchConfig", async () => {
    resetMapConfigCacheForTest();
    let calls = 0;
    const fetchConfig = (): Promise<GeoMapConfig> => {
        calls++;
        return Promise.resolve(enabledConfig);
    };
    const [a, b] = await Promise.all([loadMapConfig(fetchConfig), loadMapConfig(fetchConfig)]);
    assert.equal(calls, 1);
    assert.deepEqual(a, enabledConfig);
    assert.deepEqual(b, enabledConfig);
});

test("loadMapConfig：请求失败不缓存，下一次调用会重新尝试", async () => {
    resetMapConfigCacheForTest();
    let calls = 0;
    const fetchConfig = (): Promise<GeoMapConfig> => {
        calls++;
        return calls === 1 ? Promise.reject(new Error("网络抖动")) : Promise.resolve(disabledConfig);
    };
    await assert.rejects(() => loadMapConfig(fetchConfig));
    const ok = await loadMapConfig(fetchConfig);
    assert.equal(calls, 2);
    assert.deepEqual(ok, disabledConfig);
});
