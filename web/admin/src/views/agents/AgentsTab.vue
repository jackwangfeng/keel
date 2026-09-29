<script setup lang="ts">
// AI 员工与密钥。只有本店管理员能进（AgentStaffView 按 can.manageAgents() 决定
// 要不要渲染这个 tab），服务端同样只认本店管理员（403 role-forbidden）。
//
// 密钥的明文（secret）只在发密钥的那一次响应里出现，服务端只存哈希——
// 弹窗关掉就再也看不到，这里不缓存它、不放进任何会被刷新覆盖的 ref 之外的地方。
import { computed, ref, onMounted } from "vue";
import { ElMessage, ElMessageBox } from "element-plus";
import { Plus, Refresh } from "@element-plus/icons-vue";
import {
    keel,
    ProblemError,
    type AdminAgent,
    type AdminRegion,
    type AdminStore,
    type AgentAutoPolicyInput,
    type AgentCreateRequest,
    type AgentKey,
    type AgentKeyCreated,
    type AgentWebhook,
    type AgentWebhookDelivery,
} from "../../api/client.ts";
import { AGENT_ROLES, mcpConfigSnippet } from "../../api/agents.ts";
import {
    AUTO_POLICY_KINDS,
    AUTO_POLICY_KIND_LABEL,
    capFieldOf,
    capInputText,
    parseCap,
    withCap,
    type AutoPolicyKind,
} from "../../api/agentPolicyRules.ts";
import { listAllRegions, listAllStores } from "../../api/stores.ts";
import { IdempotentSubmission, withIdempotency } from "../../api/idempotency.ts";
import { ROLE, ROLE_TEXT } from "../../auth/permissions.ts";
import { datetime, STAFF_STATUS } from "../../ui/format.ts";
import { notifyOk } from "../../ui/notify.ts";
import { useMobile } from "../../ui/useMobile.ts";
import ProblemAlert from "../../components/ProblemAlert.vue";

const mobile = useMobile();

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

/** 手机卡片：点「查看密钥」展开，展开时按需拉一次（与桌面版展开行同一套 keysByAgent/loadKeys）。 */
const expandedAgentIds = ref<Set<number>>(new Set());
function toggleAgentKeys(row: AdminAgent): void {
    const next = new Set(expandedAgentIds.value);
    if (next.has(row.id)) {
        next.delete(row.id);
    } else {
        next.add(row.id);
        if (keysByAgent.value[row.id] === undefined) void loadKeys(row.id);
    }
    expandedAgentIds.value = next;
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

// ------------------------------------------------------------------ 设置（自动执行策略 + 事件 webhook，AI 经营 M11）

const settingsVisible = ref(false);
const settingsTarget = ref<AdminAgent | null>(null);
const settingsTab = ref<"policy" | "webhook">("policy");

interface PolicyFormRow {
    kind: AutoPolicyKind;
    enabled: boolean;
    /** 单笔上限的输入框内容——`capFieldOf(kind)` 为 null（改文案）时不用。 */
    capInput: string;
    dailyLimit: number;
    saving: boolean;
    /** 原始四字段，保存时只替换这一种相关的那一个，其余原样带回。 */
    base: AgentAutoPolicyInput;
}

const policyLoading = ref(false);
const policyError = ref<unknown>(null);
const policyRows = ref<PolicyFormRow[]>([]);

async function loadPolicies(staffId: number): Promise<void> {
    policyLoading.value = true;
    policyError.value = null;
    try {
        const list = await keel.get("/admin/agents/{staff_id}/auto-policies", { path: { staff_id: staffId } });
        // 服务端固定给四条；按 AUTO_POLICY_KINDS 的顺序展示，缺的（不该发生）用全零兜底。
        policyRows.value = AUTO_POLICY_KINDS.map((kind) => {
            const found = list.items.find((p) => p.kind === kind);
            const base: AgentAutoPolicyInput = found ?? {
                enabled: false,
                max_units: 0,
                min_discount_rate: 0,
                max_discount_cents: 0,
                daily_limit: 0,
            };
            return { kind, enabled: base.enabled, capInput: capInputText(kind, base), dailyLimit: base.daily_limit, saving: false, base };
        });
    } catch (err) {
        policyError.value = err;
    } finally {
        policyLoading.value = false;
    }
}

async function savePolicy(row: PolicyFormRow): Promise<void> {
    const target = settingsTarget.value;
    if (target === null) return;
    let capValue = 0;
    if (capFieldOf(row.kind) !== null) {
        const parsed = parseCap(row.kind, row.capInput);
        if (!parsed.ok) {
            ElMessage.error(parsed.msg);
            return;
        }
        capValue = parsed.value;
    }
    row.saving = true;
    try {
        const body = withCap(row.kind, { ...row.base, enabled: row.enabled, daily_limit: row.dailyLimit }, capValue);
        const saved = await keel.request("put", "/admin/agents/{staff_id}/auto-policies/{kind}", {
            path: { staff_id: target.id, kind: row.kind },
            body,
        });
        row.base = saved;
        row.enabled = saved.enabled;
        row.capInput = capInputText(row.kind, saved);
        row.dailyLimit = saved.daily_limit;
        notifyOk("已保存");
    } catch (err) {
        policyError.value = err;
    } finally {
        row.saving = false;
    }
}

// -------- 事件 webhook

const webhookLoading = ref(false);
const webhookError = ref<unknown>(null);
const webhookSaving = ref(false);
const webhook = ref<AgentWebhook | null>(null);
const webhookExists = ref(false);
const webhookUrl = ref("");
const webhookEnabled = ref(true);
/** 签名密钥明文，只在新建 / 轮换那一次响应里出现——同接入密钥的规矩，不缓存到别处。 */
const issuedWebhookSecret = ref<string | null>(null);
const webhookSecretCopied = ref(false);

async function loadWebhook(staffId: number): Promise<void> {
    webhookLoading.value = true;
    webhookError.value = null;
    issuedWebhookSecret.value = null;
    try {
        const w = await keel.get("/admin/agents/{staff_id}/webhook", { path: { staff_id: staffId } });
        webhook.value = w;
        webhookExists.value = true;
        webhookUrl.value = w.url;
        webhookEnabled.value = w.enabled;
    } catch (err) {
        // 404：这名 AI 员工还没配 webhook——不是错误，是空状态。
        if (err instanceof ProblemError && err.status === 404) {
            webhook.value = null;
            webhookExists.value = false;
            webhookUrl.value = "";
            webhookEnabled.value = true;
        } else {
            webhookError.value = err;
        }
    } finally {
        webhookLoading.value = false;
    }
}

async function saveWebhook(rotate: boolean): Promise<void> {
    const target = settingsTarget.value;
    if (target === null || webhookUrl.value.trim() === "") return;
    webhookSaving.value = true;
    webhookError.value = null;
    try {
        const saved = await keel.request("put", "/admin/agents/{staff_id}/webhook", {
            path: { staff_id: target.id },
            body: { url: webhookUrl.value.trim(), enabled: webhookEnabled.value, rotate_secret: rotate },
        });
        webhook.value = saved;
        webhookExists.value = true;
        issuedWebhookSecret.value = saved.secret ?? null;
        notifyOk("已保存");
    } catch (err) {
        webhookError.value = err;
    } finally {
        webhookSaving.value = false;
    }
}

async function deleteWebhook(): Promise<void> {
    const target = settingsTarget.value;
    if (target === null) return;
    try {
        await ElMessageBox.confirm("删除这个 webhook？删除后事件不再推送给它（AI 员工仍能用 list_events 自己拉）。", "确认删除", {
            type: "warning",
            confirmButtonText: "删除",
            cancelButtonText: "取消",
        });
    } catch {
        return;
    }
    try {
        await keel.request("delete", "/admin/agents/{staff_id}/webhook", { path: { staff_id: target.id } });
        webhook.value = null;
        webhookExists.value = false;
        webhookUrl.value = "";
        notifyOk("已删除");
    } catch (err) {
        webhookError.value = err;
    }
}

function copyWebhookSecret(): void {
    if (issuedWebhookSecret.value === null) return;
    navigator.clipboard.writeText(issuedWebhookSecret.value).then(
        () => {
            webhookSecretCopied.value = true;
            setTimeout(() => (webhookSecretCopied.value = false), 1500);
        },
        () => undefined,
    );
}

function openSettings(row: AdminAgent): void {
    settingsTarget.value = row;
    settingsTab.value = "policy";
    settingsVisible.value = true;
    void loadPolicies(row.id);
    void loadWebhook(row.id);
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

        <el-table v-if="!mobile" :data="agents" v-loading="loading" border stripe row-key="id" @expand-change="onExpandChange">
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
            <el-table-column label="操作" width="200" fixed="right">
                <template #default="{ row }: { row: AdminAgent }">
                    <el-button link type="primary" @click="openIssue(row)">发密钥</el-button>
                    <el-button link type="primary" @click="openEdit(row)">编辑</el-button>
                    <el-button link type="primary" @click="openSettings(row)">设置</el-button>
                </template>
            </el-table-column>
        </el-table>

        <!-- 手机：卡片列表；「查看密钥」展开显示该员工的密钥，操作按钮直接可点 -->
        <div v-else class="agent-cards" v-loading="loading">
            <el-card v-for="row in agents" :key="row.id" shadow="never" class="agent-card">
                <div class="card-head">
                    <span class="card-title">{{ row.name }}</span>
                    <el-tag :type="STAFF_STATUS[row.status].tag" size="small">{{ STAFF_STATUS[row.status].text }}</el-tag>
                </div>
                <div class="card-meta muted">
                    <span>{{ ROLE_TEXT[row.role] }}</span>
                    <span>{{ scopeText(row) }}</span>
                </div>
                <div class="card-meta muted">
                    <span>有效密钥 {{ row.live_keys }}</span>
                    <span>最近使用 {{ datetime(row.last_used_at) }}</span>
                </div>
                <div class="card-actions">
                    <el-button size="small" type="primary" @click="openIssue(row)">发密钥</el-button>
                    <el-button size="small" @click="openEdit(row)">编辑</el-button>
                    <el-button size="small" @click="openSettings(row)">设置</el-button>
                </div>
                <el-button link type="primary" class="expand-btn" @click="toggleAgentKeys(row)">
                    {{ expandedAgentIds.has(row.id) ? "收起密钥 ▲" : "查看密钥 ▼" }}
                </el-button>
                <div v-if="expandedAgentIds.has(row.id)" class="keys-list" v-loading="keysLoading[row.id] === true">
                    <div v-for="k in keysByAgent[row.id] ?? []" :key="k.id" class="key-item">
                        <div class="key-row">
                            <code>{{ k.prefix }}…</code>
                            <span class="key-name">{{ k.name }}</span>
                        </div>
                        <div class="key-meta muted">
                            <span>过期：{{ k.expires_at ? datetime(k.expires_at) : "不过期" }}</span>
                            <span>最近使用：{{ datetime(k.last_used_at) }}</span>
                        </div>
                        <div v-if="k.revoked_at" class="key-meta muted">已吊销：{{ datetime(k.revoked_at) }}</div>
                        <el-button v-if="!k.revoked_at" link type="danger" size="small" @click="revoke(row, k)">吊销</el-button>
                    </div>
                    <el-empty
                        v-if="(keysByAgent[row.id]?.length ?? 0) === 0 && keysLoading[row.id] !== true"
                        description="还没有密钥"
                        :image-size="40"
                    />
                </div>
            </el-card>
            <el-empty v-if="!loading && agents.length === 0" description="还没有 AI 员工" />
        </div>

        <el-empty v-if="!mobile && !loading && agents.length === 0" description="还没有 AI 员工" />

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

        <!-- 设置：自动执行策略（M11 §6）+ 事件 webhook（M10 §3） -->
        <el-dialog v-model="settingsVisible" :title="`AI 员工设置：${settingsTarget?.name ?? ''}`" width="680px" @closed="issuedWebhookSecret = null">
            <el-tabs v-model="settingsTab">
                <el-tab-pane label="自动执行策略" name="policy">
                    <ProblemAlert v-if="policyError" :error="policyError" />
                    <el-alert type="warning" :closable="false" show-icon class="mb12">
                        <template #title>开启后符合条件的提案会不经审批直接执行；售后审核不支持自动执行。</template>
                    </el-alert>
                    <el-table v-if="!mobile" :data="policyRows" v-loading="policyLoading" size="small" border row-key="kind">
                        <el-table-column label="种类" width="90">
                            <template #default="{ row }: { row: PolicyFormRow }">{{ AUTO_POLICY_KIND_LABEL[row.kind] }}</template>
                        </el-table-column>
                        <el-table-column label="启用" width="70">
                            <template #default="{ row }: { row: PolicyFormRow }">
                                <el-switch v-model="row.enabled" />
                            </template>
                        </el-table-column>
                        <el-table-column label="单笔上限" min-width="170">
                            <template #default="{ row }: { row: PolicyFormRow }">
                                <el-input v-if="capFieldOf(row.kind) !== null" v-model="row.capInput" size="small" style="width: 120px" />
                                <span v-else class="hint">没有单笔上限</span>
                            </template>
                        </el-table-column>
                        <el-table-column label="每 24 小时至多" width="160">
                            <template #default="{ row }: { row: PolicyFormRow }">
                                <el-input-number v-model="row.dailyLimit" :min="0" size="small" style="width: 100px" />
                                <span class="hint suffix">条，0 = 不自动</span>
                            </template>
                        </el-table-column>
                        <el-table-column label="操作" width="80">
                            <template #default="{ row }: { row: PolicyFormRow }">
                                <el-button link type="primary" size="small" :loading="row.saving" @click="savePolicy(row)">保存</el-button>
                            </template>
                        </el-table-column>
                    </el-table>

                    <!-- 手机：每种类一张卡片，字段竖排 -->
                    <div v-else class="policy-cards" v-loading="policyLoading">
                        <el-card v-for="row in policyRows" :key="row.kind" shadow="never" class="policy-card">
                            <div class="policy-head">
                                <span class="policy-title">{{ AUTO_POLICY_KIND_LABEL[row.kind] }}</span>
                                <el-switch v-model="row.enabled" />
                            </div>
                            <div v-if="capFieldOf(row.kind) !== null" class="policy-field">
                                <span class="hint">单笔上限</span>
                                <el-input v-model="row.capInput" size="small" />
                            </div>
                            <div class="policy-field">
                                <span class="hint">每 24 小时至多（0 = 不自动）</span>
                                <el-input-number v-model="row.dailyLimit" :min="0" size="small" style="width: 100%" />
                            </div>
                            <el-button type="primary" size="small" :loading="row.saving" class="policy-save" @click="savePolicy(row)">
                                保存
                            </el-button>
                        </el-card>
                    </div>
                </el-tab-pane>
                <el-tab-pane label="事件 Webhook" name="webhook">
                    <ProblemAlert v-if="webhookError" :error="webhookError" />
                    <p class="hint mb12">
                        事件写入后 Keel 会 POST 到这个地址（JSON 体 + HMAC 签名，5 秒超时，非 2xx 指数退避重试至多 6 次）；
                        AI 员工自己也能用 MCP 工具 list_events 拉。
                    </p>
                    <el-form label-width="70px" @submit.prevent v-loading="webhookLoading">
                        <el-form-item label="URL">
                            <el-input v-model="webhookUrl" placeholder="https://…（本机联调可用 http://127.0.0.1 / localhost）" />
                        </el-form-item>
                        <el-form-item label="启用">
                            <el-switch v-model="webhookEnabled" />
                        </el-form-item>
                    </el-form>
                    <template v-if="issuedWebhookSecret">
                        <el-alert type="warning" :closable="false" show-icon class="mb12">
                            <template #title>签名密钥只显示这一次，关掉就看不到了。请立刻复制。</template>
                        </el-alert>
                        <el-input :model-value="issuedWebhookSecret" readonly class="mb12">
                            <template #append>
                                <el-button @click="copyWebhookSecret">{{ webhookSecretCopied ? "已复制" : "复制" }}</el-button>
                            </template>
                        </el-input>
                    </template>
                    <div class="webhook-actions mb12">
                        <el-button type="primary" :loading="webhookSaving" :disabled="webhookUrl.trim() === ''" @click="saveWebhook(false)">
                            {{ webhookExists ? "保存" : "新建" }}
                        </el-button>
                        <el-button v-if="webhookExists" :loading="webhookSaving" @click="saveWebhook(true)">轮换签名密钥</el-button>
                        <el-button v-if="webhookExists" type="danger" @click="deleteWebhook">删除</el-button>
                    </div>
                    <template v-if="webhook?.recent_deliveries && webhook.recent_deliveries.length > 0">
                        <h4 class="section-title">最近投递（新的在前）</h4>
                        <el-table v-if="!mobile" :data="webhook.recent_deliveries" size="small" border>
                            <el-table-column label="第几次" width="70">
                                <template #default="{ row }: { row: AgentWebhookDelivery }">{{ row.attempt }}</template>
                            </el-table-column>
                            <el-table-column label="状态码" width="80">
                                <template #default="{ row }: { row: AgentWebhookDelivery }">{{ row.status_code ?? "—" }}</template>
                            </el-table-column>
                            <el-table-column label="失败原因" min-width="160" show-overflow-tooltip>
                                <template #default="{ row }: { row: AgentWebhookDelivery }">{{ row.error || "—" }}</template>
                            </el-table-column>
                            <el-table-column label="时间" width="170">
                                <template #default="{ row }: { row: AgentWebhookDelivery }">{{ datetime(row.delivered_at) }}</template>
                            </el-table-column>
                        </el-table>
                        <div v-else class="delivery-cards">
                            <el-card v-for="(row, i) in webhook.recent_deliveries" :key="i" shadow="never" class="delivery-card">
                                <div class="delivery-row">第 {{ row.attempt }} 次 · 状态码 {{ row.status_code ?? "—" }}</div>
                                <div v-if="row.error" class="delivery-row muted">{{ row.error }}</div>
                                <div class="delivery-row muted">{{ datetime(row.delivered_at) }}</div>
                            </el-card>
                        </div>
                    </template>
                </el-tab-pane>
            </el-tabs>
            <template #footer>
                <el-button @click="settingsVisible = false">关闭</el-button>
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
.section-title {
    margin: 8px 0;
    font-size: 13px;
    color: var(--el-text-color-secondary);
}
.webhook-actions .el-button {
    margin-right: 8px;
}

/* 手机：员工卡片列表 */
.agent-cards {
    display: flex;
    flex-direction: column;
    gap: 10px;
    min-height: 60px;
}
.agent-card :deep(.el-card__body) {
    padding: 12px 14px;
}
.card-head {
    display: flex;
    align-items: flex-start;
    justify-content: space-between;
    gap: 8px;
    font-size: 15px;
    font-weight: 600;
}
.card-title {
    flex: 1;
    word-break: break-word;
}
.card-meta {
    display: flex;
    flex-wrap: wrap;
    gap: 8px;
    margin-top: 6px;
    font-size: 13px;
}
.card-meta.muted {
    color: var(--el-text-color-secondary);
    font-size: 12px;
}
.card-actions {
    display: flex;
    gap: 8px;
    margin-top: 10px;
    flex-wrap: wrap;
}
.card-actions .el-button {
    flex: 1;
    min-width: 88px;
}
.expand-btn {
    margin-top: 8px;
    padding-left: 0;
}
.keys-list {
    margin-top: 6px;
    padding-top: 8px;
    border-top: 1px solid var(--el-border-color-lighter);
    min-height: 32px;
}
.key-item {
    padding: 6px 0;
    border-bottom: 1px dashed var(--el-border-color-lighter);
}
.key-item:last-child {
    border-bottom: none;
}
.key-row {
    display: flex;
    align-items: center;
    gap: 8px;
    font-size: 13px;
}
.key-name {
    word-break: break-word;
}
.key-meta {
    display: flex;
    flex-wrap: wrap;
    gap: 8px;
    font-size: 12px;
    margin-top: 2px;
}

/* 手机：自动执行策略卡片 */
.policy-cards {
    display: flex;
    flex-direction: column;
    gap: 10px;
}
.policy-card :deep(.el-card__body) {
    padding: 12px 14px;
}
.policy-head {
    display: flex;
    align-items: center;
    justify-content: space-between;
    font-weight: 600;
    margin-bottom: 8px;
}
.policy-field {
    display: flex;
    flex-direction: column;
    gap: 4px;
    margin-bottom: 8px;
}
.policy-field .el-input,
.policy-field .el-input-number {
    width: 100%;
}
.policy-save {
    width: 100%;
}

/* 手机：webhook 投递记录卡片 */
.delivery-cards {
    display: flex;
    flex-direction: column;
    gap: 8px;
}
.delivery-card :deep(.el-card__body) {
    padding: 10px 12px;
}
.delivery-row {
    font-size: 13px;
}
.delivery-row.muted {
    color: var(--el-text-color-secondary);
    font-size: 12px;
    margin-top: 2px;
    word-break: break-word;
}
</style>
