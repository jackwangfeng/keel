<script setup lang="ts">
// 把 `errors[]` 给出的命中位置在原文上标出来。
//
// 为什么不是直接在输入框里染色：`<input>` / `<textarea>` 的内容是纯文本，
// 浏览器不给在里面放 `<mark>` 的办法。真要做只能拿一个 contenteditable 顶替
// 输入框，而那会换掉 Element Plus 的整套表单行为（校验、清空、禁用、IME）。
// 代价不成比例，所以这里在输入框**下面**贴一条对照：原文照抄，违禁词标红。
// 商家看到的仍然是「哪几个字」，而不是一句「文案不合规」。
import { computed } from "vue";
import { sliceByCodePoints } from "../api/errors.ts";
import type { FieldError } from "../api/client.ts";

const props = defineProps<{ text: string; hits: readonly FieldError[] }>();

interface Segment {
    text: string;
    hit: boolean;
    message: string;
}

/**
 * 按码点区间把原文切成若干段。
 *
 * 多条命中要先排序再合并推进，否则两条重叠 / 乱序的命中会把后半截原文重复
 * 渲染出来 —— 服务端一条命中一条地给，不承诺有序也不承诺不重叠。
 */
const segments = computed<Segment[]>(() => {
    const ranges = props.hits
        .filter((h): h is FieldError & { offset: number } => typeof h.offset === "number")
        .map((h) => ({ start: h.offset, end: h.offset + (h.length ?? 0), message: h.message ?? "命中违禁词" }))
        .sort((a, b) => a.start - b.start);

    const out: Segment[] = [];
    let cursor = 0;
    for (const r of ranges) {
        const start = Math.max(r.start, cursor);
        if (r.end <= start) continue;
        const [before, hit] = sliceByCodePoints(props.text, start, r.end - start);
        const lead = sliceByCodePoints(props.text, cursor, start - cursor)[1];
        void before;
        if (lead !== "") out.push({ text: lead, hit: false, message: "" });
        if (hit !== "") out.push({ text: hit, hit: true, message: r.message });
        cursor = r.end;
    }
    const tail = sliceByCodePoints(props.text, cursor, Number.MAX_SAFE_INTEGER)[1];
    if (tail !== "") out.push({ text: tail, hit: false, message: "" });
    return out;
});
</script>

<template>
    <p v-if="segments.length > 0" class="hl">
        <template v-for="(s, i) in segments" :key="i">
            <mark v-if="s.hit" :title="s.message">{{ s.text }}</mark>
            <span v-else>{{ s.text }}</span>
        </template>
    </p>
</template>

<style scoped>
.hl {
    margin: 4px 0 0;
    padding: 6px 8px;
    background: var(--el-fill-color-light);
    border-radius: 4px;
    font-size: 13px;
    line-height: 1.7;
    white-space: pre-wrap;
    word-break: break-all;
}
mark {
    background: var(--el-color-danger-light-7);
    color: var(--el-color-danger);
    font-weight: 600;
    border-radius: 2px;
    padding: 0 1px;
}
</style>
