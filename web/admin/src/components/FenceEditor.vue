<script setup lang="ts">
// 电子围栏编辑器：Leaflet + OpenStreetMap 瓦片。
//
// ## 为什么是 OSM 而不是高德 / 腾讯 / 百度
//
// 库里是 `GEOGRAPHY(POLYGON, 4326)`——WGS-84。OSM 的瓦片与 Leaflet 的
// lat/lng 都是 WGS-84，在这张地图上点出来的顶点**原样**就是要存的值，
// 这条路上没有任何换算，也就没有「忘了换算」这种会偏几百米而不报错的错。
// 国内地图给的是 GCJ-02 / BD-09（完整说明在 api/geo.ts 的文件头）；
// 用它们就必须在存之前换算，而换算是一段只要漏一次就会把买家判错店的代码。
//
// 代价：OSM 瓦片在国内有时加载慢或加载不出来，国内路网细节也不如高德。
// 所以旁边有一个「粘贴 GeoJSON / 坐标」入口——没网、瓦片出不来时也能配围栏，
// 并且在那里（且只在那里）可以显式声明「这批坐标是从高德 / 百度抄的」，
// 由 api/geo.ts 换回 WGS-84（换算有测试：geo.test.ts）。
//
// ## 画法
//
// 点地图加顶点，拖顶点改位置，「撤销上一个点」「清空重画」。
// 没有引 leaflet-draw：它多年没有维护，而这里只需要画一个环。
// 顶点用 divIcon（纯 CSS），不用 Leaflet 默认的图片 marker——
// 那套图片的路径在打包器下是出了名会坏的。
//
// 自交**不在前端判**：服务端 ST_IsValid 判，失败时 detail 里是 PostGIS 的原话，
// 界面把它原样显示并在地图上标出位置（errorPoint）。前端再写一套判据，
// 两边迟早分叉。

import { computed, onBeforeUnmount, onMounted, ref, watch } from "vue";
import L from "leaflet";
import "leaflet/dist/leaflet.css";
import type { GeoPolygon } from "../api/client.ts";
import {
    closeRing,
    openRing,
    parseFenceInput,
    polygonFromVertices,
    toLatLng,
    toPosition,
    verticesFromPolygon,
    type CoordSystem,
    type Position,
} from "../api/geo.ts";

const props = defineProps<{
    /** 已保存的围栏。null 表示没有围栏。 */
    saved: GeoPolygon | null;
    /** 门店自己的坐标（WGS-84），地图初始视野用。 */
    center?: { lat: number; lng: number } | null;
    /** 服务端 invalid-fence 时 PostGIS 报的位置 [经度, 纬度]。 */
    errorPoint?: [number, number] | null;
    busy?: boolean;
    /** 是不是默认门店。决定「清空围栏」合不合法（服务端判，这里只提示）。 */
    isDefault: boolean;
    /** 当前角色改不了围栏（src/auth/permissions.ts）。只置灰保存 / 清空，照样能看。 */
    readonly?: boolean;
}>();

const emit = defineEmits<{ save: [fence: GeoPolygon | null] }>();

const mapEl = ref<HTMLDivElement | null>(null);
const vertices = ref<Position[]>(verticesFromPolygon(props.saved));
/** 内环（洞）：原样保留，地图只编辑外环。一律存成**不闭合**的顶点序列，保存时再闭合。 */
function holesOf(fence: GeoPolygon | null | undefined): Position[][] {
    return (fence?.coordinates.slice(1) ?? []).map((r) => openRing(r.map((p): Position => [p[0] ?? NaN, p[1] ?? NaN])));
}
const holes = ref<Position[][]>(holesOf(props.saved));
const localError = ref("");

let map: L.Map | null = null;
let polygonLayer: L.Polygon | null = null;
let savedLayer: L.Polygon | null = null;
let markers: L.Marker[] = [];
let errorLayer: L.CircleMarker | null = null;
let resizeObserver: ResizeObserver | null = null;

const vertexIcon = L.divIcon({ className: "fence-vertex", iconSize: [14, 14], iconAnchor: [7, 7] });

function redraw(): void {
    if (map === null) return;
    const m = map;
    for (const mk of markers) mk.remove();
    markers = vertices.value.map((pos, i) => {
        const mk = L.marker(toLatLng(pos), { icon: vertexIcon, draggable: true, title: `第 ${i + 1} 个点` });
        mk.on("drag", () => {
            const ll = mk.getLatLng();
            vertices.value[i] = toPosition(ll);
            drawPolygon();
        });
        mk.addTo(m);
        return mk;
    });
    drawPolygon();
}

function drawPolygon(): void {
    if (map === null) return;
    polygonLayer?.remove();
    polygonLayer = null;
    if (vertices.value.length >= 2) {
        polygonLayer = L.polygon(vertices.value.map(toLatLng), { color: "#409eff", weight: 2, fillOpacity: 0.15 }).addTo(map);
    }
}

function drawSaved(): void {
    if (map === null) return;
    savedLayer?.remove();
    savedLayer = null;
    const outer = verticesFromPolygon(props.saved);
    if (outer.length >= 3) {
        savedLayer = L.polygon(outer.map(toLatLng), {
            color: "#909399",
            weight: 1,
            dashArray: "4 4",
            fill: false,
            interactive: false,
        }).addTo(map);
    }
}

function drawError(): void {
    if (map === null) return;
    errorLayer?.remove();
    errorLayer = null;
    const p = props.errorPoint;
    if (p === null || p === undefined) return;
    errorLayer = L.circleMarker(toLatLng(p), { radius: 12, color: "#f56c6c", weight: 3, fillOpacity: 0.2 })
        .bindTooltip("PostGIS 说问题在这里", { permanent: true, direction: "top" })
        .addTo(map);
}

function fitView(): void {
    if (map === null) return;
    const pts = vertices.value.length > 0 ? vertices.value : verticesFromPolygon(props.saved);
    if (pts.length >= 2) {
        map.fitBounds(L.latLngBounds(pts.map(toLatLng)), { padding: [30, 30], maxZoom: 16 });
    } else if (props.center) {
        map.setView(props.center, 14);
    } else {
        map.setView({ lat: 35, lng: 105 }, 4);
    }
}

onMounted(() => {
    if (mapEl.value === null) return;
    map = L.map(mapEl.value, { zoomControl: true });
    L.tileLayer("https://tile.openstreetmap.org/{z}/{x}/{y}.png", {
        maxZoom: 19,
        // OSM 的瓦片使用政策要求署名，这一行不是装饰。
        attribution: '&copy; <a href="https://www.openstreetmap.org/copyright">OpenStreetMap</a> 贡献者（WGS-84）',
    }).addTo(map);
    if (props.center) {
        L.circleMarker(props.center, { radius: 5, color: "#e6a23c", fillOpacity: 1 })
            .bindTooltip("门店坐标")
            .addTo(map);
    }
    map.on("click", (e: L.LeafletMouseEvent) => {
        vertices.value.push(toPosition(e.latlng));
        localError.value = "";
        redraw();
    });
    drawSaved();
    redraw();
    drawError();
    fitView();
    // 地图放在 el-tabs 里：切到这个 tab 之前容器尺寸是 0，Leaflet 会画成一个点。
    // 监听容器尺寸而不是监听 tab 切换，这样放在哪儿都对。
    resizeObserver = new ResizeObserver(() => map?.invalidateSize());
    resizeObserver.observe(mapEl.value);
});

onBeforeUnmount(() => {
    resizeObserver?.disconnect();
    map?.remove();
    map = null;
});

watch(
    () => props.saved,
    (fence) => {
        vertices.value = verticesFromPolygon(fence);
        holes.value = holesOf(fence);
        drawSaved();
        redraw();
    },
);
watch(() => props.errorPoint, drawError);

function undo(): void {
    vertices.value.pop();
    redraw();
}

function clearDraft(): void {
    vertices.value = [];
    holes.value = [];
    redraw();
}

function revert(): void {
    vertices.value = verticesFromPolygon(props.saved);
    redraw();
    fitView();
}

const dirty = computed(
    () => JSON.stringify(vertices.value) !== JSON.stringify(verticesFromPolygon(props.saved)),
);

function save(): void {
    localError.value = "";
    try {
        const outer = polygonFromVertices(vertices.value);
        const rings = [outer.coordinates[0] ?? [], ...holes.value.map((h) => closeRing(h))];
        emit("save", { type: "Polygon", coordinates: rings });
    } catch (e) {
        localError.value = e instanceof Error ? e.message : String(e);
    }
}

function clearFence(): void {
    emit("save", null);
}

// ------------------------------------------------------------ 粘贴入口

const pasteText = ref("");
const pasteFrom = ref<CoordSystem>("wgs84");
const pasteNote = ref("");
const pasteWarnings = ref<string[]>([]);

function applyPaste(): void {
    pasteNote.value = "";
    pasteWarnings.value = [];
    const r = parseFenceInput(pasteText.value, pasteFrom.value);
    if (!r.ok) {
        localError.value = r.error;
        return;
    }
    localError.value = "";
    vertices.value = verticesFromPolygon(r.polygon);
    holes.value = holesOf(r.polygon);
    pasteWarnings.value = r.warnings;
    if (pasteFrom.value !== "wgs84") {
        pasteNote.value = `已从 ${pasteFrom.value === "gcj02" ? "GCJ-02" : "BD-09"} 换回 WGS-84，顶点最多挪了 ${r.shiftedMeters.toFixed(0)} 米——不换的话围栏就偏这么多。`;
    }
    redraw();
    fitView();
}

/** 当前草稿的 GeoJSON，给人复制走（备份、贴进别的工具核对）。 */
const draftGeoJson = computed(() => {
    try {
        return JSON.stringify(polygonFromVertices(vertices.value));
    } catch {
        return "";
    }
});
</script>

<template>
    <div class="fence-editor">
        <el-alert type="info" :closable="false" show-icon class="mb8">
            <template #title>坐标系：WGS-84（与库里的 GEOGRAPHY 4326、买家端定位一致）</template>
            在地图上点一下加一个顶点，拖动顶点改位置。地图是 OpenStreetMap，它本身就是 WGS-84，点出来的坐标原样保存。
            <b>别从高德 / 腾讯 / 百度地图上抄坐标直接贴</b>——那是 GCJ-02 / BD-09，城区会偏几百米而且不报错；
            真要贴，在下面「粘贴坐标」里选对来源，会先换回 WGS-84。
        </el-alert>

        <div ref="mapEl" class="map" />

        <div class="toolbar">
            <span class="hint">顶点 {{ vertices.length }} 个（灰色虚线是已保存的围栏）</span>
            <span class="grow" />
            <el-button size="small" :disabled="vertices.length === 0" @click="undo">撤销上一个点</el-button>
            <el-button size="small" :disabled="vertices.length === 0" @click="clearDraft">清空重画</el-button>
            <el-button size="small" :disabled="!dirty" @click="revert">还原为已保存</el-button>
            <el-button
                size="small"
                type="danger"
                plain
                :disabled="saved === null || busy || readonly"
                :title="isDefault ? '' : '非默认门店清空围栏会被服务端拒绝（409 store-fence-required）'"
                @click="clearFence"
            >
                清空围栏
            </el-button>
            <el-button size="small" type="primary" :loading="busy" :disabled="vertices.length < 3 || readonly" @click="save">
                保存围栏
            </el-button>
        </div>

        <el-alert v-if="localError" type="error" :closable="false" show-icon :title="localError" class="mb8" />

        <el-collapse>
            <el-collapse-item title="粘贴 GeoJSON / 坐标（没网、瓦片出不来时用）" name="paste">
                <p class="hint">
                    接受 GeoJSON（Polygon / Feature / 单要素 FeatureCollection / 裸坐标数组），
                    或每行一个点 <code>经度,纬度</code>。<b>顺序是 [经度, 纬度]</b>，和口语里的「纬度、经度」相反。
                </p>
                <el-input v-model="pasteText" type="textarea" :rows="5" placeholder='{"type":"Polygon","coordinates":[[[116.30,39.85],[116.50,39.85],[116.50,39.95],[116.30,39.95],[116.30,39.85]]]}' />
                <div class="toolbar">
                    <span>这批坐标来自：</span>
                    <el-radio-group v-model="pasteFrom" size="small">
                        <el-radio-button value="wgs84">WGS-84（GPS / OSM / 买家端定位）</el-radio-button>
                        <el-radio-button value="gcj02">GCJ-02（高德 / 腾讯）</el-radio-button>
                        <el-radio-button value="bd09">BD-09（百度）</el-radio-button>
                    </el-radio-group>
                    <span class="grow" />
                    <el-button size="small" type="primary" plain @click="applyPaste">解析并放到地图上</el-button>
                </div>
                <el-alert v-for="w in pasteWarnings" :key="w" type="warning" :closable="false" :title="w" class="mb8" />
                <el-alert v-if="pasteNote" type="success" :closable="false" :title="pasteNote" class="mb8" />
                <p class="hint">放到地图上之后还要点「保存围栏」才会提交。</p>
            </el-collapse-item>
            <el-collapse-item title="当前草稿的 GeoJSON（WGS-84，可复制）" name="json">
                <pre class="geojson">{{ draftGeoJson || "（顶点不足 3 个）" }}</pre>
            </el-collapse-item>
        </el-collapse>
    </div>
</template>

<style scoped>
.map {
    height: 460px;
    border: 1px solid var(--el-border-color);
    border-radius: 4px;
}
.toolbar {
    display: flex;
    align-items: center;
    gap: 8px;
    flex-wrap: wrap;
    margin: 8px 0;
}
.grow {
    flex: 1;
}
.mb8 {
    margin-bottom: 8px;
}
.geojson {
    white-space: pre-wrap;
    word-break: break-all;
    font-size: 12px;
    background: var(--el-fill-color-light);
    padding: 8px;
    border-radius: 4px;
}
code {
    background: var(--el-fill-color-light);
    padding: 1px 4px;
    border-radius: 3px;
}
</style>

<style>
/* divIcon 的样式必须是全局的：Leaflet 把 marker 挂在 scoped 样式够不着的地方。 */
.fence-vertex {
    background: #fff;
    border: 2px solid #409eff;
    border-radius: 50%;
    box-shadow: 0 0 2px rgba(0, 0, 0, 0.4);
    cursor: move;
}
</style>
