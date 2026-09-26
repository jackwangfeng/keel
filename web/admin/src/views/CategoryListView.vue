<script setup lang="ts">
// 类目（后台，扁平）。
//
// 契约刻意返回**扁平数组**而不是前台那棵树：后台要管的是单个节点
// （改名、挪位置、停用），而树在表格里还得再拍平一次。`path` 与 `level`
// 都在每一行上，这里按 level 缩进显示，够用了。
//
// 两处必须在界面上说清楚的语义：
//
//   · **`parent_id` 传 null 与不传是两件事**：null 是「移到根」，
//     不传是「不动层级」。所以编辑对话框里「移动」是一个要显式打开的开关，
//     而不是一个默认就带着当前父节点值提交的下拉框。
//   · **停用不等于下架**：status = 0 只让这个类目从前台目录树里消失，
//     挂在它下面的商品仍然在架、仍然搜得到。

import { computed, onMounted, ref } from "vue";
import { Plus, Refresh } from "@element-plus/icons-vue";
import { ElMessageBox } from "element-plus";
import { keel, type AdminCategory, type CategoryCreateRequest, type CategoryUpdateRequest } from "../api/client.ts";
import { indentedLabel, listCategories } from "../api/catalog.ts";
import { IdempotentSubmission, withIdempotency } from "../api/idempotency.ts";
import { CATEGORY_STATUS } from "../ui/format.ts";
import { notifyError, notifyOk } from "../ui/notify.ts";
import { can, NO_PERMISSION } from "../auth/permissions.ts";
import ProblemAlert from "../components/ProblemAlert.vue";

const loading = ref(false);
const error = ref<unknown>(null);
const rows = ref<AdminCategory[]>([]);

async function load(): Promise<void> {
    loading.value = true;
    error.value = null;
    try {
        rows.value = await listCategories();
    } catch (err) {
        error.value = err;
    } finally {
        loading.value = false;
    }
}

onMounted(() => void load());

const nameById = computed(() => {
    const map = new Map<number, string>();
    for (const c of rows.value) map.set(c.id, c.name);
    return map;
});

// ------------------------------------------------------------------ 新建

const createVisible = ref(false);
const createError = ref<unknown>(null);
const creating = ref(false);
const createSubmission = new IdempotentSubmission();
const draft = ref<CategoryCreateRequest>({ name: "", parent_id: null, sort_order: 0 });

function openCreate(parent: AdminCategory | null): void {
    draft.value = { name: "", parent_id: parent?.id ?? null, sort_order: 0 };
    createError.value = null;
    createSubmission.rotate();
    createVisible.value = true;
}

async function submitCreate(): Promise<void> {
    creating.value = true;
    createError.value = null;
    try {
        await withIdempotency(createSubmission, (key) =>
            keel.request("post", "/admin/categories", {
                body: draft.value,
                headers: { "Idempotency-Key": key },
            }),
        );
        createVisible.value = false;
        notifyOk("已创建。path 与 level 由服务端从 parent_id 算出。");
        await load();
    } catch (err) {
        createError.value = err;
    } finally {
        creating.value = false;
    }
}

// ------------------------------------------------------------------ 编辑

const editVisible = ref(false);
const editError = ref<unknown>(null);
const saving = ref(false);
const editing = ref<AdminCategory | null>(null);
const editName = ref("");
const editSort = ref(0);
const editStatus = ref<0 | 1>(1);
/** 打开才动层级。关着时请求体里**不出现** parent_id。 */
const moveEnabled = ref(false);
/** null 表示移到根。 */
const moveTarget = ref<number | null>(null);

function openEdit(row: AdminCategory): void {
    editing.value = row;
    editName.value = row.name;
    editSort.value = row.sort_order;
    editStatus.value = row.status;
    moveEnabled.value = false;
    moveTarget.value = row.parent_id ?? null;
    editError.value = null;
    editVisible.value = true;
}

/** 可以移到哪些节点下：不能是自己，也不能是自己的后代（服务端判据用 path）。 */
const moveCandidates = computed(() => {
    const me = editing.value;
    if (me === null) return [];
    return rows.value.filter((c) => c.id !== me.id && !c.path.startsWith(me.path));
});

async function submitEdit(): Promise<void> {
    const me = editing.value;
    if (me === null) return;
    saving.value = true;
    editError.value = null;
    try {
        // 只有打开「移动」时才放 parent_id 进请求体。
        // 无条件带上当前值也能跑，但那会把「改个名字」变成一次子树重写
        // （服务端要重算 path 与 level），而且请求体表达的意思不是用户的意思。
        const body: CategoryUpdateRequest = {
            name: editName.value,
            sort_order: editSort.value,
            status: editStatus.value,
            ...(moveEnabled.value ? { parent_id: moveTarget.value } : {}),
        };
        await keel.request("patch", "/admin/categories/{category_id}", {
            path: { category_id: me.id },
            body,
        });
        editVisible.value = false;
        notifyOk(moveEnabled.value ? "已保存（子树的 path 与 level 由服务端同事务重写）" : "已保存");
        await load();
    } catch (err) {
        editError.value = err;
    } finally {
        saving.value = false;
    }
}

// ------------------------------------------------------------------ 删除

async function removeCategory(row: AdminCategory): Promise<void> {
    try {
        await ElMessageBox.confirm(
            `软删类目「${row.name}」？还有子类目或还有商品挂在它下面时，服务端会拒绝（409）——要从叶子往上删。`,
            "确认删除",
            { type: "warning" },
        );
    } catch {
        return;
    }
    try {
        await keel.request("delete", "/admin/categories/{category_id}", { path: { category_id: row.id } });
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
            <span class="hint">
                按 path 升序，含停用（status = 0），不含软删。<strong>停用不等于下架</strong>——
                停掉的是目录入口，不是商品。
            </span>
            <span class="grow" />
            <el-button :icon="Refresh" :loading="loading" @click="load">刷新</el-button>
            <el-button type="primary" :icon="Plus" :disabled="!can.editCatalog()" :title="can.editCatalog() ? '' : NO_PERMISSION" @click="openCreate(null)">新建根类目</el-button>
        </div>

        <el-table :data="rows" v-loading="loading" border stripe row-key="id">
            <el-table-column prop="id" label="ID" width="80" />
            <el-table-column label="名称" min-width="220">
                <template #default="{ row }: { row: AdminCategory }">
                    <span :style="{ paddingLeft: `${(row.level - 1) * 18}px` }">{{ row.name }}</span>
                </template>
            </el-table-column>
            <el-table-column label="父类目" width="160">
                <template #default="{ row }: { row: AdminCategory }">
                    {{ row.parent_id === null || row.parent_id === undefined ? "（根）" : (nameById.get(row.parent_id) ?? `#${row.parent_id}`) }}
                </template>
            </el-table-column>
            <el-table-column prop="path" label="path" width="160" />
            <el-table-column prop="level" label="层级" width="80" />
            <el-table-column prop="sort_order" label="排序" width="80" />
            <el-table-column label="状态" width="90">
                <template #default="{ row }: { row: AdminCategory }">
                    <el-tag :type="CATEGORY_STATUS[row.status].tag" size="small">
                        {{ CATEGORY_STATUS[row.status].text }}
                    </el-tag>
                </template>
            </el-table-column>
            <el-table-column label="操作" width="220" fixed="right">
                <template #default="{ row }: { row: AdminCategory }">
                    <el-button link type="primary" :disabled="!can.editCatalog()" :title="can.editCatalog() ? '' : NO_PERMISSION" @click="openEdit(row)">编辑</el-button>
                    <el-button link type="primary" :disabled="!can.editCatalog()" :title="can.editCatalog() ? '' : NO_PERMISSION" @click="openCreate(row)">加子类目</el-button>
                    <el-button link type="danger" :disabled="!can.editCatalog()" :title="can.editCatalog() ? '' : NO_PERMISSION" @click="removeCategory(row)">删除</el-button>
                </template>
            </el-table-column>
        </el-table>

        <el-dialog v-model="createVisible" title="新建类目" width="520px">
            <ProblemAlert v-if="createError" :error="createError" />
            <p class="hint">path 与 level 不在请求体里，由服务端从 parent_id 算出（它们是索引的内容）。</p>
            <el-form label-width="80px" @submit.prevent>
                <el-form-item label="名称" required>
                    <el-input v-model="draft.name" />
                </el-form-item>
                <el-form-item label="父类目">
                    <el-select v-model="draft.parent_id" clearable filterable placeholder="不选即根类目">
                        <el-option v-for="c in rows" :key="c.id" :value="c.id" :label="indentedLabel(c)" />
                    </el-select>
                </el-form-item>
                <el-form-item label="排序">
                    <el-input-number v-model="draft.sort_order" :min="0" />
                </el-form-item>
            </el-form>
            <template #footer>
                <el-button @click="createVisible = false">取消</el-button>
                <el-button type="primary" :loading="creating" :disabled="draft.name.trim() === ''" @click="submitCreate">
                    创建
                </el-button>
            </template>
        </el-dialog>

        <el-dialog v-model="editVisible" :title="`编辑类目 ${editing?.name ?? ''}`" width="520px">
            <ProblemAlert v-if="editError" :error="editError" />
            <el-form label-width="96px" @submit.prevent>
                <el-form-item label="名称">
                    <el-input v-model="editName" />
                </el-form-item>
                <el-form-item label="排序">
                    <el-input-number v-model="editSort" :min="0" />
                </el-form-item>
                <el-form-item label="状态">
                    <el-radio-group v-model="editStatus">
                        <el-radio :value="1">启用</el-radio>
                        <el-radio :value="0">停用</el-radio>
                    </el-radio-group>
                </el-form-item>
                <el-form-item label="移动子树">
                    <el-switch v-model="moveEnabled" />
                    <span class="hint ml8">
                        不打开时请求体里<strong>不出现</strong> parent_id，层级不动。
                    </span>
                </el-form-item>
                <el-form-item v-if="moveEnabled" label="移动到">
                    <el-select v-model="moveTarget" filterable placeholder="（根）">
                        <el-option :value="null" label="（根）—— parent_id 显式传 null" />
                        <el-option v-for="c in moveCandidates" :key="c.id" :value="c.id" :label="indentedLabel(c)" />
                    </el-select>
                    <p class="hint">
                        目标不能是自己或自己的后代（会成环，服务端 409）。上面的候选里已经排除了。
                    </p>
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
.ml8 {
    margin-left: 8px;
}
</style>
