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
    type GeoPlace,
    type GeoPolygon,
    type StoreUpdateRequest,
} from "../api/client.ts";
import { detailAddress } from "../api/geo.ts";
import { fenceErrorPoint } from "../api/errors.ts";
import { deleteLocalDelivery, getLocalDelivery, putLocalDelivery, type AdminLocalDelivery } from "../api/localDelivery.ts";
import { tiersSummary, type RuleConfig } from "../api/localDeliveryRules.ts";
import { listLocalDeliveryTemplates, type LocalDeliveryTemplate } from "../api/localDeliveryTemplates.ts";
import { centsToYuanInput } from "../api/money.ts";
import { isIncomplete, listAllRegions } from "../api/stores.ts";
import { listAllStoreInventories } from "../api/storeInventory.ts";
import { datetime } from "../ui/format.ts";
import { notifyError, notifyOk } from "../ui/notify.ts";
import { can, NO_PERMISSION } from "../auth/permissions.ts";
import ProblemAlert from "../components/ProblemAlert.vue";
import FenceEditor from "../components/FenceEditor.vue";
import LocationPicker, { type LatLng } from "../components/LocationPicker.vue";
import ScopedProducts from "../components/ScopedProducts.vue";
import InventoryDialog, { type InventoryTarget } from "../components/InventoryDialog.vue";
import LocalDeliveryRuleForm from "../components/LocalDeliveryRuleForm.vue";

const props = defineProps<{ storeId: string }>();
const route = useRoute();
const router = useRouter();
const id = computed(() => Number(props.storeId));

const tabFromQuery = typeof route.query["tab"] === "string" ? route.query["tab"] : "basic";
const tab = ref(
    ["basic", "fence", "products", "inventory", "local-delivery"].includes(tabFromQuery) ? tabFromQuery : "basic",
);

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
    if (tab.value === "local-delivery") void loadLocalDelivery();
});

const center = computed(() => {
    const s = store.value;
    return s && typeof s.lat === "number" && typeof s.lng === "number" ? { lat: s.lat, lng: s.lng } : null;
});

// ---------------------------------------------------------------- 基本信息

const form = ref<StoreUpdateRequest>({});
/** 地图选的点。门店必须有坐标（2026-09-27）：老门店没有时要先选，才能保存基本信息。 */
const point = ref<LatLng | null>(null);
const saving = ref(false);
const saveError = ref<unknown>(null);
/** 上一次是不是由「选点」自动填的地址：给个提示，不是锁字段——填完照样能改。 */
const addressAutofilled = ref(false);

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
    };
    point.value = typeof s.lat === "number" && typeof s.lng === "number" ? { lat: s.lat, lng: s.lng } : null;
    addressAutofilled.value = false;
}

/** LocationPicker 搜索选点 / 点地图之后回填省市区与地址；填完用户还能改。 */
function onPlace(p: GeoPlace): void {
    form.value.province = p.province;
    form.value.city = p.city;
    form.value.district = p.district;
    form.value.address = detailAddress(p);
    addressAutofilled.value = true;
}

const regionChanged = computed(() => store.value !== null && form.value.region_id !== store.value.region_id);

async function saveBasic(): Promise<void> {
    saving.value = true;
    saveError.value = null;
    try {
        const body: StoreUpdateRequest = { ...form.value };
        const s = store.value;
        // 坐标只在变了的时候发：服务端对改坐标会查「还在不在围栏内」，没动就不该触发那条判定。
        if (point.value !== null && (s === null || point.value.lat !== s.lat || point.value.lng !== s.lng)) {
            body.lat = point.value.lat;
            body.lng = point.value.lng;
        }
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
            // 契约说 ST_IsValidReason 在 detail 里；**实测服务端把它放在 title 里、
            // detail 缺席**（internal/handler/admin_store.go 用的是只写 title 的
            // problem.Write）。两处都找，谁有用谁——界面上反正两个都原样显示。
            fenceErrorAt.value =
                fenceErrorPoint(err.problem.detail ?? "") ?? fenceErrorPoint(err.problem.title);
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
    if (name === "local-delivery") void loadLocalDelivery();
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

// ---------------------------------------------------------------- 同城配送
//
// 三种模式（对应 AdminLocalDelivery.source，00111）：
//   跟随默认模板 —— 门店什么都没存，DELETE 回到这个状态；
//   使用模板     —— PUT { template_id }；
//   自定义       —— PUT 三个字段（起送价 / 满额免配送费 / 距离分档），交给
//                    LocalDeliveryRuleForm（与模板管理共用同一份表单 + 校验）。
// 顶层三个数永远是「生效的」规则，不管当前是哪种模式，都在页面上摊开显示
// （来源 + 具体数字），免得运营切来切去时看不出「现在到底按什么收」。

type LdMode = "default" | "template" | "custom";

function modeOf(ld: AdminLocalDelivery): LdMode {
    if (ld.source === "custom") return "custom";
    if (ld.source === "template") return "template";
    return "default"; // default_template | none
}

const ldLoading = ref(false);
const ldLoadError = ref<unknown>(null);
const ldSaveError = ref<unknown>(null);
const ldLocalError = ref("");
const ldSaving = ref(false);
const localDelivery = ref<AdminLocalDelivery | null>(null);
const ldTemplates = ref<LocalDeliveryTemplate[]>([]);
const ldMode = ref<LdMode>("default");
const ldTemplateId = ref<number | null>(null);
const ruleFormRef = ref<InstanceType<typeof LocalDeliveryRuleForm> | null>(null);

/** 默认模板的名字，给「跟随默认模板」那一档单选按钮的说明用；商家还没设默认模板时说清楚。 */
const defaultTemplateName = computed(() => ldTemplates.value.find((t) => t.is_default)?.name ?? null);

/** 「自定义」那一档的初始值：有自己存过的 custom 就用它，否则退回当前生效的规则。 */
const customInitial = computed<RuleConfig | null>(() => {
    const ld = localDelivery.value;
    if (ld === null) return null;
    if (ld.custom) return ld.custom;
    return { min_order_cents: ld.min_order_cents, free_over_cents: ld.free_over_cents, fee_tiers: ld.fee_tiers };
});

function sourceLabel(ld: AdminLocalDelivery): string {
    if (ld.source === "custom") return "门店自定义";
    if (ld.source === "template") return `模板「${ld.template_name ?? `#${ld.template_id}`}」`;
    if (ld.source === "default_template") return `默认模板「${ld.template_name ?? `#${ld.template_id}`}」`;
    return "无（还没设过默认模板，门店也没有自定义规则）";
}

async function loadLocalDelivery(): Promise<void> {
    ldLoading.value = true;
    ldLoadError.value = null;
    try {
        const [ld, templates] = await Promise.all([getLocalDelivery(id.value), listLocalDeliveryTemplates()]);
        localDelivery.value = ld;
        ldTemplates.value = templates;
        ldMode.value = modeOf(ld);
        ldTemplateId.value = ld.source === "template" ? (ld.template_id ?? null) : null;
    } catch (err) {
        ldLoadError.value = err;
    } finally {
        ldLoading.value = false;
    }
}

async function saveLocalDelivery(): Promise<void> {
    ldLocalError.value = "";
    ldSaveError.value = null;

    if (ldMode.value === "template" && ldTemplateId.value === null) {
        ldLocalError.value = "请选一个模板";
        return;
    }

    let cfg: RuleConfig | null = null;
    if (ldMode.value === "custom") {
        cfg = ruleFormRef.value?.submit() ?? null;
        if (cfg === null) return; // 规则表单已经把错误显示在自己的错误条上
    }

    ldSaving.value = true;
    try {
        if (ldMode.value === "default") {
            localDelivery.value = await deleteLocalDelivery(id.value);
        } else if (ldMode.value === "template") {
            localDelivery.value = await putLocalDelivery(id.value, { template_id: ldTemplateId.value! });
        } else {
            localDelivery.value = await putLocalDelivery(id.value, cfg!);
        }
        ldMode.value = modeOf(localDelivery.value);
        ldTemplateId.value = localDelivery.value.source === "template" ? (localDelivery.value.template_id ?? null) : null;
        notifyOk("已保存");
    } catch (err) {
        ldSaveError.value = err;
    } finally {
        ldSaving.value = false;
    }
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
                <el-button :disabled="store.is_default || store.status !== 1 || !can.setDefaultStore()" @click="makeDefault">设为默认门店</el-button>
                <el-button type="danger" plain :icon="Delete" :disabled="!can.manageStore(store)" :title="can.manageStore(store) ? '' : NO_PERMISSION" @click="remove">删除</el-button>
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
                        <el-form-item label="地址">
                            <el-input v-model="form.address" @input="addressAutofilled = false" />
                            <p v-if="addressAutofilled" class="hint">已按选点填写地址，可修改。</p>
                        </el-form-item>
                        <el-form-item label="位置" required>
                            <LocationPicker
                                v-if="tab === 'basic'"
                                v-model="point"
                                :fence="store.fence ?? null"
                                :readonly="!can.manageStore(store)"
                                @place="onPlace"
                            />
                            <p class="hint">接单范围看围栏；这个点是门店自己的位置，「按距离排」按它算，而且必须落在围栏内。</p>
                        </el-form-item>
                        <el-form-item label="营业状态">
                            <el-radio-group v-model="form.status">
                                <el-radio :value="1">营业</el-radio>
                                <el-radio :value="0">停业</el-radio>
                            </el-radio-group>
                            <p class="hint">停业的门店不参与围栏判定，也不能下单。</p>
                        </el-form-item>
                        <el-form-item>
                            <el-button type="primary" :loading="saving" :disabled="!can.manageStore(store) || point === null" :title="!can.manageStore(store) ? NO_PERMISSION : point === null ? '先在地图上选门店位置' : ''" @click="saveBasic">保存</el-button>
                            <span class="hint ml8">更新于 {{ datetime(store.updated_at) }}</span>
                        </el-form-item>
                    </el-form>
                </el-tab-pane>

                <!-- ------------------------------------------------- 围栏 -->
                <el-tab-pane label="围栏" name="fence">
                    <ProblemAlert v-if="fenceError" :error="fenceError" />
                    <el-alert v-if="center === null" type="warning" :closable="false" show-icon class="mb12"
                        title="这家门店还没有坐标。先到「基本信息」用地图选点，再画围栏——门店必须落在自己的围栏内。" />
                    <FenceEditor
                        :saved="store.fence ?? null"
                        :center="center"
                        :error-point="fenceErrorAt"
                        :busy="fenceBusy"
                        :is-default="store.is_default"
                        :readonly="!can.manageStore(store)"
                        @save="saveFence"
                    />
                </el-tab-pane>

                <!-- -------------------------------------------- 商品与定价 -->
                <el-tab-pane label="商品与定价" name="products" lazy>
                    <ScopedProducts
                        :scope="{ kind: 'store', id: store.id, regionId: store.region_id, name: store.name, regionName: store.region_name ?? `#${store.region_id}` }"
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
                                <el-button link type="primary" :disabled="!can.operateStore(store)" :title="can.operateStore(store) ? '' : NO_PERMISSION" @click="openInventory(row)">改库存</el-button>
                            </template>
                        </el-table-column>
                    </el-table>
                    <el-empty v-if="!invLoading && inventories.length === 0" description="这家店还没有任何库存行" :image-size="70" />
                </el-tab-pane>

                <!-- ------------------------------------------------ 同城配送 -->
                <el-tab-pane label="同城配送" name="local-delivery" lazy>
                    <ProblemAlert v-if="ldLoadError" :error="ldLoadError" />
                    <div v-loading="ldLoading">
                        <template v-if="localDelivery">
                            <el-alert
                                :type="localDelivery.active ? 'success' : 'info'"
                                :closable="false"
                                show-icon
                                class="mb12"
                                :title="
                                    localDelivery.active
                                        ? '本店有围栏，按下面的配置收配送费（不读运费模板）'
                                        : '本店是默认门店或没有围栏，走运费模板，这里的配置暂不生效'
                                "
                            />

                            <el-descriptions title="生效规则" :column="1" border size="small" class="mb12">
                                <el-descriptions-item label="来源">{{ sourceLabel(localDelivery) }}</el-descriptions-item>
                                <el-descriptions-item label="起送价">
                                    {{ localDelivery.min_order_cents > 0 ? `${centsToYuanInput(localDelivery.min_order_cents)} 元` : "不设" }}
                                </el-descriptions-item>
                                <el-descriptions-item label="满多少免配送费">
                                    {{ localDelivery.free_over_cents > 0 ? `${centsToYuanInput(localDelivery.free_over_cents)} 元` : "不设" }}
                                </el-descriptions-item>
                                <el-descriptions-item label="距离分档">{{ tiersSummary(localDelivery.fee_tiers) }}</el-descriptions-item>
                            </el-descriptions>

                            <ProblemAlert v-if="ldSaveError" :error="ldSaveError" />
                            <el-alert v-if="ldLocalError" :title="ldLocalError" type="error" :closable="false" show-icon class="mb12" />

                            <el-form label-width="140px" style="max-width: 640px" @submit.prevent>
                                <el-form-item label="配送费怎么算">
                                    <el-radio-group v-model="ldMode" :disabled="!can.operateStore(store)">
                                        <el-radio value="default">
                                            跟随默认模板（当前：{{ defaultTemplateName ?? "未设默认模板" }}）
                                        </el-radio>
                                        <el-radio value="template">使用模板</el-radio>
                                        <el-radio value="custom">自定义</el-radio>
                                    </el-radio-group>
                                </el-form-item>

                                <el-form-item v-if="ldMode === 'template'" label="选模板">
                                    <el-select v-model="ldTemplateId" style="width: 280px" :disabled="!can.operateStore(store)" placeholder="选一个模板">
                                        <el-option v-for="t in ldTemplates" :key="t.id" :value="t.id" :label="t.is_default ? `${t.name}（默认）` : t.name" />
                                    </el-select>
                                </el-form-item>

                                <template v-if="ldMode === 'custom'">
                                    <LocalDeliveryRuleForm ref="ruleFormRef" :initial="customInitial" :disabled="!can.operateStore(store)" />
                                </template>

                                <el-form-item>
                                    <el-button
                                        type="primary"
                                        :loading="ldSaving"
                                        :disabled="!can.operateStore(store)"
                                        :title="can.operateStore(store) ? '' : NO_PERMISSION"
                                        @click="saveLocalDelivery"
                                    >保存</el-button>
                                    <span v-if="localDelivery.updated_at" class="hint ml8">更新于 {{ datetime(localDelivery.updated_at) }}</span>
                                </el-form-item>
                            </el-form>
                        </template>
                    </div>
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
