<script setup lang="ts">
// 商品列表（后台）。
//
// 与前台 `GET /products` 是两条路：这一条默认返回**全部未软删**的商品，
// 含草稿与已下架 —— 后台要管的恰恰是前台看不见的那些。
// `include_deleted=true` 还能把软删的翻出来核对（一期没有恢复接口，
// 这个开关只为「看得见」存在）。

import { computed, onMounted, ref } from "vue";
import { useRouter } from "vue-router";
import { Plus, Refresh } from "@element-plus/icons-vue";
import { ElMessageBox } from "element-plus";
import {
    keel,
    type AdminCategory,
    type AdminProduct,
    type AdminProductPage,
    type ProductCreateRequest,
} from "../api/client.ts";
import { indentedLabel, listCategories } from "../api/catalog.ts";
import { IdempotentSubmission, withIdempotency } from "../api/idempotency.ts";
import { datetime, PRODUCT_STATUS, priceRange } from "../ui/format.ts";
import { notifyError, notifyOk } from "../ui/notify.ts";
import { can, NO_PERMISSION } from "../auth/permissions.ts";
import ProblemAlert from "../components/ProblemAlert.vue";

const router = useRouter();

const loading = ref(false);
const error = ref<unknown>(null);
const page = ref<AdminProductPage | null>(null);
const categories = ref<AdminCategory[]>([]);

// 筛选。status 用 undefined 表示「不按状态筛」——契约刻意没给 default，
// 传一个默认值会静默改变「返回哪些行」。
const filterStatus = ref<0 | 1 | 2 | undefined>(undefined);
const filterCategory = ref<number | undefined>(undefined);
const includeDeleted = ref(false);
const pageNo = ref(1);
const pageSize = ref(20);

const categoryName = computed(() => {
    const map = new Map<number, string>();
    for (const c of categories.value) map.set(c.id, c.name);
    return map;
});

async function load(): Promise<void> {
    loading.value = true;
    error.value = null;
    try {
        // query 的形状由契约推导：写错一个参数名编译不过。
        page.value = await keel.get("/admin/products", {
            query: {
                page: pageNo.value,
                page_size: pageSize.value,
                ...(filterStatus.value === undefined ? {} : { status: filterStatus.value }),
                ...(filterCategory.value === undefined ? {} : { category_id: filterCategory.value }),
                ...(includeDeleted.value ? { include_deleted: true } : {}),
            },
        });
    } catch (err) {
        error.value = err;
    } finally {
        loading.value = false;
    }
}

async function loadCategories(): Promise<void> {
    try {
        categories.value = await listCategories();
    } catch (err) {
        notifyError(err);
    }
}

function resetAndLoad(): void {
    pageNo.value = 1;
    void load();
}

onMounted(() => {
    void loadCategories();
    void load();
});

// ---------------------------------------------------------------- 新建商品

const createVisible = ref(false);
const createError = ref<unknown>(null);
const creating = ref(false);
// 一个对话框一把钥匙。判据写在 api/idempotency.ts 的文件头。
const createSubmission = new IdempotentSubmission();
const draft = ref<ProductCreateRequest>({ category_id: 0, title: "" });

function openCreate(): void {
    draft.value = { category_id: categories.value[0]?.id ?? 0, title: "" };
    createError.value = null;
    createSubmission.rotate();
    createVisible.value = true;
}

async function submitCreate(): Promise<void> {
    creating.value = true;
    createError.value = null;
    try {
        const created = await withIdempotency(createSubmission, (key) =>
            keel.request("post", "/admin/products", {
                body: draft.value,
                headers: { "Idempotency-Key": key },
            }),
        );
        createVisible.value = false;
        notifyOk(`已创建草稿「${created.title}」。上架前至少要有一个 SKU。`);
        await router.push({ name: "product-detail", params: { productId: created.id } });
    } catch (err) {
        createError.value = err;
    } finally {
        creating.value = false;
    }
}

// ------------------------------------------------------------ 上下架 / 软删
//
// 上下架也带幂等键（契约里那条 POST 声明了 Idempotency-Key）。
// 每一次点击是一次独立提交，所以这里每次都用一把新的。

async function togglePublication(row: AdminProduct): Promise<void> {
    const action = row.status === 1 ? "unpublish" : "publish";
    const submission = new IdempotentSubmission();
    try {
        const updated = await withIdempotency(submission, (key) =>
            keel.request("post", "/admin/products/{product_id}/publication", {
                path: { product_id: row.id },
                body: { action },
                headers: { "Idempotency-Key": key },
            }),
        );
        notifyOk(action === "publish" ? "已上架" : "已下架");
        row.status = updated.status;
        row.published_at = updated.published_at ?? null;
    } catch (err) {
        // 422 合规拒绝、409 没有 SKU、503 合规检查没结论 —— 三种都在这里
        // 原样显示。notifyError 会把 errors[] 里的「第几个字」带出来；
        // 要在标题上把违禁词标红，点进详情页。
        notifyError(err);
    }
}

async function removeProduct(row: AdminProduct): Promise<void> {
    try {
        await ElMessageBox.confirm(
            `软删「${row.title}」？行不会被删掉，只是置 deleted_at。一期没有恢复接口。`,
            "确认删除",
            { type: "warning", confirmButtonText: "删除", cancelButtonText: "取消" },
        );
    } catch {
        return;
    }
    try {
        await keel.request("delete", "/admin/products/{product_id}", { path: { product_id: row.id } });
        notifyOk("已软删");
        await load();
    } catch (err) {
        notifyError(err);
    }
}
</script>

<template>
    <div>
        <ProblemAlert v-if="error" :error="error" />

        <div class="page-toolbar">
            <el-select v-model="filterStatus" placeholder="全部状态" clearable style="width: 140px" @change="resetAndLoad">
                <el-option :value="0" label="草稿" />
                <el-option :value="1" label="上架" />
                <el-option :value="2" label="下架" />
            </el-select>

            <el-select
                v-model="filterCategory"
                placeholder="全部类目"
                clearable
                filterable
                style="width: 220px"
                @change="resetAndLoad"
            >
                <el-option v-for="c in categories" :key="c.id" :value="c.id" :label="indentedLabel(c)" />
            </el-select>

            <el-checkbox v-model="includeDeleted" @change="resetAndLoad">含已软删</el-checkbox>

            <span class="grow" />

            <el-button :icon="Refresh" :loading="loading" @click="load">刷新</el-button>
            <el-button type="primary" :icon="Plus" :disabled="!can.editCatalog()" :title="can.editCatalog() ? '' : NO_PERMISSION" @click="openCreate">新建商品</el-button>
        </div>

        <el-table :data="page?.items ?? []" v-loading="loading" border stripe row-key="id">
            <el-table-column prop="id" label="ID" width="80" />
            <el-table-column label="标题" min-width="220">
                <template #default="{ row }: { row: AdminProduct }">
                    <router-link :to="{ name: 'product-detail', params: { productId: row.id } }">
                        {{ row.title }}
                    </router-link>
                    <div v-if="row.subtitle" class="hint">{{ row.subtitle }}</div>
                </template>
            </el-table-column>
            <el-table-column label="状态" width="120">
                <template #default="{ row }: { row: AdminProduct }">
                    <el-tag :type="PRODUCT_STATUS[row.status].tag" size="small">
                        {{ PRODUCT_STATUS[row.status].text }}
                    </el-tag>
                    <el-tag v-if="row.deleted_at" type="danger" size="small" class="ml4">已删</el-tag>
                </template>
            </el-table-column>
            <el-table-column label="类目" width="140">
                <template #default="{ row }: { row: AdminProduct }">
                    {{ categoryName.get(row.category_id) ?? `#${row.category_id}` }}
                </template>
            </el-table-column>
            <el-table-column label="价格" width="160">
                <template #default="{ row }: { row: AdminProduct }">
                    {{ priceRange(row.min_price_cents, row.max_price_cents) }}
                </template>
            </el-table-column>
            <el-table-column prop="total_stock" label="库存" width="90" />
            <el-table-column prop="sales_count" label="销量" width="90" />
            <el-table-column label="首次上架" width="180">
                <template #default="{ row }: { row: AdminProduct }">{{ datetime(row.published_at) }}</template>
            </el-table-column>
            <el-table-column label="操作" width="220" fixed="right">
                <template #default="{ row }: { row: AdminProduct }">
                    <el-button
                        link
                        type="primary"
                        @click="router.push({ name: 'product-detail', params: { productId: row.id } })"
                    >
                        编辑
                    </el-button>
                    <el-button link type="primary" :disabled="!!row.deleted_at || !can.editCatalog()" @click="togglePublication(row)">
                        {{ row.status === 1 ? "下架" : "上架" }}
                    </el-button>
                    <el-button link type="danger" :disabled="!!row.deleted_at || !can.editCatalog()" @click="removeProduct(row)">
                        删除
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
                    pageNo = 1;
                    load();
                }
            "
        />

        <el-dialog v-model="createVisible" title="新建商品" width="560px">
            <p class="hint">
                新建的商品一律是<strong>草稿</strong>，不接受传 status —— 创建与发布是两个动作。
                上架前至少要有一个 SKU，那条闸门在上架接口上。
            </p>
            <ProblemAlert v-if="createError" :error="createError" />
            <el-form label-width="80px" @submit.prevent>
                <el-form-item label="类目" required>
                    <el-select v-model="draft.category_id" filterable placeholder="必填，没有「未分类」这个态">
                        <el-option v-for="c in categories" :key="c.id" :value="c.id" :label="indentedLabel(c)" />
                    </el-select>
                </el-form-item>
                <el-form-item label="标题" required>
                    <el-input v-model="draft.title" maxlength="200" show-word-limit />
                </el-form-item>
                <el-form-item label="副标题">
                    <el-input v-model="draft.subtitle" />
                </el-form-item>
                <el-form-item label="详情">
                    <el-input v-model="draft.description" type="textarea" :rows="4" />
                </el-form-item>
            </el-form>
            <template #footer>
                <el-button @click="createVisible = false">取消</el-button>
                <el-button
                    type="primary"
                    :loading="creating"
                    :disabled="draft.title.trim() === '' || draft.category_id === 0"
                    @click="submitCreate"
                >
                    创建草稿
                </el-button>
            </template>
        </el-dialog>
    </div>
</template>

<style scoped>
.pager {
    margin-top: 12px;
    justify-content: flex-end;
}
.ml4 {
    margin-left: 4px;
}
</style>
