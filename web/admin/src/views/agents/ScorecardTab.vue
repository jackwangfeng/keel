<script setup lang="ts">
// AI 员工成绩单（AI 经营 M10 §4，00122）：选一名 AI 员工，看它近 30 天按种类的提案数、
// 批准率、执行成功 / 失败、驳回、过期、待处理，以及执行后复盘的 verdict 分布——
// 店长据此决定要不要放手（M11 自动执行，AgentsTab 的「自动执行策略」）。
//
// 批准率 / 正面率是**客户端算的**（服务端只给分子分母的原始计数），除数为 0 时给
// 「—」而不是 0% —— 一条都没提过的种类，0% 会被误读成「批了但都没成」。
import { ref, onMounted } from "vue";
import { Refresh } from "@element-plus/icons-vue";
import { keel, type AdminAgent, type AgentScorecard, type AgentScorecardEntry, type AgentScorecardKind } from "../../api/client.ts";
import { KIND_LABEL, VERDICT, describeOutcome, safeRatePercent, type AgentProposalKind } from "../../api/agentProposalRules.ts";
import { datetime } from "../../ui/format.ts";
import { useMobile } from "../../ui/useMobile.ts";
import ProblemAlert from "../../components/ProblemAlert.vue";

const mobile = useMobile();

const loading = ref(false);
const error = ref<unknown>(null);
const agents = ref<AdminAgent[]>([]);
const staffId = ref<number | null>(null);
const card = ref<AgentScorecard | null>(null);

function kindLabel(kind: string): string {
    return KIND_LABEL[kind as AgentProposalKind] ?? kind;
}

async function loadAgents(): Promise<void> {
    try {
        agents.value = (await keel.get("/admin/agents", {})).items;
        if (staffId.value === null && agents.value.length > 0) staffId.value = agents.value[0]!.id;
    } catch (err) {
        error.value = err;
    }
}

async function loadCard(): Promise<void> {
    if (staffId.value === null) {
        card.value = null;
        return;
    }
    loading.value = true;
    error.value = null;
    try {
        card.value = await keel.get("/admin/agents/{staff_id}/scorecard", { path: { staff_id: staffId.value } });
    } catch (err) {
        error.value = err;
        card.value = null;
    } finally {
        loading.value = false;
    }
}

onMounted(async () => {
    await loadAgents();
    await loadCard();
});
defineExpose({ reload: loadCard });

function onStaffChange(): void {
    void loadCard();
}

function verdictOf(entry: AgentScorecardEntry): ReturnType<typeof describeOutcome> {
    return describeOutcome(entry.kind as AgentProposalKind, entry.outcome as Record<string, unknown>);
}

function positiveRate(k: AgentScorecardKind): string {
    return safeRatePercent(k.positive, k.positive + k.neutral + k.negative);
}
function approveRate(k: AgentScorecardKind): string {
    return safeRatePercent(k.approved, k.proposed);
}

/** 手机卡片：宽表的每一列在这里变成一行「标签：值」，两列键值排布（见 styles.css 的 .kv-grid）。 */
function kindStats(k: AgentScorecardKind): { label: string; value: string | number }[] {
    return [
        { label: "提案", value: k.proposed },
        { label: "批准", value: k.approved },
        { label: "批准率", value: approveRate(k) },
        { label: "执行成功", value: k.executed },
        { label: "失败", value: k.failed },
        { label: "驳回", value: k.rejected },
        { label: "过期", value: k.expired },
        { label: "待处理", value: k.open },
        { label: "正面", value: k.positive },
        { label: "中性", value: k.neutral },
        { label: "负面", value: k.negative },
        { label: "正面率", value: positiveRate(k) },
    ];
}
</script>

<template>
    <div>
        <ProblemAlert v-if="error" :error="error" />

        <div class="page-toolbar">
            <el-select v-model="staffId" placeholder="选一名 AI 员工" style="width: 220px" @change="onStaffChange">
                <el-option v-for="a in agents" :key="a.id" :label="a.name" :value="a.id" />
            </el-select>
            <span class="grow" />
            <el-button :icon="Refresh" :loading="loading" @click="loadCard">刷新</el-button>
        </div>

        <el-empty v-if="!loading && agents.length === 0" description="还没有 AI 员工" />

        <template v-if="card">
            <p class="hint">统计区间：{{ datetime(card.since) }} 至今（近 30 天）</p>

            <el-table v-if="!mobile" :data="card.kinds" v-loading="loading" border stripe size="small">
                <el-table-column label="种类" width="100">
                    <template #default="{ row }: { row: AgentScorecardKind }">{{ kindLabel(row.kind) }}</template>
                </el-table-column>
                <el-table-column prop="proposed" label="提案" width="70" align="right" />
                <el-table-column prop="approved" label="批准" width="70" align="right" />
                <el-table-column label="批准率" width="80" align="right">
                    <template #default="{ row }: { row: AgentScorecardKind }">{{ approveRate(row) }}</template>
                </el-table-column>
                <el-table-column prop="executed" label="执行成功" width="80" align="right" />
                <el-table-column prop="failed" label="失败" width="70" align="right" />
                <el-table-column prop="rejected" label="驳回" width="70" align="right" />
                <el-table-column prop="expired" label="过期" width="70" align="right" />
                <el-table-column prop="open" label="待处理" width="80" align="right" />
                <el-table-column prop="positive" label="正面" width="70" align="right" />
                <el-table-column prop="neutral" label="中性" width="70" align="right" />
                <el-table-column prop="negative" label="负面" width="70" align="right" />
                <el-table-column label="正面率" width="80" align="right">
                    <template #default="{ row }: { row: AgentScorecardKind }">{{ positiveRate(row) }}</template>
                </el-table-column>
            </el-table>

            <!-- 手机：每个种类一张卡片，统计两列键值排 -->
            <div v-else class="kind-cards" v-loading="loading">
                <el-card v-for="row in card.kinds" :key="row.kind" shadow="never" class="kind-card">
                    <div class="kind-title">{{ kindLabel(row.kind) }}</div>
                    <div class="kv-grid">
                        <div v-for="s in kindStats(row)" :key="s.label" class="kv-item">
                            <span class="kv-label">{{ s.label }}</span>
                            <span class="kv-value">{{ s.value }}</span>
                        </div>
                    </div>
                </el-card>
            </div>

            <h4 class="section-title">最近已复盘</h4>
            <el-table v-if="!mobile" :data="card.recent" border stripe size="small">
                <el-table-column label="标题" min-width="220" show-overflow-tooltip>
                    <template #default="{ row }: { row: AgentScorecardEntry }">{{ row.title }}</template>
                </el-table-column>
                <el-table-column label="种类" width="90">
                    <template #default="{ row }: { row: AgentScorecardEntry }">{{ kindLabel(row.kind) }}</template>
                </el-table-column>
                <el-table-column label="结论" width="90">
                    <template #default="{ row }: { row: AgentScorecardEntry }">
                        <el-tag v-if="verdictOf(row).verdict" :type="VERDICT[verdictOf(row).verdict!].tag" size="small">
                            {{ VERDICT[verdictOf(row).verdict!].text }}
                        </el-tag>
                        <span v-else class="muted">—</span>
                    </template>
                </el-table-column>
                <el-table-column label="说明" min-width="240" show-overflow-tooltip>
                    <template #default="{ row }: { row: AgentScorecardEntry }">{{ verdictOf(row).explanation ?? "—" }}</template>
                </el-table-column>
                <el-table-column label="复盘时间" width="170">
                    <template #default="{ row }: { row: AgentScorecardEntry }">{{ datetime(row.outcome_at) }}</template>
                </el-table-column>
            </el-table>

            <!-- 手机：最近复盘也是卡片 -->
            <div v-else class="recent-cards">
                <el-card v-for="(row, i) in card.recent" :key="i" shadow="never" class="recent-card">
                    <div class="card-head">
                        <span class="card-title">{{ row.title }}</span>
                        <el-tag v-if="verdictOf(row).verdict" :type="VERDICT[verdictOf(row).verdict!].tag" size="small">
                            {{ VERDICT[verdictOf(row).verdict!].text }}
                        </el-tag>
                        <span v-else class="muted">—</span>
                    </div>
                    <div class="card-meta muted">{{ kindLabel(row.kind) }} · {{ datetime(row.outcome_at) }}</div>
                    <p v-if="verdictOf(row).explanation" class="explanation">{{ verdictOf(row).explanation }}</p>
                </el-card>
            </div>
            <el-empty v-if="card.recent.length === 0" description="还没有已复盘的提案" :image-size="50" />
        </template>
    </div>
</template>

<style scoped>
.section-title {
    margin: 16px 0 8px;
    font-size: 13px;
    color: var(--el-text-color-secondary);
}
.muted {
    color: var(--el-text-color-secondary);
}

/* 手机：种类统计卡片 */
.kind-cards {
    display: flex;
    flex-direction: column;
    gap: 10px;
    min-height: 60px;
}
.kind-card :deep(.el-card__body) {
    padding: 12px 14px;
}
.kind-title {
    font-size: 15px;
    font-weight: 600;
    margin-bottom: 8px;
}
.kv-grid {
    display: grid;
    grid-template-columns: 1fr 1fr;
    gap: 6px 12px;
}
.kv-item {
    display: flex;
    justify-content: space-between;
    font-size: 13px;
    padding: 2px 0;
    border-bottom: 1px dashed var(--el-border-color-lighter);
}
.kv-label {
    color: var(--el-text-color-secondary);
}
.kv-value {
    font-variant-numeric: tabular-nums;
}

/* 手机：最近复盘卡片 */
.recent-cards {
    display: flex;
    flex-direction: column;
    gap: 10px;
}
.recent-card :deep(.el-card__body) {
    padding: 12px 14px;
}
.card-head {
    display: flex;
    align-items: flex-start;
    justify-content: space-between;
    gap: 8px;
}
.card-title {
    flex: 1;
    font-weight: 600;
    word-break: break-word;
}
.card-meta {
    margin-top: 4px;
    font-size: 12px;
}
.explanation {
    margin: 6px 0 0;
    font-size: 13px;
    white-space: pre-wrap;
    word-break: break-word;
}
</style>
