<script setup lang="ts">
// 订单（GET /admin/orders）：筛选、分页，点一行打开详情抽屉，已支付的单可以直接发货。
//
// **范围由服务端收窄**：大区管理员拿到的本来就只有本大区门店的单，门店管理员只有自己门店的，
// 这里不再过滤一遍（过滤只有服务端那一份）。门店下拉用的 GET /admin/stores 同样是收窄过的。
//
// 「待发货」是这一页最常用的视图，所以打开时默认筛 status=20；清掉就是全部。
// 地址栏的 ?order_no= 会直接打开那一单（售后页跳过来时用）。

import { computed, onMounted, ref, watch } from "vue";
import { useRoute, useRouter } from "vue-router";
import { Refresh, Search } from "@element-plus/icons-vue";
import { keel, type AdminOrderPage, type AdminOrderSummary, type AdminStore } from "../../api/client.ts";
import { listAllStores } from "../../api/stores.ts";
import { ORDER_REFUND_STATUS, ORDER_STATUS, orderQuery, shipAction, type OrderFilterForm } from "../../api/orderRules.ts";
import { can } from "../../auth/permissions.ts";
import { datetime, yuan } from "../../ui/format.ts";
import { notifyError, notifyOk } from "../../ui/notify.ts";
import ProblemAlert from "../../components/ProblemAlert.vue";
import OrderDetailDrawer from "./OrderDetailDrawer.vue";
import ShipDialog from "./ShipDialog.vue";

const route = useRoute();
const router = useRouter();

const loading = ref(false);
const error = ref<unknown>(null);
const page = ref<AdminOrderPage | null>(null);
const pageNo = ref(1);
const pageSize = ref(20);

const initialNo = typeof route.query["order_no"] === "string" ? route.query["order_no"] : "";
const filters = ref<OrderFilterForm>(initialNo === "" ? { status: 20 } : { orderNo: initialNo });

const stores = ref<AdminStore[]>([]);
const storeMap = computed(() => new Map(stores.value.map((s) => [s.id, s])));

async function load(): Promise<void> {
    loading.value = true;
    error.value = null;
    try {
        page.value = await keel.get("/admin/orders", { query: orderQuery(filters.value, pageNo.value, pageSize.value) });
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

function reset(): void {
    filters.value = {};
    search();
}

onMounted(async () => {
    void load();
    try {
        stores.value = (await listAllStores()).stores;
    } catch (err) {
        notifyError(err);
    }
});

// ------------------------------------------------------------------ 详情抽屉

const openNo = ref<string | null>(initialNo === "" ? null : initialNo);
function openDetail(row: AdminOrderSummary): void {
    openNo.value = row.order_no;
}
function closeDetail(): void {
    openNo.value = null;
    if (route.query["order_no"] !== undefined) void router.replace({ query: {} });
}
watch(
    () => route.query["order_no"],
    (v) => {
        if (typeof v === "string" && v !== "") {
            filters.value = { orderNo: v };
            openNo.value = v;
            search();
        }
    },
);

// ------------------------------------------------------------------ 列表上直接发货

function canOperate(row: AdminOrderSummary): boolean {
    const st = storeMap.value.get(row.store_id);
    return can.handleOrder({ id: row.store_id, region_id: st?.region_id ?? row.region_id ?? 0 });
}
const shipNo = ref<string | null>(null);
const shipVisible = ref(false);
function openShip(row: AdminOrderSummary): void {
    shipNo.value = row.order_no;
    shipVisible.value = true;
}
function onShipped(): void {
    notifyOk(`已发货：${shipNo.value ?? ""}`);
    void load();
}

const statusOptions = Object.entries(ORDER_STATUS).map(([k, v]) => ({ value: Number(k) as keyof typeof ORDER_STATUS, label: v.text }));
</script>

<template>
    <div>
        <ProblemAlert v-if="error" :error="error" />

        <el-form :inline="true" class="filters" @submit.prevent="search">
            <el-form-item label="状态">
                <el-select v-model="filters.status" clearable placeholder="全部" style="width: 150px" data-test="status">
                    <el-option v-for="o in statusOptions" :key="o.value" :label="o.label" :value="o.value" />
                </el-select>
            </el-form-item>
            <el-form-item label="门店">
                <el-select v-model="filters.storeId" clearable filterable placeholder="全部（你的范围内）" style="width: 180px">
                    <el-option v-for="s in stores" :key="s.id" :label="s.name" :value="s.id" />
                </el-select>
            </el-form-item>
            <el-form-item label="下单日期">
                <el-date-picker
                    v-model="filters.dateRange"
                    type="daterange"
                    value-format="YYYY-MM-DD"
                    start-placeholder="开始"
                    end-placeholder="结束"
                    style="width: 240px"
                />
            </el-form-item>
            <el-form-item label="单号">
                <el-input v-model="filters.orderNo" clearable placeholder="精确匹配" style="width: 200px" />
            </el-form-item>
            <el-form-item label="手机号">
                <el-input v-model="filters.phone" clearable placeholder="收货人或买家，精确匹配" style="width: 190px" />
            </el-form-item>
            <el-form-item>
                <el-button type="primary" :icon="Search" native-type="submit">查询</el-button>
                <el-button @click="reset">清空</el-button>
                <el-button :icon="Refresh" :loading="loading" @click="load">刷新</el-button>
            </el-form-item>
        </el-form>

        <el-table
            v-loading="loading"
            :data="page?.items ?? []"
            row-key="order_no"
            empty-text="没有符合条件的订单"
            highlight-current-row
            @row-click="openDetail"
        >
            <el-table-column label="单号" min-width="220">
                <template #default="{ row }">
                    <el-link type="primary">{{ row.order_no }}</el-link>
                </template>
            </el-table-column>
            <el-table-column label="状态" width="170">
                <template #default="{ row }">
                    <el-tag size="small" :type="ORDER_STATUS[row.status as keyof typeof ORDER_STATUS].tag">
                        {{ ORDER_STATUS[row.status as keyof typeof ORDER_STATUS].text }}
                    </el-tag>
                    <el-tag v-if="row.has_open_refund" size="small" type="danger" class="ml4">售后中</el-tag>
                    <el-tag
                        v-else-if="row.refund_status !== 0"
                        size="small"
                        :type="ORDER_REFUND_STATUS[row.refund_status as keyof typeof ORDER_REFUND_STATUS].tag"
                        class="ml4"
                    >
                        {{ ORDER_REFUND_STATUS[row.refund_status as keyof typeof ORDER_REFUND_STATUS].text }}
                    </el-tag>
                </template>
            </el-table-column>
            <el-table-column label="收货人" min-width="150">
                <template #default="{ row }">
                    {{ row.receiver.receiver_name }}
                    <div class="hint">{{ row.receiver.phone }}</div>
                </template>
            </el-table-column>
            <el-table-column label="门店" min-width="120">
                <template #default="{ row }">{{ row.store.store_name || `门店 #${row.store_id}` }}</template>
            </el-table-column>
            <el-table-column label="实付" width="100">
                <template #default="{ row }">{{ yuan(row.paid_cents ?? 0) }}</template>
            </el-table-column>
            <el-table-column label="下单时间" width="170">
                <template #default="{ row }">{{ datetime(row.created_at) }}</template>
            </el-table-column>
            <el-table-column label="" width="90" fixed="right">
                <template #default="{ row }">
                    <el-button
                        v-if="shipAction(row, canOperate(row)).visible"
                        size="small"
                        type="primary"
                        :disabled="!shipAction(row, canOperate(row)).enabled"
                        :title="shipAction(row, canOperate(row)).hint"
                        @click.stop="openShip(row)"
                    >
                        发货
                    </el-button>
                </template>
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

        <OrderDetailDrawer :order-no="openNo" :stores="storeMap" @close="closeDetail" @changed="load" />
        <ShipDialog v-model="shipVisible" :order-no="shipNo" @shipped="onShipped" />
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
.ml4 {
    margin-left: 4px;
}
</style>
