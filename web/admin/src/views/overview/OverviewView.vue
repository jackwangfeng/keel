<script setup lang="ts">
// 经营概览（后台首页）：指标卡 + 环比、销售趋势、商品排行、门店 / 大区对比、库存预警、搜索概况。
//
// 六块各自调一条 GET /admin/reports/*，各自有自己的加载态与错误 —— 一块失败（比如
// 大区管理员调搜索概况是 403）不该把整页拖成一片红。
//
// **口径全在服务端**（契约 ReportWindow / ReportMetrics），这里只展示；每张指标卡的
// 问号提示写的是契约里那一句口径，免得运营把「销售额」理解成下单金额。
// **范围也在服务端**：大区 / 门店管理员拿到的本来就只有自己范围内的数，这里不再过滤。
// 搜索概况只对全店范围的人显示（服务端对其他人 403，契约 GET /admin/reports/search）。

import { computed, onMounted, ref, watch } from "vue";
import { useRoute, useRouter } from "vue-router";
import { Download, QuestionFilled, Refresh } from "@element-plus/icons-vue";
import {
    downloadAuthedFile,
    keel,
    type AdminCategory,
    type AdminStore,
    type ReportInventoryAlerts,
    type ReportMetrics,
    type ReportOverview,
    type ReportProductRanking,
    type ReportSearchOverview,
    type ReportStoreComparison,
    type ReportTrend,
} from "../../api/client.ts";
import { listAllStores } from "../../api/stores.ts";
import { indentedLabel, listCategories } from "../../api/catalog.ts";
import {
    MAX_CUSTOM_DAYS,
    PERIOD_OPTIONS,
    delta,
    deltaText,
    inclusiveDays,
    previousLabel,
    rateDeltaText,
    rateText,
    reportCsvPath,
    reportQuery,
    type ReportCsvKind,
    type ReportFilterForm,
} from "../../api/reports.ts";
import { merchantWide } from "../../auth/permissions.ts";
import { yuan } from "../../ui/format.ts";
import { notifyError } from "../../ui/notify.ts";
import ProblemAlert from "../../components/ProblemAlert.vue";
import TrendChart from "./TrendChart.vue";

const route = useRoute();
const router = useRouter();

// 地址栏的 ?period= 决定打开时看哪一段（今日 / 近 7 天……），方便把一个视图发给别人。
const initialPeriod = PERIOD_OPTIONS.find((o) => o.value === route.query["period"] && o.value !== "custom")?.value ?? "today";
const filters = ref<ReportFilterForm>({ period: initialPeriod, dateRange: null, storeId: null });
const rangeError = ref("");

const stores = ref<AdminStore[]>([]);
const categories = ref<AdminCategory[]>([]);
const canSeeSearch = computed(() => merchantWide());

// ------------------------------------------------------------------ 六块数据

interface Block<T> {
    loading: boolean;
    error: unknown;
    data: T | null;
}
function block<T>() {
    return ref<Block<T>>({ loading: false, error: null, data: null });
}
const overview = block<ReportOverview>();
const trend = block<ReportTrend>();
const products = block<ReportProductRanking>();
const storeCmp = block<ReportStoreComparison>();
const alerts = block<ReportInventoryAlerts>();
const search = block<ReportSearchOverview>();

const sortBy = ref<"amount" | "quantity">("amount");
const categoryId = ref<number | null>(null);
const storeView = ref<"stores" | "regions">("stores");

async function run<T>(b: { value: Block<T> }, fetcher: () => Promise<T>): Promise<void> {
    b.value = { ...b.value, loading: true, error: null };
    try {
        b.value = { loading: false, error: null, data: await fetcher() };
    } catch (err) {
        b.value = { loading: false, error: err, data: null };
    }
}

function windowQuery() {
    rangeError.value = "";
    const q = reportQuery(filters.value);
    if (q === null) {
        rangeError.value = "请选择自定义的起止日期";
        return null;
    }
    if (q.start_date !== undefined && q.end_date !== undefined && inclusiveDays(q.start_date, q.end_date) > MAX_CUSTOM_DAYS) {
        rangeError.value = `自定义时间最多 ${MAX_CUSTOM_DAYS} 天（服务端同样会拒绝）：大范围的现场聚合会压在交易库上`;
        return null;
    }
    return q;
}

/** 商品排行的查询。界面那张表与 CSV 导出共用这一份：导出的就是你看到的那张表。 */
function productsQuery() {
    const q = windowQuery();
    if (q === null) return null;
    return { ...q, sort_by: sortBy.value, limit: 10, ...(categoryId.value === null ? {} : { category_id: categoryId.value }) };
}

function loadProducts(): void {
    const query = productsQuery();
    if (query === null) return;
    void run(products, () => keel.get("/admin/reports/products", { query }));
}

const exporting = ref<ReportCsvKind | null>(null);

/** 导出 CSV（UTF-8 带 BOM，Excel 直接打开）。判权与范围同那张表，服务端再判一遍。 */
async function exportCsv(kind: ReportCsvKind): Promise<void> {
    let query: Record<string, string | number | undefined> | null;
    if (kind === "products") {
        query = productsQuery();
    } else {
        const q = windowQuery();
        // 门店对比本来就按门店展开，不吃门店筛选（与上面那张表一致）。
        query = q === null ? null : { period: q.period, start_date: q.start_date, end_date: q.end_date };
    }
    if (query === null) return;
    exporting.value = kind;
    try {
        await downloadAuthedFile(reportCsvPath(kind, query), kind === "products" ? "商品排行.csv" : "门店对比.csv");
    } catch (err) {
        notifyError(err);
    } finally {
        exporting.value = null;
    }
}

function loadAll(): void {
    const q = windowQuery();
    if (q === null) return;
    const { store_id, ...win } = q;
    void run(overview, () => keel.get("/admin/reports/overview", { query: q }));
    void run(trend, () => keel.get("/admin/reports/trend", { query: q }));
    loadProducts();
    // 门店对比本来就按门店展开，不吃门店筛选。
    void run(storeCmp, () => keel.get("/admin/reports/stores", { query: win }));
    void run(alerts, () =>
        keel.get("/admin/reports/inventory-alerts", { query: { limit: 20, ...(store_id === undefined ? {} : { store_id }) } }),
    );
    // 搜索概况没有门店维度，也不吃门店筛选。
    if (canSeeSearch.value) void run(search, () => keel.get("/admin/reports/search", { query: { ...win, limit: 10 } }));
}

watch(() => [filters.value.period, filters.value.dateRange, filters.value.storeId], loadAll);
watch([sortBy, categoryId], loadProducts);

onMounted(async () => {
    loadAll();
    try {
        stores.value = (await listAllStores()).stores;
    } catch {
        // 下拉框取不到只影响筛选，不影响看板本身。
    }
    try {
        categories.value = await listCategories();
    } catch {
        // 同上。
    }
});

// ------------------------------------------------------------------ 指标卡

interface Card {
    key: string;
    title: string;
    hint: string;
    value: string;
    prev: string;
    change: string;
    /** 变化的好坏：退款涨了是坏事。 */
    tone: "good" | "bad" | "flat";
}

function tone(cur: number, prev: number, upIsGood: boolean): Card["tone"] {
    if (cur === prev) return "flat";
    return cur > prev === upIsGood ? "good" : "bad";
}

function moneyCard(key: string, title: string, hint: string, c: number, p: number, upIsGood = true): Card {
    return { key, title, hint, value: yuan(c), prev: yuan(p), change: deltaText(delta(c, p)), tone: tone(c, p, upIsGood) };
}
function countCard(key: string, title: string, hint: string, c: number, p: number): Card {
    return { key, title, hint, value: String(c), prev: String(p), change: deltaText(delta(c, p)), tone: tone(c, p, true) };
}

/** 退款卡的变化行：金额环比 + 本期退款率；两期都有退款率时再补一句百分点变化。 */
function refundChange(c: ReportMetrics, p: ReportMetrics): string {
    const base = `${deltaText(delta(c.refund_amount_cents, p.refund_amount_cents))} · 退款率 ${rateText(c.refund_rate)}`;
    const pp = rateDeltaText(c.refund_rate, p.refund_rate);
    return pp === "—" ? base : `${base}（${pp}）`;
}

const cards = computed<Card[]>(() => {
    const d = overview.value.data;
    if (d === null) return [];
    const c: ReportMetrics = d.current;
    const p: ReportMetrics = d.previous;
    return [
        moneyCard("net", "销售额（净）", "支付金额 − 退款金额。可以为负（退的比卖的多）。", c.net_sales_cents, p.net_sales_cents),
        moneyCard("paid", "支付金额", "按支付时间计：已支付的订单（待发货、已发货、已完成、退款中、已退款）的实付之和，含运费、已扣券。草稿、待支付、已关闭不计。", c.paid_amount_cents, p.paid_amount_cents),
        countCard("orders", "支付订单数", "支付时间落在这段时间里的已支付订单笔数。", c.order_count, p.order_count),
        countCard("buyers", "支付买家数", "上述订单的下单买家去重数。", c.buyer_count, p.buyer_count),
        moneyCard("aov", "客单价", "支付金额 ÷ 支付买家数，四舍五入到分。", c.avg_order_value_cents, p.avg_order_value_cents),
        {
            key: "refund",
            title: "退款金额",
            hint: "按到账时间计：退款到账（已退款）的金额之和，不论那一单哪天付的款。审核中、退款中、已驳回不计。退款率 = 退款金额 ÷ 支付金额，可以大于 100%（本期退的是上期卖的）。",
            value: yuan(c.refund_amount_cents),
            prev: `${yuan(p.refund_amount_cents)} · 退款率 ${rateText(p.refund_rate)}`,
            change: refundChange(c, p),
            tone: tone(c.refund_amount_cents, p.refund_amount_cents, false),
        },
    ];
});

const windowCaption = computed(() => {
    const w = overview.value.data?.window;
    if (w === undefined) return "";
    const cur = w.current.start_date === w.current.end_date ? w.current.start_date : `${w.current.start_date} ～ ${w.current.end_date}`;
    return `${cur}（${w.timezone}）· ${previousLabel(w.period)}`;
});

// ------------------------------------------------------------------ 门店对比

const maxStoreNet = computed(() =>
    Math.max(1, ...(storeCmp.value.data?.stores ?? []).map((s) => Math.max(0, s.net_sales_cents))),
);
const maxRegionNet = computed(() =>
    Math.max(1, ...(storeCmp.value.data?.regions ?? []).map((r) => Math.max(0, r.net_sales_cents))),
);
function barWidth(v: number, max: number): string {
    return `${Math.max(0, Math.round((v / max) * 100))}%`;
}

function openStore(id: number): void {
    void router.push({ name: "store-detail", params: { storeId: String(id) } });
}
</script>

<template>
    <div class="overview">
        <div class="page-toolbar">
            <el-radio-group v-model="filters.period" data-test="period">
                <el-radio-button v-for="o in PERIOD_OPTIONS" :key="o.value" :value="o.value">{{ o.label }}</el-radio-button>
            </el-radio-group>
            <el-date-picker
                v-if="filters.period === 'custom'"
                v-model="filters.dateRange"
                type="daterange"
                value-format="YYYY-MM-DD"
                start-placeholder="开始"
                end-placeholder="结束"
                style="width: 240px"
            />
            <el-select v-model="filters.storeId" clearable filterable placeholder="全部门店（你的范围内）" style="width: 200px">
                <el-option v-for="s in stores" :key="s.id" :label="s.name" :value="s.id" />
            </el-select>
            <el-button :icon="Refresh" @click="loadAll">刷新</el-button>
            <span class="caption">{{ windowCaption }}</span>
        </div>
        <el-alert v-if="rangeError" :title="rangeError" type="warning" :closable="false" show-icon class="mb12" />

        <!-- 指标卡 -->
        <ProblemAlert v-if="overview.error" :error="overview.error" />
        <div v-loading="overview.loading" class="cards">
            <el-card v-for="c in cards" :key="c.key" shadow="never" class="card" :data-test="`card-${c.key}`">
                <div class="card-title">
                    {{ c.title }}
                    <el-tooltip :content="c.hint" placement="top" :show-after="200">
                        <el-icon class="hint"><QuestionFilled /></el-icon>
                    </el-tooltip>
                </div>
                <div class="card-value">{{ c.value }}</div>
                <div class="card-change" :class="c.tone">{{ c.change }}</div>
                <div class="card-prev">上一周期 {{ c.prev }}</div>
            </el-card>
        </div>

        <!-- 趋势 -->
        <el-card shadow="never" class="section">
            <template #header>销售趋势</template>
            <ProblemAlert v-if="trend.error" :error="trend.error" />
            <div v-loading="trend.loading">
                <TrendChart v-if="trend.data" :points="trend.data.points" :granularity="trend.data.granularity" />
            </div>
        </el-card>

        <div class="grid2">
            <!-- 商品排行 -->
            <el-card shadow="never" class="section">
                <template #header>
                    <div class="section-head">
                        <span>商品排行 Top 10</span>
                        <div class="section-tools">
                            <el-select v-model="categoryId" clearable filterable placeholder="全部类目" size="small" style="width: 150px">
                                <el-option v-for="c in categories" :key="c.id" :label="indentedLabel(c)" :value="c.id" />
                            </el-select>
                            <el-radio-group v-model="sortBy" size="small">
                                <el-radio-button value="amount">销售额</el-radio-button>
                                <el-radio-button value="quantity">销量</el-radio-button>
                            </el-radio-group>
                            <el-button size="small" :icon="Download" :loading="exporting === 'products'"
                                data-test="export-products" @click="exportCsv('products')">导出</el-button>
                        </div>
                    </div>
                </template>
                <ProblemAlert v-if="products.error" :error="products.error" />
                <el-table v-loading="products.loading" :data="products.data?.items ?? []" size="small" empty-text="这段时间没有成交">
                    <el-table-column prop="rank" label="#" width="44" />
                    <el-table-column label="商品" min-width="160" show-overflow-tooltip>
                        <template #default="{ row }">
                            <router-link :to="{ name: 'product-detail', params: { productId: String(row.product_id) } }">{{ row.title }}</router-link>
                        </template>
                    </el-table-column>
                    <el-table-column label="销量" width="80" align="right">
                        <template #default="{ row }">{{ row.quantity }}</template>
                    </el-table-column>
                    <el-table-column label="销售额" width="110" align="right">
                        <template #default="{ row }">{{ yuan(row.amount_cents) }}</template>
                    </el-table-column>
                    <el-table-column label="已退" width="110" align="right">
                        <template #default="{ row }">
                            <span class="muted">{{ row.refunded_quantity }} 件 / {{ yuan(row.refunded_amount_cents) }}</span>
                        </template>
                    </el-table-column>
                </el-table>
                <p class="foot">销售额 = 订单行实付分摊（不含运费），按支付时间计；「已退」是这些订单行截至此刻已退的，不按退款时间切。</p>
            </el-card>

            <!-- 门店 / 大区对比 -->
            <el-card shadow="never" class="section">
                <template #header>
                    <div class="section-head">
                        <span>门店对比 · 净销售额</span>
                        <div class="section-tools">
                            <el-radio-group v-model="storeView" size="small">
                                <el-radio-button value="stores">按门店</el-radio-button>
                                <el-radio-button value="regions">按大区</el-radio-button>
                            </el-radio-group>
                            <el-button size="small" :icon="Download" :loading="exporting === 'stores'"
                                data-test="export-stores" @click="exportCsv('stores')">导出</el-button>
                        </div>
                    </div>
                </template>
                <ProblemAlert v-if="storeCmp.error" :error="storeCmp.error" />
                <div v-loading="storeCmp.loading" class="bars">
                    <template v-if="storeView === 'stores'">
                        <div v-for="s in storeCmp.data?.stores ?? []" :key="s.store_id" class="bar-row">
                            <div class="bar-name" :title="s.region_name">
                                <el-link v-if="!s.deleted" @click="openStore(s.store_id)">{{ s.store_name }}</el-link>
                                <span v-else class="muted">{{ s.store_name }}（已删除）</span>
                            </div>
                            <div class="bar-track"><div class="bar" :style="{ width: barWidth(s.net_sales_cents, maxStoreNet) }" /></div>
                            <div class="bar-value">{{ yuan(s.net_sales_cents) }}<small>{{ s.order_count }} 单</small></div>
                        </div>
                    </template>
                    <template v-else>
                        <div v-for="r in storeCmp.data?.regions ?? []" :key="r.region_id" class="bar-row">
                            <div class="bar-name">{{ r.region_name }}<small class="muted">（{{ r.store_count }} 店）</small></div>
                            <div class="bar-track"><div class="bar" :style="{ width: barWidth(r.net_sales_cents, maxRegionNet) }" /></div>
                            <div class="bar-value">{{ yuan(r.net_sales_cents) }}<small>{{ r.order_count }} 单</small></div>
                        </div>
                    </template>
                    <el-empty v-if="(storeCmp.data?.stores.length ?? 0) === 0 && !storeCmp.loading" description="没有门店" :image-size="60" />
                </div>
                <p class="foot">大区取门店此刻所属的大区。没有成交的门店以 0 列出；已删除的门店只在这段时间有成交或退款时出现。</p>
            </el-card>
        </div>

        <div class="grid2">
            <!-- 库存预警 -->
            <el-card shadow="never" class="section">
                <template #header>
                    <div class="section-head">
                        <span>库存预警<small v-if="alerts.data" class="muted">（共 {{ alerts.data.total }} 条，最缺的在前）</small></span>
                    </div>
                </template>
                <ProblemAlert v-if="alerts.error" :error="alerts.error" />
                <el-table v-loading="alerts.loading" :data="alerts.data?.items ?? []" size="small" empty-text="没有低于预警线的库存" max-height="360">
                    <el-table-column label="门店" prop="store_name" width="120" show-overflow-tooltip />
                    <el-table-column label="商品 / 规格" min-width="160" show-overflow-tooltip>
                        <template #default="{ row }">
                            {{ row.product_title }}
                            <span class="muted">{{ Object.values(row.spec_values).join(" / ") || row.sku_code }}</span>
                        </template>
                    </el-table-column>
                    <el-table-column label="可售 / 预警线" width="110" align="right">
                        <template #default="{ row }">
                            <el-tag size="small" :type="row.available_qty === 0 ? 'danger' : 'warning'">
                                {{ row.available_qty === 0 ? "卖空" : "偏低" }}
                            </el-tag>
                            {{ row.available_qty }} / {{ row.warning_qty }}
                        </template>
                    </el-table-column>
                </el-table>
            </el-card>

            <!-- 搜索概况 -->
            <el-card v-if="canSeeSearch" shadow="never" class="section">
                <template #header>搜索概况</template>
                <ProblemAlert v-if="search.error" :error="search.error" />
                <div v-loading="search.loading">
                    <div v-if="search.data" class="search-stats">
                        <div><span class="muted">搜索次数</span><b>{{ search.data.search_count }}</b></div>
                        <div><span class="muted">无结果率</span><b>{{ rateText(search.data.zero_result_rate) }}</b></div>
                        <div><span class="muted">有点击的搜索</span><b>{{ search.data.click_count }}</b></div>
                    </div>
                    <div class="terms">
                        <div>
                            <h4>热门搜索词</h4>
                            <ol>
                                <li v-for="q in search.data?.top_queries ?? []" :key="q.query">
                                    <span>{{ q.query }}</span><small>{{ q.search_count }} 次</small>
                                </li>
                            </ol>
                            <p v-if="(search.data?.top_queries.length ?? 0) === 0" class="muted">这段时间没有搜索</p>
                        </div>
                        <div>
                            <h4>无结果搜索词 <small class="muted">买家在找、店里没有</small></h4>
                            <ol>
                                <li v-for="q in search.data?.zero_result_queries ?? []" :key="q.query">
                                    <span>{{ q.query }}</span><small>{{ q.zero_result_count }} / {{ q.search_count }} 次无结果</small>
                                </li>
                            </ol>
                            <p v-if="(search.data?.zero_result_queries.length ?? 0) === 0" class="muted">没有无结果的搜索</p>
                        </div>
                    </div>
                </div>
            </el-card>
        </div>
    </div>
</template>

<style scoped>
.caption {
    color: var(--el-text-color-secondary);
    font-size: 13px;
}
.mb12 {
    margin-bottom: 12px;
}
.cards {
    display: grid;
    grid-template-columns: repeat(auto-fill, minmax(170px, 1fr));
    gap: 12px;
    margin-bottom: 12px;
    min-height: 60px;
}
.card-title {
    font-size: 13px;
    color: var(--el-text-color-secondary);
    display: flex;
    align-items: center;
    gap: 4px;
}
.hint {
    cursor: help;
    color: var(--el-text-color-placeholder);
}
.card-value {
    font-size: 24px;
    font-weight: 600;
    margin: 6px 0 4px;
    font-variant-numeric: tabular-nums;
    color: var(--el-text-color-primary);
}
.card-change {
    font-size: 12px;
}
.card-change.good {
    color: var(--el-color-success);
}
.card-change.bad {
    color: var(--el-color-danger);
}
.card-change.flat {
    color: var(--el-text-color-secondary);
}
.card-prev {
    font-size: 12px;
    color: var(--el-text-color-placeholder);
    margin-top: 2px;
}
.section {
    margin-bottom: 12px;
}
.grid2 {
    display: grid;
    grid-template-columns: repeat(auto-fit, minmax(420px, 1fr));
    gap: 12px;
}
.section-head {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 8px;
    flex-wrap: wrap;
}
.section-tools {
    display: flex;
    gap: 8px;
}
.muted {
    color: var(--el-text-color-secondary);
}
.foot {
    font-size: 12px;
    color: var(--el-text-color-placeholder);
    margin: 8px 0 0;
}
.bars {
    min-height: 60px;
}
.bar-row {
    display: grid;
    grid-template-columns: 150px 1fr 130px;
    align-items: center;
    gap: 8px;
    height: 28px;
    font-size: 13px;
}
.bar-name {
    overflow: hidden;
    white-space: nowrap;
    text-overflow: ellipsis;
}
.bar-track {
    height: 10px;
}
.bar {
    height: 100%;
    background: #2f6fdb;
    border-radius: 0 4px 4px 0;
    min-width: 1px;
}
.bar-value {
    text-align: right;
    font-variant-numeric: tabular-nums;
}
.bar-value small {
    margin-left: 6px;
    color: var(--el-text-color-secondary);
}
.search-stats {
    display: flex;
    gap: 24px;
    margin-bottom: 8px;
}
.search-stats div {
    display: flex;
    flex-direction: column;
}
.search-stats b {
    font-size: 20px;
    font-variant-numeric: tabular-nums;
}
.terms {
    display: grid;
    grid-template-columns: 1fr 1fr;
    gap: 16px;
}
.terms h4 {
    margin: 8px 0;
    font-size: 13px;
}
.terms ol {
    margin: 0;
    padding-left: 20px;
    font-size: 13px;
}
.terms li {
    line-height: 24px;
}
.terms li small {
    margin-left: 8px;
    color: var(--el-text-color-secondary);
}
</style>
