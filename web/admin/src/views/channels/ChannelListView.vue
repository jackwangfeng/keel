<script setup lang="ts">
// 渠道账号列表（GET /admin/channel-bindings）：商家接的 Shopify / 美团……账号。
// 凭据只显示「已配置 / 未配置」（ChannelBinding.has_secrets），值永不回显。
// 写操作只有管理员能做（服务端 requireMerchantAdmin），别的角色不显示按钮。
//
// 「订单」视图（?view=orders，可带 &store_id=）：所有账号的渠道订单，通知「渠道订单」跳到这里、按门店筛。
// 视图与门店筛选都写回地址栏（replace），刷新 / 分享链接停在同一处。

import { computed, onMounted, ref } from "vue";
import { useRoute, useRouter } from "vue-router";
import { CopyDocument, Plus, Refresh } from "@element-plus/icons-vue";
import { keel, type AdminStore, type ChannelBinding, type ChannelKind } from "../../api/client.ts";
import { listAllStores } from "../../api/stores.ts";
import { bindingStatusLabel, channelLabel, rolesLabel } from "../../api/channelRules.ts";
import { can } from "../../auth/permissions.ts";
import { notifyError, notifyOk } from "../../ui/notify.ts";
import ProblemAlert from "../../components/ProblemAlert.vue";
import ChannelBindingDialog from "./ChannelBindingDialog.vue";
import ChannelOrders from "./ChannelOrders.vue";

const router = useRouter();
const route = useRoute();
const view = computed<"bindings" | "orders">({
    get: () => (route.query.view === "orders" ? "orders" : "bindings"),
    set: (v) => void router.replace({ query: v === "orders" ? { ...route.query, view: "orders" } : {} }),
});
const queryStoreId = computed<number | null>(() => {
    const n = Number(route.query.store_id);
    return Number.isInteger(n) && n > 0 ? n : null;
});
function onOrdersStore(storeId: number | null): void {
    const q: Record<string, string> = { view: "orders" };
    if (storeId !== null) q.store_id = String(storeId);
    void router.replace({ query: q });
}
const stores = ref<AdminStore[]>([]);
const loading = ref(false);
const error = ref<unknown>(null);
const bindings = ref<ChannelBinding[]>([]);
const kinds = ref<ChannelKind[]>([]);
const canWrite = computed(() => can.manageChannels());

async function load(): Promise<void> {
    loading.value = true;
    error.value = null;
    try {
        const [b, k, st] = await Promise.all([
            keel.get("/admin/channel-bindings", {}),
            keel.get("/admin/channel-kinds", {}),
            listAllStores(),
        ]);
        bindings.value = b.items;
        kinds.value = k.items;
        stores.value = st.stores.filter((x) => !x.deleted_at);
    } catch (err) {
        error.value = err;
    } finally {
        loading.value = false;
    }
}
onMounted(() => void load());

function webhookUrl(b: ChannelBinding): string {
    return `${location.origin}${b.webhook_path}`;
}

function copy(text: string): void {
    navigator.clipboard.writeText(text).then(
        () => notifyOk("已复制"),
        (err: unknown) => notifyError(err),
    );
}

const dialogVisible = ref(false);
async function onCreated(b: ChannelBinding): Promise<void> {
    notifyOk(`已新建「${b.name}」（停用状态）。配好凭据和门店映射后再启用。`);
    await router.push({ name: "channel-detail", params: { bindingId: b.id } });
}
</script>

<template>
    <div>
        <ProblemAlert v-if="error" :error="error" />
        <el-tabs v-model="view" class="view-tabs">
            <el-tab-pane label="账号" name="bindings" />
            <el-tab-pane label="订单" name="orders" />
        </el-tabs>

        <ChannelOrders
            v-if="view === 'orders' && !loading"
            :binding-id="null"
            :bindings="bindings"
            :kinds="kinds"
            :stores="stores"
            :initial-store-id="queryStoreId"
            @store-change="onOrdersStore"
        />

        <template v-if="view === 'bindings'">
            <div class="page-toolbar">
                <span class="hint">每个账号是商家在一个渠道上的一家店。新建之后到详情页配凭据、映射门店、设库存与价格规则，再启用。</span>
                <span class="grow" />
                <el-button :icon="Refresh" :loading="loading" @click="load">刷新</el-button>
                <el-button v-if="canWrite" type="primary" :icon="Plus" :disabled="kinds.length === 0" @click="dialogVisible = true">
                    新建账号
                </el-button>
            </div>

            <div class="table-wrap">
                <el-table :data="bindings" v-loading="loading" border stripe empty-text="还没有接任何渠道">
                    <el-table-column prop="id" label="ID" width="64" />
                    <el-table-column label="名称" min-width="160">
                        <template #default="{ row }: { row: ChannelBinding }">
                            <router-link :to="{ name: 'channel-detail', params: { bindingId: row.id } }">{{ row.name }}</router-link>
                        </template>
                    </el-table-column>
                    <el-table-column label="渠道" width="100">
                        <template #default="{ row }: { row: ChannelBinding }">{{ channelLabel(row.channel) }}</template>
                    </el-table-column>
                    <el-table-column prop="external_account" label="店铺账号" min-width="200" />
                    <el-table-column label="角色" min-width="150">
                        <template #default="{ row }: { row: ChannelBinding }">{{ rolesLabel(row.roles) }}</template>
                    </el-table-column>
                    <el-table-column label="状态" width="100">
                        <template #default="{ row }: { row: ChannelBinding }">
                            <el-tag :type="bindingStatusLabel(row.status).type" size="small">{{
                                bindingStatusLabel(row.status).text
                            }}</el-tag>
                        </template>
                    </el-table-column>
                    <el-table-column label="凭据" width="90">
                        <template #default="{ row }: { row: ChannelBinding }">
                            <el-tag :type="row.has_secrets ? 'success' : 'warning'" size="small">{{
                                row.has_secrets ? "已配置" : "未配置"
                            }}</el-tag>
                        </template>
                    </el-table-column>
                    <el-table-column label="回调路径" min-width="260">
                        <template #default="{ row }: { row: ChannelBinding }">
                            <code class="path">{{ row.webhook_path }}</code>
                            <el-button link type="primary" :icon="CopyDocument" title="复制完整回调地址" @click="copy(webhookUrl(row))" />
                        </template>
                    </el-table-column>
                </el-table>
            </div>
        </template>

        <ChannelBindingDialog v-model="dialogVisible" :binding="null" :kinds="kinds" @saved="onCreated" />
    </div>
</template>

<style scoped>
.view-tabs {
    margin-bottom: 4px;
}
.table-wrap {
    overflow-x: auto;
}
.path {
    font-size: 12px;
    word-break: break-all;
}
</style>
