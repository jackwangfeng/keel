<script setup lang="ts">
// 推送状态（GET /admin/channel-bindings/{id}/listings）：每个 (门店, SKU) 最近一次推到渠道的可售数与价格。
// 可按门店筛、只看出错的。契约只回 items、没有 total，所以分页是「上一页 / 下一页」。

import { computed, onMounted, ref } from "vue";
import { Refresh } from "@element-plus/icons-vue";
import type { AdminStore, ChannelListing } from "../../api/client.ts";
import { listListings } from "../../api/channels.ts";
import { datetime, yuan } from "../../ui/format.ts";
import ProblemAlert from "../../components/ProblemAlert.vue";

const props = defineProps<{ bindingId: number; stores: AdminStore[] }>();

const loading = ref(false);
const error = ref<unknown>(null);
const items = ref<ChannelListing[]>([]);
const hasMore = ref(false);
const page = ref(1);
const storeId = ref<number | null>(null);
const errorsOnly = ref(false);
const storeName = computed(() => new Map(props.stores.map((s) => [s.id, s.name])));

async function load(): Promise<void> {
    loading.value = true;
    error.value = null;
    try {
        const res = await listListings(props.bindingId, { storeId: storeId.value, errorsOnly: errorsOnly.value, page: page.value });
        items.value = res.items;
        hasMore.value = res.hasMore;
    } catch (err) {
        error.value = err;
    } finally {
        loading.value = false;
    }
}
onMounted(() => void load());

function refilter(): void {
    page.value = 1;
    void load();
}
function go(delta: number): void {
    page.value = Math.max(1, page.value + delta);
    void load();
}
</script>

<template>
    <div v-loading="loading">
        <ProblemAlert v-if="error" :error="error" />
        <div class="page-toolbar">
            <el-select v-model="storeId" clearable placeholder="全部门店" style="width: 180px" @change="refilter">
                <el-option v-for="s in stores" :key="s.id" :value="s.id" :label="s.name" />
            </el-select>
            <el-checkbox v-model="errorsOnly" @change="refilter">只看出错的</el-checkbox>
            <span class="grow" />
            <el-button :icon="Refresh" :loading="loading" @click="load">刷新</el-button>
        </div>

        <div class="table-wrap">
            <el-table :data="items" border stripe :empty-text="errorsOnly ? '没有出错的推送' : '还没有推送过'">
                <el-table-column prop="sku_code" label="SKU 货号" min-width="130" />
                <el-table-column prop="product_title" label="商品" min-width="180" />
                <el-table-column label="门店" min-width="120">
                    <template #default="{ row }: { row: ChannelListing }">{{ storeName.get(row.store_id) ?? `门店 #${row.store_id}` }}</template>
                </el-table-column>
                <el-table-column label="对外可售" width="90">
                    <template #default="{ row }: { row: ChannelListing }">{{ row.published_qty }}</template>
                </el-table-column>
                <el-table-column label="价格" width="100">
                    <template #default="{ row }: { row: ChannelListing }">{{ yuan(row.published_cents) }}</template>
                </el-table-column>
                <el-table-column label="推送时间" width="170">
                    <template #default="{ row }: { row: ChannelListing }">{{ datetime(row.pushed_at) }}</template>
                </el-table-column>
                <el-table-column label="错误" min-width="220">
                    <template #default="{ row }: { row: ChannelListing }">
                        <span v-if="row.last_error" class="err">{{ row.last_error }}</span>
                        <span v-else class="ok">—</span>
                    </template>
                </el-table-column>
            </el-table>
        </div>

        <div class="pager">
            <el-button :disabled="page === 1 || loading" @click="go(-1)">上一页</el-button>
            <span class="hint">第 {{ page }} 页</span>
            <el-button :disabled="!hasMore || loading" @click="go(1)">下一页</el-button>
        </div>
    </div>
</template>

<style scoped>
.table-wrap {
    overflow-x: auto;
}
.err {
    color: var(--el-color-danger);
    font-size: 12px;
    word-break: break-word;
}
.ok {
    color: var(--el-text-color-placeholder);
}
.pager {
    margin-top: 12px;
    display: flex;
    align-items: center;
    justify-content: flex-end;
    gap: 8px;
}
</style>
