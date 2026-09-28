<script setup lang="ts">
// 同城配送规则表单：起送价 + 满额免配送费 + 距离分档。
//
// 模板管理（新建 / 编辑模板）与门店「同城配送」页的「自定义」都用这一份 —— 两处
// 除了「保存到哪个接口」之外，字段、换算（元 ↔ 分、公里 ↔ 米）、校验规矩完全相同，
// 曾经各写一遍（StoreDetailView 与本组件抽出前），改一条校验规则要改两处。
//
// 用法：`initial` 给服务端形状的配置（或 null = 空白草稿），本组件自己管草稿状态；
// 调用方拿到组件实例后调 `submit()`——合法就返回服务端形状（api/localDeliveryRules.ts
// 的 `RuleConfig`），不合法就在组件自己的错误条上显示，返回 null。

import { ref, watch } from "vue";
import { Delete, Plus } from "@element-plus/icons-vue";
import {
    emptyRuleDraft,
    kmToMeters,
    metersToKmInput,
    parseRuleDraft,
    ruleDraftOf,
    sortTierDrafts,
    type RuleConfig,
    type RuleDraft,
} from "../api/localDeliveryRules.ts";

const props = defineProps<{ initial: RuleConfig | null; disabled?: boolean }>();

const draft = ref<RuleDraft>(props.initial === null ? emptyRuleDraft() : ruleDraftOf(props.initial));
const localError = ref("");

watch(
    () => props.initial,
    (v) => {
        draft.value = v === null ? emptyRuleDraft() : ruleDraftOf(v);
        localError.value = "";
    },
);

function addTier(): void {
    const last = draft.value.tiers[draft.value.tiers.length - 1];
    const lastM = last === undefined ? null : kmToMeters(last.withinKm);
    const nextM = Math.min((lastM ?? 0) + 1000, 100000);
    draft.value.tiers.push({ withinKm: metersToKmInput(nextM), feeYuan: last?.feeYuan ?? "0" });
}

function removeTier(i: number): void {
    draft.value.tiers.splice(i, 1);
}

/** 校验 + 转换成服务端形状；不合法时把错误显示在自己的错误条上并返回 null。 */
function submit(): RuleConfig | null {
    localError.value = "";
    sortTierDrafts(draft.value.tiers);
    const r = parseRuleDraft(draft.value);
    if (!r.ok) {
        localError.value = r.msg;
        return null;
    }
    return r.config;
}

defineExpose({ submit });
</script>

<template>
    <div class="ld-rule-form">
        <el-alert v-if="localError" :title="localError" type="error" :closable="false" show-icon class="mb8" />
        <el-form-item label="起送价（元）">
            <el-input v-model="draft.minOrder" style="width: 160px" placeholder="0 = 不设" :disabled="disabled" />
            <span class="hint ml8">活动之后、用券之前的商品金额没到它，下单会被拒</span>
        </el-form-item>
        <el-form-item label="满多少免配送费">
            <el-input v-model="draft.freeOver" style="width: 160px" placeholder="0 = 不设" :disabled="disabled">
                <template #append>元</template>
            </el-input>
        </el-form-item>
        <el-form-item label="距离分档">
            <div class="ld-tiers">
                <div v-for="(t, i) in draft.tiers" :key="i" class="ld-tier-row">
                    <span>距离 ≤</span>
                    <el-input v-model="t.withinKm" size="small" class="ld-num" :disabled="disabled" @change="sortTierDrafts(draft.tiers)">
                        <template #append>公里</template>
                    </el-input>
                    <span>配送费</span>
                    <el-input v-model="t.feeYuan" size="small" class="ld-num" :disabled="disabled">
                        <template #append>元</template>
                    </el-input>
                    <el-button link type="danger" :icon="Delete" :disabled="disabled" @click="removeTier(i)">删除</el-button>
                </div>
                <el-button :icon="Plus" size="small" :disabled="disabled" @click="addTier">加一档</el-button>
                <p class="hint">超出最后一档或买家地址没有坐标时按最后一档收；一档都不配 = 配送费恒为 0。</p>
            </div>
        </el-form-item>
    </div>
</template>

<style scoped>
.mb8 {
    margin-bottom: 8px;
}
.ml8 {
    margin-left: 8px;
}
.hint {
    color: var(--el-text-color-secondary);
    font-size: 12px;
    margin: 4px 0 0;
}
.ld-tiers {
    width: 100%;
}
.ld-tier-row {
    display: flex;
    align-items: center;
    gap: 8px;
    margin-bottom: 8px;
}
.ld-num {
    width: 140px;
}
</style>
