<script setup lang="ts">
import { useRouter } from "vue-router";

const router = useRouter();
</script>

<template>
    <el-result icon="warning" title="页面不存在" sub-title="这个地址在后台里没有对应的页面。">
        <template #extra>
            <el-button type="primary" @click="router.push('/')">回到商品列表</el-button>
        </template>
    </el-result>
</template>
