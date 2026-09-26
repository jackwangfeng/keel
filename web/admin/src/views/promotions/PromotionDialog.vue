<script setup lang="ts">
// 新建 / 编辑营销活动（POST /admin/promotions、PATCH /admin/promotions/{id}，数据模型 §7·二）。
//
// 按类型显示不同的规则块：满减满折是阶梯，限时折扣 / 秒杀是活动商品，新人礼是一批券。
// 表单 → 请求体的换算与校验全在 api/promotionRules.ts（纯函数，make admin-test 跑它）。
//
// 上线中的活动这里只能改名：服务端 409 promotion-online，改规则请先在列表里下线。
// 新建的活动一律是下线的，保存之后回列表点「上线」。

import { computed, ref, watch } from "vue";
import { Delete, Plus } from "@element-plus/icons-vue";
import { keel, type AdminCategory, type AdminProduct, type AdminRegion, type AdminSku, type AdminStore } from "../../api/client.ts";
import { indentedLabel, listCategories } from "../../api/catalog.ts";
import { SCOPE_TYPE, listProductsForPicker, type AdminCouponTemplate, type CouponScopeInput } from "../../api/coupons.ts";
import { IdempotentSubmission, withIdempotency } from "../../api/idempotency.ts";
import {
    PROMOTION_TYPE,
    buildRules,
    emptyPromotionForm,
    formOfPromotion,
    isPriced,
    isTiered,
    type AdminPromotion,
    type PromotionCreateRequest,
    type PromotionForm,
    type PromotionPatchRequest,
    type PromotionType,
} from "../../api/promotionRules.ts";
import { listAllRegions, listAllStores } from "../../api/stores.ts";
import { yuan } from "../../ui/format.ts";
import ProblemAlert from "../../components/ProblemAlert.vue";

const props = defineProps<{ modelValue: boolean; promotion: AdminPromotion | null }>();
const emit = defineEmits<{ "update:modelValue": [boolean]; saved: [AdminPromotion] }>();

const visible = computed({
    get: () => props.modelValue,
    set: (v: boolean) => emit("update:modelValue", v),
});
const editing = computed(() => props.promotion !== null);
/** 上线中：只能改名（服务端同样拒绝别的修改）。 */
const online = computed(() => props.promotion?.status === 1);

const form = ref<PromotionForm>(emptyPromotionForm());
const localError = ref("");
const error = ref<unknown>(null);
const saving = ref(false);
const submission = new IdempotentSubmission();

// 下拉素材：范围目标、商品与它们的 SKU、券模板。
const categories = ref<AdminCategory[]>([]);
const products = ref<AdminProduct[]>([]);
const regions = ref<AdminRegion[]>([]);
const stores = ref<AdminStore[]>([]);
const templates = ref<AdminCouponTemplate[]>([]);
/** 每个活动商品行选的商品（只用来筛 SKU，不提交）。 */
const rowProduct = ref<(number | null)[]>([]);
const skusOf = ref<Record<number, AdminSku[]>>({});
const loadingOptions = ref(false);

type ScopeType = CouponScopeInput["scope_type"];
const scopeTypes = computed(() =>
    Object.entries(SCOPE_TYPE)
        .map(([k, v]) => ({ value: Number(k) as ScopeType, label: v }))
        // 限时折扣 / 秒杀的商品已经由活动商品点名，范围只按大区 / 门店限定。
        .filter((o) => !isPriced(form.value.type) || o.value === 5 || o.value === 6),
);

watch(
    () => props.modelValue,
    async (open) => {
        if (!open) return;
        form.value = props.promotion === null ? emptyPromotionForm() : formOfPromotion(props.promotion);
        rowProduct.value = form.value.skus.map(() => null);
        localError.value = "";
        error.value = null;
        submission.rotate();
        loadingOptions.value = true;
        try {
            const [c, p, r, st, tpl] = await Promise.all([
                listCategories(),
                listProductsForPicker(),
                listAllRegions(),
                listAllStores(),
                keel.get("/admin/coupon-templates", { query: { page: 1, page_size: 100, status: 1 } }),
            ]);
            categories.value = c;
            products.value = p;
            regions.value = r.filter((x) => !x.deleted_at);
            stores.value = st.stores.filter((x) => !x.deleted_at);
            templates.value = tpl.items;
        } catch (err) {
            error.value = err;
        } finally {
            loadingOptions.value = false;
        }
    },
);

async function onRowProduct(i: number, productId: number | null): Promise<void> {
    rowProduct.value[i] = productId;
    const row = form.value.skus[i];
    if (row !== undefined) row.skuId = null;
    if (productId === null || skusOf.value[productId] !== undefined) return;
    try {
        const d = await keel.request("get", "/admin/products/{product_id}", { path: { product_id: productId } });
        skusOf.value = { ...skusOf.value, [productId]: d.skus };
    } catch (err) {
        error.value = err;
    }
}

function skuLabel(s: AdminSku): string {
    const spec = Object.values(s.spec_values ?? {}).join(" / ");
    return `${s.sku_code}${spec ? `（${spec}）` : ""} · 基准价 ${yuan(s.price_cents)}`;
}

function addTier(): void {
    form.value.tiers.push({ threshold: "", benefit: "" });
}
function addSku(): void {
    form.value.skus.push({ skuId: null, mode: "price", value: "", perUserLimit: 0, stockQty: form.value.type === 4 ? 100 : 0 });
    rowProduct.value.push(null);
}
function removeSku(i: number): void {
    form.value.skus.splice(i, 1);
    rowProduct.value.splice(i, 1);
}
function addScope(): void {
    form.value.scopes.push({ scope_type: isPriced(form.value.type) ? 5 : 2, target_id: null, include: true });
}
function onTypeChange(t: PromotionType): void {
    form.value.type = t;
    // 换类型时把别的类型的规则清掉：服务端对每种类型只认一种写法（chk_promotion_shape）。
    form.value.tiers = isTiered(t) ? [{ threshold: "", benefit: "" }] : [];
    form.value.skus = [];
    rowProduct.value = [];
    form.value.scopes = [];
    form.value.giftTemplateId = null;
}

async function submit(): Promise<void> {
    localError.value = "";
    error.value = null;
    const f = form.value;
    if (f.name.trim() === "") {
        localError.value = "请填活动名称";
        return;
    }
    saving.value = true;
    try {
        let saved: AdminPromotion;
        if (props.promotion === null) {
            const built = buildRules(f);
            if (!built.ok) {
                localError.value = built.msg;
                return;
            }
            const body: PromotionCreateRequest = { ...built.body, name: f.name.trim(), promotion_type: f.type };
            saved = await withIdempotency(submission, (key) =>
                keel.request("post", "/admin/promotions", { body, headers: { "Idempotency-Key": key } }),
            );
        } else {
            let body: PromotionPatchRequest = { name: f.name.trim() };
            if (!online.value) {
                const built = buildRules(f);
                if (!built.ok) {
                    localError.value = built.msg;
                    return;
                }
                body = { ...built.body, ...body };
            }
            saved = await keel.request("patch", "/admin/promotions/{promotion_id}", {
                path: { promotion_id: props.promotion.id },
                body,
            });
        }
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
    <el-dialog v-model="visible" :title="editing ? `编辑活动 #${promotion?.id}` : '新建营销活动'" width="820px">
        <ProblemAlert v-if="error" :error="error" />
        <el-alert v-if="localError" :title="localError" type="error" :closable="false" show-icon class="mb8" />
        <el-alert
            v-if="online"
            type="info"
            :closable="false"
            show-icon
            class="mb8"
            title="活动上线中，只能改名"
            description="上线中的活动随时可能正被一笔试算或下单读着，边读边改会让两者按两套规则算钱。要改规则请先在列表里下线。已成交的订单不受影响（订单上有活动快照）。"
        />
        <el-form label-width="104px" v-loading="loadingOptions" @submit.prevent>
            <el-form-item label="名称" required><el-input v-model="form.name" maxlength="60" show-word-limit /></el-form-item>
            <el-form-item label="类型" required>
                <el-radio-group
                    :model-value="form.type"
                    :disabled="editing"
                    @change="(v: string | number | boolean | undefined) => onTypeChange(Number(v) as PromotionType)"
                >
                    <el-radio v-for="(label, k) in PROMOTION_TYPE" :key="k" :value="Number(k)">{{ label }}</el-radio>
                </el-radio-group>
            </el-form-item>
            <el-form-item label="活动时间" required>
                <el-date-picker v-model="form.range" type="datetimerange" :disabled="online" start-placeholder="开始" end-placeholder="结束" />
            </el-form-item>
            <el-form-item v-if="form.type !== 5" label="与券同享">
                <el-switch v-model="form.stack" :disabled="online" />
                <span class="hint ml8">关掉后，命中这个活动的订单不能再用优惠券</span>
            </el-form-item>

            <!-- 满减 / 满折：阶梯 -->
            <template v-if="isTiered(form.type)">
                <el-form-item label="门槛按">
                    <el-radio-group v-model="form.unit" :disabled="online">
                        <el-radio :value="1">金额（元）</el-radio>
                        <el-radio :value="2">件数</el-radio>
                    </el-radio-group>
                </el-form-item>
                <el-form-item label="阶梯">
                    <div class="w100">
                        <div v-for="(t, i) in form.tiers" :key="i" class="tier-row">
                            <span>满</span>
                            <el-input v-model="t.threshold" :disabled="online" class="tier-input" :placeholder="form.unit === 2 ? '件数' : '元'" />
                            <span>{{ form.unit === 2 ? "件" : "元" }}，{{ form.type === 1 ? "减" : "打" }}</span>
                            <el-input v-model="t.benefit" :disabled="online" class="tier-input" :placeholder="form.type === 1 ? '元' : '如 9 或 8.5'" />
                            <span>{{ form.type === 1 ? "元" : "折" }}</span>
                            <el-button link type="danger" :icon="Delete" :disabled="online" @click="form.tiers.splice(i, 1)" />
                        </div>
                        <el-button :icon="Plus" size="small" :disabled="online || form.tiers.length >= 10" @click="addTier">加一档</el-button>
                        <p class="hint">
                            命中门槛不超过的最高一档；门槛比的是活动价之后的金额。一行商品至多参与一个满减满折
                            （范围重叠时减得多的那个先占）。可以先存成没有阶梯的草稿，上线时再补齐。
                        </p>
                    </div>
                </el-form-item>
            </template>

            <!-- 限时折扣 / 秒杀：活动商品 -->
            <el-form-item v-if="isPriced(form.type)" label="活动商品">
                <div class="w100">
                    <el-table :data="form.skus" border size="small">
                        <el-table-column label="商品 / SKU" min-width="260">
                            <template #default="{ row, $index }: { row: PromotionForm['skus'][number]; $index: number }">
                                <div v-if="row.skuId !== null && rowProduct[$index] === null" class="hint">
                                    SKU #{{ row.skuId }}
                                    <el-button link type="primary" :disabled="online" @click="row.skuId = null">换一个</el-button>
                                </div>
                                <template v-else>
                                    <el-select
                                        :model-value="rowProduct[$index]"
                                        filterable
                                        placeholder="先选商品"
                                        :disabled="online"
                                        class="w100"
                                        @change="(v: number | null) => onRowProduct($index, v)"
                                    >
                                        <el-option v-for="p in products" :key="p.id" :value="p.id" :label="`${p.title}（#${p.id}）`" />
                                    </el-select>
                                    <el-select v-model="row.skuId" placeholder="再选 SKU" :disabled="online || rowProduct[$index] === null" class="w100 mt4">
                                        <el-option v-for="s in skusOf[rowProduct[$index] ?? 0] ?? []" :key="s.id" :value="s.id" :label="skuLabel(s)" />
                                    </el-select>
                                </template>
                            </template>
                        </el-table-column>
                        <el-table-column label="活动价" width="200">
                            <template #default="{ row }: { row: PromotionForm['skus'][number] }">
                                <el-input v-model="row.value" :disabled="online" :placeholder="row.mode === 'price' ? '特价（元）' : '折扣，如 8.5'">
                                    <template #prepend>
                                        <el-select v-model="row.mode" :disabled="online" style="width: 70px">
                                            <el-option value="price" label="特价" />
                                            <el-option value="rate" label="折扣" />
                                        </el-select>
                                    </template>
                                </el-input>
                            </template>
                        </el-table-column>
                        <el-table-column label="每人限购" width="120">
                            <template #default="{ row }: { row: PromotionForm['skus'][number] }">
                                <el-input-number v-model="row.perUserLimit" :min="0" :max="999" :disabled="online" size="small" controls-position="right" />
                            </template>
                        </el-table-column>
                        <el-table-column v-if="form.type === 4" label="秒杀配额" width="130">
                            <template #default="{ row }: { row: PromotionForm['skus'][number] }">
                                <el-input-number v-model="row.stockQty" :min="1" :disabled="online" size="small" controls-position="right" />
                            </template>
                        </el-table-column>
                        <el-table-column width="50">
                            <template #default="{ $index }: { $index: number }">
                                <el-button link type="danger" :icon="Delete" :disabled="online" @click="removeSku($index)" />
                            </template>
                        </el-table-column>
                    </el-table>
                    <el-button class="mt8" :icon="Plus" size="small" :disabled="online" @click="addSku">加一个活动商品</el-button>
                    <p class="hint">
                        活动价 = min(门店价, 特价)：特价比某家店的门店价还高时，那家店按门店价卖。每人限购 0 表示不限。
                        <template v-if="form.type === 4">
                            秒杀配额是「按秒杀价最多卖几件」，每一件仍从门店库存扣；配额抢光之后按门店价卖。
                        </template>
                    </p>
                </div>
            </el-form-item>

            <!-- 新人礼：一批券 -->
            <el-form-item v-if="form.type === 5" label="送哪批券" required>
                <el-select v-model="form.giftTemplateId" filterable placeholder="选一批启用中的券" :disabled="online" class="w100">
                    <el-option v-for="t in templates" :key="t.id" :value="t.id" :label="`${t.name}（#${t.id}）`" />
                </el-select>
                <p class="hint">
                    首单前的买家登录后自动发一张（一人一张），占这批券的总量；券停用或发完就不再发。
                </p>
            </el-form-item>

            <!-- 范围 -->
            <el-form-item v-if="form.type !== 5" label="适用范围">
                <div class="w100">
                    <el-table :data="form.scopes" border size="small">
                        <el-table-column label="类型" width="160">
                            <template #default="{ row }: { row: CouponScopeInput }">
                                <el-select v-model="row.scope_type" :disabled="online" @change="row.target_id = null">
                                    <el-option v-for="o in scopeTypes" :key="o.value" :value="o.value" :label="o.label" />
                                </el-select>
                            </template>
                        </el-table-column>
                        <el-table-column label="目标" min-width="260">
                            <template #default="{ row }: { row: CouponScopeInput }">
                                <span v-if="row.scope_type === 1" class="hint">全部商品</span>
                                <el-select v-else-if="row.scope_type === 2" v-model="row.target_id" filterable :disabled="online" class="w100">
                                    <el-option v-for="c in categories" :key="c.id" :value="c.id" :label="indentedLabel(c)" />
                                </el-select>
                                <el-select v-else-if="row.scope_type === 3" v-model="row.target_id" filterable :disabled="online" class="w100">
                                    <el-option v-for="p in products" :key="p.id" :value="p.id" :label="`${p.title}（#${p.id}）`" />
                                </el-select>
                                <el-input-number v-else-if="row.scope_type === 4" v-model="row.target_id" :min="1" :disabled="online" class="w100" />
                                <el-select v-else-if="row.scope_type === 5" v-model="row.target_id" filterable :disabled="online" class="w100">
                                    <el-option v-for="r in regions" :key="r.id" :value="r.id" :label="`${r.name}（${r.code}）`" />
                                </el-select>
                                <el-select v-else v-model="row.target_id" filterable :disabled="online" class="w100">
                                    <el-option v-for="s in stores" :key="s.id" :value="s.id" :label="`${s.name}（${s.code}）`" />
                                </el-select>
                            </template>
                        </el-table-column>
                        <el-table-column label="包含 / 排除" width="130">
                            <template #default="{ row }: { row: CouponScopeInput }">
                                <el-switch
                                    :model-value="row.include !== false"
                                    :disabled="online || row.scope_type === 1"
                                    active-text="包含"
                                    inactive-text="排除"
                                    inline-prompt
                                    @change="(v: string | number | boolean) => (row.include = v === true)"
                                />
                            </template>
                        </el-table-column>
                        <el-table-column width="50">
                            <template #default="{ $index }: { $index: number }">
                                <el-button link type="danger" :icon="Delete" :disabled="online" @click="form.scopes.splice($index, 1)" />
                            </template>
                        </el-table-column>
                    </el-table>
                    <el-button class="mt8" :icon="Plus" size="small" :disabled="online" @click="addScope">加一条</el-button>
                    <p class="hint">一条都不配 = 全场、全店。与优惠券的适用范围同一套规则：排除优先，分类含子分类。</p>
                </div>
            </el-form-item>
        </el-form>
        <template #footer>
            <el-button @click="visible = false">取消</el-button>
            <el-button type="primary" :loading="saving" @click="submit">{{ editing ? "保存" : "创建（下线状态）" }}</el-button>
        </template>
    </el-dialog>
</template>

<style scoped>
.mb8 {
    margin-bottom: 8px;
}
.mt4 {
    margin-top: 4px;
}
.mt8 {
    margin-top: 8px;
}
.ml8 {
    margin-left: 8px;
}
.w100 {
    width: 100%;
}
.tier-row {
    display: flex;
    align-items: center;
    gap: 6px;
    margin-bottom: 6px;
}
.tier-input {
    width: 110px;
}
.hint {
    color: var(--el-text-color-secondary);
    font-size: 12px;
}
</style>
