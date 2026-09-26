<script setup lang="ts">
// 「这一块还没接上」。
//
// 这个组件存在的理由是：**画一个假表格比留一句实话糟得多。** 假表格会让
// 看 Demo 的人以为这块能用，等他点下去才发现什么都没有；而「还没接上」
// 一眼就说清了边界。这个仓库的习惯是把没做的事写在看得见的地方。
defineProps<{
    title: string;
    /** 为什么还没有。一句话，说清是谁在做、卡在哪。 */
    reason: string;
    /** 接上之后会有哪些东西。让读者知道这个位置留给了什么。 */
    upcoming?: string[];
}>();
</script>

<template>
    <div class="not-wired">
        <el-result icon="info" :title="title" :sub-title="reason">
            <template v-if="upcoming && upcoming.length > 0" #extra>
                <div class="upcoming">
                    <p>接上之后这里会有：</p>
                    <ul>
                        <li v-for="item in upcoming" :key="item">{{ item }}</li>
                    </ul>
                </div>
            </template>
        </el-result>
    </div>
</template>

<style scoped>
.not-wired {
    padding-top: 24px;
}
.upcoming {
    display: inline-block;
    text-align: left;
    color: var(--el-text-color-regular);
    font-size: 13px;
}
.upcoming ul {
    margin: 4px 0 0;
    padding-left: 18px;
}
</style>
