<script setup lang="ts">
// 员工。
//
// 两处契约里写死、界面必须如实转达的事：
//
//   · **新员工的租户归属从会话继承，请求体里没有 merchant_id。** 允许前端
//     指定租户等于把越权做成一个入参。所以这个表单里没有那一格，
//     而且不是「隐藏了」——契约生成的 StaffCreateRequest 根本没有这个字段。
//   · **不设密码。** 建好后服务端生成一串一次性登录链接 token。本轮没有接
//     邮件服务，它只进进程日志，所以这里把取它的办法写出来。

import { onMounted, ref } from "vue";
import { Plus, Refresh } from "@element-plus/icons-vue";
import { keel, currentSession, type Staff, type StaffPage, type StaffCreateRequest } from "../api/client.ts";
import { IdempotentSubmission, withIdempotency } from "../api/idempotency.ts";
import { datetime, STAFF_ROLE, STAFF_STATUS } from "../ui/format.ts";
import { notifyOk } from "../ui/notify.ts";
import ProblemAlert from "../components/ProblemAlert.vue";

const loading = ref(false);
const error = ref<unknown>(null);
const page = ref<StaffPage | null>(null);
const pageNo = ref(1);
const pageSize = ref(20);

const isPlatform = currentSession()?.staff.merchant_id === null;

async function load(): Promise<void> {
    loading.value = true;
    error.value = null;
    try {
        page.value = await keel.get("/admin/staff", { query: { page: pageNo.value, page_size: pageSize.value } });
    } catch (err) {
        error.value = err;
    } finally {
        loading.value = false;
    }
}

onMounted(() => void load());

// ------------------------------------------------------------------ 新建

const createVisible = ref(false);
const createError = ref<unknown>(null);
const creating = ref(false);
const createSubmission = new IdempotentSubmission();
const draft = ref<StaffCreateRequest>({ email: "", name: "", role: 2 });

function openCreate(): void {
    draft.value = { email: "", name: "", role: 2 };
    createError.value = null;
    createSubmission.rotate();
    createVisible.value = true;
}

async function submitCreate(): Promise<void> {
    creating.value = true;
    createError.value = null;
    try {
        await withIdempotency(createSubmission, (key) =>
            keel.request("post", "/admin/staff", { body: draft.value, headers: { "Idempotency-Key": key } }),
        );
        createVisible.value = false;
        notifyOk("已创建。一次性登录 token 在进程日志里：docker compose logs app | grep 登录链接");
        await load();
    } catch (err) {
        createError.value = err;
    } finally {
        creating.value = false;
    }
}

// ------------------------------------------------------------------ 改

const editVisible = ref(false);
const editError = ref<unknown>(null);
const saving = ref(false);
const editing = ref<Staff | null>(null);
const editRole = ref<1 | 2>(2);
const editStatus = ref<1 | 2>(1);

function openEdit(row: Staff): void {
    editing.value = row;
    editRole.value = row.role;
    editStatus.value = row.status;
    editError.value = null;
    editVisible.value = true;
}

async function submitEdit(): Promise<void> {
    const target = editing.value;
    if (target === null) return;
    saving.value = true;
    editError.value = null;
    try {
        await keel.request("patch", "/admin/staff/{staff_id}", {
            path: { staff_id: target.id },
            body: { role: editRole.value, status: editStatus.value },
        });
        editVisible.value = false;
        notifyOk("已保存");
        await load();
    } catch (err) {
        // 409：会导致该租户没有在职管理员。服务端的 title 说得比这里清楚。
        editError.value = err;
    } finally {
        saving.value = false;
    }
}
</script>

<template>
    <div>
        <ProblemAlert v-if="error" :error="error" />

        <div class="page-toolbar">
            <span class="hint">
                {{ isPlatform ? "你是平台级操作员，这里是平台操作员名单。" : "只看得见自己店里的人。" }}
                新员工的租户归属从你的会话继承，不接受请求体传入。
            </span>
            <span class="grow" />
            <el-button :icon="Refresh" :loading="loading" @click="load">刷新</el-button>
            <el-button type="primary" :icon="Plus" @click="openCreate">加员工</el-button>
        </div>

        <el-table :data="page?.items ?? []" v-loading="loading" border stripe>
            <el-table-column prop="id" label="ID" width="80" />
            <el-table-column prop="email" label="邮箱" min-width="220" />
            <el-table-column prop="name" label="姓名" width="140" />
            <el-table-column label="角色" width="110">
                <template #default="{ row }: { row: Staff }">{{ STAFF_ROLE[row.role] }}</template>
            </el-table-column>
            <el-table-column label="归属" width="140">
                <template #default="{ row }: { row: Staff }">
                    <el-tag v-if="row.merchant_id === null" type="warning" size="small">平台级</el-tag>
                    <span v-else>商家 #{{ row.merchant_id }}</span>
                </template>
            </el-table-column>
            <el-table-column label="状态" width="90">
                <template #default="{ row }: { row: Staff }">
                    <el-tag :type="STAFF_STATUS[row.status].tag" size="small">
                        {{ STAFF_STATUS[row.status].text }}
                    </el-tag>
                </template>
            </el-table-column>
            <el-table-column label="最近登录" width="180">
                <template #default="{ row }: { row: Staff }">{{ datetime(row.last_login_at) }}</template>
            </el-table-column>
            <el-table-column label="操作" width="100" fixed="right">
                <template #default="{ row }: { row: Staff }">
                    <el-button link type="primary" @click="openEdit(row)">编辑</el-button>
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
            @current-change="
                (n: number) => {
                    pageNo = n;
                    load();
                }
            "
        />

        <el-dialog v-model="createVisible" title="加员工" width="520px">
            <ProblemAlert v-if="createError" :error="createError" />
            <p class="hint">
                <strong>不设密码。</strong>建好后服务端生成一串 15 分钟有效的一次性登录 token。
                本轮没有接邮件服务，它只进进程日志：<code>docker compose logs app | grep 登录链接</code>
            </p>
            <el-form label-width="80px" @submit.prevent>
                <el-form-item label="邮箱" required>
                    <el-input v-model="draft.email" />
                </el-form-item>
                <el-form-item label="姓名">
                    <el-input v-model="draft.name" />
                </el-form-item>
                <el-form-item label="角色">
                    <el-radio-group v-model="draft.role">
                        <el-radio :value="1">管理员</el-radio>
                        <el-radio :value="2">操作员</el-radio>
                    </el-radio-group>
                </el-form-item>
            </el-form>
            <template #footer>
                <el-button @click="createVisible = false">取消</el-button>
                <el-button
                    type="primary"
                    :loading="creating"
                    :disabled="String(draft.email).trim() === ''"
                    @click="submitCreate"
                >
                    创建并发链接
                </el-button>
            </template>
        </el-dialog>

        <el-dialog v-model="editVisible" :title="`编辑 ${editing?.email ?? ''}`" width="480px">
            <ProblemAlert v-if="editError" :error="editError" />
            <p class="hint">不能把最后一个在职管理员降级或停用——那会让这个租户失去全部管理能力（409）。</p>
            <el-form label-width="80px" @submit.prevent>
                <el-form-item label="角色">
                    <el-radio-group v-model="editRole">
                        <el-radio :value="1">管理员</el-radio>
                        <el-radio :value="2">操作员</el-radio>
                    </el-radio-group>
                </el-form-item>
                <el-form-item label="状态">
                    <el-radio-group v-model="editStatus">
                        <el-radio :value="1">正常</el-radio>
                        <el-radio :value="2">停用</el-radio>
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
.pager {
    margin-top: 12px;
    justify-content: flex-end;
}
code {
    background: var(--el-fill-color-light);
    padding: 1px 4px;
    border-radius: 3px;
}
</style>
