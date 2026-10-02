<script setup lang="ts">
// 渠道订单（GET /admin/channel-orders）：平台单号、状态、keel 订单号、异常（标红），操作重试 / 接单 / 拒单；
// 点单号开详情抽屉（ChannelOrderDrawer：行、金额（税单列）、收货人、平台申请）。
//
// 两处用：渠道账号详情的「订单」页签（bindingId 固定）；渠道页的「订单」视图（/channels?view=orders&store_id=，
// 通知「渠道订单」跳过来只带门店，可再按账号筛）。动作的权限服务端按渠道单的门店判（同发货），
// 前端只按 can.handleOrder 置灰，确认框在页面里（ElMessageBox）。

import { computed, onMounted, ref, watch } from "vue";
import { Refresh } from "@element-plus/icons-vue";
import { ElMessageBox } from "element-plus";
import type { AdminStore, ChannelBinding, ChannelKind, ChannelOrder, ChannelOrderPage } from "../../api/client.ts";
import { channelOrderAction, listChannelOrders } from "../../api/channels.ts";
import { CHANNEL_ORDER_STATUS_OPTIONS, channelOrderActions, channelOrderStatusLabel } from "../../api/channelRules.ts";
import { can, merchantWide } from "../../auth/permissions.ts";
import { datetime, yuan } from "../../ui/format.ts";
import { notifyError, notifyOk } from "../../ui/notify.ts";
import { useMobile } from "../../ui/useMobile.ts";
import ProblemAlert from "../../components/ProblemAlert.vue";
import ChannelOrderDrawer from "./ChannelOrderDrawer.vue";

const props = defineProps<{
    /** 固定在一个渠道账号上（账号详情的页签）；null 时可按账号筛。 */
    bindingId: number | null;
    bindings: ChannelBinding[];
    kinds: ChannelKind[];
    stores: AdminStore[];
    initialStoreId?: number | null;
}>();
const emit = defineEmits<{ storeChange: [storeId: number | null] }>();
const mobile = useMobile();

const loading = ref(false);
const error = ref<unknown>(null);
const page = ref<ChannelOrderPage | null>(null);
const pageNo = ref(1);
const pageSize = ref(20);
const bindingFilter = ref<number | null>(null);
const storeId = ref<number | null>(props.initialStoreId ?? null);
const status = ref<number | null>(null);
const exceptionOnly = ref(false);

const storeById = computed(() => new Map(props.stores.map((s) => [s.id, s])));
const bindingById = computed(() => new Map(props.bindings.map((b) => [b.id, b])));
const acceptRequiredOf = computed(() => new Map(props.kinds.map((k) => [k.channel, k.accept_required])));

async function load(): Promise<void> {
    loading.value = true;
    error.value = null;
    try {
        page.value = await listChannelOrders({
            bindingId: props.bindingId ?? bindingFilter.value,
            storeId: storeId.value,
            status: status.value,
            exceptionOnly: exceptionOnly.value,
            page: pageNo.value,
            pageSize: pageSize.value,
        });
    } catch (err) {
        error.value = err;
    } finally {
        loading.value = false;
    }
}
onMounted(() => void load());
watch(
    () => props.initialStoreId,
    (v) => {
        if ((v ?? null) !== storeId.value) {
            storeId.value = v ?? null;
            refilter();
        }
    },
);

function refilter(): void {
    pageNo.value = 1;
    void load();
}
function onStoreChange(): void {
    emit("storeChange", storeId.value);
    refilter();
}

function bindingName(id: number): string {
    return bindingById.value.get(id)?.name ?? `账号 #${id}`;
}
function storeName(id: number | null | undefined): string {
    if (id === null || id === undefined) return "未映射";
    return storeById.value.get(id)?.name ?? `门店 #${id}`;
}
function actionsOf(o: ChannelOrder): { retry: boolean; accept: boolean; reject: boolean } {
    const kind = bindingById.value.get(o.binding_id)?.channel ?? "";
    return channelOrderActions(o, acceptRequiredOf.value.get(kind) ?? false);
}
/** 能不能处理这张单：服务端按渠道单门店判（同发货）；没映射门店的要全店范围。 */
function canHandle(o: ChannelOrder): boolean {
    if (o.store_id === null || o.store_id === undefined) return merchantWide();
    const st = storeById.value.get(o.store_id);
    return can.handleOrder({ id: o.store_id, region_id: st?.region_id ?? 0 });
}

const busy = ref<number | null>(null);
async function act(o: ChannelOrder, action: "retry" | "accept" | "reject"): Promise<void> {
    let reason: string | undefined;
    try {
        if (action === "reject") {
            const r = await ElMessageBox.prompt(
                `拒掉平台单 ${o.external_order_name}？拒单会回给平台，平台按自己的规则通知顾客、退款。`,
                "确认拒单",
                {
                    inputPlaceholder: "拒单原因（可不填，默认「商家拒单」）",
                    inputValidator: (v: string) => (v ?? "").length <= 200 || "最多 200 个字",
                    confirmButtonText: "拒单",
                    type: "warning",
                },
            );
            reason = (r.value ?? "").trim();
        } else {
            const msg =
                action === "accept"
                    ? `接下平台单 ${o.external_order_name}？会建 keel 订单并扣门店库存。`
                    : `重试平台单 ${o.external_order_name}？会重新回读平台、再走一遍接单（补了库存或映射之后用）。`;
            await ElMessageBox.confirm(msg, action === "accept" ? "确认接单" : "确认重试", { type: "info" });
        }
    } catch {
        return;
    }
    busy.value = o.id;
    try {
        const d = await channelOrderAction(o.id, action, reason);
        if (d.exception) notifyError(new Error(`没成单：${d.exception}`));
        else notifyOk(action === "reject" ? "已拒单" : d.order_no ? `已成单 ${d.order_no}` : "已提交，稍后刷新看结果");
    } catch (err) {
        notifyError(err);
    } finally {
        busy.value = null;
        await load();
    }
}

const openId = ref<number | null>(null);
</script>

<template>
    <div>
        <ProblemAlert v-if="error" :error="error" />
        <div class="page-toolbar">
            <el-select
                v-if="bindingId === null"
                v-model="bindingFilter"
                clearable
                placeholder="全部账号"
                style="width: 160px"
                @change="refilter"
            >
                <el-option v-for="b in bindings" :key="b.id" :value="b.id" :label="b.name" />
            </el-select>
            <el-select v-model="storeId" clearable placeholder="全部门店" style="width: 160px" @change="onStoreChange">
                <el-option v-for="s in stores" :key="s.id" :value="s.id" :label="s.name" />
            </el-select>
            <el-select v-model="status" clearable placeholder="全部状态" style="width: 120px" @change="refilter">
                <el-option v-for="o in CHANNEL_ORDER_STATUS_OPTIONS" :key="o.value" :value="o.value" :label="o.text" />
            </el-select>
            <el-checkbox v-model="exceptionOnly" @change="refilter">只看异常</el-checkbox>
            <span class="grow" />
            <el-button :icon="Refresh" :loading="loading" @click="load">刷新</el-button>
        </div>

        <div v-if="!mobile" class="table-wrap">
            <el-table
                v-loading="loading"
                :data="page?.items ?? []"
                border
                stripe
                :empty-text="exceptionOnly ? '没有异常的渠道单' : '还没有渠道订单'"
            >
                <el-table-column label="平台单号" min-width="130">
                    <template #default="{ row }: { row: ChannelOrder }">
                        <el-link type="primary" @click="openId = row.id">{{ row.external_order_name || row.external_order_id }}</el-link>
                        <el-tag v-if="row.test" size="small" type="info" class="ml4">测试单</el-tag>
                        <div v-if="bindingId === null" class="hint">{{ bindingName(row.binding_id) }}</div>
                    </template>
                </el-table-column>
                <el-table-column label="状态" min-width="200">
                    <template #default="{ row }: { row: ChannelOrder }">
                        <el-tag size="small" :type="channelOrderStatusLabel(row.status).type">{{
                            channelOrderStatusLabel(row.status).text
                        }}</el-tag>
                        <div v-if="row.exception" class="err">{{ row.exception }}</div>
                        <div v-else-if="row.accept_deadline && row.status === 2" class="hint">
                            接单截止 {{ datetime(row.accept_deadline) }}
                        </div>
                    </template>
                </el-table-column>
                <el-table-column label="keel 订单" min-width="170">
                    <template #default="{ row }: { row: ChannelOrder }">
                        <router-link v-if="row.order_no" :to="{ path: '/orders', query: { order_no: row.order_no } }">{{
                            row.order_no
                        }}</router-link>
                        <span v-else class="placeholder">—</span>
                    </template>
                </el-table-column>
                <el-table-column label="门店" min-width="110">
                    <template #default="{ row }: { row: ChannelOrder }">{{ storeName(row.store_id) }}</template>
                </el-table-column>
                <el-table-column label="实付" width="100">
                    <template #default="{ row }: { row: ChannelOrder }">{{ yuan(row.amounts.buyer_paid) }}</template>
                </el-table-column>
                <el-table-column label="收单时间" width="160">
                    <template #default="{ row }: { row: ChannelOrder }">{{ datetime(row.created_at) }}</template>
                </el-table-column>
                <el-table-column label="操作" width="150" fixed="right">
                    <template #default="{ row }: { row: ChannelOrder }">
                        <template v-if="canHandle(row)">
                            <el-button
                                v-if="actionsOf(row).retry"
                                size="small"
                                type="warning"
                                :loading="busy === row.id"
                                @click="act(row, 'retry')"
                                >重试</el-button
                            >
                            <el-button
                                v-if="actionsOf(row).accept"
                                size="small"
                                type="primary"
                                :loading="busy === row.id"
                                @click="act(row, 'accept')"
                                >接单</el-button
                            >
                            <el-button
                                v-if="actionsOf(row).reject"
                                size="small"
                                type="danger"
                                plain
                                :disabled="busy === row.id"
                                @click="act(row, 'reject')"
                                >拒单</el-button
                            >
                        </template>
                    </template>
                </el-table-column>
            </el-table>
        </div>

        <div v-else v-loading="loading" class="co-cards">
            <el-empty
                v-if="!loading && (page?.items?.length ?? 0) === 0"
                :description="exceptionOnly ? '没有异常的渠道单' : '还没有渠道订单'"
            />
            <div v-for="row in page?.items ?? []" :key="row.id" class="co-card" @click="openId = row.id">
                <div class="co-top">
                    <el-link type="primary" class="co-no">{{ row.external_order_name || row.external_order_id }}</el-link>
                    <span class="co-amount">{{ yuan(row.amounts.buyer_paid) }}</span>
                </div>
                <div class="co-tags">
                    <el-tag size="small" :type="channelOrderStatusLabel(row.status).type">{{
                        channelOrderStatusLabel(row.status).text
                    }}</el-tag>
                    <el-tag v-if="row.test" size="small" type="info">测试单</el-tag>
                </div>
                <div v-if="row.exception" class="err">{{ row.exception }}</div>
                <div class="co-info">
                    <span v-if="bindingId === null">{{ bindingName(row.binding_id) }}</span>
                    <span>{{ storeName(row.store_id) }}</span>
                    <span v-if="row.order_no">
                        keel
                        <router-link :to="{ path: '/orders', query: { order_no: row.order_no } }" @click.stop>{{
                            row.order_no
                        }}</router-link>
                    </span>
                    <span>{{ datetime(row.created_at) }}</span>
                </div>
                <div v-if="canHandle(row) && (actionsOf(row).retry || actionsOf(row).accept)" class="co-actions">
                    <el-button v-if="actionsOf(row).retry" type="warning" :loading="busy === row.id" @click.stop="act(row, 'retry')"
                        >重试</el-button
                    >
                    <el-button v-if="actionsOf(row).accept" type="primary" :loading="busy === row.id" @click.stop="act(row, 'accept')"
                        >接单</el-button
                    >
                    <el-button v-if="actionsOf(row).reject" type="danger" plain :disabled="busy === row.id" @click.stop="act(row, 'reject')"
                        >拒单</el-button
                    >
                </div>
            </div>
        </div>

        <el-pagination
            v-if="page && page.total > 0"
            class="pager"
            :layout="mobile ? 'total, prev, next' : 'total, sizes, prev, pager, next'"
            :total="page.total"
            :current-page="page.page"
            :page-size="page.page_size"
            :page-sizes="[10, 20, 50, 100]"
            @current-change="
                (p: number) => {
                    pageNo = p;
                    load();
                }
            "
            @size-change="
                (s: number) => {
                    pageSize = s;
                    refilter();
                }
            "
        />

        <ChannelOrderDrawer
            :channel-order-id="openId"
            :bindings="bindings"
            :kinds="kinds"
            :stores="stores"
            @close="openId = null"
            @changed="load"
        />
    </div>
</template>

<style scoped>
.table-wrap {
    overflow-x: auto;
}
.ml4 {
    margin-left: 4px;
}
.err {
    color: var(--el-color-danger);
    font-size: 12px;
    margin-top: 4px;
    word-break: break-word;
}
.placeholder {
    color: var(--el-text-color-placeholder);
}
.pager {
    margin-top: 12px;
    justify-content: flex-end;
}
.co-cards {
    display: flex;
    flex-direction: column;
    gap: 10px;
    min-height: 80px;
}
.co-card {
    border: 1px solid var(--el-border-color-lighter);
    border-radius: 8px;
    padding: 10px 12px;
    background: var(--el-bg-color);
}
.co-top {
    display: flex;
    justify-content: space-between;
    align-items: center;
}
.co-no {
    font-weight: 600;
}
.co-amount {
    font-weight: 600;
}
.co-tags {
    margin-top: 6px;
    display: flex;
    gap: 6px;
    flex-wrap: wrap;
}
.co-info {
    margin-top: 6px;
    display: flex;
    flex-wrap: wrap;
    gap: 4px 12px;
    font-size: 13px;
    color: var(--el-text-color-secondary);
}
.co-actions {
    margin-top: 8px;
    display: flex;
    gap: 8px;
    justify-content: flex-end;
}
</style>
