// 地图底图配置（`GET /geo/map`，公开接口）：LocationPicker 与 FenceEditor 两个
// 组件共用同一份，只真正请求一次——模块级缓存的 Promise，而不是各调各的。
//
// 这个文件**不 import client.ts 的运行时**（`keel` / `KeelClient`）：取配置的
// 函数由调用方注入（两个组件本来就在用 `keel.get` 调 `/geo/suggest` 等接口，
// 原样传一个 `() => keel.get("/geo/map")` 进来即可）。理由和 geo.ts 文件头一致：
// `make admin-test` 用 `node --test` 直接跑这里的测试，不经过 vite / vue-tsc，
// 认不得 "@contract/" 这个只有打包器才认的路径别名，也没有 node_modules——
// 运行时 import client.ts（它又运行时 import `@contract/client.mts`）会在这里
// 以 ERR_MODULE_NOT_FOUND 失败。`GeoMapConfig` 是 `import type`，会被整个擦掉，
// 不受影响。
//
// `enabled: false`（部署没配 `KEEL_MAP_TILES`）时：`layers` 是空数组。
// 调用方据此不画 tileLayer，退回手填坐标 / 只读提示——具体怎么退，看两个
// 组件各自文件头。

import type { GeoMapConfig } from "./client.ts";

export type { GeoMapConfig };

export interface TileLayerSpec {
    /** 契约里 layers 数组的原始层名，比如 "tianditu-base" / "tianditu-label"。 */
    layer: string;
    /** 传给 `L.tileLayer` 的模板：`{apiBase}/geo/tiles/{layer}/{z}/{x}/{y}`。 */
    urlTemplate: string;
}

/**
 * `config.layers`（从下往上叠的层名，天地图是 base 底图 + label 注记）
 * → Leaflet `tileLayer` 的 URL 模板列表，顺序原样保留。
 */
export function tileLayerSpecs(apiBase: string, config: GeoMapConfig): TileLayerSpec[] {
    return config.layers.map((layer): TileLayerSpec => ({
        layer,
        urlTemplate: `${apiBase}/geo/tiles/${layer}/{z}/{x}/{y}`,
    }));
}

let cached: Promise<GeoMapConfig> | null = null;

/**
 * 拿地图底图配置。`fetchConfig` 由调用方传（通常是 `() => keel.get("/geo/map")`）。
 *
 * 模块级缓存：这个标签页的生命周期里，不管多少个组件、调用多少次，
 * 只真正发一次请求（第二个组件挂载时拿到的是同一个已 resolve 的 Promise）。
 * 请求失败（网络抖动之类）不缓存失败结果，下一次调用会重新尝试。
 */
export function loadMapConfig(fetchConfig: () => Promise<GeoMapConfig>): Promise<GeoMapConfig> {
    if (cached === null) {
        cached = fetchConfig().catch((err: unknown) => {
            cached = null;
            throw err;
        });
    }
    return cached;
}

/** 仅供测试：清掉模块级缓存，让下一次 loadMapConfig 重新调用 fetchConfig。 */
export function resetMapConfigCacheForTest(): void {
    cached = null;
}
