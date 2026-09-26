<script setup lang="ts">
// 大区 / 门店维度的「商品可见性 + 生效价」。两个页面共用这一个组件，
// 因为两条路径的形状在契约里就是同一个（ScopedProductListing / ScopedSkuPrice），
// 差别只在路径与层级。
//
// ## 可见性是两层「与」，不是「或」
//
// 大区下架的商品，门店这一层开着也捞不回来。契约因此给了两个字段：
//   · `listed`           —— **本层**有没有把它排掉（这一行开关管的就是它）
//   · `effective_listed` —— 两层都算完之后买家到底看不看得见
// 界面把两个分开显示，并在「本层开着、买家却看不到」时说出原因。
// 只显示 listed 的话，门店运营会看到「已上架」而买家看不到，然后以为开关坏了。
//
// ## 定价是三层 COALESCE：门店价 → 大区价 → 基准价
//
// 契约**没有**「按 SKU 读出本层覆盖价」的接口：商品这一行只给生效价区间与
// `price_source`（这一行有没有被本地覆盖过）。所以展开到 SKU 时，这里能显示的是
// 基准价（从商品详情来）与**这次会话里写过的结果**（PUT 返回的 ScopedSkuPrice）；
// 没写过的 SKU 显示「看上面这一行的区间」。这是契约现在的形状，不是界面偷懒——
// 真要逐 SKU 回显，得在契约里加一条读接口。

import { computed, onMounted, ref } from "vue";
import { Refresh } from "@element-plus/icons-vue";
import { ElMessage } from "element-plus";
import {
    keel,
    type AdminInventory,
    type AdminSku,
    type ScopedProductListing,
    type ScopedProductPage,
    type ScopedSkuPrice,
} from "../api/client.ts";
import { listAllStoreInventories } from "../api/storeInventory.ts";
import { PRICE_SOURCE } from "../api/stores.ts";
import { PRODUCT_STATUS, yuan } from "../ui/format.ts";
import { notifyError, notifyOk } from "../ui/notify.ts";
import ProblemAlert from "./ProblemAlert.vue";
import InventoryDialog, { type InventoryTarget } from "./InventoryDialog.vue";

export type Scope =
    | { kind: "region"; id: number; name: string }
    | { kind: "store"; id: number; name: string; regionName: string };

const props = defineProps<{ scope: Scope }>();

const layer = computed(() => (props.scope.kind === "store" ? "本店" : "本大区"));

const loading = ref(false);
const error = ref<unknown>(null);
const page = ref<ScopedProductPage | null>(null);
const pageNo = ref(1);
const listedFilter = ref<boolean | undefined>(undefined);

async function fetchPage(): Promise<ScopedProductPage> {
    const query = {
        page: pageNo.value,
        page_size: 20,
        ...(listedFilter.value === undefined ? {} : { listed: listedFilter.value }),
    };
    const s = props.scope;
    return s.kind === "store"
        ? keel.get("/admin/stores/{store_id}/products", { path: { store_id: s.id }, query })
        : keel.get("/admin/regions/{region_id}/products", { path: { region_id: s.id }, query });
}

async function load(): Promise<void> {
    loading.value = true;
    error.value = null;
    try {
        page.value = await fetchPage();
        if (props.scope.kind === "store") await loadInventories();
    } catch (err) {
        error.value = err;
    } finally {
        loading.value = false;
    }
}

onMounted(() => void load());

// ---------------------------------------------------------------- 可见性

/** 本层开着、买家却看不到的原因。空串表示买家看得见。 */
function invisibleReason(row: ScopedProductListing): string {
    if (row.effective_listed) return "";
    if (!row.listed) return `${layer.value}已下架`;
    if (row.status !== undefined && row.status !== 1) {
        return "商品本身不是上架状态（草稿或已下架）。先到「商品」里上架";
    }
    if (props.scope.kind === "store") {
        return `被大区「${props.scope.regionName}」下架了——本店这一层开着也捞不回来（两层是「与」）。去大区页改`;
    }
    return "买家看不到";
}

const toggling = ref<number | null>(null);

async function setListed(row: ScopedProductListing, listed: boolean): Promise<void> {
    toggling.value = row.product_id;
    try {
        const s = props.scope;
        const body = { listed };
        const updated =
            s.kind === "store"
                ? await keel.request("put", "/admin/stores/{store_id}/products/{product_id}/listing", {
                      path: { store_id: s.id, product_id: row.product_id },
                      body,
                  })
                : await keel.request("put", "/admin/regions/{region_id}/products/{product_id}/listing", {
                      path: { region_id: s.id, product_id: row.product_id },
                      body,
                  });
        Object.assign(row, updated);
        if (updated.listed && !updated.effective_listed) {
            // 点了「上架」但买家仍看不到：不能报成功，要说为什么。
            ElMessage({
                type: "warning",
                duration: 8000,
                message: `${layer.value}这一层已经开了，但买家仍然看不到：${invisibleReason(updated)}`,
            });
        } else {
            notifyOk(listed ? `${layer.value}已上架` : `${layer.value}已下架`);
        }
    } catch (err) {
        notifyError(err);
    } finally {
        toggling.value = null;
    }
}

// ---------------------------------------------------------------- 定价

const expandedKeys = ref<number[]>([]);
const skusByProduct = ref(new Map<number, AdminSku[]>());
const skuLoadError = ref(new Map<number, unknown>());
/** 这次会话里 PUT 回来的结果。契约没有按 SKU 读覆盖价的接口，见文件头。 */
const priceResult = ref(new Map<number, ScopedSkuPrice>());
const priceDraft = ref(new Map<number, number>());
const priceBusy = ref<number | null>(null);

async function onExpand(row: ScopedProductListing, expanded: ScopedProductListing[]): Promise<void> {
    expandedKeys.value = expanded.map((r) => r.product_id);
    if (!expanded.includes(row) || skusByProduct.value.has(row.product_id)) return;
    try {
        const detail = await keel.get("/admin/products/{product_id}", { path: { product_id: row.product_id } });
        skusByProduct.value.set(row.product_id, detail.skus);
        for (const sku of detail.skus) if (!priceDraft.value.has(sku.id)) priceDraft.value.set(sku.id, sku.price_cents);
    } catch (err) {
        skuLoadError.value.set(row.product_id, err);
    }
}

async function setPrice(sku: AdminSku): Promise<void> {
    priceBusy.value = sku.id;
    try {
        const s = props.scope;
        const body = { price_cents: priceDraft.value.get(sku.id) ?? sku.price_cents };
        const res =
            s.kind === "store"
                ? await keel.request("put", "/admin/stores/{store_id}/skus/{sku_id}/price", {
                      path: { store_id: s.id, sku_id: sku.id },
                      body,
                  })
                : await keel.request("put", "/admin/regions/{region_id}/skus/{sku_id}/price", {
                      path: { region_id: s.id, sku_id: sku.id },
                      body,
                  });
        priceResult.value.set(sku.id, res);
        notifyOk(`${sku.sku_code} 生效价 ${yuan(res.effective_price_cents)}（来自${PRICE_SOURCE[res.price_source].text}）`);
        page.value = await fetchPage();
    } catch (err) {
        notifyError(err);
    } finally {
        priceBusy.value = null;
    }
}

async function revokePrice(sku: AdminSku): Promise<void> {
    priceBusy.value = sku.id;
    try {
        const s = props.scope;
        if (s.kind === "store") {
            await keel.request("delete", "/admin/stores/{store_id}/skus/{sku_id}/price", {
                path: { store_id: s.id, sku_id: sku.id },
            });
        } else {
            await keel.request("delete", "/admin/regions/{region_id}/skus/{sku_id}/price", {
                path: { region_id: s.id, sku_id: sku.id },
            });
        }
        priceResult.value.delete(sku.id);
        notifyOk(`${sku.sku_code} 撤销了${layer.value}价，回到上一层`);
        page.value = await fetchPage();
    } catch (err) {
        notifyError(err);
    } finally {
        priceBusy.value = null;
    }
}

// ------------------------------------------------------ 门店维度的库存

const inventoryBySku = ref(new Map<number, AdminInventory>());

async function loadInventories(): Promise<void> {
    if (props.scope.kind !== "store") return;
    const rows = await listAllStoreInventories(props.scope.id);
    inventoryBySku.value = new Map(rows.map((r) => [r.sku_id, r]));
}

const invDialog = ref(false);
const invTarget = ref<InventoryTarget | null>(null);

function openInventory(sku: AdminSku): void {
    const inv = inventoryBySku.value.get(sku.id);
    invTarget.value = {
        skuId: sku.id,
        skuCode: sku.sku_code,
        spec: Object.entries(sku.spec_values ?? {})
            .map(([k, v]) => `${k}:${v}`)
            .join(" / "),
        // 这家店缺这一行 = 0，而且首次录入时 expected 必须传 0（契约原话）。
        availableQty: inv?.available_qty ?? 0,
        warningQty: inv?.warning_qty ?? 0,
    };
    invDialog.value = true;
}

function onInventoryUpdated(inv: AdminInventory): void {
    inventoryBySku.value.set(inv.sku_id, inv);
}
</script>

<template>
    <div>
        <ProblemAlert v-if="error" :error="error" />

        <div class="page-toolbar">
            <el-select v-model="listedFilter" placeholder="全部" clearable style="width: 180px" @change="(pageNo = 1), load()">
                <el-option :value="true" :label="`${layer}在售`" />
                <el-option :value="false" :label="`${layer}已下架`" />
            </el-select>
            <span class="hint">
                {{ scope.kind === "store" ? "门店价是最内层；" : "大区价是中间层；" }}价 = 门店价 → 大区价 → 基准价，取第一个有的。
                可见性两层是「与」：大区下架的，门店捞不回来。
            </span>
            <span class="grow" />
            <el-button :icon="Refresh" :loading="loading" @click="load">刷新</el-button>
        </div>

        <el-table
            :data="page?.items ?? []"
            v-loading="loading"
            border
            row-key="product_id"
            :expand-row-keys="expandedKeys"
            @expand-change="onExpand"
        >
            <el-table-column type="expand">
                <template #default="{ row }: { row: ScopedProductListing }">
                    <div class="skus">
                        <ProblemAlert v-if="skuLoadError.get(row.product_id)" :error="skuLoadError.get(row.product_id)" />
                        <el-table :data="skusByProduct.get(row.product_id) ?? []" size="small" border>
                            <el-table-column prop="sku_code" label="货号" width="140" />
                            <el-table-column label="规格" min-width="120">
                                <template #default="{ row: sku }: { row: AdminSku }">
                                    {{ Object.entries(sku.spec_values ?? {}).map(([k, v]) => `${k}:${v}`).join(" / ") || "—" }}
                                </template>
                            </el-table-column>
                            <el-table-column label="基准价" width="100">
                                <template #default="{ row: sku }: { row: AdminSku }">{{ yuan(sku.price_cents) }}</template>
                            </el-table-column>
                            <el-table-column :label="`${layer}价（分）`" width="300">
                                <template #default="{ row: sku }: { row: AdminSku }">
                                    <el-input-number
                                        :model-value="priceDraft.get(sku.id) ?? sku.price_cents"
                                        :min="0"
                                        :step="100"
                                        size="small"
                                        @update:model-value="(v: number | undefined) => priceDraft.set(sku.id, v ?? 0)"
                                    />
                                    <el-button size="small" type="primary" link :loading="priceBusy === sku.id" @click="setPrice(sku)">
                                        设价
                                    </el-button>
                                    <el-button size="small" link :disabled="priceBusy === sku.id" @click="revokePrice(sku)">
                                        撤销
                                    </el-button>
                                </template>
                            </el-table-column>
                            <el-table-column label="生效价" min-width="170">
                                <template #default="{ row: sku }: { row: AdminSku }">
                                    <template v-if="priceResult.get(sku.id)">
                                        <b>{{ yuan(priceResult.get(sku.id)!.effective_price_cents) }}</b>
                                        <el-tag size="small" :type="PRICE_SOURCE[priceResult.get(sku.id)!.price_source].tag" class="ml4">
                                            {{ PRICE_SOURCE[priceResult.get(sku.id)!.price_source].text }}
                                        </el-tag>
                                    </template>
                                    <span v-else class="hint">看上面这一行的区间</span>
                                </template>
                            </el-table-column>
                            <el-table-column v-if="scope.kind === 'store'" label="本店库存" width="160">
                                <template #default="{ row: sku }: { row: AdminSku }">
                                    <span>{{ inventoryBySku.get(sku.id)?.available_qty ?? 0 }}</span>
                                    <span v-if="!inventoryBySku.get(sku.id)" class="hint">（未录入）</span>
                                    <el-button size="small" link type="primary" @click="openInventory(sku)">改</el-button>
                                </template>
                            </el-table-column>
                        </el-table>
                    </div>
                </template>
            </el-table-column>
            <el-table-column label="商品" min-width="220">
                <template #default="{ row }: { row: ScopedProductListing }">
                    <router-link :to="{ name: 'product-detail', params: { productId: row.product_id } }">
                        {{ row.title }}
                    </router-link>
                    <el-tag v-if="row.status !== undefined" :type="PRODUCT_STATUS[row.status].tag" size="small" class="ml4">
                        {{ PRODUCT_STATUS[row.status].text }}
                    </el-tag>
                </template>
            </el-table-column>
            <el-table-column :label="`${layer}开关`" width="110">
                <template #default="{ row }: { row: ScopedProductListing }">
                    <el-switch
                        :model-value="row.listed"
                        :loading="toggling === row.product_id"
                        @update:model-value="(v: string | number | boolean) => setListed(row, v === true)"
                    />
                </template>
            </el-table-column>
            <el-table-column label="买家看得见吗" min-width="240">
                <template #default="{ row }: { row: ScopedProductListing }">
                    <el-tag v-if="row.effective_listed" type="success" size="small">看得见</el-tag>
                    <template v-else>
                        <el-tag type="danger" size="small">看不见</el-tag>
                        <div class="reason">{{ invisibleReason(row) }}</div>
                    </template>
                </template>
            </el-table-column>
            <el-table-column label="生效价" width="170">
                <template #default="{ row }: { row: ScopedProductListing }">
                    <template v-if="row.min_price_cents !== undefined">
                        {{ yuan(row.min_price_cents) }}<template v-if="row.max_price_cents !== undefined && row.max_price_cents !== row.min_price_cents"> ~ {{ yuan(row.max_price_cents) }}</template>
                    </template>
                    <span v-else class="hint">—</span>
                </template>
            </el-table-column>
            <el-table-column label="价来自" width="100">
                <template #default="{ row }: { row: ScopedProductListing }">
                    <el-tag size="small" :type="PRICE_SOURCE[row.price_source].tag">{{ PRICE_SOURCE[row.price_source].text }}</el-tag>
                </template>
            </el-table-column>
        </el-table>

        <el-pagination
            v-if="page"
            class="pager"
            layout="total, prev, pager, next"
            :total="page.total"
            :current-page="page.page"
            :page-size="page.page_size"
            @current-change="(n: number) => ((pageNo = n), load())"
        />

        <InventoryDialog
            v-if="scope.kind === 'store'"
            v-model="invDialog"
            :sku="invTarget"
            :store-id="scope.id"
            :store-name="scope.name"
            @updated="onInventoryUpdated"
        />
    </div>
</template>

<style scoped>
.skus {
    padding: 4px 24px 8px 48px;
}
.reason {
    margin-top: 2px;
    font-size: 12px;
    color: var(--el-color-danger);
    line-height: 1.5;
}
.ml4 {
    margin-left: 4px;
}
.pager {
    margin-top: 12px;
    justify-content: flex-end;
}
</style>
