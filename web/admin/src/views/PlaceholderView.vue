<script setup lang="ts">
// 占位页。参数从路由的静态 props 来（见 src/router/modules/stores.ts），
// 于是「留个位置」这件事不需要为每一块写一个新的 .vue。
import NotWiredUp from "../components/NotWiredUp.vue";

defineProps<{ title: string; reason: string; upcoming?: string[] }>();
</script>

<template>
    <NotWiredUp :title="title" :reason="reason" :upcoming="upcoming" />
</template>
