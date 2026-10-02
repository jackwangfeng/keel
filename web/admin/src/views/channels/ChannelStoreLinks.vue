<script setup lang="ts">
// 门店映射：keel 的门店 ↔ 渠道上的门店（Shopify 是 location ID）。
// GET / PUT / DELETE /admin/channel-bindings/{id}/store-links[/{store_id}]；PUT 是按门店覆盖写，同一家店再存一次就是改。

import { computed, onMounted, ref } from "vue";
import { ElMessageBox } from "element-plus";
import { keel, type AdminStore, type ChannelStoreLink } from "../../api/client.ts";
import { datetime } from "../../ui/format.ts";
import { notifyError, notifyOk } from "../../ui/notify.ts";
import ProblemAlert from "../../components/ProblemAlert.vue";

const props = defineProps<{ bindingId: number; channel: string; stores: AdminStore[]; canWrite: boolean }>();
const emit = defineEmits<{ changed: [number] }>();

const loading = ref(false);
const error = ref<unknown>(null);
const links = ref<ChannelStoreLink[]>([]);
const draft = ref<{ storeId: number | null; externalId: string }>({ storeId: null, externalId: "" });
const saving = ref(false);
const saveError = ref<unknown>(null);
const localError = ref("");

const storeName = computed(() => new Map(props.stores.map((s) => [s.id, s.name])));
const externalHint = computed(() =>
    props.channel === "shopify" ? "Shopify 的 location ID（后台 设置 → 地点，地址栏末尾那串数字）" : "渠道上的门店 ID",
);

async function load(): Promise<void> {
    loading.value = true;
    error.value = null;
    try {
        const res = await keel.request("get", "/admin/channel-bindings/{binding_id}/store-links", {
            path: { binding_id: props.bindingId },
        });
        links.value = res.items;
        emit("changed", res.items.length);
    } catch (err) {
        error.value = err;
    } finally {
        loading.value = false;
    }
}
onMounted(() => void load());

function edit(l: ChannelStoreLink): void {
    draft.value = { storeId: l.store_id, externalId: l.external_store_id };
}

async function save(): Promise<void> {
    const { storeId, externalId } = draft.value;
    if (storeId === null || externalId.trim() === "") {
        localError.value = "门店和外部门店 ID 都要填";
        return;
    }
    localError.value = "";
    saving.value = true;
    saveError.value = null;
    try {
        await keel.request("put", "/admin/channel-bindings/{binding_id}/store-links/{store_id}", {
            path: { binding_id: props.bindingId, store_id: storeId },
            body: { external_store_id: externalId.trim() },
        });
        notifyOk("已保存门店映射");
        draft.value = { storeId: null, externalId: "" };
        await load();
    } catch (err) {
        saveError.value = err;
    } finally {
        saving.value = false;
    }
}

async function remove(l: ChannelStoreLink): Promise<void> {
    try {
        await ElMessageBox.confirm(
            `删掉「${storeName.value.get(l.store_id) ?? `门店 #${l.store_id}`}」的映射之后，这家店不再往渠道推库存与价格。`,
            "确认删除映射",
            { type: "warning" },
        );
    } catch {
        return;
    }
    try {
        await keel.request("delete", "/admin/channel-bindings/{binding_id}/store-links/{store_id}", {
            path: { binding_id: props.bindingId, store_id: l.store_id },
        });
        notifyOk("已删除");
    } catch (err) {
        notifyError(err);
    }
    await load();
}
</script>

<template>
    <div v-loading="loading">
        <ProblemAlert v-if="error" :error="error" />

        <el-form v-if="canWrite" inline class="link-form" @submit.prevent>
            <el-form-item label="门店">
                <el-select v-model="draft.storeId" filterable placeholder="选门店" style="width: 200px">
                    <el-option v-for="s in stores" :key="s.id" :value="s.id" :label="s.name" />
                </el-select>
            </el-form-item>
            <el-form-item label="外部门店 ID">
                <el-input v-model="draft.externalId" :placeholder="externalHint" style="width: 260px" />
            </el-form-item>
            <el-form-item>
                <el-button type="primary" :loading="saving" @click="save">保存映射</el-button>
            </el-form-item>
        </el-form>
        <el-alert v-if="localError" :title="localError" type="error" :closable="false" show-icon class="mb8" />
        <ProblemAlert v-if="saveError" :error="saveError" />

        <div class="table-wrap">
            <el-table :data="links" border stripe empty-text="还没有映射任何门店：没有映射的渠道账号不推库存与价格">
                <el-table-column label="门店" min-width="160">
                    <template #default="{ row }: { row: ChannelStoreLink }">
                        {{ storeName.get(row.store_id) ?? `门店 #${row.store_id}` }}
                    </template>
                </el-table-column>
                <el-table-column prop="external_store_id" label="外部门店 ID" min-width="180" />
                <el-table-column label="建立时间" width="170">
                    <template #default="{ row }: { row: ChannelStoreLink }">{{ datetime(row.created_at) }}</template>
                </el-table-column>
                <el-table-column v-if="canWrite" label="操作" width="120">
                    <template #default="{ row }: { row: ChannelStoreLink }">
                        <el-button link type="primary" @click="edit(row)">改</el-button>
                        <el-button link type="danger" @click="remove(row)">删除</el-button>
                    </template>
                </el-table-column>
            </el-table>
        </div>
    </div>
</template>

<style scoped>
.table-wrap {
    overflow-x: auto;
}
.mb8 {
    margin-bottom: 8px;
}
.link-form {
    margin-bottom: 4px;
}
</style>
