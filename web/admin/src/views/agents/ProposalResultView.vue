<script setup lang="ts">
// 提案的执行结果（AgentProposal.result）：加库存是可售库存前后；M10 各种类是 result.detail，
// 按不可信输入渲染（纯文本插值）；促销 / 券没有按 id 的详情路由，只链去对应的列表页。
import { computed } from "vue";
import { useRouter } from "vue-router";
import { describeResult, type AgentProposalKind } from "../../api/agentProposalRules.ts";

const props = defineProps<{ kind: AgentProposalKind; result?: Record<string, unknown> }>();
const router = useRouter();
const view = computed(() => describeResult(props.kind, props.result));

function gotoPromotions(): void {
    void router.push({ name: "promotions" });
}
function gotoCoupons(): void {
    void router.push({ name: "coupons" });
}
</script>

<template>
    <div v-if="view.failure" class="failure">
        <span>{{ view.failure.error }}</span>
        <span v-if="view.failure.errorType" class="muted">（{{ view.failure.errorType }}）</span>
    </div>
    <div v-else-if="view.beforeAfter">
        <p>可售库存：{{ view.beforeAfter.before }} → {{ view.beforeAfter.after }}</p>
    </div>
    <div v-else-if="view.lines.length > 0">
        <p v-for="(l, i) in view.lines" :key="i">{{ l.label }}：{{ l.value }}</p>
        <p class="links">
            <el-button v-if="view.hasPromotionLink" link type="primary" size="small" @click="gotoPromotions">查看营销活动列表 →</el-button>
            <el-button v-if="view.hasCouponLink" link type="primary" size="small" @click="gotoCoupons">查看优惠券列表 →</el-button>
        </p>
    </div>
    <span v-else class="muted">—</span>
</template>

<style scoped>
.failure {
    color: var(--el-color-danger);
}
.muted {
    color: var(--el-text-color-secondary);
    font-size: 12px;
}
p {
    margin: 4px 0;
}
.links {
    margin-top: 2px;
}
</style>
