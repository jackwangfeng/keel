<script setup lang="ts">
// 适用范围（PUT /admin/coupon-templates/{id}/scopes，整组替换）。
//
// 两个维度（数据模型 §7）：分类 / 商品 / 品牌决定订单里哪几行参与计算，
// 大区 / 门店决定在哪家店下单可用。排除优先于包含；某个维度一条包含都没有即不限。
// 分类含子分类，与前台按分类筛商品同一个语义。

import { computed, ref, watch } from "vue";
import { Delete, Plus } from "@element-plus/icons-vue";
import { keel, type AdminCategory, type AdminProduct, type AdminRegion, type AdminStore } from "../../api/client.ts";
import { indentedLabel, listCategories } from "../../api/catalog.ts";
import { SCOPE_TYPE, listProductsForPicker, type AdminCouponTemplate, type CouponScopeInput } from "../../api/coupons.ts";
import { listAllRegions, listAllStores } from "../../api/stores.ts";
import ProblemAlert from "../../components/ProblemAlert.vue";

const props = defineProps<{ modelValue: boolean; template: AdminCouponTemplate | null }>();
const emit = defineEmits<{ "update:modelValue": [boolean]; saved: [AdminCouponTemplate] }>();

const visible = computed({
    get: () => props.modelValue,
    set: (v: boolean) => emit("update:modelValue", v),
});
const locked = computed(() => props.template?.locked === true);

type ScopeType = CouponScopeInput["scope_type"];
interface Row {
    scopeType: ScopeType;
    targetId: number | null;
    include: boolean;
}

const rows = ref<Row[]>([]);
const categories = ref<AdminCategory[]>([]);
const products = ref<AdminProduct[]>([]);
const regions = ref<AdminRegion[]>([]);
const stores = ref<AdminStore[]>([]);
const loadingOptions = ref(false);
const error = ref<unknown>(null);
const localError = ref("");
const saving = ref(false);

const scopeTypes = Object.entries(SCOPE_TYPE).map(([k, v]) => ({ value: Number(k) as ScopeType, label: v }));

watch(
    () => props.modelValue,
    async (open) => {
        if (!open || props.template === null) return;
        rows.value = props.template.scopes.map((s) => ({
            scopeType: s.scope_type,
            targetId: s.target_id,
            include: s.include,
        }));
        error.value = null;
        localError.value = "";
        loadingOptions.value = true;
        try {
            const [c, p, r, st] = await Promise.all([
                listCategories(),
                listProductsForPicker(),
                listAllRegions(),
                listAllStores(),
            ]);
            categories.value = c;
            products.value = p;
            regions.value = r.filter((x) => !x.deleted_at);
            stores.value = st.stores.filter((x) => !x.deleted_at);
        } catch (err) {
            error.value = err;
        } finally {
            loadingOptions.value = false;
        }
    },
);

function addRow(): void {
    rows.value.push({ scopeType: 2, targetId: null, include: true });
}

function onTypeChange(row: Row): void {
    row.targetId = null;
    if (row.scopeType === 1) row.include = true;
}

async function save(): Promise<void> {
    const t = props.template;
    if (t === null) return;
    localError.value = "";
    error.value = null;
    const scopes: CouponScopeInput[] = [];
    for (const [i, r] of rows.value.entries()) {
        if (r.scopeType !== 1 && (r.targetId === null || r.targetId <= 0)) {
            localError.value = `第 ${i + 1} 条没有选目标`;
            return;
        }
        scopes.push({ scope_type: r.scopeType, target_id: r.scopeType === 1 ? null : r.targetId, include: r.include });
    }
    saving.value = true;
    try {
        const saved = await keel.request("put", "/admin/coupon-templates/{template_id}/scopes", {
            path: { template_id: t.id },
            body: { scopes },
        });
        emit("saved", saved);
        visible.value = false;
    } catch (err) {
        error.value = err;
    } finally {
        saving.value = false;
    }
}
</script>

<template>
    <el-dialog v-model="visible" :title="`适用范围：${template?.name ?? ''}`" width="760px">
        <ProblemAlert v-if="error" :error="error" />
        <el-alert v-if="localError" :title="localError" type="error" :closable="false" show-icon class="mb8" />
        <el-alert
            v-if="locked"
            type="info"
            :closable="false"
            show-icon
            class="mb8"
            title="这批券已经发出过，适用范围不能再改"
            description="范围决定券能用在哪儿，改它等于改掉买家手里的券。要换范围请新建一批。"
        />
        <p class="hint">
            一条都不配 = 全场、全店可用。分类 / 商品 / 品牌决定哪些商品参与计算（分类含子分类）；
            大区 / 门店决定在哪家店下单可用。「排除」优先于「包含」。
        </p>
        <el-table :data="rows" v-loading="loadingOptions" border size="small">
            <el-table-column label="类型" width="170">
                <template #default="{ row }: { row: Row }">
                    <el-select v-model="row.scopeType" :disabled="locked" @change="onTypeChange(row)">
                        <el-option v-for="o in scopeTypes" :key="o.value" :value="o.value" :label="o.label" />
                    </el-select>
                </template>
            </el-table-column>
            <el-table-column label="目标" min-width="300">
                <template #default="{ row }: { row: Row }">
                    <span v-if="row.scopeType === 1" class="hint">全部商品</span>
                    <el-select v-else-if="row.scopeType === 2" v-model="row.targetId" filterable :disabled="locked" class="w100">
                        <el-option v-for="c in categories" :key="c.id" :value="c.id" :label="indentedLabel(c)" />
                    </el-select>
                    <el-select v-else-if="row.scopeType === 3" v-model="row.targetId" filterable :disabled="locked" class="w100">
                        <el-option v-for="p in products" :key="p.id" :value="p.id" :label="`${p.title}（#${p.id}）`" />
                    </el-select>
                    <el-input-number
                        v-else-if="row.scopeType === 4"
                        v-model="row.targetId"
                        :min="1"
                        :disabled="locked"
                        placeholder="品牌 id"
                        class="w100"
                    />
                    <el-select v-else-if="row.scopeType === 5" v-model="row.targetId" filterable :disabled="locked" class="w100">
                        <el-option v-for="r in regions" :key="r.id" :value="r.id" :label="`${r.name}（${r.code}）`" />
                    </el-select>
                    <el-select v-else v-model="row.targetId" filterable :disabled="locked" class="w100">
                        <el-option v-for="s in stores" :key="s.id" :value="s.id" :label="`${s.name}（${s.code}）`" />
                    </el-select>
                </template>
            </el-table-column>
            <el-table-column label="包含 / 排除" width="150">
                <template #default="{ row }: { row: Row }">
                    <el-tooltip :disabled="row.scopeType !== 1" content="全场不能是排除：那等于这张券哪儿都不能用">
                        <el-switch
                            v-model="row.include"
                            :disabled="locked || row.scopeType === 1"
                            active-text="包含"
                            inactive-text="排除"
                            inline-prompt
                        />
                    </el-tooltip>
                </template>
            </el-table-column>
            <el-table-column width="60">
                <template #default="{ $index }: { $index: number }">
                    <el-button link type="danger" :icon="Delete" :disabled="locked" @click="rows.splice($index, 1)" />
                </template>
            </el-table-column>
        </el-table>
        <el-button class="mt8" :icon="Plus" :disabled="locked" @click="addRow">加一条</el-button>
        <template #footer>
            <el-button @click="visible = false">取消</el-button>
            <el-button type="primary" :loading="saving" :disabled="locked" @click="save">保存（整组替换）</el-button>
        </template>
    </el-dialog>
</template>

<style scoped>
.mb8 {
    margin-bottom: 8px;
}
.mt8 {
    margin-top: 8px;
}
.w100 {
    width: 100%;
}
.hint {
    color: var(--el-text-color-secondary);
    font-size: 12px;
}
</style>
