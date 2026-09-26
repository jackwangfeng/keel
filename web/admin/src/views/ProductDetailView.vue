<script setup lang="ts">
// 商品详情 / 编辑。上架一件商品这条路的主战场：
// 改文案 → 传图 → 加 SKU → 设库存 → 上架，全在这一页。
//
// 三处后端刻意设计过的东西，在这一页上都要看得见：
//
//   · **合规拒绝**（422 `compliance-rejected`）：`errors[]` 逐条给
//     `field` / `offset` / `length`。这里把违禁词在原文上标出来
//     （见 components/HighlightedText.vue 里为什么不是直接染输入框）。
//   · **合规检查没结论**（503 `compliance-unavailable`）：保守拒绝，
//     改动没有落库。这是全系统唯一一处「宁可误拒」，界面要说清
//     「没改成，不是改了没生效」。
//   · **库存 CAS**：见 components/InventoryDialog.vue。

import { computed, onMounted, ref } from "vue";
import { useRouter } from "vue-router";
import { ArrowLeft, Delete, Plus, Top } from "@element-plus/icons-vue";
import { ElMessageBox } from "element-plus";
import {
    asComplianceRejection,
    keel,
    uploadProductImage,
    type AdminCategory,
    type AdminInventory,
    type AdminProductDetail,
    type AdminSku,
    type FieldError,
    type ProductImage,
    type ProductUpdateRequest,
    type SkuCreateRequest,
    type SkuUpdateRequest,
} from "../api/client.ts";
import { indentedLabel, listCategories } from "../api/catalog.ts";
import { IdempotentSubmission, withIdempotency } from "../api/idempotency.ts";
import { fieldErrorsOf } from "../api/errors.ts";
import { datetime, PRODUCT_STATUS, SKU_STATUS, yuan } from "../ui/format.ts";
import { notifyError, notifyOk } from "../ui/notify.ts";
import ProblemAlert from "../components/ProblemAlert.vue";
import HighlightedText from "../components/HighlightedText.vue";
import InventoryDialog, { type InventoryTarget } from "../components/InventoryDialog.vue";

const props = defineProps<{ productId: string }>();
const router = useRouter();

const id = computed(() => Number(props.productId));

const loading = ref(false);
const loadError = ref<unknown>(null);
const product = ref<AdminProductDetail | null>(null);
const categories = ref<AdminCategory[]>([]);
const tab = ref("basic");

async function load(): Promise<void> {
    loading.value = true;
    loadError.value = null;
    try {
        product.value = await keel.get("/admin/products/{product_id}", { path: { product_id: id.value } });
        syncForm();
        syncImages();
    } catch (err) {
        loadError.value = err;
    } finally {
        loading.value = false;
    }
}

onMounted(() => {
    void load();
    listCategories().then(
        (cs) => (categories.value = cs),
        (err: unknown) => notifyError(err),
    );
});

// ---------------------------------------------------------------- 基本信息

const form = ref<ProductUpdateRequest>({});
const saving = ref(false);
const saveError = ref<unknown>(null);
/** 合规拒绝时，服务端逐条给出的命中位置。按字段分组渲染。 */
const complianceHits = ref<FieldError[]>([]);

function syncForm(): void {
    const p = product.value;
    if (p === null) return;
    form.value = {
        title: p.title,
        subtitle: p.subtitle ?? "",
        description: p.description ?? "",
        category_id: p.category_id,
    };
}

async function saveBasic(): Promise<void> {
    saving.value = true;
    saveError.value = null;
    complianceHits.value = [];
    try {
        const updated = await keel.request("patch", "/admin/products/{product_id}", {
            path: { product_id: id.value },
            body: form.value,
        });
        // PATCH 返回 AdminProduct（不含 skus / images），所以只合并它给的那些字段。
        const p = product.value;
        if (p !== null) product.value = { ...p, ...updated };
        notifyOk("已保存");
    } catch (err) {
        saveError.value = err;
        const rejection = asComplianceRejection(err);
        if (rejection !== null) complianceHits.value = rejection.errors ?? [];
    } finally {
        saving.value = false;
    }
}

function hitsFor(field: string): FieldError[] {
    return fieldErrorsOf(complianceHits.value, field);
}

// ------------------------------------------------------------------ 上下架

const publishing = ref(false);
const publishError = ref<unknown>(null);

async function togglePublication(): Promise<void> {
    const p = product.value;
    if (p === null) return;
    const action = p.status === 1 ? "unpublish" : "publish";
    publishing.value = true;
    publishError.value = null;
    complianceHits.value = [];
    const submission = new IdempotentSubmission();
    try {
        const updated = await withIdempotency(submission, (key) =>
            keel.request("post", "/admin/products/{product_id}/publication", {
                path: { product_id: id.value },
                body: { action },
                headers: { "Idempotency-Key": key },
            }),
        );
        product.value = { ...p, ...updated };
        notifyOk(action === "publish" ? "已上架" : "已下架");
    } catch (err) {
        publishError.value = err;
        const rejection = asComplianceRejection(err);
        if (rejection !== null) {
            // 上架被合规拒绝：把命中位置显示在基本信息页上，并切过去。
            complianceHits.value = rejection.errors ?? [];
            tab.value = "basic";
        }
    } finally {
        publishing.value = false;
    }
}

async function removeProduct(): Promise<void> {
    try {
        await ElMessageBox.confirm("软删这件商品？在架的商品会被服务端拒绝（先下架再删）。", "确认删除", {
            type: "warning",
        });
    } catch {
        return;
    }
    try {
        await keel.request("delete", "/admin/products/{product_id}", { path: { product_id: id.value } });
        notifyOk("已软删");
        await router.push({ name: "products" });
    } catch (err) {
        notifyError(err);
    }
}

// ---------------------------------------------------------------- 商品图
//
// 整组替换：`PUT .../images` 一次写整个数组，顺序即展示顺序，images[0] 是主图。
// 所以这一页的编辑是「先在本地把数组摆好，再一次提交」，没有「加一张」
// 这个单独的服务端动作。

const draftImages = ref<ProductImage[]>([]);
const uploading = ref(false);
const imagesSaving = ref(false);
const imagesError = ref<unknown>(null);

function syncImages(): void {
    draftImages.value = [...(product.value?.images ?? [])];
}

const imagesDirty = computed(() => {
    const current = (product.value?.images ?? []).map((i) => i.upload_id).join(",");
    return current !== draftImages.value.map((i) => i.upload_id).join(",");
});

function beforeUpload(file: File): boolean {
    void doUpload(file);
    // 返回 false 阻止 el-upload 自己发请求：上传要带 Authorization 与
    // Idempotency-Key，而那两样在 api/client.ts 里已经有唯一的出口。
    return false;
}

async function doUpload(file: File): Promise<void> {
    uploading.value = true;
    imagesError.value = null;
    // 一次上传 = 一次提交 = 一把新钥匙。
    const submission = new IdempotentSubmission();
    try {
        const uploaded = await withIdempotency(submission, (key) => uploadProductImage(file, key));
        draftImages.value.push({
            upload_id: uploaded.id,
            url: uploaded.url,
            sort_order: draftImages.value.length,
        });
        notifyOk("已上传。还要点「保存图片顺序」才会挂到商品上。");
    } catch (err) {
        imagesError.value = err;
    } finally {
        uploading.value = false;
    }
}

function moveImageToFront(index: number): void {
    const list = draftImages.value;
    const item = list[index];
    if (item === undefined || index === 0) return;
    list.splice(index, 1);
    list.unshift(item);
}

function removeImage(index: number): void {
    draftImages.value.splice(index, 1);
}

async function saveImages(): Promise<void> {
    imagesSaving.value = true;
    imagesError.value = null;
    try {
        // 请求体只收 upload_id，不收 URL —— 收 URL 等于让客户端往这里写
        // 任意地址（站外图床、别家租户的文件）。
        const saved = await keel.request("put", "/admin/products/{product_id}/images", {
            path: { product_id: id.value },
            body: { images: draftImages.value.map((i) => ({ upload_id: i.upload_id })) },
        });
        const p = product.value;
        if (p !== null) product.value = { ...p, images: saved };
        draftImages.value = [...saved];
        notifyOk("商品图已替换");
    } catch (err) {
        imagesError.value = err;
    } finally {
        imagesSaving.value = false;
    }
}

// -------------------------------------------------------------------- SKU

const skuDialog = ref(false);
const skuEditing = ref<AdminSku | null>(null);
const skuError = ref<unknown>(null);
const skuBusy = ref(false);
const skuSubmission = new IdempotentSubmission();
const specRows = ref<{ key: string; value: string }[]>([]);
const skuForm = ref<{
    sku_code: string;
    price_cents: number;
    cost_cents: number;
    weight_gram: number;
    available_qty: number;
    warning_qty: number;
    status: 0 | 1;
}>({ sku_code: "", price_cents: 0, cost_cents: 0, weight_gram: 0, available_qty: 0, warning_qty: 0, status: 1 });

function openSkuCreate(): void {
    skuEditing.value = null;
    skuError.value = null;
    skuSubmission.rotate();
    specRows.value = [{ key: "", value: "" }];
    skuForm.value = {
        sku_code: "",
        price_cents: 0,
        cost_cents: 0,
        weight_gram: 0,
        available_qty: 0,
        warning_qty: 0,
        status: 1,
    };
    skuDialog.value = true;
}

function openSkuEdit(sku: AdminSku): void {
    skuEditing.value = sku;
    skuError.value = null;
    specRows.value = Object.entries(sku.spec_values ?? {}).map(([key, value]) => ({ key, value }));
    if (specRows.value.length === 0) specRows.value = [{ key: "", value: "" }];
    skuForm.value = {
        sku_code: sku.sku_code,
        price_cents: sku.price_cents,
        cost_cents: sku.cost_cents ?? 0,
        weight_gram: sku.weight_gram ?? 0,
        // 改 SKU 的请求体里**没有** available_qty，这是刻意的（库存走 CAS 那条）。
        // 这两个值在编辑态是只读的展示。
        available_qty: sku.available_qty,
        warning_qty: sku.warning_qty ?? 0,
        status: sku.status,
    };
    skuDialog.value = true;
}

function specValues(): Record<string, string> {
    const out: Record<string, string> = {};
    for (const row of specRows.value) {
        const k = row.key.trim();
        if (k !== "") out[k] = row.value;
    }
    return out;
}

async function submitSku(): Promise<void> {
    skuBusy.value = true;
    skuError.value = null;
    try {
        const editing = skuEditing.value;
        if (editing === null) {
            const body: SkuCreateRequest = {
                sku_code: skuForm.value.sku_code.trim(),
                spec_values: specValues(),
                price_cents: skuForm.value.price_cents,
                cost_cents: skuForm.value.cost_cents,
                weight_gram: skuForm.value.weight_gram,
                available_qty: skuForm.value.available_qty,
                warning_qty: skuForm.value.warning_qty,
            };
            await withIdempotency(skuSubmission, (key) =>
                keel.request("post", "/admin/products/{product_id}/skus", {
                    path: { product_id: id.value },
                    body,
                    headers: { "Idempotency-Key": key },
                }),
            );
            notifyOk("SKU 已创建（库存行在同一个事务里建出）");
        } else {
            const body: SkuUpdateRequest = {
                sku_code: skuForm.value.sku_code.trim(),
                spec_values: specValues(),
                price_cents: skuForm.value.price_cents,
                cost_cents: skuForm.value.cost_cents,
                weight_gram: skuForm.value.weight_gram,
                status: skuForm.value.status,
            };
            await keel.request("patch", "/admin/skus/{sku_id}", { path: { sku_id: editing.id }, body });
            notifyOk("SKU 已保存");
        }
        skuDialog.value = false;
        await load();
    } catch (err) {
        skuError.value = err;
    } finally {
        skuBusy.value = false;
    }
}

async function removeSku(sku: AdminSku): Promise<void> {
    try {
        await ElMessageBox.confirm(
            `软删 SKU「${sku.sku_code}」？在架商品的最后一个 SKU 会被服务端拒绝。`,
            "确认删除",
            { type: "warning" },
        );
    } catch {
        return;
    }
    try {
        await keel.request("delete", "/admin/skus/{sku_id}", { path: { sku_id: sku.id } });
        notifyOk("已软删");
        await load();
    } catch (err) {
        notifyError(err);
    }
}

// ------------------------------------------------------------------ 库存

const inventoryDialog = ref(false);
const inventorySku = ref<InventoryTarget | null>(null);

function openInventory(sku: AdminSku): void {
    inventorySku.value = {
        skuId: sku.id,
        skuCode: sku.sku_code,
        spec: Object.entries(sku.spec_values ?? {})
            .map(([k, v]) => `${k}:${v}`)
            .join(" / "),
        availableQty: sku.available_qty,
        warningQty: sku.warning_qty ?? 0,
    };
    inventoryDialog.value = true;
}

function onInventoryUpdated(inv: AdminInventory): void {
    const p = product.value;
    if (p === null) return;
    // 只动那一行，不整页重载：重载会把用户正在编辑的图片顺序草稿冲掉。
    product.value = {
        ...p,
        skus: p.skus.map((s) =>
            s.id === inv.sku_id ? { ...s, available_qty: inv.available_qty, warning_qty: inv.warning_qty } : s,
        ),
    };
}
</script>

<template>
    <div v-loading="loading">
        <ProblemAlert v-if="loadError" :error="loadError" />

        <template v-if="product">
            <div class="page-toolbar">
                <el-button :icon="ArrowLeft" @click="router.push({ name: 'products' })">返回列表</el-button>
                <span class="title">{{ product.title }}</span>
                <el-tag :type="PRODUCT_STATUS[product.status].tag" size="small">
                    {{ PRODUCT_STATUS[product.status].text }}
                </el-tag>
                <el-tag v-if="product.deleted_at" type="danger" size="small">已软删</el-tag>
                <span class="grow" />
                <span class="hint">首次上架：{{ datetime(product.published_at) }}</span>
                <el-button type="primary" :loading="publishing" @click="togglePublication">
                    {{ product.status === 1 ? "下架" : "上架" }}
                </el-button>
                <el-button type="danger" plain :icon="Delete" @click="removeProduct">删除</el-button>
            </div>

            <ProblemAlert v-if="publishError" :error="publishError" />

            <el-tabs v-model="tab" type="border-card">
                <!-- ------------------------------------------------ 基本信息 -->
                <el-tab-pane label="基本信息" name="basic">
                    <ProblemAlert v-if="saveError" :error="saveError" />
                    <el-alert
                        v-if="complianceHits.length > 0"
                        type="error"
                        :closable="false"
                        show-icon
                        title="文案命中《广告法》第九条的绝对化用语"
                        class="mb12"
                    >
                        改动<strong>没有落库</strong>。下面每个字段的输入框下方标红的就是命中的字。
                    </el-alert>

                    <el-form label-width="90px" style="max-width: 760px" @submit.prevent>
                        <el-form-item label="标题">
                            <el-input v-model="form.title" maxlength="200" show-word-limit />
                            <HighlightedText
                                v-if="hitsFor('title').length > 0"
                                :text="form.title ?? ''"
                                :hits="hitsFor('title')"
                            />
                        </el-form-item>
                        <el-form-item label="副标题">
                            <el-input v-model="form.subtitle" />
                            <HighlightedText
                                v-if="hitsFor('subtitle').length > 0"
                                :text="form.subtitle ?? ''"
                                :hits="hitsFor('subtitle')"
                            />
                        </el-form-item>
                        <el-form-item label="详情">
                            <el-input v-model="form.description" type="textarea" :rows="6" />
                            <HighlightedText
                                v-if="hitsFor('description').length > 0"
                                :text="form.description ?? ''"
                                :hits="hitsFor('description')"
                            />
                        </el-form-item>
                        <el-form-item label="类目">
                            <el-select v-model="form.category_id" filterable>
                                <el-option
                                    v-for="c in categories"
                                    :key="c.id"
                                    :value="c.id"
                                    :label="indentedLabel(c)"
                                />
                            </el-select>
                        </el-form-item>
                        <el-form-item>
                            <el-button type="primary" :loading="saving" @click="saveBasic">保存</el-button>
                            <span class="hint ml8">
                                在架商品的文案改动要过一次违禁词检查；草稿与已下架的不过。
                            </span>
                        </el-form-item>
                    </el-form>

                    <el-descriptions :column="3" border size="small" class="derived">
                        <el-descriptions-item label="价格区间">
                            {{ yuan(product.min_price_cents) }} ~ {{ yuan(product.max_price_cents) }}
                        </el-descriptions-item>
                        <el-descriptions-item label="总库存">{{ product.total_stock }}</el-descriptions-item>
                        <el-descriptions-item label="销量">{{ product.sales_count }}</el-descriptions-item>
                    </el-descriptions>
                    <p class="hint">
                        这三个数是服务端按需现算的派生字段（迁移 00019），不接受写入 —— 改 SKU 之后下一次读就是新的。
                    </p>
                </el-tab-pane>

                <!-- -------------------------------------------------- 商品图 -->
                <el-tab-pane :label="`商品图（${draftImages.length}）`" name="images">
                    <ProblemAlert v-if="imagesError" :error="imagesError" />
                    <p class="hint">
                        一次调用替换整组，<strong>第一张就是主图</strong>，没有 is_primary 布尔。
                        先上传、排好序，再点保存。传空即清空全部图片。
                    </p>

                    <div class="page-toolbar">
                        <el-upload
                            :show-file-list="false"
                            :before-upload="beforeUpload"
                            accept="image/jpeg,image/png,image/webp"
                        >
                            <el-button :icon="Plus" :loading="uploading">上传图片</el-button>
                        </el-upload>
                        <span class="hint">单文件不超过 10 MB，只接受 jpeg / png / webp（服务端判据）</span>
                        <span class="grow" />
                        <el-button type="primary" :loading="imagesSaving" :disabled="!imagesDirty" @click="saveImages">
                            保存图片顺序
                        </el-button>
                    </div>

                    <div class="images">
                        <div v-for="(img, i) in draftImages" :key="img.upload_id" class="image-card">
                            <el-image :src="img.url" fit="cover" class="thumb" />
                            <div class="image-meta">
                                <el-tag v-if="i === 0" type="success" size="small">主图</el-tag>
                                <span class="hint">upload_id {{ img.upload_id }}</span>
                            </div>
                            <div class="image-actions">
                                <el-button link size="small" :icon="Top" :disabled="i === 0" @click="moveImageToFront(i)">
                                    设为主图
                                </el-button>
                                <el-button link size="small" type="danger" @click="removeImage(i)">移除</el-button>
                            </div>
                        </div>
                        <el-empty v-if="draftImages.length === 0" description="还没有图片" :image-size="80" />
                    </div>
                </el-tab-pane>

                <!-- ----------------------------------------------------- SKU -->
                <el-tab-pane :label="`SKU（${product.skus.length}）`" name="skus">
                    <div class="page-toolbar">
                        <span class="hint">
                            上架前至少要有一个 SKU。加 SKU 时服务端在同一个事务里建出库存行 ——
                            漏建的话这件商品会表现为「永远缺货」。
                        </span>
                        <span class="grow" />
                        <el-button type="primary" :icon="Plus" @click="openSkuCreate">加 SKU</el-button>
                    </div>

                    <el-table :data="product.skus" border stripe>
                        <el-table-column prop="sku_code" label="货号" width="140" />
                        <el-table-column label="规格" min-width="160">
                            <template #default="{ row }: { row: AdminSku }">
                                <el-tag v-for="(v, k) in row.spec_values ?? {}" :key="k" size="small" class="mr4">
                                    {{ k }}：{{ v }}
                                </el-tag>
                            </template>
                        </el-table-column>
                        <el-table-column label="售价" width="110">
                            <template #default="{ row }: { row: AdminSku }">{{ yuan(row.price_cents) }}</template>
                        </el-table-column>
                        <el-table-column label="成本" width="110">
                            <template #default="{ row }: { row: AdminSku }">
                                {{ row.cost_cents === undefined ? "—" : yuan(row.cost_cents) }}
                            </template>
                        </el-table-column>
                        <el-table-column label="可售库存" width="110">
                            <template #default="{ row }: { row: AdminSku }">
                                <b>{{ row.available_qty }}</b>
                                <span v-if="row.warning_qty" class="hint"> / 预警 {{ row.warning_qty }}</span>
                            </template>
                        </el-table-column>
                        <el-table-column label="状态" width="90">
                            <template #default="{ row }: { row: AdminSku }">
                                <el-tag :type="SKU_STATUS[row.status].tag" size="small">
                                    {{ SKU_STATUS[row.status].text }}
                                </el-tag>
                            </template>
                        </el-table-column>
                        <el-table-column label="操作" width="220" fixed="right">
                            <template #default="{ row }: { row: AdminSku }">
                                <el-button link type="primary" @click="openSkuEdit(row)">编辑</el-button>
                                <el-button link type="primary" @click="openInventory(row)">改库存</el-button>
                                <el-button link type="danger" @click="removeSku(row)">删除</el-button>
                            </template>
                        </el-table-column>
                    </el-table>
                </el-tab-pane>
            </el-tabs>
        </template>

        <!-- ------------------------------------------------------ SKU 对话框 -->
        <el-dialog v-model="skuDialog" :title="skuEditing === null ? '加 SKU' : `改 SKU（${skuEditing.sku_code}）`" width="640px">
            <ProblemAlert v-if="skuError" :error="skuError" />
            <el-form label-width="110px" @submit.prevent>
                <el-form-item label="货号" required>
                    <el-input v-model="skuForm.sku_code" placeholder="租户内唯一，不是全局唯一" />
                </el-form-item>
                <el-form-item label="规格">
                    <div class="spec-rows">
                        <div v-for="(row, i) in specRows" :key="i" class="spec-row">
                            <el-input v-model="row.key" placeholder="键，如 颜色" />
                            <el-input v-model="row.value" placeholder="值，如 黑" />
                            <el-button link type="danger" @click="specRows.splice(i, 1)">删</el-button>
                        </div>
                        <el-button link type="primary" @click="specRows.push({ key: '', value: '' })">
                            加一行
                        </el-button>
                    </div>
                    <p class="hint">一期不校验同一商品下各 SKU 的规格键是否一致（没有规格模板的表）。</p>
                </el-form-item>
                <el-form-item label="售价（分）">
                    <el-input-number v-model="skuForm.price_cents" :min="0" :step="100" />
                    <span class="hint ml8">{{ yuan(skuForm.price_cents) }}</span>
                </el-form-item>
                <el-form-item label="成本（分）">
                    <el-input-number v-model="skuForm.cost_cents" :min="0" :step="100" />
                    <span class="hint ml8">只在后台接口里出现，前台永远看不到</span>
                </el-form-item>
                <el-form-item label="重量（克）">
                    <el-input-number v-model="skuForm.weight_gram" :min="0" />
                </el-form-item>
                <template v-if="skuEditing === null">
                    <el-form-item label="初始库存">
                        <el-input-number v-model="skuForm.available_qty" :min="0" />
                    </el-form-item>
                    <el-form-item label="预警线">
                        <el-input-number v-model="skuForm.warning_qty" :min="0" />
                    </el-form-item>
                </template>
                <template v-else>
                    <el-form-item label="售卖状态">
                        <el-radio-group v-model="skuForm.status">
                            <el-radio :value="1">在售</el-radio>
                            <el-radio :value="0">停售</el-radio>
                        </el-radio-group>
                    </el-form-item>
                    <el-form-item label="库存">
                        <span class="hint">
                            当前 {{ skuForm.available_qty }}。改 SKU 的请求体里<strong>没有</strong>库存字段——
                            它走「比较并设置」那条，否则一次改价就能把并发下单扣掉的量抹掉。
                        </span>
                    </el-form-item>
                </template>
            </el-form>
            <template #footer>
                <el-button @click="skuDialog = false">取消</el-button>
                <el-button
                    type="primary"
                    :loading="skuBusy"
                    :disabled="skuForm.sku_code.trim() === ''"
                    @click="submitSku"
                >
                    提交
                </el-button>
            </template>
        </el-dialog>

        <InventoryDialog v-model="inventoryDialog" :sku="inventorySku" @updated="onInventoryUpdated" />
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
.mr4 {
    margin-right: 4px;
}
.derived {
    margin-top: 16px;
    max-width: 760px;
}
.images {
    display: flex;
    flex-wrap: wrap;
    gap: 12px;
}
.image-card {
    width: 160px;
    border: 1px solid var(--el-border-color-light);
    border-radius: 6px;
    padding: 8px;
}
.thumb {
    width: 144px;
    height: 144px;
    border-radius: 4px;
    background: var(--el-fill-color-light);
}
.image-meta {
    display: flex;
    align-items: center;
    gap: 6px;
    margin-top: 6px;
}
.image-actions {
    display: flex;
    justify-content: space-between;
    margin-top: 4px;
}
.spec-rows {
    width: 100%;
}
.spec-row {
    display: flex;
    gap: 8px;
    margin-bottom: 6px;
}
</style>
