<script setup lang="ts">
// 一家门店：基本信息、围栏、商品与定价、库存。
//
// 围栏那一页是这个后台里唯一「画错了不报错、只是悄悄判错店」的地方，
// 所以它的两件事写在最显眼处：
//   · 坐标系是 WGS-84（components/FenceEditor.vue 文件头）；
//   · 服务端 ST_IsValid 拒绝时，detail 里 PostGIS 的原话原样显示，
//     方括号里的出错位置在地图上画红圈。

import { computed, onMounted, ref } from "vue";
import { useRoute, useRouter } from "vue-router";
import { ArrowLeft, Delete, Refresh } from "@element-plus/icons-vue";
import { ElMessageBox } from "element-plus";
import {
    isProblemType,
    keel,
    ProblemError,
    ProblemType,
    type AdminInventory,
    type AdminRegion,
    type AdminStore,
    type GeoPolygon,
    type StoreUpdateRequest,
} from "../api/client.ts";
import { fenceErrorPoint } from "../api/errors.ts";
import { isIncomplete, listAllRegions } from "../api/stores.ts";
import { listAllStoreInventories } from "../api/storeInventory.ts";
import { datetime } from "../ui/format.ts";
import { notifyError, notifyOk } from "../ui/notify.ts";
import ProblemAlert from "../components/ProblemAlert.vue";
import FenceEditor from "../components/FenceEditor.vue";
import ScopedProducts from "../components/ScopedProducts.vue";
import InventoryDialog, { type InventoryTarget } from "../components/InventoryDialog.vue";

const props = defineProps<{ storeId: string }>();
const route = useRoute();
const router = useRouter();
const id = computed(() => Number(props.storeId));

const tabFromQuery = typeof route.query["tab"] === "string" ? route.query["tab"] : "basic";
const tab = ref(["basic", "fence", "products", "inventory"].includes(tabFromQuery) ? tabFromQuery : "basic");

const loading = ref(false);
const loadError = ref<unknown>(null);
const store = ref<AdminStore | null>(null);
const regions = ref<AdminRegion[]>([]);

async function load(): Promise<void> {
    loading.value = true;
    loadError.value = null;
    try {
        store.value = await keel.get("/admin/stores/{store_id}", { path: { store_id: id.value } });
        syncForm();
    } catch (err) {
        loadError.value = err;
    } finally {
        loading.value = false;
    }
}

onMounted(() => {
    void load();
    listAllRegions().then(
        (rs) => (regions.value = rs),
        (e: unknown) => notifyError(e),
    );
    if (tab.value === "inventory") void loadInventory();
});

const center = computed(() => {
    const s = store.value;
    return s && typeof s.lat === "number" && typeof s.lng === "number" ? { lat: s.lat, lng: s.lng } : null;
});

// ---------------------------------------------------------------- 基本信息

const form = ref<StoreUpdateRequest>({});
const saving = ref(false);
const saveError = ref<unknown>(null);

function syncForm(): void {
    const s = store.value;
    if (s === null) return;
    form.value = {
        region_id: s.region_id,
        code: s.code,
        name: s.name,
        phone: s.phone ?? "",
        province: s.province ?? "",
        city: s.city ?? "",
        district: s.district ?? "",
        address: s.address ?? "",
        status: s.status,
        ...(typeof s.lat === "number" ? { lat: s.lat } : {}),
        ...(typeof s.lng === "number" ? { lng: s.lng } : {}),
    };
}

const regionChanged = computed(() => store.value !== null && form.value.region_id !== store.value.region_id);

async function saveBasic(): Promise<void> {
    saving.value = true;
    saveError.value = null;
    try {
        const body: StoreUpdateRequest = { ...form.value };
        for (const k of ["lat", "lng"] as const) if (body[k] === null || body[k] === undefined) delete body[k];
        store.value = await keel.request("patch", "/admin/stores/{store_id}", { path: { store_id: id.value }, body });
        syncForm();
        notifyOk("已保存");
    } catch (err) {
        saveError.value = err;
    } finally {
        saving.value = false;
    }
}

// ---------------------------------------------------------------- 默认 / 删除

async function makeDefault(): Promise<void> {
    try {
        store.value = await keel.request("put", "/admin/stores/{store_id}/default", { path: { store_id: id.value } });
        notifyOk("现在是默认门店：不在任何围栏内的买家都回落到它");
    } catch (err) {
        notifyError(err);
    }
}

async function remove(): Promise<void> {
    const s = store.value;
    if (s === null) return;
    try {
        await ElMessageBox.confirm(
            s.is_default ? "这是默认门店，删掉后商家就没有回落目标了。确定删？" : `软删门店「${s.name}」？`,
            "确认删除",
            { type: "warning" },
        );
    } catch {
        return;
    }
    try {
        await keel.request("delete", "/admin/stores/{store_id}", { path: { store_id: id.value } });
        notifyOk("已软删");
        await router.push({ name: "stores" });
    } catch (err) {
        notifyError(err);
    }
}

// ---------------------------------------------------------------- 围栏

const fenceBusy = ref(false);
const fenceError = ref<unknown>(null);
const fenceErrorAt = ref<[number, number] | null>(null);

async function saveFence(fence: GeoPolygon | null): Promise<void> {
    fenceBusy.value = true;
    fenceError.value = null;
    fenceErrorAt.value = null;
    try {
        store.value = await keel.request("put", "/admin/stores/{store_id}/fence", {
            path: { store_id: id.value },
            body: { fence },
        });
        notifyOk(fence === null ? "围栏已清空" : "围栏已保存");
    } catch (err) {
        fenceError.value = err;
        if (isProblemType(err, ProblemType.invalidFence) && err instanceof ProblemError) {
            fenceErrorAt.value = fenceErrorPoint(err.problem.detail ?? "");
        }
    } finally {
        fenceBusy.value = false;
    }
}

// ---------------------------------------------------------------- 库存

const invLoading = ref(false);
const invError = ref<unknown>(null);
const inventories = ref<AdminInventory[]>([]);
const lowOnly = ref(false);
const highlightSku = typeof route.query["sku"] === "string" ? Number(route.query["sku"]) : null;

async function loadInventory(): Promise<void> {
    invLoading.value = true;
    invError.value = null;
    try {
        inventories.value = await listAllStoreInventories(id.value, lowOnly.value);
    } catch (err) {
        invError.value = err;
    } finally {
        invLoading.value = false;
    }
}

function onTabChange(name: string | number): void {
    if (name === "inventory") void loadInventory();
    void router.replace({ query: { ...route.query, tab: String(name) } });
}

const invDialog = ref(false);
const invTarget = ref<InventoryTarget | null>(null);

function openInventory(row: AdminInventory): void {
    invTarget.value = {
        skuId: row.sku_id,
        skuCode: row.sku_code ?? `#${row.sku_id}`,
        availableQty: row.available_qty,
        warningQty: row.warning_qty,
    };
    invDialog.value = true;
}

function onInventoryUpdated(inv: AdminInventory): void {
    const i = inventories.value.findIndex((r) => r.sku_id === inv.sku_id);
    if (i >= 0) inventories.value[i] = inv;
    else inventories.value.push(inv);
}
</script>

<template>
    <div v-loading="loading">
        <ProblemAlert v-if="loadError" :error="loadError" />

        <template v-if="store">
            <div class="page-toolbar">
                <el-button :icon="ArrowLeft" @click="router.push({ name: 'stores' })">返回门店</el-button>
                <span class="title">{{ store.name }}</span>
                <el-tag v-if="store.is_default" type="primary" effect="dark" size="small">默认门店</el-tag>
                <el-tag :type="store.status === 1 ? 'success' : 'info'" size="small">{{ store.status === 1 ? "营业" : "停业" }}</el-tag>
                <el-tag v-if="isIncomplete(store)" type="warning" effect="dark" size="small">未完成</el-tag>
                <span class="hint">大区：{{ store.region_name ?? `#${store.region_id}` }}</span>
                <span class="grow" />
                <el-button :disabled="store.is_default || store.status !== 1" @click="makeDefault">设为默认门店</el-button>
                <el-button type="danger" plain :icon="Delete" @click="remove">删除</el-button>
            </div>

            <el-alert v-if="isIncomplete(store)" type="warning" :closable="false" show-icon class="mb12">
                <template #title>这家店现在接不到任何单</template>
                它没有围栏，也不是默认门店：不会被任何买家的坐标命中，也不是回落目标。到「围栏」里画一个，或者把它设为默认门店。
            </el-alert>

            <el-tabs v-model="tab" type="border-card" @tab-change="onTabChange">
                <!-- ----------------------------------------------- 基本信息 -->
                <el-tab-pane label="基本信息" name="basic">
                    <ProblemAlert v-if="saveError" :error="saveError" />
                    <el-form label-width="100px" style="max-width: 720px" @submit.prevent>
                        <el-form-item label="大区">
                            <el-select v-model="form.region_id">
                                <el-option v-for="r in regions" :key="r.id" :value="r.id" :label="r.name" />
                            </el-select>
                            <p v-if="regionChanged" class="warn">
                                换大区会同时改变这家店的<b>价格与可见性</b>——大区是两层覆盖里的外层。
                            </p>
                        </el-form-item>
                        <el-form-item label="编号"><el-input v-model="form.code" /></el-form-item>
                        <el-form-item label="名称"><el-input v-model="form.name" /></el-form-item>
                        <el-form-item label="电话"><el-input v-model="form.phone" /></el-form-item>
                        <el-form-item label="省 / 市 / 区">
                            <div class="row3">
                                <el-input v-model="form.province" placeholder="省" />
                                <el-input v-model="form.city" placeholder="市" />
                                <el-input v-model="form.district" placeholder="区" />
                            </div>
                        </el-form-item>
                        <el-form-item label="地址"><el-input v-model="form.address" /></el-form-item>
                        <el-form-item label="坐标">
                            <div class="row3">
                                <el-input-number v-model="form.lat" :precision="6" :step="0.001" :min="-90" :max="90" placeholder="纬度" controls-position="right" />
                                <el-input-number v-model="form.lng" :precision="6" :step="0.001" :min="-180" :max="180" placeholder="经度" controls-position="right" />
                            </div>
                            <p class="hint">WGS-84。它只用于展示与地图定位，接单范围看围栏，不看这个点。</p>
                        </el-form-item>
                        <el-form-item label="营业状态">
                            <el-radio-group v-model="form.status">
                                <el-radio :value="1">营业</el-radio>
                                <el-radio :value="0">停业</el-radio>
                            </el-radio-group>
                            <p class="hint">停业的门店不参与围栏判定，也不能下单。</p>
                        </el-form-item>
                        <el-form-item>
                            <el-button type="primary" :loading="saving" @click="saveBasic">保存</el-button>
                            <span class="hint ml8">更新于 {{ datetime(store.updated_at) }}</span>
                        </el-form-item>
                    </el-form>
                </el-tab-pane>

                <!-- ------------------------------------------------- 围栏 -->
                <el-tab-pane label="围栏" name="fence">
                    <ProblemAlert v-if="fenceError" :error="fenceError" />
                    <FenceEditor
                        :saved="store.fence ?? null"
                        :center="center"
                        :error-point="fenceErrorAt"
                        :busy="fenceBusy"
                        :is-default="store.is_default"
                        @save="saveFence"
                    />
                </el-tab-pane>

                <!-- -------------------------------------------- 商品与定价 -->
                <el-tab-pane label="商品与定价" name="products" lazy>
                    <ScopedProducts
                        :scope="{ kind: 'store', id: store.id, name: store.name, regionName: store.region_name ?? `#${store.region_id}` }"
                    />
                </el-tab-pane>

                <!-- ------------------------------------------------- 库存 -->
                <el-tab-pane label="库存" name="inventory">
                    <ProblemAlert v-if="invError" :error="invError" />
                    <div class="page-toolbar">
                        <el-checkbox v-model="lowOnly" @change="loadInventory">只看低于预警线的</el-checkbox>
                        <span class="hint">
                            只列出已经录过的行。<b>缺行 = 0，不是「不卖」</b>——新店还没录库存的 SKU 在「商品与定价」里展开商品逐个录入。
                        </span>
                        <span class="grow" />
                        <el-button :icon="Refresh" :loading="invLoading" @click="loadInventory">刷新</el-button>
                    </div>
                    <el-table :data="inventories" v-loading="invLoading" border stripe :row-class-name="({ row }: { row: AdminInventory }) => (row.sku_id === highlightSku ? 'hl-row' : '')">
                        <el-table-column prop="sku_id" label="SKU ID" width="90" />
                        <el-table-column label="货号" min-width="140">
                            <template #default="{ row }: { row: AdminInventory }">{{ row.sku_code ?? "—" }}</template>
                        </el-table-column>
                        <el-table-column label="可售" width="100">
                            <template #default="{ row }: { row: AdminInventory }">
                                <b :class="{ low: row.available_qty <= row.warning_qty }">{{ row.available_qty }}</b>
                            </template>
                        </el-table-column>
                        <el-table-column prop="warning_qty" label="预警线" width="100" />
                        <el-table-column label="更新时间" width="180">
                            <template #default="{ row }: { row: AdminInventory }">{{ datetime(row.updated_at) }}</template>
                        </el-table-column>
                        <el-table-column label="操作" width="120">
                            <template #default="{ row }: { row: AdminInventory }">
                                <el-button link type="primary" @click="openInventory(row)">改库存</el-button>
                            </template>
                        </el-table-column>
                    </el-table>
                    <el-empty v-if="!invLoading && inventories.length === 0" description="这家店还没有任何库存行" :image-size="70" />
                </el-tab-pane>
            </el-tabs>
        </template>

        <InventoryDialog
            v-model="invDialog"
            :sku="invTarget"
            :store-id="id"
            :store-name="store?.name"
            @updated="onInventoryUpdated"
        />
    </div>
</template>

<style scoped>
.title {
    font-size: 16px;
    font-weight: 600;
}
.mb12 {
    margin-bottom: 12px;
}
.ml8 {
    margin-left: 8px;
}
.row3 {
    display: flex;
    gap: 8px;
    width: 100%;
}
.warn {
    margin: 4px 0 0;
    font-size: 12px;
    color: var(--el-color-warning-dark-2);
}
.low {
    color: var(--el-color-danger);
}
:deep(.hl-row) {
    --el-table-tr-bg-color: var(--el-color-warning-light-9);
}
</style>
