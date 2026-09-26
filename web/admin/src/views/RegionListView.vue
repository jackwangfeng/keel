<script setup lang="ts">
// 大区。大区**没有几何**——它是门店的分组，归属由 stores.region_id 决定。
// 它管两件事：这一片卖哪些商品（可见性的外层）、这一片卖什么价（价格的中间层）。
// 那两件事在「商品与定价」里（点进去）。

import { onMounted, ref } from "vue";
import { useRouter } from "vue-router";
import { Plus, Refresh } from "@element-plus/icons-vue";
import { ElMessageBox } from "element-plus";
import { keel, type AdminRegion, type RegionCreateRequest, type RegionPage } from "../api/client.ts";
import { IdempotentSubmission, withIdempotency } from "../api/idempotency.ts";
import { CATEGORY_STATUS } from "../ui/format.ts";
import { notifyError, notifyOk } from "../ui/notify.ts";
import { can, NO_PERMISSION } from "../auth/permissions.ts";
import ProblemAlert from "../components/ProblemAlert.vue";

const router = useRouter();
const loading = ref(false);
const error = ref<unknown>(null);
const page = ref<RegionPage | null>(null);
const pageNo = ref(1);
const includeDeleted = ref(false);

async function load(): Promise<void> {
    loading.value = true;
    error.value = null;
    try {
        page.value = await keel.get("/admin/regions", {
            query: { page: pageNo.value, page_size: 20, ...(includeDeleted.value ? { include_deleted: true } : {}) },
        });
    } catch (err) {
        error.value = err;
    } finally {
        loading.value = false;
    }
}
onMounted(() => void load());

// ---------------------------------------------------------------- 新建

const createVisible = ref(false);
const createError = ref<unknown>(null);
const creating = ref(false);
const submission = new IdempotentSubmission();
const draft = ref<RegionCreateRequest>({ code: "", name: "" });

function openCreate(): void {
    draft.value = { code: "", name: "" };
    createError.value = null;
    submission.rotate();
    createVisible.value = true;
}

async function submitCreate(): Promise<void> {
    creating.value = true;
    createError.value = null;
    try {
        const created = await withIdempotency(submission, (key) =>
            keel.request("post", "/admin/regions", { body: draft.value, headers: { "Idempotency-Key": key } }),
        );
        createVisible.value = false;
        notifyOk(`已建大区「${created.name}」`);
        await load();
    } catch (err) {
        createError.value = err;
    } finally {
        creating.value = false;
    }
}

// ---------------------------------------------------------------- 改

const editVisible = ref(false);
const editError = ref<unknown>(null);
const saving = ref(false);
const editing = ref<AdminRegion | null>(null);
const editCode = ref("");
const editName = ref("");
const editStatus = ref<0 | 1>(1);

function openEdit(row: AdminRegion): void {
    editing.value = row;
    editCode.value = row.code;
    editName.value = row.name;
    editStatus.value = row.status;
    editError.value = null;
    editVisible.value = true;
}

async function submitEdit(): Promise<void> {
    const r = editing.value;
    if (r === null) return;
    saving.value = true;
    editError.value = null;
    try {
        await keel.request("patch", "/admin/regions/{region_id}", {
            path: { region_id: r.id },
            body: { code: editCode.value, name: editName.value, status: editStatus.value },
        });
        editVisible.value = false;
        notifyOk("已保存");
        await load();
    } catch (err) {
        editError.value = err;
    } finally {
        saving.value = false;
    }
}

async function remove(row: AdminRegion): Promise<void> {
    try {
        await ElMessageBox.confirm(`软删大区「${row.name}」？`, "确认删除", { type: "warning" });
    } catch {
        return;
    }
    try {
        await keel.request("delete", "/admin/regions/{region_id}", { path: { region_id: row.id } });
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
            <span class="hint">大区是门店的分组：它决定这一片卖哪些商品、卖什么价。门店价可以在大区价之上再覆盖一层。</span>
            <el-checkbox v-model="includeDeleted" @change="(pageNo = 1), load()">含已软删</el-checkbox>
            <span class="grow" />
            <el-button :icon="Refresh" :loading="loading" @click="load">刷新</el-button>
            <el-button type="primary" :icon="Plus" :disabled="!can.createRegion()" :title="can.createRegion() ? '' : NO_PERMISSION" @click="openCreate">新建大区</el-button>
        </div>

        <el-table :data="page?.items ?? []" v-loading="loading" border stripe>
            <el-table-column prop="id" label="ID" width="70" />
            <el-table-column prop="code" label="编号" width="140" />
            <el-table-column label="名称" min-width="180">
                <template #default="{ row }: { row: AdminRegion }">
                    <router-link :to="{ name: 'region-detail', params: { regionId: row.id } }">{{ row.name }}</router-link>
                    <el-tag v-if="row.deleted_at" type="danger" size="small" class="ml4">已删</el-tag>
                </template>
            </el-table-column>
            <el-table-column label="状态" width="90">
                <template #default="{ row }: { row: AdminRegion }">
                    <el-tag :type="CATEGORY_STATUS[row.status].tag" size="small">{{ CATEGORY_STATUS[row.status].text }}</el-tag>
                </template>
            </el-table-column>
            <el-table-column label="门店数" width="100">
                <template #default="{ row }: { row: AdminRegion }">
                    <router-link :to="{ name: 'stores', query: { region_id: row.id } }">{{ row.store_count }}</router-link>
                </template>
            </el-table-column>
            <el-table-column label="操作" width="260" fixed="right">
                <template #default="{ row }: { row: AdminRegion }">
                    <el-button link type="primary" @click="router.push({ name: 'region-detail', params: { regionId: row.id } })">
                        商品与定价
                    </el-button>
                    <el-button link type="primary" :disabled="!!row.deleted_at || !can.manageRegion(row.id)" @click="openEdit(row)">编辑</el-button>
                    <el-tooltip
                        :disabled="row.store_count === 0"
                        :content="`名下还有 ${row.store_count} 家门店，删不掉（服务端 409）。先把门店挪走或删掉`"
                    >
                        <span>
                            <el-button link type="danger" :disabled="!!row.deleted_at || row.store_count > 0 || !can.manageRegion(row.id)" @click="remove(row)">
                                删除
                            </el-button>
                        </span>
                    </el-tooltip>
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

        <el-dialog v-model="createVisible" title="新建大区" width="480px">
            <ProblemAlert v-if="createError" :error="createError" />
            <el-form label-width="80px" @submit.prevent>
                <el-form-item label="编号" required><el-input v-model="draft.code" placeholder="租户内唯一，如 north" /></el-form-item>
                <el-form-item label="名称" required><el-input v-model="draft.name" placeholder="如 华北大区" /></el-form-item>
            </el-form>
            <template #footer>
                <el-button @click="createVisible = false">取消</el-button>
                <el-button
                    type="primary"
                    :loading="creating"
                    :disabled="draft.code.trim() === '' || draft.name.trim() === ''"
                    @click="submitCreate"
                >
                    创建
                </el-button>
            </template>
        </el-dialog>

        <el-dialog v-model="editVisible" :title="`编辑大区 ${editing?.name ?? ''}`" width="480px">
            <ProblemAlert v-if="editError" :error="editError" />
            <el-form label-width="80px" @submit.prevent>
                <el-form-item label="编号"><el-input v-model="editCode" /></el-form-item>
                <el-form-item label="名称"><el-input v-model="editName" /></el-form-item>
                <el-form-item label="状态">
                    <el-radio-group v-model="editStatus">
                        <el-radio :value="1">启用</el-radio>
                        <el-radio :value="0">停用</el-radio>
                    </el-radio-group>
                </el-form-item>
            </el-form>
            <template #footer>
                <el-button @click="editVisible = false">取消</el-button>
                <el-button type="primary" :loading="saving" @click="submitEdit">保存</el-button>
            </template>
        </el-dialog>
    </div>
</template>

<style scoped>
.ml4 {
    margin-left: 4px;
}
.pager {
    margin-top: 12px;
    justify-content: flex-end;
}
</style>
