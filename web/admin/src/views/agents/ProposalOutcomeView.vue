<script setup lang="ts">
// 提案执行后的复盘（AgentProposal.outcome，AI 经营 M10 §4，00122）：verdict 标签 + 一句解释 +
// 关键指标；执行了但还没到复盘时间点（没有 outcome）时说清楚「还在等」，不是空白。
import { computed } from "vue";
import { describeOutcome, VERDICT, type AgentProposalKind } from "../../api/agentProposalRules.ts";
import { datetime } from "../../ui/format.ts";

const props = defineProps<{ kind: AgentProposalKind; outcome?: Record<string, unknown>; executedAt?: string; outcomeAt?: string }>();
const view = computed(() => describeOutcome(props.kind, props.outcome));
</script>

<template>
    <div v-if="view.verdict">
        <el-tag :type="VERDICT[view.verdict].tag" size="small">{{ VERDICT[view.verdict].text }}</el-tag>
        <span v-if="outcomeAt" class="muted"> · {{ datetime(outcomeAt) }}</span>
        <p v-if="view.explanation" class="pre">{{ view.explanation }}</p>
        <p v-for="m in view.metrics" :key="m.label" class="metric">{{ m.label }}：{{ m.value }}</p>
        <table v-if="view.cells.length > 0" class="cells">
            <thead>
                <tr>
                    <th>商品</th>
                    <th>方向</th>
                    <th>挂零小时（分配造成）</th>
                    <th>缺货拒单</th>
                    <th>卖出</th>
                    <th>渠道净收入</th>
                    <th>备注</th>
                </tr>
            </thead>
            <tbody>
                <tr v-for="(c, i) in view.cells" :key="i">
                    <td>{{ c.skuText }}</td>
                    <td>{{ c.directionText }}</td>
                    <td>{{ c.heldZeroText }}</td>
                    <td>{{ c.rejectsText }}</td>
                    <td>{{ c.soldText }}</td>
                    <td>{{ c.netText }}</td>
                    <td class="muted">{{ c.excludedReason ?? "—" }}</td>
                </tr>
            </tbody>
        </table>
    </div>
    <p v-else-if="executedAt" class="muted">待复盘（到期后自动计算）</p>
    <span v-else class="muted">—</span>
</template>

<style scoped>
.muted {
    color: var(--el-text-color-secondary);
    font-size: 12px;
}
.metric {
    margin: 2px 0;
    font-size: 12px;
}
.pre {
    white-space: pre-wrap;
    word-break: break-word;
    margin: 4px 0;
    font-size: 13px;
}
.cells {
    margin-top: 6px;
    border-collapse: collapse;
    font-size: 12px;
}
.cells th,
.cells td {
    padding: 2px 8px 2px 0;
    text-align: left;
    white-space: nowrap;
}
.cells th {
    color: var(--el-text-color-secondary);
    font-weight: normal;
}
</style>
