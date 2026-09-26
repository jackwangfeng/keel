<script setup lang="ts">
// 把一个错误摊开给人看：标题、详情、type、字段级错误、trace_id。
//
// 刻意**全部显示**，不做「只给用户看友好的那句」。后台的使用者是商家与运营，
// 他们要把问题描述给别人听；而 `type` 与 `trace_id` 正是那通电话里唯一有用的
// 两样东西。藏起来的结果是每一次报障都从「你截个图」开始。
import { computed } from "vue";
import { describeError } from "../api/errors.ts";

const props = defineProps<{ error: unknown }>();

const info = computed(() => describeError(props.error));

const severity = computed<"error" | "warning">(() =>
    info.value.status !== null && info.value.status >= 500 ? "warning" : "error",
);
</script>

<template>
    <el-alert :title="info.title" :type="severity" :closable="false" show-icon class="problem-alert">
        <template #default>
            <p v-if="info.detail" class="problem-detail">{{ info.detail }}</p>

            <ul v-if="info.fields.length > 0" class="problem-fields">
                <li v-for="(f, i) in info.fields" :key="i">
                    <code v-if="f.field">{{ f.field }}</code>
                    <span>{{ f.message ?? "（服务端没给这一条的说明）" }}</span>
                    <span v-if="typeof f.offset === 'number'" class="problem-pos">
                        第 {{ f.offset + 1 }} 个字起，共 {{ f.length ?? 0 }} 个字
                    </span>
                </li>
            </ul>

            <p v-if="info.hint" class="problem-hint">下一步：{{ info.hint }}</p>

            <pre v-if="info.rawBody" class="problem-raw">{{ info.rawBody }}</pre>

            <p class="problem-meta">
                <span v-if="info.status !== null">HTTP {{ info.status }}</span>
                <code v-if="info.type">{{ info.type }}</code>
                <span v-if="info.traceId">trace_id: {{ info.traceId }}</span>
            </p>
        </template>
    </el-alert>
</template>

<style scoped>
.problem-alert {
    margin-bottom: 12px;
}
.problem-detail {
    margin: 4px 0;
    white-space: pre-wrap;
}
.problem-fields {
    margin: 6px 0;
    padding-left: 18px;
}
.problem-fields li {
    margin: 2px 0;
}
.problem-fields code {
    margin-right: 6px;
    padding: 0 4px;
    background: rgba(0, 0, 0, 0.06);
    border-radius: 3px;
}
.problem-pos {
    margin-left: 6px;
    color: var(--el-color-warning);
}
.problem-hint {
    margin: 6px 0;
    padding: 6px 8px;
    background: rgba(255, 255, 255, 0.6);
    border-radius: 4px;
    color: var(--el-text-color-primary);
}
.problem-raw {
    max-height: 160px;
    overflow: auto;
    margin: 6px 0;
    padding: 6px;
    background: rgba(0, 0, 0, 0.05);
    font-size: 12px;
    white-space: pre-wrap;
    word-break: break-all;
}
.problem-meta {
    margin: 6px 0 0;
    font-size: 12px;
    opacity: 0.75;
}
.problem-meta > * {
    margin-right: 10px;
}
</style>
