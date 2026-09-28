<script setup lang="ts">
// AI 员工与密钥。只有本店管理员能进（AgentStaffView 按 can.manageAgents() 决定
// 要不要渲染这个 tab），服务端同样只认本店管理员（403 role-forbidden）。
//
// 密钥的明文（secret）只在发密钥的那一次响应里出现，服务端只存哈希——
// 弹窗关掉就再也看不到，这里不缓存它、不放进任何会被刷新覆盖的 ref 之外的地方。
import { computed, ref, onMounted } from "vue";
import { ElMessageBox } from "element-plus";
import { Plus, Refresh } from "@element-plus/icons-vue";
import {
    keel,
    type AdminAgent,
    type AdminRegion,
    type AdminStore,
    type AgentCreateRequest,
    type AgentKey,
    type AgentKeyCreated,
} from "../../api/client.ts";
import { AGENT_ROLES, mcpConfigSnippet } from "../../api/agents.ts";
import { listAllRegions, listAllStores } from "../../api/stores.ts";
import { IdempotentSubmission, withIdempotency } from "../../api/idempotency.ts";
import { ROLE, ROLE_TEXT } from "../../auth/permissions.ts";
import { datetime, STAFF_STATUS } from "../../ui/format.ts";
import { notifyOk } from "../../ui/notify.ts";
import ProblemAlert from "../../components/ProblemAlert.vue";

const loading = ref(false);
const error = ref<unknown>(null);
const agents = ref<AdminAgent[]>([]);

const regions = ref<AdminRegion[]>([]);
const stores = ref<AdminStore[]>([]);
const regionName = computed(() => new Map(regions.value.map((r) => [r.id, r.name])));
const storeName = computed(() => new Map(stores.value.map((s) => [s.id, s.name])));
function scopeText(row: AdminAgent): string {
    if (row.region_ids.length > 0) return row.region_ids.map((id) => regionName.value.get(id) ?? `大区 #${id}`).join("、");
    if (row.store_ids.length > 0) return row.store_ids.map((id) => storeName.value.get(id) ?? `门店 #${id}`).join("、");
    return row.role === ROLE.operator ? "全店" : "—";
}

async function load(): Promise<void> {
    loading.value = true;
    error.value = null;
    try {
        agents.value = (await keel.get("/admin/agents", {})).items;
    } catch (err) {
        error.value = err;
    } finally {
        loading.value = false;
    }
}

async function loadScopeOptions(): Promise<void> {
    try {
        const [r, s] = await Promise.all([listAllRegions(), listAllStores()]);
        regions.value = r;
        stores.value = s.stores;
    } catch (err) {
        error.value = err;
    }
}

onMounted(() => {
    void load();
    void loadScopeOptions();
});
defineExpose({ reload: load });

// ------------------------------------------------------------------ 新建

const createVisible = ref(false);
const createError = ref<unknown>(null);
const creating = ref(false);
const createSubmission = new IdempotentSubmission();
const draft = ref<AgentCreateRequest>({ name: "", role: 2 });
const draftRegions = ref<number[]>([]);
const draftStores = ref<number[]>([]);

function openCreate(): void {
    draft.value = { name: "", role: 2 };
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
            keel.request("post", "/admin/agents", {
                body: {
                    ...draft.value,
                    // 范围只随对应角色带上，同员工那张表的理由：角色切换后残留在
                    // 另一个下拉里的选择不该被悄悄提交。
                    ...(draft.value.role === ROLE.regionManager ? { region_ids: draftRegions.value } : {}),
                    ...(draft.value.role === ROLE.storeManager ? { store_ids: draftStores.value } : {}),
                },
                headers: { "Idempotency-Key": key },
            }),
        );
        createVisible.value = false;
        notifyOk("已创建。到「密钥」里给它发一把接入密钥");
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
const editing = ref<AdminAgent | null>(null);
const editName = ref("");
const editRole = ref<AdminAgent["role"]>(2);
const editStatus = ref<1 | 2>(1);
const editRegions = ref<number[]>([]);
const editStores = ref<number[]>([]);

function openEdit(row: AdminAgent): void {
    editing.value = row;
    editName.value = row.name;
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
        await keel.request("patch", "/admin/agents/{staff_id}", {
            path: { staff_id: target.id },
            body: {
                name: editName.value,
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
        editError.value = err;
    } finally {
        saving.value = false;
    }
}

// ------------------------------------------------------------------ 密钥（按需展开加载）

const keysByAgent = ref<Record<number, AgentKey[]>>({});
const keysLoading = ref<Record<number, boolean>>({});

async function loadKeys(agentId: number): Promise<void> {
    keysLoading.value = { ...keysLoading.value, [agentId]: true };
    try {
        const detail = await keel.get("/admin/agents/{staff_id}", { path: { staff_id: agentId } });
        keysByAgent.value = { ...keysByAgent.value, [agentId]: detail.keys ?? [] };
    } catch (err) {
        error.value = err;
    } finally {
        keysLoading.value = { ...keysLoading.value, [agentId]: false };
    }
}

function onExpandChange(row: AdminAgent, expandedRows: AdminAgent[]): void {
    if (expandedRows.some((r) => r.id === row.id) && keysByAgent.value[row.id] === undefined) void loadKeys(row.id);
}

// -------- 发密钥

const keyVisible = ref(false);
const keyError = ref<unknown>(null);
const issuing = ref(false);
const keyTarget = ref<AdminAgent | null>(null);
const keyName = ref("");
const keyExpiresDays = ref(0);
/** 签出来的那一把。只在弹窗里显示这一次。 */
const issued = ref<AgentKeyCreated | null>(null);

function openIssue(row: AdminAgent): void {
    keyTarget.value = row;
    keyName.value = "";
    keyExpiresDays.value = 0;
    keyError.value = null;
    issued.value = null;
    keyVisible.value = true;
}

function closeIssue(): void {
    keyVisible.value = false;
    if (issued.value !== null && keyTarget.value !== null) void loadKeys(keyTarget.value.id);
    issued.value = null;
}

async function submitIssue(): Promise<void> {
    const target = keyTarget.value;
    if (target === null || keyName.value.trim() === "") return;
    issuing.value = true;
    keyError.value = null;
    try {
        issued.value = await keel.request("post", "/admin/agents/{staff_id}/keys", {
            path: { staff_id: target.id },
            body: { name: keyName.value.trim(), expires_in_days: keyExpiresDays.value },
        });
    } catch (err) {
        keyError.value = err;
    } finally {
        issuing.value = false;
    }
}

const copied = ref(false);
function copySecret(): void {
    if (issued.value === null) return;
    navigator.clipboard.writeText(issued.value.secret).then(
        () => {
            copied.value = true;
            setTimeout(() => (copied.value = false), 1500);
        },
        () => undefined,
    );
}

// -------- 吊销

async function revoke(agent: AdminAgent, key: AgentKey): Promise<void> {
    try {
        await ElMessageBox.confirm(`吊销密钥「${key.name}」（${key.prefix}…）？吊销后用它接入的 MCP 客户端立即失效，不能撤回。`, "确认吊销", {
            type: "warning",
            confirmButtonText: "吊销",
            cancelButtonText: "取消",
        });
    } catch {
        return;
    }
    try {
        await keel.request("delete", "/admin/agents/{staff_id}/keys/{key_id}", {
            path: { staff_id: agent.id, key_id: key.id },
        });
        notifyOk("已吊销");
        await loadKeys(agent.id);
    } catch (err) {
        error.value = err;
    }
}
</script>

<template>
    <div>
        <ProblemAlert v-if="error" :error="error" />

        <div class="page-toolbar">
            <span class="hint">AI 员工是一种特殊的员工：不登录后台，靠密钥调 MCP 工具；写操作走「提案」，不直接执行。先「加 AI 员工」，再点那一行的「发密钥」；展开一行可看、可吊销已发的密钥。</span>
            <span class="grow" />
            <el-button :icon="Refresh" :loading="loading" @click="load">刷新</el-button>
            <el-button type="primary" :icon="Plus" @click="openCreate">加 AI 员工</el-button>
        </div>

        <el-table :data="agents" v-loading="loading" border stripe row-key="id" @expand-change="onExpandChange">
            <el-table-column type="expand">
                <template #default="{ row }: { row: AdminAgent }">
                    <div class="keys-panel">
                        <div class="keys-toolbar">
                            <span class="hint">密钥（明文只在发出的那一次显示）</span>
                            <span class="grow" />
                            <el-button size="small" type="primary" @click="openIssue(row)">发密钥</el-button>
                        </div>
                        <el-table :data="keysByAgent[row.id] ?? []" v-loading="keysLoading[row.id] === true" size="small" border>
                            <el-table-column prop="name" label="名称" width="140" />
                            <el-table-column label="前缀" width="140">
                                <template #default="{ row: k }: { row: AgentKey }">
                                    <code>{{ k.prefix }}…</code>
                                </template>
                            </el-table-column>
                            <el-table-column label="过期时间" width="170">
                                <template #default="{ row: k }: { row: AgentKey }">
                                    {{ k.expires_at ? datetime(k.expires_at) : "不过期" }}
                                </template>
                            </el-table-column>
                            <el-table-column label="吊销时间" width="170">
                                <template #default="{ row: k }: { row: AgentKey }">
                                    {{ k.revoked_at ? datetime(k.revoked_at) : "—" }}
                                </template>
                            </el-table-column>
                            <el-table-column label="最近使用" width="170">
                                <template #default="{ row: k }: { row: AgentKey }">{{ datetime(k.last_used_at) }}</template>
                            </el-table-column>
                            <el-table-column label="操作" width="90">
                                <template #default="{ row: k }: { row: AgentKey }">
                                    <el-button v-if="!k.revoked_at" link type="danger" size="small" @click="revoke(row, k)">
                                        吊销
                                    </el-button>
                                </template>
                            </el-table-column>
                        </el-table>
                        <el-empty
                            v-if="keysByAgent[row.id]?.length === 0 && keysLoading[row.id] !== true"
                            description="还没有密钥"
                            :image-size="50"
                        />
                    </div>
                </template>
            </el-table-column>
            <el-table-column prop="name" label="名称" min-width="140" />
            <el-table-column label="角色" width="110">
                <template #default="{ row }: { row: AdminAgent }">{{ ROLE_TEXT[row.role] }}</template>
            </el-table-column>
            <el-table-column label="管辖范围" min-width="160">
                <template #default="{ row }: { row: AdminAgent }">{{ scopeText(row) }}</template>
            </el-table-column>
            <el-table-column label="状态" width="90">
                <template #default="{ row }: { row: AdminAgent }">
                    <el-tag :type="STAFF_STATUS[row.status].tag" size="small">{{ STAFF_STATUS[row.status].text }}</el-tag>
                </template>
            </el-table-column>
            <el-table-column label="有效密钥" width="90" align="right">
                <template #default="{ row }: { row: AdminAgent }">{{ row.live_keys }}</template>
            </el-table-column>
            <el-table-column label="最近使用" width="170">
                <template #default="{ row }: { row: AdminAgent }">{{ datetime(row.last_used_at) }}</template>
            </el-table-column>
            <el-table-column label="操作" width="150" fixed="right">
                <template #default="{ row }: { row: AdminAgent }">
                    <el-button link type="primary" @click="openIssue(row)">发密钥</el-button>
                    <el-button link type="primary" @click="openEdit(row)">编辑</el-button>
                </template>
            </el-table-column>
        </el-table>

        <el-empty v-if="!loading && agents.length === 0" description="还没有 AI 员工" />

        <!-- 新建 -->
        <el-dialog v-model="createVisible" title="加 AI 员工" width="520px">
            <ProblemAlert v-if="createError" :error="createError" />
            <el-form label-width="90px" @submit.prevent>
                <el-form-item label="名称" required>
                    <el-input v-model="draft.name" placeholder="给它起个名字，比如「补货助手」" />
                </el-form-item>
                <el-form-item label="角色">
                    <el-radio-group v-model="draft.role">
                        <el-radio v-for="r in AGENT_ROLES" :key="r" :value="r">{{ ROLE_TEXT[r] }}</el-radio>
                    </el-radio-group>
                </el-form-item>
                <el-form-item v-if="draft.role === ROLE.regionManager" label="管的大区" required>
                    <el-select v-model="draftRegions" multiple placeholder="至少选一个大区" style="width: 100%">
                        <el-option v-for="r in regions" :key="r.id" :label="r.name" :value="r.id" />
                    </el-select>
                </el-form-item>
                <el-form-item v-if="draft.role === ROLE.storeManager" label="管的门店" required>
                    <el-select v-model="draftStores" multiple placeholder="至少选一家门店" style="width: 100%">
                        <el-option v-for="s in stores" :key="s.id" :label="`${s.name}（${s.region_name}）`" :value="s.id" />
                    </el-select>
                </el-form-item>
            </el-form>
            <template #footer>
                <el-button @click="createVisible = false">取消</el-button>
                <el-button type="primary" :loading="creating" :disabled="draft.name.trim() === ''" @click="submitCreate">
                    创建
                </el-button>
            </template>
        </el-dialog>

        <!-- 编辑 -->
        <el-dialog v-model="editVisible" :title="`编辑 ${editing?.name ?? ''}`" width="520px">
            <ProblemAlert v-if="editError" :error="editError" />
            <el-form label-width="90px" @submit.prevent>
                <el-form-item label="名称" required>
                    <el-input v-model="editName" />
                </el-form-item>
                <el-form-item label="角色">
                    <el-radio-group v-model="editRole">
                        <el-radio v-for="r in AGENT_ROLES" :key="r" :value="r">{{ ROLE_TEXT[r] }}</el-radio>
                    </el-radio-group>
                </el-form-item>
                <el-form-item v-if="editRole === ROLE.regionManager" label="管的大区">
                    <el-select v-model="editRegions" multiple style="width: 100%">
                        <el-option v-for="r in regions" :key="r.id" :label="r.name" :value="r.id" />
                    </el-select>
                </el-form-item>
                <el-form-item v-if="editRole === ROLE.storeManager" label="管的门店">
                    <el-select v-model="editStores" multiple style="width: 100%">
                        <el-option v-for="s in stores" :key="s.id" :label="`${s.name}（${s.region_name}）`" :value="s.id" />
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

        <!-- 发密钥 -->
        <el-dialog v-model="keyVisible" :title="`发密钥：${keyTarget?.name ?? ''}`" width="560px" @closed="closeIssue">
            <ProblemAlert v-if="keyError" :error="keyError" />
            <template v-if="issued === null">
                <el-form label-width="90px" @submit.prevent>
                    <el-form-item label="名称" required>
                        <el-input v-model="keyName" placeholder="给这把密钥起个名字，比如它跑在哪台机器上" />
                    </el-form-item>
                    <el-form-item label="有效期">
                        <el-input-number v-model="keyExpiresDays" :min="0" />
                        <span class="hint suffix">天，0 = 不过期</span>
                    </el-form-item>
                </el-form>
            </template>
            <template v-else>
                <el-alert type="warning" :closable="false" show-icon class="mb12">
                    <template #title>只显示这一次，关掉就看不到了。请立刻复制并交给要接入的那一侧。</template>
                </el-alert>
                <p class="hint">密钥明文：</p>
                <el-input :model-value="issued.secret" readonly>
                    <template #append>
                        <el-button @click="copySecret">{{ copied ? "已复制" : "复制" }}</el-button>
                    </template>
                </el-input>
                <p class="hint">MCP 接入配置（把里面的密钥换成上面这串）：</p>
                <pre class="snippet">{{ mcpConfigSnippet(issued.secret) }}</pre>
            </template>
            <template #footer>
                <el-button v-if="issued === null" @click="keyVisible = false">取消</el-button>
                <el-button v-if="issued === null" type="primary" :loading="issuing" :disabled="keyName.trim() === ''" @click="submitIssue">
                    发放
                </el-button>
                <el-button v-else type="primary" @click="keyVisible = false">关闭</el-button>
            </template>
        </el-dialog>
    </div>
</template>

<style scoped>
.keys-panel {
    padding: 8px 16px 16px;
    background: var(--el-fill-color-lighter);
}
.keys-toolbar {
    display: flex;
    align-items: center;
    margin-bottom: 8px;
}
.suffix {
    margin-left: 8px;
}
.mb12 {
    margin-bottom: 12px;
}
.snippet {
    background: var(--el-fill-color-light);
    padding: 10px;
    border-radius: 4px;
    font-size: 12px;
    white-space: pre-wrap;
    word-break: break-all;
    max-height: 220px;
    overflow: auto;
}
code {
    background: var(--el-fill-color-light);
    padding: 1px 4px;
    border-radius: 3px;
}
</style>
