<script setup lang="ts">
// 一张渠道单的详情（GET /admin/channel-orders/{id}）：平台快照的行、金额（税单列：平台总价 = 实付 + 税）、
// 收货人、平台申请（待处理的可同意 / 拒绝）；操作同列表（重试 / 接单 / 拒单）。
// 同意申请不动 keel 订单：要等平台确认之后按平台事实处理（服务端 DecideRequest 的约定），这里写在提示里。

import { computed, ref, watch } from "vue";
import { ElMessageBox } from "element-plus";
import type { AdminStore, ChannelBinding, ChannelKind, ChannelOrderDetail, ChannelOrderRequest } from "../../api/client.ts";
import { channelOrderAction, decideChannelOrderRequest, getChannelOrder } from "../../api/channels.ts";
import {
    channelAddressText,
    channelLabel,
    channelOrderActions,
    channelOrderStatusLabel,
    channelOrderTotals,
    requestKindLabel,
    requestStatusLabel,
} from "../../api/channelRules.ts";
import { can, merchantWide } from "../../auth/permissions.ts";
import { datetime, yuan } from "../../ui/format.ts";
import { notifyError, notifyOk } from "../../ui/notify.ts";
import { useMobile } from "../../ui/useMobile.ts";
import ProblemAlert from "../../components/ProblemAlert.vue";

const props = defineProps<{ channelOrderId: number | null; bindings: ChannelBinding[]; kinds: ChannelKind[]; stores: AdminStore[] }>();
const emit = defineEmits<{ close: []; changed: [] }>();
const mobile = useMobile();

const visible = computed({
    get: () => props.channelOrderId !== null,
    set: (v: boolean) => {
        if (!v) emit("close");
    },
});

const loading = ref(false);
const error = ref<unknown>(null);
const co = ref<ChannelOrderDetail | null>(null);

async function load(): Promise<void> {
    const id = props.channelOrderId;
    if (id === null) return;
    loading.value = true;
    error.value = null;
    try {
        co.value = await getChannelOrder(id);
    } catch (err) {
        co.value = null;
        error.value = err;
    } finally {
        loading.value = false;
    }
}
watch(
    () => props.channelOrderId,
    () => {
        co.value = null;
        void load();
    },
    { immediate: true },
);

const binding = computed(() => props.bindings.find((b) => b.id === co.value?.binding_id) ?? null);
const store = computed(() => props.stores.find((s) => s.id === co.value?.store_id) ?? null);
const status = computed(() => (co.value === null ? null : channelOrderStatusLabel(co.value.status)));
const totals = computed(() => (co.value === null ? null : channelOrderTotals(co.value.amounts)));
const acceptRequired = computed(() => props.kinds.find((k) => k.channel === binding.value?.channel)?.accept_required ?? false);
const actions = computed(() => (co.value === null ? null : channelOrderActions(co.value, acceptRequired.value)));
const canHandle = computed(() => {
    const o = co.value;
    if (o === null) return false;
    if (o.store_id === null || o.store_id === undefined) return merchantWide();
    return can.handleOrder({ id: o.store_id, region_id: store.value?.region_id ?? 0 });
});

const busy = ref(false);
async function run(fn: () => Promise<ChannelOrderDetail>, ok: (d: ChannelOrderDetail) => string): Promise<void> {
    busy.value = true;
    try {
        const d = await fn();
        co.value = d;
        if (d.exception) notifyError(new Error(`没成单：${d.exception}`));
        else notifyOk(ok(d));
        emit("changed");
    } catch (err) {
        notifyError(err);
        await load();
        emit("changed");
    } finally {
        busy.value = false;
    }
}

async function act(action: "retry" | "accept" | "reject"): Promise<void> {
    const o = co.value;
    if (o === null) return;
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
            await ElMessageBox.confirm(
                action === "accept" ? "接单会建 keel 订单并扣门店库存。" : "重新回读平台、再走一遍接单（补了库存或映射之后用）。",
                action === "accept" ? "确认接单" : "确认重试",
                { type: "info" },
            );
        }
    } catch {
        return;
    }
    await run(
        () => channelOrderAction(o.id, action, reason),
        (d) => (action === "reject" ? "已拒单" : d.order_no ? `已成单 ${d.order_no}` : "已提交，稍后刷新看结果"),
    );
}

async function decide(req: ChannelOrderRequest, agree: boolean): Promise<void> {
    try {
        await ElMessageBox.confirm(
            agree
                ? `同意平台的「${requestKindLabel(req.kind)}」申请？会回给平台；keel 订单要等平台确认之后才按平台的结果处理（退款、关单）。`
                : `拒绝平台的「${requestKindLabel(req.kind)}」申请？会回给平台，订单照常履约。`,
            agree ? "同意申请" : "拒绝申请",
            { type: agree ? "warning" : "info", confirmButtonText: agree ? "同意" : "拒绝" },
        );
    } catch {
        return;
    }
    await run(
        () => decideChannelOrderRequest(req.id, agree),
        () => (agree ? "已同意，等平台确认" : "已拒绝"),
    );
}

function deliveryText(mode: number): string {
    const parts = [
        [1, "快递"],
        [2, "本地配送"],
        [4, "平台骑手"],
        [8, "商家自配送"],
    ] as const;
    const out = parts.filter(([bit]) => (mode & bit) !== 0).map(([, t]) => t);
    return out.length === 0 ? "—" : out.join("、");
}
</script>

<template>
    <el-drawer v-model="visible" :title="`渠道单 ${co?.external_order_name ?? ''}`" size="720px" destroy-on-close>
        <ProblemAlert v-if="error" :error="error" />
        <div v-loading="loading">
            <template v-if="co && status && totals && actions">
                <div class="page-toolbar">
                    <el-tag :type="status.type">{{ status.text }}</el-tag>
                    <el-tag v-if="co.test" type="info">测试单</el-tag>
                    <span class="grow" />
                    <template v-if="canHandle">
                        <el-button v-if="actions.retry" type="warning" :loading="busy" @click="act('retry')">重试</el-button>
                        <el-button v-if="actions.accept" type="primary" :loading="busy" @click="act('accept')">接单</el-button>
                        <el-button v-if="actions.reject" type="danger" plain :disabled="busy" @click="act('reject')">拒单</el-button>
                    </template>
                </div>
                <el-alert v-if="co.exception" type="error" :closable="false" show-icon class="mb12" :title="`异常：${co.exception}`">
                    {{ co.order_no ? "keel 订单已经建了，去订单 / 售后里按实际情况处理。" : "补了库存或映射之后点「重试」。" }}
                </el-alert>

                <el-descriptions :column="mobile ? 1 : 2" border size="small" class="mb12">
                    <el-descriptions-item label="渠道账号">
                        {{ binding ? `${binding.name}（${channelLabel(binding.channel)}）` : `账号 #${co.binding_id}` }}
                    </el-descriptions-item>
                    <el-descriptions-item label="平台状态">{{ co.platform_status }}</el-descriptions-item>
                    <el-descriptions-item label="keel 订单">
                        <router-link v-if="co.order_no" :to="{ path: '/orders', query: { order_no: co.order_no } }">{{
                            co.order_no
                        }}</router-link>
                        <span v-else>还没成单</span>
                    </el-descriptions-item>
                    <el-descriptions-item label="门店">{{
                        store?.name ?? (co.store_id ? `门店 #${co.store_id}` : "未映射")
                    }}</el-descriptions-item>
                    <el-descriptions-item label="买家">渠道顾客</el-descriptions-item>
                    <el-descriptions-item label="配送">{{ deliveryText(co.delivery_mode) }}</el-descriptions-item>
                    <el-descriptions-item v-if="co.accept_deadline" label="接单截止">{{
                        datetime(co.accept_deadline)
                    }}</el-descriptions-item>
                    <el-descriptions-item label="收单时间">{{ datetime(co.created_at) }}</el-descriptions-item>
                    <el-descriptions-item label="收货人" :span="mobile ? 1 : 2">
                        {{ co.receiver.name || "—" }} · {{ co.receiver.phone || "—" }}
                        <el-tag v-if="co.receiver.phone_kind === 1" size="small" type="info" class="ml4">隐私号</el-tag>
                        <div class="hint">{{ channelAddressText(co.receiver.address) }}</div>
                    </el-descriptions-item>
                </el-descriptions>

                <h4>商品（平台快照）</h4>
                <el-table v-if="!mobile" :data="co.lines" size="small" class="mb12">
                    <el-table-column label="商品" min-width="180">
                        <template #default="{ row }">
                            {{ row.title }}
                            <div class="hint">渠道 SKU {{ row.external_sku_id || "—" }}</div>
                        </template>
                    </el-table-column>
                    <el-table-column label="单价" width="90">
                        <template #default="{ row }">{{ yuan(row.price_cents) }}</template>
                    </el-table-column>
                    <el-table-column prop="qty" label="件数" width="60" />
                    <el-table-column label="已退" width="60">
                        <template #default="{ row }">{{ row.refunded_qty || "—" }}</template>
                    </el-table-column>
                    <el-table-column label="小计" width="100">
                        <template #default="{ row }">{{ yuan(row.price_cents * row.qty) }}</template>
                    </el-table-column>
                </el-table>
                <div v-else class="mobile-list mb12">
                    <div v-for="row in co.lines" :key="row.external_line_id" class="mobile-row">
                        <div class="mr-title">{{ row.title }}</div>
                        <div class="mr-line hint">渠道 SKU {{ row.external_sku_id || "—" }}</div>
                        <div class="mr-line">
                            {{ yuan(row.price_cents) }} × {{ row.qty }} = {{ yuan(row.price_cents * row.qty) }}
                            <span v-if="row.refunded_qty"> · 已退 {{ row.refunded_qty }}</span>
                        </div>
                    </div>
                </div>

                <h4>金额</h4>
                <el-descriptions :column="mobile ? 1 : 3" border size="small" class="mb12">
                    <el-descriptions-item label="商品">{{ yuan(co.amounts.goods) }}</el-descriptions-item>
                    <el-descriptions-item label="运费">{{ yuan(co.amounts.freight) }}</el-descriptions-item>
                    <el-descriptions-item label="补贴">
                        −{{ yuan(totals.subsidy) }}
                        <div class="hint">平台 {{ yuan(co.amounts.platform_subsidy) }} · 商家 {{ yuan(co.amounts.merchant_subsidy) }}</div>
                    </el-descriptions-item>
                    <el-descriptions-item label="实付（keel 订单）">
                        <strong>{{ yuan(totals.paid) }}</strong>
                    </el-descriptions-item>
                    <el-descriptions-item label="税（不进 keel 订单）">{{ yuan(totals.tax) }}</el-descriptions-item>
                    <el-descriptions-item label="平台总价">
                        <strong>{{ yuan(totals.platformTotal) }}</strong>
                        <div class="hint">= 实付 + 税</div>
                    </el-descriptions-item>
                    <el-descriptions-item label="平台佣金">{{ yuan(co.amounts.commission) }}</el-descriptions-item>
                    <el-descriptions-item label="商家应收">{{ yuan(co.amounts.merchant_receivable) }}</el-descriptions-item>
                    <el-descriptions-item label="已退">{{ yuan(co.amounts.refunded) }}</el-descriptions-item>
                </el-descriptions>

                <h4>平台申请</h4>
                <el-empty v-if="co.requests.length === 0" description="平台没有发起过申请" :image-size="48" />
                <div v-else class="mobile-list">
                    <div v-for="r in co.requests" :key="r.id" class="mobile-row">
                        <div class="req-top">
                            <span class="mr-title">{{ requestKindLabel(r.kind) }}</span>
                            <el-tag size="small" :type="requestStatusLabel(r.status).type">{{ requestStatusLabel(r.status).text }}</el-tag>
                            <span v-if="r.amount_cents > 0">{{ yuan(r.amount_cents) }}</span>
                        </div>
                        <div class="mr-line">{{ r.reason || "（没写原因）" }}</div>
                        <div class="mr-line hint">
                            收到 {{ datetime(r.created_at) }}
                            <span v-if="r.deadline"> · 平台截止 {{ datetime(r.deadline) }}</span>
                            <span v-if="r.decided_at"> · 处理于 {{ datetime(r.decided_at) }}</span>
                        </div>
                        <div v-if="r.lines.length > 0" class="mr-line hint">
                            涉及：{{ r.lines.map((l) => `${l.external_sku_id} × ${l.qty}`).join("，") }}
                        </div>
                        <div v-if="r.status === 1 && canHandle" class="req-actions">
                            <el-button size="small" type="primary" :disabled="busy" @click="decide(r, true)">同意</el-button>
                            <el-button size="small" :disabled="busy" @click="decide(r, false)">拒绝</el-button>
                        </div>
                    </div>
                </div>
            </template>
        </div>
    </el-drawer>
</template>

<style scoped>
.mb12 {
    margin-bottom: 12px;
}
h4 {
    margin: 16px 0 8px;
}
.ml4 {
    margin-left: 4px;
}
.err {
    color: var(--el-color-danger);
    font-size: 12px;
}
.mobile-list {
    display: flex;
    flex-direction: column;
    gap: 8px;
}
.mobile-row {
    border: 1px solid var(--el-border-color-lighter);
    border-radius: 6px;
    padding: 8px 10px;
}
.mr-title {
    font-weight: 500;
}
.mr-line {
    margin-top: 4px;
    font-size: 13px;
}
.req-top {
    display: flex;
    align-items: center;
    gap: 8px;
    flex-wrap: wrap;
}
.req-actions {
    margin-top: 8px;
    display: flex;
    gap: 8px;
    justify-content: flex-end;
}
</style>
