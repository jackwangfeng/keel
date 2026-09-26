<script setup lang="ts">
// 切到别家店之后，每一页顶上一条醒目的横幅：平台管理员必须一眼看出自己在改哪家。
// 顶栏那个按钮会被人习惯性忽略；一条贯穿每一页、颜色不一样的横幅不会。
import { computed } from "vue";
import { currentSession } from "../api/client.ts";
import { currentMerchantScope, setMerchantScope } from "../api/merchantScope.ts";

const scope = computed(() => currentMerchantScope(currentSession()));

function back(): void {
    setMerchantScope(currentSession(), null);
    globalThis.location.reload();
}
</script>

<template>
    <div v-if="scope" class="scope-banner" :class="{ disabled: scope.disabled }">
        你正在管理 <b>{{ scope.name }}</b>（{{ scope.code }}）<template v-if="scope.disabled">——这家店已停用，买家打不开它</template>。
        这一页的读写全部落在这家店上（请求带着 X-Keel-Merchant: {{ scope.code }}）。
        <el-button link size="small" class="back" @click="back">切回按当前域名</el-button>
    </div>
</template>

<style scoped>
.scope-banner {
    margin: -20px -20px 16px;
    padding: 8px 20px;
    background: #fdf6ec;
    border-bottom: 2px solid #e6a23c;
    color: #8a5a00;
    font-size: 13px;
}
.scope-banner.disabled {
    background: #fef0f0;
    border-bottom-color: #f56c6c;
    color: #a1372f;
}
.back {
    margin-left: 8px;
}
</style>
