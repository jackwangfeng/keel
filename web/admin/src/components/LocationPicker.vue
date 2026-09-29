<script setup lang="ts">
// 门店坐标的地图选点：点地图落点，拖动标记微调。
//
// 与 FenceEditor 同一套地图（Leaflet + 服务端代理的底图瓦片，WGS-84），理由写在
// 那个文件头：库里存的是 WGS-84，在这张图上点出来的坐标原样就是要存的值，
// 没有换算这一步。
//
// 底图瓦片走 GET /geo/map（配置：开没开、叠哪几层、最大级别、署名）+
// GET /geo/tiles/{layer}/{z}/{x}/{y}（转发），两个组件共用的取值 / 缓存逻辑在
// api/mapTiles.ts。部署没配 KEEL_MAP_TILES 时 enabled=false：这里不画地图，
// 退回经纬度输入框——地图从来不是必需品，手填坐标 / 拖标记选点都能达到同一个
// 目的，只是没图直观。
//
// 规则（2026-09-27）：门店必须有坐标；有围栏时门店必须在围栏内。围栏在这里画成虚线
// 只是为了让人看得见边界 —— 「在不在内」由服务端判（ST_Covers，边界上算在内），
// 前端不另写一套判据，两边迟早分叉。越界时服务端回 422 store-outside-fence。

import { nextTick, onBeforeUnmount, onMounted, ref, watch } from "vue";
import L from "leaflet";
import "leaflet/dist/leaflet.css";
import { API_BASE, keel, ProblemError, type GeoPlace, type GeoPolygon } from "../api/client.ts";
import { loadMapConfig, tileLayerSpecs, type GeoMapConfig } from "../api/mapTiles.ts";
import { toLatLng, verticesFromPolygon } from "../api/geo.ts";

export type LatLng = { lat: number; lng: number };

const props = defineProps<{
    /** 当前选中的点。null 表示还没选。 */
    modelValue: LatLng | null;
    /** 这家店已保存的围栏，画成虚线作参照。 */
    fence?: GeoPolygon | null;
    readonly?: boolean;
    height?: string;
}>();

const emit = defineEmits<{
    "update:modelValue": [v: LatLng];
    /** 搜索选点 / 点地图 / 拖标记之后，服务端解析出来的完整地址（可能省略号码等）。 */
    place: [p: GeoPlace];
}>();

// ---------------------------------------------------------------------------
// 地点搜索（GET /geo/suggest、/geo/reverse）。两个接口都公开、免鉴权，
// 没配地图服务商时回 501，服务商挂了回 503——都不能挡住手工填写/选点，
// 所以这里只做「能用就用，不能用就退回手填」，不抛错到外面。
// ---------------------------------------------------------------------------

type GeoStatus = "ok" | "not-configured" | "unavailable";
const geoStatus = ref<GeoStatus>("ok");
const searchQuery = ref("");

/** 501 永久退回手填（不再显示搜索框）；503 只是暂时的，下次调用还会再试。 */
function noteGeoError(err: unknown): void {
    if (err instanceof ProblemError && err.status === 501) {
        geoStatus.value = "not-configured";
        return;
    }
    if (err instanceof ProblemError && err.status === 503) {
        geoStatus.value = "unavailable";
        return;
    }
    // 其它错误（网络抖动之类）：安静忽略，不打断选点。
}

let suggestSeq = 0;

function fetchSuggestions(
    queryString: string,
    cb: (items: Array<{ value: string; address: string; place: GeoPlace }>) => void,
): void {
    const q = queryString.trim();
    if (q.length < 2 || geoStatus.value === "not-configured") {
        cb([]);
        return;
    }
    const seq = ++suggestSeq;
    const query = props.modelValue ? { q, lat: props.modelValue.lat, lng: props.modelValue.lng } : { q };
    keel
        .get("/geo/suggest", { query })
        .then((res) => {
            if (seq !== suggestSeq) return; // 输入已经变了，这一批结果作废
            if (geoStatus.value !== "ok") geoStatus.value = "ok";
            cb(res.items.map((p) => ({ value: p.name, address: p.address, place: p })));
        })
        .catch((err: unknown) => {
            noteGeoError(err);
            if (seq === suggestSeq) cb([]);
        });
}

/** 拿到一个点之后补全省市区（候选点的省市区可能不全，见 /geo/suggest 文档）。 */
async function resolvePlace(point: LatLng, fallback?: GeoPlace): Promise<void> {
    if (geoStatus.value === "not-configured") {
        if (fallback) emit("place", fallback);
        return;
    }
    try {
        const full = await keel.get("/geo/reverse", { query: point });
        if (geoStatus.value !== "ok") geoStatus.value = "ok";
        emit("place", full);
    } catch (err) {
        noteGeoError(err);
        if (fallback) emit("place", fallback);
    }
}

let reverseTimer: ReturnType<typeof setTimeout> | null = null;

/** 点地图 / 拖标记触发的逆地理编码：防抖，失败静默忽略（不影响已经落下的点）。 */
function scheduleReverse(point: LatLng): void {
    if (reverseTimer) clearTimeout(reverseTimer);
    reverseTimer = setTimeout(() => void resolvePlace(point), 300);
}

function onSelectSuggestion(item: { value: string; address: string; place: GeoPlace }): void {
    const p = item.place;
    const point = { lat: round6(p.lat), lng: round6(p.lng) };
    emit("update:modelValue", point);
    map?.setView(point, 17);
    void resolvePlace(point, p);
}

const mapEl = ref<HTMLDivElement | null>(null);
let map: L.Map | null = null;
let marker: L.Marker | null = null;
let fenceLayer: L.Polygon | null = null;
let resizeObserver: ResizeObserver | null = null;

const pinIcon = L.divIcon({ className: "store-pin", iconSize: [18, 18], iconAnchor: [9, 9] });

/** 6 位小数约 0.1 米，够用；再多只是噪声。 */
function round6(v: number): number {
    return Math.round(v * 1e6) / 1e6;
}

// ---------------------------------------------------------------------------
// 底图瓦片（GET /geo/map）。enabled=false（部署没配 KEEL_MAP_TILES）或者请求
// 本身失败时，一律退回手填经纬度——不把一张加载不出瓦片的空白地图留在页面上。
// ---------------------------------------------------------------------------

const mapStatus = ref<"loading" | "enabled" | "disabled">("loading");
/** 没地图时的手填坐标；两个输入框都填了才 emit。跟 modelValue 双向同步。 */
const manualLat = ref<number | null>(props.modelValue?.lat ?? null);
const manualLng = ref<number | null>(props.modelValue?.lng ?? null);

function commitManual(): void {
    if (props.readonly || manualLat.value === null || manualLng.value === null) return;
    const point = { lat: round6(manualLat.value), lng: round6(manualLng.value) };
    emit("update:modelValue", point);
    scheduleReverse(point);
}

async function bootstrapMap(): Promise<void> {
    try {
        const config = await loadMapConfig(() => keel.get("/geo/map"));
        if (!config.enabled) {
            mapStatus.value = "disabled";
            return;
        }
        mapStatus.value = "enabled";
        await nextTick(); // v-if 要先把 mapEl 的 div 渲染出来
        if (mapEl.value === null) return; // 组件在这中间被卸载了
        createLeafletMap(config);
    } catch {
        // /geo/map 本身请求失败（比如反代没配好）：和「明确没配」一样处理。
        mapStatus.value = "disabled";
    }
}

function createLeafletMap(config: GeoMapConfig): void {
    if (mapEl.value === null) return;
    map = L.map(mapEl.value, { zoomControl: true });
    for (const spec of tileLayerSpecs(API_BASE, config)) {
        L.tileLayer(spec.urlTemplate, { maxZoom: config.max_zoom, attribution: config.attribution }).addTo(map);
    }
    map.on("click", (e: L.LeafletMouseEvent) => pick(e.latlng));
    drawFence();
    drawMarker();
    fitView();
    // 放在对话框 / el-tabs 里时容器一开始是 0 尺寸，监听尺寸而不是监听打开事件。
    resizeObserver = new ResizeObserver(() => map?.invalidateSize());
    resizeObserver.observe(mapEl.value);
}

function pick(ll: L.LatLng): void {
    if (props.readonly) return;
    const point = { lat: round6(ll.lat), lng: round6(ll.lng) };
    emit("update:modelValue", point);
    scheduleReverse(point);
}

function drawMarker(): void {
    if (map === null) return;
    const p = props.modelValue;
    if (p === null) {
        marker?.remove();
        marker = null;
        return;
    }
    if (marker === null) {
        marker = L.marker(p, { icon: pinIcon, draggable: !props.readonly, title: "门店位置" }).addTo(map);
        marker.on("dragend", () => marker && pick(marker.getLatLng()));
    } else {
        marker.setLatLng(p);
    }
}

function drawFence(): void {
    if (map === null) return;
    fenceLayer?.remove();
    fenceLayer = null;
    const outer = verticesFromPolygon(props.fence ?? null);
    if (outer.length >= 3) {
        // 醒目的蓝：选点时最要紧的就是看清边界。原先的灰色细虚线压在底图上几乎看不见。
        // 门店标记是橙色，两者对比分明。
        fenceLayer = L.polygon(outer.map(toLatLng), {
            color: "#1677ff",
            weight: 3,
            dashArray: "8 6",
            fillColor: "#1677ff",
            fillOpacity: 0.12,
            interactive: false,
        }).addTo(map);
    }
}

function fitView(): void {
    if (map === null) return;
    if (fenceLayer) {
        // 有围栏时视野框住整个围栏（连同门店点）：只按门店点放大到 15 级的话，
        // 围栏比视野大，边界全在屏幕外，看上去像没有围栏。
        const bounds = fenceLayer.getBounds();
        if (props.modelValue) bounds.extend(props.modelValue);
        map.fitBounds(bounds, { padding: [30, 30], maxZoom: 16 });
    } else if (props.modelValue) {
        map.setView(props.modelValue, 15);
    } else {
        map.setView({ lat: 35, lng: 105 }, 4);
    }
}

onMounted(() => {
    void bootstrapMap();
});

onBeforeUnmount(() => {
    if (reverseTimer) clearTimeout(reverseTimer);
    resizeObserver?.disconnect();
    map?.remove();
    map = null;
    marker = null;
});

watch(() => props.modelValue, (v) => {
    drawMarker();
    manualLat.value = v?.lat ?? null;
    manualLng.value = v?.lng ?? null;
});
watch(() => props.fence, drawFence);
</script>

<template>
    <div class="picker">
        <el-autocomplete
            v-if="!readonly && geoStatus !== 'not-configured'"
            v-model="searchQuery"
            class="search"
            clearable
            :trigger-on-focus="false"
            :fetch-suggestions="fetchSuggestions"
            placeholder="搜索地点（小区 / 商场 / 门牌），至少 2 个字"
            @select="onSelectSuggestion"
        >
            <template #default="{ item }: { item: { value: string; address: string } }">
                <div class="sugg-name">{{ item.value }}</div>
                <div class="sugg-address">{{ item.address }}</div>
            </template>
        </el-autocomplete>
        <p v-if="geoStatus === 'not-configured'" class="hint geo-hint">
            地图服务商未配置，请手填地址并在地图上选点。
        </p>
        <p v-else-if="geoStatus === 'unavailable'" class="hint geo-hint">地图服务暂时不可用，可以先手填地址，或稍后再搜。</p>

        <div v-if="mapStatus === 'enabled'" ref="mapEl" class="map" :style="{ height: height ?? '320px' }" />
        <p v-else-if="mapStatus === 'loading'" class="hint">地图加载中…</p>
        <template v-else>
            <p class="hint geo-hint">未配置地图底图（KEEL_MAP_TILES），请直接填写坐标。</p>
            <div v-if="!readonly" class="manual-latlng">
                <el-input-number
                    v-model="manualLat"
                    :precision="6"
                    :step="0.0001"
                    controls-position="right"
                    placeholder="纬度"
                    @change="commitManual"
                />
                <el-input-number
                    v-model="manualLng"
                    :precision="6"
                    :step="0.0001"
                    controls-position="right"
                    placeholder="经度"
                    @change="commitManual"
                />
            </div>
        </template>

        <p class="hint">
            <template v-if="modelValue">
                已选：纬度 {{ modelValue.lat.toFixed(6) }}，经度 {{ modelValue.lng.toFixed(6) }}（WGS-84）。
                <template v-if="!readonly && mapStatus === 'enabled'">点地图换位置，或拖动标记微调。</template>
            </template>
            <template v-else-if="readonly">还没有定位。</template>
            <template v-else-if="mapStatus === 'enabled'">在地图上点一下门店所在的位置（必填），或用上面的搜索框找地点。</template>
            <template v-else-if="mapStatus === 'disabled'">门店坐标必填，在上面填经纬度。</template>
            <template v-if="fence && mapStatus === 'enabled'">蓝色区域是这家店的围栏，门店必须落在围栏内。</template>
        </p>
    </div>
</template>

<style scoped>
.picker {
    width: 100%;
}
.search {
    width: 100%;
    margin-bottom: 8px;
}
.sugg-name {
    font-size: 13px;
}
.sugg-address {
    font-size: 12px;
    color: var(--el-text-color-secondary);
}
.map {
    width: 100%;
    border: 1px solid var(--el-border-color);
    border-radius: 4px;
}
.manual-latlng {
    display: flex;
    gap: 8px;
    flex-wrap: wrap;
}
.hint {
    margin: 6px 0 0;
    color: var(--el-text-color-secondary);
    font-size: 12px;
    line-height: 1.5;
}
.geo-hint {
    margin: 0 0 6px;
}
:global(.store-pin) {
    background: #e6a23c;
    border: 3px solid #fff;
    border-radius: 50%;
    box-shadow: 0 0 0 1px #e6a23c, 0 1px 4px rgba(0, 0, 0, 0.4);
}
</style>
