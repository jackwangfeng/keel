<script setup lang="ts">
// 售后（GET /admin/refunds）：退款单列表，点一行打开详情抽屉，在抽屉里审核 / 确认收到退货。
//
// 打开时默认筛「待审核」（status=10）—— 这是售后每天第一眼要看的那一堆；
// 「待买家退货」（20）是第二堆，状态下拉里切。范围由服务端收窄（与订单页同一个判据）。
// 地址栏的 ?refund_no= 会直接打开那一张（订单详情跳过来时用）。

import { computed, onMounted, ref, watch } from "vue";
import { useRoute, useRouter } from "vue-router";
import { Refresh, Search } from "@element-plus/icons-vue";
import { keel, type AdminRefund, type AdminRefundPage, type AdminStore } from "../../api/client.ts";
import { listAllStores } from "../../api/stores.ts";
import { REFUND_STATUS, REFUND_TYPE, refundQuery, type RefundFilterForm } from "../../api/orderRules.ts";
import { datetime, yuan } from "../../ui/format.ts";
import { notifyError } from "../../ui/notify.ts";
import ProblemAlert from "../../components/ProblemAlert.vue";
import RefundDetailDrawer from "./RefundDetailDrawer.vue";

const route = useRoute();
const router = useRouter();

const loading = ref(false);
const error = ref<unknown>(null);
const page = ref<AdminRefundPage | null>(null);
const pageNo = ref(1);
const pageSize = ref(20);

const initialNo = typeof route.query["refund_no"] === "string" ? route.query["refund_no"] : "";
// 从订单详情点进来的那一张可能不是待审核，所以带着单号来的时候不预设状态。
const filters = ref<RefundFilterForm>(initialNo === "" ? { status: 10 } : {});

const stores = ref<AdminStore[]>([]);
const storeMap = computed(() => new Map(stores.value.map((s) => [s.id, s])));

async function load(): Promise<void> {
    loading.value = true;
    error.value = null;
    try {
        page.value = await keel.get("/admin/refunds", { query: refundQuery(filters.value, pageNo.value, pageSize.value) });
    } catch (err) {
        error.value = err;
    } finally {
        loading.value = false;
    }
}

function search(): void {
    pageNo.value = 1;
    void load();
}

onMounted(async () => {
    void load();
    try {
        stores.value = (await listAllStores()).stores;
    } catch (err) {
        notifyError(err);
    }
});

const openNo = ref<string | null>(initialNo === "" ? null : initialNo);
function openDetail(row: AdminRefund): void {
    openNo.value = row.refund_no;
}
function closeDetail(): void {
    openNo.value = null;
    if (route.query["refund_no"] !== undefined) void router.replace({ query: {} });
}
watch(
    () => route.query["refund_no"],
    (v) => {
        if (typeof v === "string" && v !== "") openNo.value = v;
    },
);

const statusOptions = Object.entries(REFUND_STATUS).map(([k, v]) => ({ value: Number(k) as keyof typeof REFUND_STATUS, label: v.text }));
</script>

<template>
    <div>
        <ProblemAlert v-if="error" :error="error" />

        <el-form :inline="true" class="filters" @submit.prevent="search">
            <el-form-item label="状态">
                <el-select v-model="filters.status" clearable placeholder="全部" style="width: 140px">
                    <el-option v-for="o in statusOptions" :key="o.value" :label="o.label" :value="o.value" />
                </el-select>
            </el-form-item>
            <el-form-item label="门店">
                <el-select v-model="filters.storeId" clearable filterable placeholder="全部（你的范围内）" style="width: 180px">
                    <el-option v-for="s in stores" :key="s.id" :label="s.name" :value="s.id" />
                </el-select>
            </el-form-item>
            <el-form-item label="申请日期">
                <el-date-picker
                    v-model="filters.dateRange"
                    type="daterange"
                    value-format="YYYY-MM-DD"
                    start-placeholder="开始"
                    end-placeholder="结束"
                    style="width: 240px"
                />
            </el-form-item>
            <el-form-item>
                <el-button type="primary" :icon="Search" native-type="submit">查询</el-button>
                <el-button
                    @click="
                        filters = {};
                        search();
                    "
                >
                    清空
                </el-button>
                <el-button :icon="Refresh" :loading="loading" @click="load">刷新</el-button>
            </el-form-item>
        </el-form>

        <el-table
            v-loading="loading"
            :data="page?.items ?? []"
            row-key="refund_no"
            empty-text="没有符合条件的退款单"
            highlight-current-row
            @row-click="openDetail"
        >
            <el-table-column label="退款单号" min-width="220">
                <template #default="{ row }">
                    <el-link type="primary">{{ row.refund_no }}</el-link>
                    <div class="hint">订单 {{ row.order_no }}</div>
                </template>
            </el-table-column>
            <el-table-column label="状态" width="120">
                <template #default="{ row }">
                    <el-tag size="small" :type="REFUND_STATUS[row.status as keyof typeof REFUND_STATUS].tag">
                        {{ REFUND_STATUS[row.status as keyof typeof REFUND_STATUS].text }}
                    </el-tag>
                </template>
            </el-table-column>
            <el-table-column label="类型" width="100">
                <template #default="{ row }">{{ REFUND_TYPE[row.refund_type as keyof typeof REFUND_TYPE] }}</template>
            </el-table-column>
            <el-table-column label="门店" min-width="120">
                <template #default="{ row }">{{ row.store.store_name || `门店 #${row.store_id}` }}</template>
            </el-table-column>
            <el-table-column label="实退" width="100">
                <template #default="{ row }">{{ yuan(row.amount_cents) }}</template>
            </el-table-column>
            <el-table-column label="申请时间" width="170">
                <template #default="{ row }">{{ datetime(row.created_at) }}</template>
            </el-table-column>
        </el-table>

        <el-pagination
            v-if="page"
            class="pager"
            layout="total, sizes, prev, pager, next"
            :total="page.total"
            :current-page="page.page"
            :page-size="page.page_size"
            :page-sizes="[10, 20, 50, 100]"
            @current-change="
                (n: number) => {
                    pageNo = n;
                    load();
                }
            "
            @size-change="
                (n: number) => {
                    pageSize = n;
                    search();
                }
            "
        />

        <RefundDetailDrawer :refund-no="openNo" :stores="storeMap" @close="closeDetail" @changed="load" />
    </div>
</template>

<style scoped>
.filters {
    margin-bottom: 4px;
}
.pager {
    margin-top: 12px;
    justify-content: flex-end;
}
</style>
