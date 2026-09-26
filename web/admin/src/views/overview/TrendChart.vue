<script setup lang="ts">
// 销售趋势折线：支付金额与退款金额两条线（同一个单位「分」，一根纵轴），
// 订单数不上图 —— 它是另一个量纲，放进同一张图就得有第二根纵轴，那会让两条线的
// 相对高低变成坐标轴的任意选择。订单数在悬停提示里。
//
// 纯 SVG，零依赖（几何在 src/api/reports.ts，有单元测试）。宽度随容器变：
// viewBox 固定、preserveAspectRatio="none" 会把字压扁，所以量容器宽度再画。
import { computed, onBeforeUnmount, onMounted, ref } from "vue";
import type { ReportTrendPoint } from "../../api/client.ts";
import { compactYuan, linePath, nearestIndex, niceTicks, xAt, yAt, type PlotBox } from "../../api/reports.ts";
import { yuan } from "../../ui/format.ts";

const props = defineProps<{ points: ReportTrendPoint[]; granularity: "hour" | "day" }>();

// 两条线的颜色过了 dataviz 的配色校验（浅色底：明度带、色度、色盲可分、对比度全过）。
const SERIES = [
    { key: "paid_amount_cents", name: "支付金额", color: "#2f6fdb" },
    { key: "refund_amount_cents", name: "退款金额", color: "#d9730d" },
] as const;

const host = ref<HTMLElement | null>(null);
const width = ref(640);
let ro: ResizeObserver | null = null;
onMounted(() => {
    if (host.value === null) return;
    width.value = host.value.clientWidth || 640;
    ro = new ResizeObserver((entries) => {
        const w = entries[0]?.contentRect.width;
        if (w !== undefined && w > 0) width.value = w;
    });
    ro.observe(host.value);
});
onBeforeUnmount(() => ro?.disconnect());

const box = computed<PlotBox>(() => ({ width: width.value, height: 240, left: 56, right: 16, top: 12, bottom: 28 }));
// 全零（或很小）时纵轴至少到 ¥100：否则五根刻度全写着 ¥0，一条贴底的线看不出是「没有成交」。
const ticks = computed(() =>
    niceTicks(Math.max(10_000, ...props.points.flatMap((p) => [p.paid_amount_cents, p.refund_amount_cents]))),
);
const top = computed(() => ticks.value[ticks.value.length - 1] ?? 1);
const paths = computed(() =>
    SERIES.map((s) => ({ ...s, d: linePath(props.points.map((p) => p[s.key]), top.value, box.value) })),
);

/** 横轴标签：点太多时隔几个标一个，免得挤在一起。 */
const xLabels = computed(() => {
    const n = props.points.length;
    const every = Math.max(1, Math.ceil(n / Math.max(2, Math.floor((width.value - 72) / 56))));
    return props.points
        .map((p, i) => ({ i, text: p.label, x: xAt(i, n, box.value) }))
        .filter((l) => l.i % every === 0 || l.i === n - 1);
});

const hover = ref<number | null>(null);
function onMove(ev: MouseEvent): void {
    const svg = ev.currentTarget as SVGSVGElement;
    const x = ev.clientX - svg.getBoundingClientRect().left;
    hover.value = props.points.length === 0 ? null : nearestIndex(x, props.points.length, box.value);
}
const hoverPoint = computed(() => (hover.value === null ? null : (props.points[hover.value] ?? null)));
const hoverX = computed(() => (hover.value === null ? 0 : xAt(hover.value, props.points.length, box.value)));
</script>

<template>
    <div ref="host" class="trend">
        <div class="legend">
            <span v-for="s in SERIES" :key="s.key" class="legend-item">
                <i :style="{ background: s.color }" />{{ s.name }}
            </span>
            <span class="legend-note">{{ granularity === "hour" ? "按小时" : "按天" }}（店铺时区）</span>
        </div>
        <svg
            :width="box.width"
            :height="box.height"
            role="img"
            aria-label="销售趋势：支付金额与退款金额"
            @mousemove="onMove"
            @mouseleave="hover = null"
        >
            <g class="grid">
                <g v-for="t in ticks" :key="t">
                    <line :x1="box.left" :x2="box.width - box.right" :y1="yAt(t, top, box)" :y2="yAt(t, top, box)" />
                    <text :x="box.left - 8" :y="yAt(t, top, box) + 4" text-anchor="end">{{ compactYuan(t) }}</text>
                </g>
                <text v-for="l in xLabels" :key="l.i" :x="l.x" :y="box.height - 8" text-anchor="middle">{{ l.text }}</text>
            </g>
            <line
                v-if="hoverPoint"
                class="crosshair"
                :x1="hoverX"
                :x2="hoverX"
                :y1="box.top"
                :y2="box.height - box.bottom"
            />
            <path v-for="p in paths" :key="p.key" :d="p.d" :stroke="p.color" class="line" />
            <template v-if="hoverPoint">
                <circle
                    v-for="s in SERIES"
                    :key="s.key"
                    :cx="hoverX"
                    :cy="yAt(Math.max(0, hoverPoint[s.key]), top, box)"
                    r="4"
                    :fill="s.color"
                    class="dot"
                />
            </template>
        </svg>
        <div
            v-if="hoverPoint"
            class="tip"
            :style="{ left: `${Math.min(hoverX + 12, box.width - 180)}px` }"
        >
            <div class="tip-title">{{ hoverPoint.label }}</div>
            <div v-for="s in SERIES" :key="s.key" class="tip-row">
                <i :style="{ background: s.color }" />{{ s.name }}<b>{{ yuan(hoverPoint[s.key]) }}</b>
            </div>
            <div class="tip-row">净销售额<b>{{ yuan(hoverPoint.net_sales_cents) }}</b></div>
            <div class="tip-row">支付订单<b>{{ hoverPoint.order_count }} 单</b></div>
        </div>
    </div>
</template>

<style scoped>
.trend {
    position: relative;
    width: 100%;
}
.legend {
    display: flex;
    gap: 16px;
    align-items: center;
    font-size: 12px;
    color: var(--el-text-color-regular);
    margin-bottom: 4px;
}
.legend-item i,
.tip-row i {
    display: inline-block;
    width: 10px;
    height: 2px;
    margin-right: 6px;
    vertical-align: middle;
}
.legend-note {
    margin-left: auto;
    color: var(--el-text-color-secondary);
}
svg {
    display: block;
}
.grid line {
    stroke: var(--el-border-color-lighter);
    stroke-width: 1;
}
.grid text {
    font-size: 11px;
    fill: var(--el-text-color-secondary);
}
.line {
    fill: none;
    stroke-width: 2;
    stroke-linejoin: round;
    stroke-linecap: round;
}
.crosshair {
    stroke: var(--el-border-color);
    stroke-dasharray: 3 3;
}
.dot {
    stroke: #fff;
    stroke-width: 2;
}
.tip {
    position: absolute;
    top: 28px;
    width: 168px;
    pointer-events: none;
    background: var(--el-bg-color-overlay);
    border: 1px solid var(--el-border-color-light);
    border-radius: 4px;
    box-shadow: var(--el-box-shadow-light);
    padding: 8px 10px;
    font-size: 12px;
    color: var(--el-text-color-regular);
}
.tip-title {
    font-weight: 600;
    margin-bottom: 4px;
    color: var(--el-text-color-primary);
}
.tip-row {
    display: flex;
    align-items: center;
    line-height: 20px;
}
.tip-row b {
    margin-left: auto;
    font-weight: 500;
    color: var(--el-text-color-primary);
    font-variant-numeric: tabular-nums;
}
</style>
