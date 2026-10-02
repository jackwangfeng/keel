<script setup lang="ts">
// 提案 payload（AI 员工写的执行参数）按 kind 翻译成人话（AI 经营 M10 §1）。
//
// payload **按不可信输入渲染**：全组件只有文本插值，不出现 v-html——与 ProposalsTab.vue
// 文件头那条约定一致，纯文本框里的字符串不会被当成标签解析。
import { computed } from "vue";
import {
    channelStockRulePayload,
    couponPayload,
    flashPricePayload,
    inventoryAdjustPayload,
    productCopyPayload,
    refundDecisionPayload,
    type AgentProposalKind,
} from "../../api/agentProposalRules.ts";
import { datetime } from "../../ui/format.ts";

const props = defineProps<{ kind: AgentProposalKind; payload: Record<string, unknown> }>();

const inventory = computed(() => (props.kind === "inventory_adjust" ? inventoryAdjustPayload(props.payload) : null));
const flash = computed(() => (props.kind === "flash_price" ? flashPricePayload(props.payload) : null));
const coupon = computed(() => (props.kind === "coupon" ? couponPayload(props.payload) : null));
const copy = computed(() => (props.kind === "product_copy" ? productCopyPayload(props.payload) : null));
const refund = computed(() => (props.kind === "refund_decision" ? refundDecisionPayload(props.payload) : null));
const channelRule = computed(() => (props.kind === "channel_stock_rule" ? channelStockRulePayload(props.payload) : null));
</script>

<template>
    <div>
        <template v-if="inventory">
            <p>{{ inventory.storeText }} · {{ inventory.skuText }}：{{ inventory.deltaText }}</p>
            <p v-if="inventory.reason" class="muted">理由：{{ inventory.reason }}</p>
        </template>
        <template v-else-if="flash">
            <p>「{{ flash.name }}」· {{ flash.storeText }} · {{ datetime(flash.startsAt) }} ~ {{ datetime(flash.endsAt) }}</p>
            <ul class="lines">
                <li v-for="it in flash.items" :key="it.skuId">SKU #{{ it.skuId }}：{{ it.discountText }}</li>
            </ul>
        </template>
        <template v-else-if="coupon">
            <p>「{{ coupon.name }}」· {{ coupon.typeText }} · {{ coupon.thresholdText }} · {{ coupon.faceText }}</p>
            <p class="muted">{{ coupon.validDaysText }} · 共 {{ coupon.totalText }} · {{ coupon.perUserLimitText }} · {{ coupon.claimableText }}</p>
        </template>
        <template v-else-if="copy">
            <p v-if="copy.titleChanged">标题：{{ copy.beforeTitle }} → {{ copy.afterTitle }}</p>
            <p v-if="copy.subtitleChanged">副标题：{{ copy.beforeSubtitle || "（空）" }} → {{ copy.afterSubtitle }}</p>
        </template>
        <template v-else-if="refund">
            <p>售后 {{ refund.refundNo }}：建议{{ refund.actionText }}（{{ refund.amountText }}）</p>
            <p v-if="refund.reason" class="muted">理由：{{ refund.reason }}</p>
        </template>
        <template v-else-if="channelRule">
            <p>{{ channelRule.bindingText }} · {{ channelRule.storeText }}</p>
            <table class="cells">
                <thead>
                    <tr>
                        <th>商品 / 级别</th>
                        <th>旧规则 → 新规则</th>
                    </tr>
                </thead>
                <tbody>
                    <tr v-for="(c, i) in channelRule.changes" :key="i">
                        <td>{{ c.cellText }}</td>
                        <td>{{ c.prevText }} → {{ c.nextText }}</td>
                    </tr>
                </tbody>
            </table>
            <template v-if="channelRule.preview.length > 0">
                <p class="muted">试算（提案时按当时 keel 可售算的对外可售数，批准时可能已经变了）：</p>
                <table class="cells">
                    <thead>
                        <tr>
                            <th>商品</th>
                            <th>keel 可售</th>
                            <th>对外可售：前 → 后</th>
                        </tr>
                    </thead>
                    <tbody>
                        <tr v-for="(pv, i) in channelRule.preview" :key="i">
                            <td>{{ pv.skuText }}</td>
                            <td>{{ pv.availableText }}</td>
                            <td>{{ pv.beforeText }} → {{ pv.afterText }}</td>
                        </tr>
                    </tbody>
                </table>
            </template>
        </template>
    </div>
</template>

<style scoped>
.lines {
    margin: 4px 0;
    padding-left: 18px;
}
.muted {
    color: var(--el-text-color-secondary);
    font-size: 12px;
}
p {
    margin: 4px 0;
}
.cells {
    margin: 4px 0;
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
