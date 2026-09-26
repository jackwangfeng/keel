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
//   · **会话 7 天过期后，回来的路只有「重签一次性登录 token」**（POST
//     /admin/staff/{staff_id}/login-token）。它的 token 明文会回到签发人手里
//     ——没有邮件服务时这是把它交给本人的唯一办法——所以弹窗里只显示一次，
//     关掉就没了；再签一次会作废上一串。
//
// 分级权限（v0.1.0）：角色多了大区管理员（3）与门店管理员（4），各带管辖范围。
// 能分配哪些角色、能选哪些大区 / 门店，由 src/auth/permissions.ts 与服务端的
// 列表决定：大区管理员登录时，GET /admin/stores 本来就只回本大区的门店，
// 所以这里的门店下拉不需要自己再过滤一遍 —— 过滤只有服务端那一份。

import { computed, onMounted, ref } from "vue";
import { Plus, Refresh } from "@element-plus/icons-vue";
import {
    keel,
    currentSession,
    type AdminRegion,
    type AdminStore,
    type Staff,
    type StaffPage,
    type StaffCreateRequest,
    type StaffLoginToken,
    type StaffRole,
} from "../api/client.ts";
import { assignableRoles, can, NO_PERMISSION, ROLE, ROLE_TEXT } from "../auth/permissions.ts";
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
const selfId = currentSession()?.staff.id ?? 0;
const roles = assignableRoles();

// 大区与门店的选项。**服务端已经按调用者的范围收窄过**（大区管理员只拿到自己
// 的大区与本大区的门店），这里原样用。平台级没有门店可选，不去拉。
const regions = ref<AdminRegion[]>([]);
const stores = ref<AdminStore[]>([]);
async function loadScopeOptions(): Promise<void> {
    if (isPlatform || roles.length === 0) return;
    try {
        const [r, s] = await Promise.all([
            keel.get("/admin/regions", { query: { page: 1, page_size: 100 } }),
            keel.get("/admin/stores", { query: { page: 1, page_size: 100 } }),
        ]);
        regions.value = r.items;
        stores.value = s.items;
    } catch (err) {
        error.value = err;
    }
}
const regionName = computed(() => new Map(regions.value.map((r) => [r.id, r.name])));
const storeName = computed(() => new Map(stores.value.map((s) => [s.id, s.name])));
function scopeText(row: Staff): string {
    if (row.region_ids.length > 0) return row.region_ids.map((id) => regionName.value.get(id) ?? `大区 #${id}`).join("、");
    if (row.store_ids.length > 0) return row.store_ids.map((id) => storeName.value.get(id) ?? `门店 #${id}`).join("、");
    return row.role === ROLE.admin || row.role === ROLE.operator ? "全店" : "—";
}

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

onMounted(() => {
    void load();
    void loadScopeOptions();
});

// ------------------------------------------------------------------ 新建

const createVisible = ref(false);
const createError = ref<unknown>(null);
const creating = ref(false);
const createSubmission = new IdempotentSubmission();
const defaultRole = (): StaffRole => roles[roles.length - 1] ?? 2;
const draft = ref<StaffCreateRequest>({ email: "", name: "", role: defaultRole() });
const draftRegions = ref<number[]>([]);
const draftStores = ref<number[]>([]);

function openCreate(): void {
    draft.value = { email: "", name: "", role: defaultRole() };
    draftRegions.value = [];
    draftStores.value = [];
    createError.value = null;
    createSubmission.rotate();
    createVisible.value = true;
}

async function submitCreate(): Promise<void> {
    creating.value = true;
    createError.value = null;
    try {
        await withIdempotency(createSubmission, (key) =>
            keel.request("post", "/admin/staff", {
                // 范围只随对应的角色带上：角色与范围不配套时服务端回 422，
                // 而切换角色后残留在另一个下拉里的选择不该被悄悄提交。
                body: {
                    ...draft.value,
                    ...(draft.value.role === ROLE.regionManager ? { region_ids: draftRegions.value } : {}),
                    ...(draft.value.role === ROLE.storeManager ? { store_ids: draftStores.value } : {}),
                },
                headers: { "Idempotency-Key": key },
            }),
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
const editRole = ref<StaffRole>(2);
const editStatus = ref<1 | 2>(1);
const editRegions = ref<number[]>([]);
const editStores = ref<number[]>([]);
/** 改的是自己：角色与范围锁住（服务端同样拒绝，403 role-forbidden）。 */
const editingSelf = computed(() => editing.value?.id === selfId);

function openEdit(row: Staff): void {
    editing.value = row;
    editRole.value = row.role;
    editStatus.value = row.status;
    editRegions.value = [...row.region_ids];
    editStores.value = [...row.store_ids];
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
            body: {
                role: editRole.value,
                status: editStatus.value,
                ...(editRole.value === ROLE.regionManager ? { region_ids: editRegions.value } : {}),
                ...(editRole.value === ROLE.storeManager ? { store_ids: editStores.value } : {}),
            },
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

// ------------------------------------------------------------------ 重签登录 token

const tokenVisible = ref(false);
const tokenError = ref<unknown>(null);
const reissuing = ref(false);
const tokenTarget = ref<Staff | null>(null);
/** 签出来的那一串。只在弹窗里显示这一次：关掉弹窗就清掉，不留在页面状态里。 */
const issued = ref<StaffLoginToken | null>(null);

function reissueDisabledReason(row: Staff): string {
    if (!can.reissueLoginToken(row)) return NO_PERMISSION;
    if (row.status !== 1) return "已停用的员工不能重签，先启用（服务端同样会拒绝）";
    return "";
}

function openReissue(row: Staff): void {
    tokenTarget.value = row;
    issued.value = null;
    tokenError.value = null;
    tokenVisible.value = true;
}

function closeReissue(): void {
    tokenVisible.value = false;
    issued.value = null;
}

async function submitReissue(): Promise<void> {
    const target = tokenTarget.value;
    if (target === null) return;
    reissuing.value = true;
    tokenError.value = null;
    try {
        // 不带 Idempotency-Key：契约刻意不收它——服务端签新的同时作废旧的，
        // 重复点击的结果是「只有最新那一串有效」。
        issued.value = await keel.request("post", "/admin/staff/{staff_id}/login-token", {
            path: { staff_id: target.id },
        });
    } catch (err) {
        tokenError.value = err;
    } finally {
        reissuing.value = false;
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
            <el-button
                type="primary"
                :icon="Plus"
                :disabled="!can.manageStaff()"
                :title="can.manageStaff() ? '' : NO_PERMISSION"
                @click="openCreate"
            >
                加员工
            </el-button>
        </div>

        <el-table :data="page?.items ?? []" v-loading="loading" border stripe>
            <el-table-column prop="id" label="ID" width="80" />
            <el-table-column prop="email" label="邮箱" min-width="220" />
            <el-table-column prop="name" label="姓名" width="140" />
            <el-table-column label="角色" width="110">
                <template #default="{ row }: { row: Staff }">{{ STAFF_ROLE[row.role] }}</template>
            </el-table-column>
            <el-table-column label="管辖范围" min-width="180">
                <template #default="{ row }: { row: Staff }">{{ scopeText(row) }}</template>
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
            <el-table-column label="操作" width="200" fixed="right">
                <template #default="{ row }: { row: Staff }">
                    <el-button
                        link
                        type="primary"
                        :disabled="!can.editStaff(row)"
                        :title="can.editStaff(row) ? '' : NO_PERMISSION"
                        @click="openEdit(row)"
                    >
                        编辑
                    </el-button>
                    <el-button
                        link
                        type="primary"
                        :disabled="reissueDisabledReason(row) !== ''"
                        :title="reissueDisabledReason(row)"
                        @click="openReissue(row)"
                    >
                        重签登录 token
                    </el-button>
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
                        <el-radio v-for="r in roles" :key="r" :value="r">{{ ROLE_TEXT[r] }}</el-radio>
                    </el-radio-group>
                </el-form-item>
                <el-form-item v-if="draft.role === ROLE.regionManager" label="管的大区" required>
                    <el-select v-model="draftRegions" multiple placeholder="至少选一个大区" style="width: 100%">
                        <el-option v-for="r in regions" :key="r.id" :label="r.name" :value="r.id" />
                    </el-select>
                </el-form-item>
                <el-form-item v-if="draft.role === ROLE.storeManager" label="管的门店" required>
                    <el-select v-model="draftStores" multiple placeholder="至少选一家门店" style="width: 100%">
                        <el-option
                            v-for="s in stores"
                            :key="s.id"
                            :label="`${s.name}（${s.region_name}）`"
                            :value="s.id"
                        />
                    </el-select>
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
            <p v-if="editingSelf" class="hint">这是你自己：角色与管辖范围不能自己改（服务端会拒绝），只能请别的管理员改。</p>
            <el-form label-width="80px" @submit.prevent>
                <el-form-item label="角色">
                    <el-radio-group v-model="editRole" :disabled="editingSelf">
                        <el-radio v-for="r in roles" :key="r" :value="r">{{ ROLE_TEXT[r] }}</el-radio>
                    </el-radio-group>
                </el-form-item>
                <el-form-item v-if="editRole === ROLE.regionManager" label="管的大区">
                    <el-select v-model="editRegions" multiple :disabled="editingSelf" style="width: 100%">
                        <el-option v-for="r in regions" :key="r.id" :label="r.name" :value="r.id" />
                    </el-select>
                </el-form-item>
                <el-form-item v-if="editRole === ROLE.storeManager" label="管的门店">
                    <el-select v-model="editStores" multiple :disabled="editingSelf" style="width: 100%">
                        <el-option
                            v-for="s in stores"
                            :key="s.id"
                            :label="`${s.name}（${s.region_name}）`"
                            :value="s.id"
                        />
                    </el-select>
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

        <el-dialog
            v-model="tokenVisible"
            :title="`重签登录 token：${tokenTarget?.email ?? ''}`"
            width="560px"
            @closed="issued = null"
        >
            <ProblemAlert v-if="tokenError" :error="tokenError" />
            <template v-if="issued === null">
                <p class="hint">
                    给这个人签一串新的一次性登录 token（15 分钟有效、用掉即失效），他拿去在登录页
                    「已有登录 token」那一栏换会话。<strong>他此前还没用掉的登录 token 会同时作废。</strong>
                </p>
                <p class="hint">
                    本轮没有接邮件服务，token 会显示在这里（只显示这一次），也照旧打进进程日志。
                    请通过可信的渠道交给本人：拿着它的人能以他的身份登录一次。
                </p>
            </template>
            <template v-else>
                <p class="hint">已签发，{{ datetime(issued.expire_at) }} 之前有效。关掉这个窗口之后不会再显示。</p>
                <el-input :model-value="issued.token" readonly type="textarea" :rows="2" />
            </template>
            <template #footer>
                <el-button @click="closeReissue">{{ issued === null ? "取消" : "关闭" }}</el-button>
                <el-button v-if="issued === null" type="primary" :loading="reissuing" @click="submitReissue">
                    签发
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
code {
    background: var(--el-fill-color-light);
    padding: 1px 4px;
    border-radius: 3px;
}
</style>
