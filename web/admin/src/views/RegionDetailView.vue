<script setup lang="ts">
// 一个大区的「商品可见性 + 大区价」。
//
// 契约没有 `GET /admin/regions/{id}`（单个大区的详情），所以标题从列表里找。
// 找不到（比如被删了）也不妨碍下面的商品表——那张表只要 id。

import { computed, onMounted, ref } from "vue";
import { useRouter } from "vue-router";
import { ArrowLeft } from "@element-plus/icons-vue";
import type { AdminRegion } from "../api/client.ts";
import { listAllRegions } from "../api/stores.ts";
import { notifyError } from "../ui/notify.ts";
import ScopedProducts from "../components/ScopedProducts.vue";

const props = defineProps<{ regionId: string }>();
const router = useRouter();
const id = computed(() => Number(props.regionId));
const region = ref<AdminRegion | null>(null);

onMounted(() => {
    listAllRegions().then(
        (rs) => (region.value = rs.find((r) => r.id === id.value) ?? null),
        (e: unknown) => notifyError(e),
    );
});
</script>

<template>
    <div>
        <div class="page-toolbar">
            <el-button :icon="ArrowLeft" @click="router.push({ name: 'regions' })">返回大区</el-button>
            <span class="title">{{ region?.name ?? `大区 #${id}` }}</span>
            <span v-if="region" class="hint">编号 {{ region.code }} · {{ region.store_count }} 家门店</span>
        </div>
        <ScopedProducts :scope="{ kind: 'region', id, name: region?.name ?? `#${id}` }" />
    </div>
</template>

<style scoped>
.title {
    font-size: 16px;
    font-weight: 600;
}
</style>
