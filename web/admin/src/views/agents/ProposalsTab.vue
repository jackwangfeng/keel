<script setup lang="ts">
// AI 员工的提案：证据 + 动作 + 预计影响，人批准后 Keel 以 AI 员工的身份执行。
//
// evidence / expected_impact / payload 都是 AI 员工写的或算的，**按不可信输入渲染**——
// 只用文本插值 + white-space: pre-wrap，全文件不出现 v-html。
import { ref, onMounted } from "vue";
import { ElMessageBox, ElNotification } from "element-plus";
import { Refresh } from "@element-plus/icons-vue";
import { keel, type AgentProposal, type AgentProposalPage } from "../../api/client.ts";
import { datetime, PROPOSAL_STATUS } from "../../ui/format.ts";
import { notifyError, notifyOk } from "../../ui/notify.ts";
import { KIND_LABEL, type AgentProposalKind } from "../../api/agentProposalRules.ts";
import ProblemAlert from "../../components/ProblemAlert.vue";
import ProposalPayloadView from "./ProposalPayloadView.vue";
import ProposalResultView from "./ProposalResultView.vue";
import ProposalOutcomeView from "./ProposalOutcomeView.vue";

const loading = ref(false);
const error = ref<unknown>(null);
const page = ref<AgentProposalPage | null>(null);
const pageNo = ref(1);
const pageSize = ref(20);
/** 默认只看待处理——这是需要人做决定的那一批。 */
const statusFilter = ref<10 | 15 | 20 | 30 | 40 | 50 | "">(10);
/** 种类筛选（AI 经营 M10 §1，五种）：不选即全部。 */
const kindFilter = ref<AgentProposalKind | "">("");

const STATUS_OPTIONS: { value: 10 | 15 | 20 | 30 | 40 | 50 | ""; label: string }[] = [
    { value: "", label: "全部状态" },
    { value: 10, label: "待处理" },
    { value: 15, label: "执行中" },
    { value: 20, label: "已执行" },
    { value: 30, label: "已驳回" },
    { value: 40, label: "执行失败" },
    { value: 50, label: "已过期" },
];

const KIND_OPTIONS: { value: AgentProposalKind | ""; label: string }[] = [
    { value: "", label: "全部种类" },
    ...(Object.entries(KIND_LABEL) as [AgentProposalKind, string][]).map(([value, label]) => ({ value, label })),
];

async function load(): Promise<void> {
    loading.value = true;
    error.value = null;
    try {
        page.value = await keel.get("/admin/agent-proposals", {
            query: {
                page: pageNo.value,
                page_size: pageSize.value,
                ...(statusFilter.value === "" ? {} : { status: statusFilter.value }),
                ...(kindFilter.value === "" ? {} : { kind: kindFilter.value }),
            },
        });
    } catch (err) {
        error.value = err;
    } finally {
        loading.value = false;
    }
}

function onFilterChange(): void {
    pageNo.value = 1;
    void load();
}

onMounted(() => void load());
defineExpose({ reload: load });

// ------------------------------------------------------------------ 批准

const approvingId = ref<number | null>(null);

async function approve(row: AgentProposal): Promise<void> {
    try {
        await ElMessageBox.confirm(
            `批准这条提案？批准后 Keel 会立即以 AI 员工「${row.agent_name}」的身份执行这个动作，不能撤回。`,
            "确认批准",
            { type: "warning", confirmButtonText: "批准", cancelButtonText: "取消" },
        );
    } catch {
        return;
    }
    approvingId.value = row.id;
    try {
        const updated = await keel.request("post", "/admin/agent-proposals/{proposal_id}/approve", {
            path: { proposal_id: row.id },
        });
        if (updated.status === 20) {
            const r = updated.result as { before_available?: number; after_available?: number } | undefined;
            notifyOk(
                r?.before_available !== undefined && r?.after_available !== undefined
                    ? `已执行：可售库存 ${r.before_available} → ${r.after_available}`
                    : "已执行",
            );
        } else if (updated.status === 40) {
            const r = updated.result as { error?: string; error_type?: string } | undefined;
            // 这不是一次请求失败（HTTP 是 200），是「批准了，但执行那一步没成」——
            // 走 notifyError/describeError 会说成「请求没发出去」，牛头不对马嘴，
            // 所以这里直接摊开服务端给的 result，不经过那条为异常准备的路径。
            ElNotification({
                title: "已批准，但执行失败",
                message: r?.error ?? "服务端没有说明原因",
                type: "error",
                duration: 0,
            });
        } else {
            notifyOk("已提交");
        }
        await load();
    } catch (err) {
        notifyError(err);
        await load();
    } finally {
        approvingId.value = null;
    }
}

// ------------------------------------------------------------------ 驳回

const rejectVisible = ref(false);
const rejectError = ref<unknown>(null);
const rejecting = ref(false);
const rejectTarget = ref<AgentProposal | null>(null);
const rejectReason = ref("");

function openReject(row: AgentProposal): void {
    rejectTarget.value = row;
    rejectReason.value = "";
    rejectError.value = null;
    rejectVisible.value = true;
}

async function submitReject(): Promise<void> {
    const target = rejectTarget.value;
    if (target === null || rejectReason.value.trim() === "") return;
    rejecting.value = true;
    rejectError.value = null;
    try {
        await keel.request("post", "/admin/agent-proposals/{proposal_id}/reject", {
            path: { proposal_id: target.id },
            body: { reason: rejectReason.value.trim() },
        });
        rejectVisible.value = false;
        notifyOk("已驳回");
        await load();
    } catch (err) {
        rejectError.value = err;
    } finally {
        rejecting.value = false;
    }
}
</script>

<template>
    <div>
        <ProblemAlert v-if="error" :error="error" />

        <div class="page-toolbar">
            <el-select v-model="kindFilter" style="width: 140px" @change="onFilterChange">
                <el-option v-for="o in KIND_OPTIONS" :key="o.value" :label="o.label" :value="o.value" />
            </el-select>
            <el-select v-model="statusFilter" style="width: 140px" @change="onFilterChange">
                <el-option v-for="o in STATUS_OPTIONS" :key="o.value" :label="o.label" :value="o.value" />
            </el-select>
            <span class="grow" />
            <el-button :icon="Refresh" :loading="loading" @click="load">刷新</el-button>
        </div>

        <el-table :data="page?.items ?? []" v-loading="loading" border stripe row-key="id">
            <el-table-column type="expand">
                <template #default="{ row }: { row: AgentProposal }">
                    <div class="detail">
                        <h4>证据</h4>
                        <p class="pre">{{ row.evidence || "—" }}</p>
                        <h4>预计影响</h4>
                        <p class="pre">{{ row.expected_impact || "—" }}</p>
                        <h4>执行参数</h4>
                        <ProposalPayloadView :kind="row.kind" :payload="row.payload" />
                        <template v-if="row.result">
                            <h4>执行结果</h4>
                            <ProposalResultView :kind="row.kind" :result="row.result" />
                        </template>
                        <template v-if="row.executed_at">
                            <h4>执行后复盘</h4>
                            <ProposalOutcomeView :outcome="row.outcome" :executed-at="row.executed_at" :outcome-at="row.outcome_at" />
                        </template>
                        <template v-if="row.status === 30">
                            <h4>驳回理由</h4>
                            <p class="pre">{{ row.reject_reason || "—" }}</p>
                            <p class="muted">{{ row.decided_by_name ?? "" }} · {{ datetime(row.decided_at) }}</p>
                        </template>
                    </div>
                </template>
            </el-table-column>
            <el-table-column label="标题" min-width="220" show-overflow-tooltip>
                <template #default="{ row }: { row: AgentProposal }">{{ row.title }}</template>
            </el-table-column>
            <el-table-column label="种类" width="90">
                <template #default="{ row }: { row: AgentProposal }">{{ KIND_LABEL[row.kind] }}</template>
            </el-table-column>
            <el-table-column label="门店" width="120" show-overflow-tooltip>
                <template #default="{ row }: { row: AgentProposal }">{{ row.store_name ?? "全店" }}</template>
            </el-table-column>
            <el-table-column label="AI 员工" width="120" show-overflow-tooltip>
                <template #default="{ row }: { row: AgentProposal }">{{ row.agent_name }}</template>
            </el-table-column>
            <el-table-column label="状态" width="150">
                <template #default="{ row }: { row: AgentProposal }">
                    <el-tag :type="PROPOSAL_STATUS[row.status].tag" size="small">
                        {{ PROPOSAL_STATUS[row.status].text }}
                    </el-tag>
                    <el-tag v-if="row.auto_approved" type="warning" size="small" class="auto-tag">自动执行</el-tag>
                </template>
            </el-table-column>
            <el-table-column label="创建时间" width="170">
                <template #default="{ row }: { row: AgentProposal }">{{ datetime(row.created_at) }}</template>
            </el-table-column>
            <el-table-column label="过期时间" width="170">
                <template #default="{ row }: { row: AgentProposal }">{{ datetime(row.expires_at) }}</template>
            </el-table-column>
            <el-table-column label="操作" width="160" fixed="right">
                <template #default="{ row }: { row: AgentProposal }">
                    <template v-if="row.status === 10">
                        <el-button link type="primary" :loading="approvingId === row.id" @click="approve(row)">
                            批准
                        </el-button>
                        <el-button link type="danger" @click="openReject(row)">驳回</el-button>
                    </template>
                    <span v-else class="muted">—</span>
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

        <el-dialog v-model="rejectVisible" title="驳回提案" width="480px">
            <ProblemAlert v-if="rejectError" :error="rejectError" />
            <p class="hint">{{ rejectTarget?.title }}</p>
            <el-form label-width="70px" @submit.prevent>
                <el-form-item label="理由" required>
                    <el-input v-model="rejectReason" type="textarea" :rows="3" placeholder="这条会记在提案上，AI 员工与后续的人都看得到" />
                </el-form-item>
            </el-form>
            <template #footer>
                <el-button @click="rejectVisible = false">取消</el-button>
                <el-button type="danger" :loading="rejecting" :disabled="rejectReason.trim() === ''" @click="submitReject">
                    驳回
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
.detail {
    padding: 4px 16px 12px;
}
.detail h4 {
    margin: 8px 0 4px;
    font-size: 12px;
    color: var(--el-text-color-secondary);
}
.pre {
    white-space: pre-wrap;
    word-break: break-word;
    margin: 0;
    font-size: 13px;
    font-family: inherit;
}
.muted {
    color: var(--el-text-color-secondary);
}
.auto-tag {
    margin-left: 4px;
}
</style>
