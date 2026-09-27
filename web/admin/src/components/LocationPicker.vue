<script setup lang="ts">
// 门店坐标的地图选点：点地图落点，拖动标记微调。
//
// 与 FenceEditor 同一套地图（Leaflet + OSM 瓦片，WGS-84），理由写在那个文件头：
// 库里存的是 WGS-84，在这张图上点出来的坐标原样就是要存的值，没有换算这一步。
//
// 规则（2026-09-27）：门店必须有坐标；有围栏时门店必须在围栏内。围栏在这里画成虚线
// 只是为了让人看得见边界 —— 「在不在内」由服务端判（ST_Covers，边界上算在内），
// 前端不另写一套判据，两边迟早分叉。越界时服务端回 422 store-outside-fence。

import { onBeforeUnmount, onMounted, ref, watch } from "vue";
import L from "leaflet";
import "leaflet/dist/leaflet.css";
import type { GeoPolygon } from "../api/client.ts";
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

const emit = defineEmits<{ "update:modelValue": [v: LatLng] }>();

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

function pick(ll: L.LatLng): void {
    if (props.readonly) return;
    emit("update:modelValue", { lat: round6(ll.lat), lng: round6(ll.lng) });
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
        fenceLayer = L.polygon(outer.map(toLatLng), {
            color: "#909399",
            weight: 1,
            dashArray: "4 4",
            fillOpacity: 0.05,
            interactive: false,
        }).addTo(map);
    }
}

function fitView(): void {
    if (map === null) return;
    if (props.modelValue) {
        map.setView(props.modelValue, 15);
    } else if (fenceLayer) {
        map.fitBounds(fenceLayer.getBounds(), { padding: [30, 30], maxZoom: 16 });
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
    map.on("click", (e: L.LeafletMouseEvent) => pick(e.latlng));
    drawFence();
    drawMarker();
    fitView();
    // 放在对话框 / el-tabs 里时容器一开始是 0 尺寸，监听尺寸而不是监听打开事件。
    resizeObserver = new ResizeObserver(() => map?.invalidateSize());
    resizeObserver.observe(mapEl.value);
});

onBeforeUnmount(() => {
    resizeObserver?.disconnect();
    map?.remove();
    map = null;
    marker = null;
});

watch(() => props.modelValue, drawMarker);
watch(() => props.fence, drawFence);
</script>

<template>
    <div class="picker">
        <div ref="mapEl" class="map" :style="{ height: height ?? '320px' }" />
        <p class="hint">
            <template v-if="modelValue">
                已选：纬度 {{ modelValue.lat.toFixed(6) }}，经度 {{ modelValue.lng.toFixed(6) }}（WGS-84）。
                <template v-if="!readonly">点地图换位置，或拖动标记微调。</template>
            </template>
            <template v-else-if="!readonly">在地图上点一下门店所在的位置（必填）。</template>
            <template v-else>还没有定位。</template>
            <template v-if="fence">虚线是这家店的围栏，门店必须落在围栏内。</template>
        </p>
    </div>
</template>

<style scoped>
.picker {
    width: 100%;
}
.map {
    width: 100%;
    border: 1px solid var(--el-border-color);
    border-radius: 4px;
}
.hint {
    margin: 6px 0 0;
    color: var(--el-text-color-secondary);
    font-size: 12px;
    line-height: 1.5;
}
:global(.store-pin) {
    background: #e6a23c;
    border: 3px solid #fff;
    border-radius: 50%;
    box-shadow: 0 0 0 1px #e6a23c, 0 1px 4px rgba(0, 0, 0, 0.4);
}
</style>
