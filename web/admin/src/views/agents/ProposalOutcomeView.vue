<script setup lang="ts">
// 提案执行后的复盘（AgentProposal.outcome，AI 经营 M10 §4，00122）：verdict 标签 + 一句解释 +
// 关键指标；执行了但还没到复盘时间点（没有 outcome）时说清楚「还在等」，不是空白。
import { computed } from "vue";
import { describeOutcome, VERDICT } from "../../api/agentProposalRules.ts";
import { datetime } from "../../ui/format.ts";

const props = defineProps<{ outcome?: Record<string, unknown>; executedAt?: string; outcomeAt?: string }>();
const view = computed(() => describeOutcome(props.outcome));
</script>

<template>
    <div v-if="view.verdict">
        <el-tag :type="VERDICT[view.verdict].tag" size="small">{{ VERDICT[view.verdict].text }}</el-tag>
        <span v-if="outcomeAt" class="muted"> · {{ datetime(outcomeAt) }}</span>
        <p v-if="view.explanation" class="pre">{{ view.explanation }}</p>
        <p v-for="m in view.metrics" :key="m.label" class="metric">{{ m.label }}：{{ m.value }}</p>
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
</style>
