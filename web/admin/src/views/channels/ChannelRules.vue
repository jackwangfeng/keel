<script setup lang="ts">
// 库存规则 / 价格规则（kind 区分），GET / PUT / DELETE /admin/channel-bindings/{id}/{stock|price}-rules。
//
// PUT 按「层级键」覆盖写：库存规则的键是 (门店, SKU)，价格规则的键是 SKU；所以编辑时层级与对象锁住，
// 只改数值——改对象等于另建一条。
//
// 库存：对外可售 = clamp(floor(可售 × 比例) − 安全库存, 0, 上限)，SKU 级 > 门店级 > 渠道级。
// 价格：SKU 固定价 > SKU 加价 > 渠道级加价；加价以价格源门店的价为底。
// 百分数 ↔ 万分比、元 ↔ 分的换算与校验在 api/channelRules.ts（make admin-test）。

import { computed, onMounted, ref } from "vue";
import { Plus } from "@element-plus/icons-vue";
import { ElMessageBox } from "element-plus";
import {
    keel,
    type AdminProduct,
    type AdminSku,
    type AdminStore,
    type ChannelPriceRule,
    type ChannelStockRule,
} from "../../api/client.ts";
import {
    bpToPercent,
    priceRuleBody,
    priceRuleSummary,
    stockRuleBody,
    stockRuleLevel,
    validatePriceRule,
    validateStockRule,
} from "../../api/channelRules.ts";
import { listProductsForPicker } from "../../api/coupons.ts";
import { centsToYuanInput } from "../../api/money.ts";
import { datetime } from "../../ui/format.ts";
import { notifyError, notifyOk } from "../../ui/notify.ts";
import { useMobile } from "../../ui/useMobile.ts";
import ProblemAlert from "../../components/ProblemAlert.vue";

const props = defineProps<{ kind: "stock" | "price"; bindingId: number; stores: AdminStore[]; canWrite: boolean }>();

const mobile = useMobile();
const loading = ref(false);
const error = ref<unknown>(null);
const stockRules = ref<ChannelStockRule[]>([]);
const priceRules = ref<ChannelPriceRule[]>([]);
const storeName = computed(() => new Map(props.stores.map((s) => [s.id, s.name])));

async function load(): Promise<void> {
    loading.value = true;
    error.value = null;
    try {
        if (props.kind === "stock") {
            const res = await keel.request("get", "/admin/channel-bindings/{binding_id}/stock-rules", {
                path: { binding_id: props.bindingId },
            });
            stockRules.value = res.items;
        } else {
            const res = await keel.request("get", "/admin/channel-bindings/{binding_id}/price-rules", {
                path: { binding_id: props.bindingId },
            });
            priceRules.value = res.items;
        }
    } catch (err) {
        error.value = err;
    } finally {
        loading.value = false;
    }
}
onMounted(() => void load());

// ------------------------------------------------------------ SKU 选择
const products = ref<AdminProduct[]>([]);
const skusOf = ref<Record<number, AdminSku[]>>({});
const skuIndex = ref<Record<number, AdminSku>>({});
const pickerProduct = ref<number | null>(null);

async function loadProducts(): Promise<void> {
    if (products.value.length > 0) return;
    try {
        products.value = await listProductsForPicker();
    } catch (err) {
        formError.value = err;
    }
}

async function onPickProduct(productId: number | null): Promise<void> {
    pickerProduct.value = productId;
    form.value.skuId = null;
    if (productId === null || skusOf.value[productId] !== undefined) return;
    try {
        const d = await keel.request("get", "/admin/products/{product_id}", { path: { product_id: productId } });
        skusOf.value = { ...skusOf.value, [productId]: d.skus };
        const idx = { ...skuIndex.value };
        for (const s of d.skus) idx[s.id] = s;
        skuIndex.value = idx;
    } catch (err) {
        formError.value = err;
    }
}

function skuLabel(id: number | null | undefined): string {
    if (id === null || id === undefined) return "—";
    const s = skuIndex.value[id];
    if (s === undefined) return `SKU #${id}`;
    const spec = Object.values(s.spec_values ?? {}).join(" / ");
    return `${s.sku_code}${spec ? `（${spec}）` : ""}`;
}

// ------------------------------------------------------------ 对话框
type Level = "channel" | "store" | "sku";
interface Form {
    level: Level;
    storeId: number | null;
    skuId: number | null;
    ratioPercent: number;
    safetyQty: number;
    capUnlimited: boolean;
    capQty: number;
    markupPercent: number;
    fixedYuan: string;
}
function emptyForm(): Form {
    return {
        level: "channel",
        storeId: null,
        skuId: null,
        ratioPercent: 100,
        safetyQty: 0,
        capUnlimited: true,
        capQty: 0,
        markupPercent: 0,
        fixedYuan: "",
    };
}

const dialogVisible = ref(false);
const editingId = ref<number | null>(null);
const form = ref<Form>(emptyForm());
const formError = ref<unknown>(null);
const localErrors = ref<string[]>([]);
const saving = ref(false);

function openCreate(): void {
    editingId.value = null;
    form.value = emptyForm();
    pickerProduct.value = null;
    formError.value = null;
    localErrors.value = [];
    dialogVisible.value = true;
    void loadProducts();
}

function openEditStock(r: ChannelStockRule): void {
    editingId.value = r.id;
    form.value = {
        ...emptyForm(),
        level: r.sku_id != null ? "sku" : r.store_id != null ? "store" : "channel",
        storeId: r.store_id ?? null,
        skuId: r.sku_id ?? null,
        ratioPercent: bpToPercent(r.ratio_bp),
        safetyQty: r.safety_qty ?? 0,
        capUnlimited: r.cap_qty === null || r.cap_qty === undefined,
        capQty: r.cap_qty ?? 0,
    };
    formError.value = null;
    localErrors.value = [];
    dialogVisible.value = true;
}

function openEditPrice(r: ChannelPriceRule): void {
    editingId.value = r.id;
    form.value = {
        ...emptyForm(),
        level: r.sku_id != null ? "sku" : "channel",
        skuId: r.sku_id ?? null,
        markupPercent: bpToPercent(r.markup_bp ?? 0),
        fixedYuan: r.fixed_cents === null || r.fixed_cents === undefined ? "" : centsToYuanInput(r.fixed_cents),
    };
    formError.value = null;
    localErrors.value = [];
    dialogVisible.value = true;
}

const levels = computed<{ value: Level; label: string }[]>(() =>
    props.kind === "stock"
        ? [
              { value: "channel", label: "渠道级" },
              { value: "store", label: "门店级" },
              { value: "sku", label: "SKU 级" },
          ]
        : [
              { value: "channel", label: "渠道级加价" },
              { value: "sku", label: "SKU 级" },
          ],
);

async function save(): Promise<void> {
    const f = form.value;
    const storeId = f.level === "channel" ? null : f.storeId;
    const skuId = f.level === "sku" ? f.skuId : null;
    if (f.level !== "channel" && props.kind === "stock" && storeId === null) {
        localErrors.value = ["选门店"];
        return;
    }
    if (f.level === "sku" && skuId === null) {
        localErrors.value = ["选 SKU"];
        return;
    }
    saving.value = true;
    formError.value = null;
    try {
        if (props.kind === "stock") {
            const r = {
                storeId,
                skuId,
                ratioPercent: f.ratioPercent,
                safetyQty: f.safetyQty,
                capQty: f.capUnlimited ? null : f.capQty,
            };
            localErrors.value = validateStockRule(r);
            if (localErrors.value.length > 0) return;
            await keel.request("put", "/admin/channel-bindings/{binding_id}/stock-rules", {
                path: { binding_id: props.bindingId },
                body: stockRuleBody(r),
            });
        } else {
            const r = { skuId, markupPercent: f.markupPercent, fixedYuan: f.level === "sku" ? f.fixedYuan : null };
            localErrors.value = validatePriceRule(r);
            if (localErrors.value.length > 0) return;
            await keel.request("put", "/admin/channel-bindings/{binding_id}/price-rules", {
                path: { binding_id: props.bindingId },
                body: priceRuleBody(r),
            });
        }
        notifyOk("已保存规则");
        dialogVisible.value = false;
        await load();
    } catch (err) {
        formError.value = err;
    } finally {
        saving.value = false;
    }
}

async function remove(id: number): Promise<void> {
    try {
        await ElMessageBox.confirm("删掉之后按上一层的规则算（都没有就是全量、原价）。", "确认删除规则", { type: "warning" });
    } catch {
        return;
    }
    try {
        if (props.kind === "stock") {
            await keel.request("delete", "/admin/channel-bindings/{binding_id}/stock-rules/{rule_id}", {
                path: { binding_id: props.bindingId, rule_id: id },
            });
        } else {
            await keel.request("delete", "/admin/channel-bindings/{binding_id}/price-rules/{rule_id}", {
                path: { binding_id: props.bindingId, rule_id: id },
            });
        }
        notifyOk("已删除");
    } catch (err) {
        notifyError(err);
    }
    await load();
}

function capText(cap: number | null | undefined): string {
    if (cap === null || cap === undefined) return "不封顶";
    return cap === 0 ? "0（在这个渠道下架）" : String(cap);
}
</script>

<template>
    <div v-loading="loading">
        <ProblemAlert v-if="error" :error="error" />
        <div class="page-toolbar">
            <span v-if="kind === 'stock'" class="hint">
                对外可售 = 可售 × 比例 − 安全库存（不低于 0、不超过上限）。SKU 级优先于门店级，门店级优先于渠道级；一条都没有就全量推。
            </span>
            <span v-else class="hint">
                渠道价 = 价格源门店的价 ×（1 + 加价）。SKU 固定价优先于加价；一条都没有就推原价。
            </span>
            <span class="grow" />
            <el-button v-if="canWrite" type="primary" :icon="Plus" @click="openCreate">新增规则</el-button>
        </div>

        <div class="table-wrap">
            <el-table v-if="kind === 'stock'" :data="stockRules" border stripe empty-text="没有库存规则：全量推">
                <el-table-column label="层级" width="90">
                    <template #default="{ row }: { row: ChannelStockRule }">{{ stockRuleLevel(row) }}</template>
                </el-table-column>
                <el-table-column label="门店" min-width="140">
                    <template #default="{ row }: { row: ChannelStockRule }">
                        {{ row.store_id == null ? "全部" : (storeName.get(row.store_id) ?? `门店 #${row.store_id}`) }}
                    </template>
                </el-table-column>
                <el-table-column label="SKU" min-width="140">
                    <template #default="{ row }: { row: ChannelStockRule }">{{ row.sku_id == null ? "全部" : skuLabel(row.sku_id) }}</template>
                </el-table-column>
                <el-table-column label="可售比例" width="100">
                    <template #default="{ row }: { row: ChannelStockRule }">{{ bpToPercent(row.ratio_bp) }}%</template>
                </el-table-column>
                <el-table-column label="安全库存" width="90">
                    <template #default="{ row }: { row: ChannelStockRule }">{{ row.safety_qty ?? 0 }}</template>
                </el-table-column>
                <el-table-column label="上限" min-width="140">
                    <template #default="{ row }: { row: ChannelStockRule }">{{ capText(row.cap_qty) }}</template>
                </el-table-column>
                <el-table-column label="更新时间" width="170">
                    <template #default="{ row }: { row: ChannelStockRule }">{{ datetime(row.updated_at) }}</template>
                </el-table-column>
                <el-table-column v-if="canWrite" label="操作" width="120">
                    <template #default="{ row }: { row: ChannelStockRule }">
                        <el-button link type="primary" @click="openEditStock(row)">编辑</el-button>
                        <el-button link type="danger" @click="remove(row.id)">删除</el-button>
                    </template>
                </el-table-column>
            </el-table>

            <el-table v-else :data="priceRules" border stripe empty-text="没有价格规则：推原价">
                <el-table-column label="层级" width="110">
                    <template #default="{ row }: { row: ChannelPriceRule }">{{ row.sku_id == null ? "渠道级" : "SKU 级" }}</template>
                </el-table-column>
                <el-table-column label="SKU" min-width="160">
                    <template #default="{ row }: { row: ChannelPriceRule }">{{ row.sku_id == null ? "全部" : skuLabel(row.sku_id) }}</template>
                </el-table-column>
                <el-table-column label="规则" min-width="140">
                    <template #default="{ row }: { row: ChannelPriceRule }">{{ priceRuleSummary(row) }}</template>
                </el-table-column>
                <el-table-column label="更新时间" width="170">
                    <template #default="{ row }: { row: ChannelPriceRule }">{{ datetime(row.updated_at) }}</template>
                </el-table-column>
                <el-table-column v-if="canWrite" label="操作" width="120">
                    <template #default="{ row }: { row: ChannelPriceRule }">
                        <el-button link type="primary" @click="openEditPrice(row)">编辑</el-button>
                        <el-button link type="danger" @click="remove(row.id)">删除</el-button>
                    </template>
                </el-table-column>
            </el-table>
        </div>

        <el-dialog
            v-model="dialogVisible"
            :title="`${editingId === null ? '新增' : '编辑'}${kind === 'stock' ? '库存' : '价格'}规则`"
            width="560px"
            :fullscreen="mobile"
        >
            <ProblemAlert v-if="formError" :error="formError" />
            <el-alert v-for="(e, i) in localErrors" :key="i" :title="e" type="error" :closable="false" show-icon class="mb8" />
            <el-form label-width="100px" @submit.prevent>
                <el-form-item label="层级">
                    <el-radio-group v-model="form.level" :disabled="editingId !== null">
                        <el-radio v-for="l in levels" :key="l.value" :value="l.value">{{ l.label }}</el-radio>
                    </el-radio-group>
                </el-form-item>
                <el-form-item v-if="kind === 'stock' && form.level !== 'channel'" label="门店" required>
                    <el-select v-model="form.storeId" filterable :disabled="editingId !== null" placeholder="选门店">
                        <el-option v-for="s in stores" :key="s.id" :value="s.id" :label="s.name" />
                    </el-select>
                </el-form-item>
                <template v-if="form.level === 'sku'">
                    <el-form-item v-if="editingId !== null" label="SKU">
                        <span>{{ skuLabel(form.skuId) }}</span>
                    </el-form-item>
                    <template v-else>
                        <el-form-item label="商品" required>
                            <el-select :model-value="pickerProduct" filterable placeholder="先选商品" @update:model-value="onPickProduct">
                                <el-option v-for="p in products" :key="p.id" :value="p.id" :label="p.title" />
                            </el-select>
                        </el-form-item>
                        <el-form-item label="SKU" required>
                            <el-select v-model="form.skuId" filterable :disabled="pickerProduct === null" placeholder="再选 SKU">
                                <el-option
                                    v-for="s in pickerProduct === null ? [] : (skusOf[pickerProduct] ?? [])"
                                    :key="s.id"
                                    :value="s.id"
                                    :label="skuLabel(s.id)"
                                />
                            </el-select>
                        </el-form-item>
                    </template>
                </template>

                <template v-if="kind === 'stock'">
                    <el-form-item label="可售比例">
                        <el-input-number v-model="form.ratioPercent" :min="0" :max="100" :precision="2" :step="5" />
                        <span class="unit">%</span>
                    </el-form-item>
                    <el-form-item label="安全库存">
                        <el-input-number v-model="form.safetyQty" :min="0" :precision="0" />
                        <span class="unit">件，先扣掉再推</span>
                    </el-form-item>
                    <el-form-item label="上限">
                        <el-checkbox v-model="form.capUnlimited">不封顶</el-checkbox>
                        <template v-if="!form.capUnlimited">
                            <el-input-number v-model="form.capQty" :min="0" :precision="0" class="ml8" />
                            <span class="unit">件（0 = 在这个渠道下架）</span>
                        </template>
                    </el-form-item>
                </template>
                <template v-else>
                    <el-form-item label="加价">
                        <el-input-number v-model="form.markupPercent" :min="-90" :max="1000" :precision="2" :step="5" />
                        <span class="unit">%（负数 = 打折）</span>
                    </el-form-item>
                    <el-form-item v-if="form.level === 'sku'" label="固定价">
                        <el-input v-model="form.fixedYuan" placeholder="留空 = 按加价算" style="width: 180px">
                            <template #prefix>¥</template>
                        </el-input>
                        <span class="unit">元，优先于加价</span>
                    </el-form-item>
                </template>
            </el-form>
            <template #footer>
                <el-button @click="dialogVisible = false">取消</el-button>
                <el-button type="primary" :loading="saving" @click="save">保存</el-button>
            </template>
        </el-dialog>
    </div>
</template>

<style scoped>
.table-wrap {
    overflow-x: auto;
}
.mb8 {
    margin-bottom: 8px;
}
.ml8 {
    margin-left: 8px;
}
.unit {
    margin-left: 8px;
    font-size: 12px;
    color: var(--el-text-color-secondary);
}
</style>
