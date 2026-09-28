<script setup lang="ts">
// 一笔订单的后台详情（GET /admin/orders/{order_no}）：收货人与门店的下单时快照、
// 金额（含每一行的优惠分摊）、支付、发货包裹、这一单的全部退款单。
//
// 这里能做的写操作只有「发货」（20 已支付）。退款单的审核与确认收货在「售后」页，
// 这里只列出来、点单号跳过去 —— 同一个动作只在一个地方做，免得两处的按钮规则分叉。

import { computed, ref, watch } from "vue";
import { useRouter } from "vue-router";
import { keel, type AdminOrderDetail, type AdminStore } from "../../api/client.ts";
import { localDistanceText } from "../../api/localDeliveryRules.ts";
import {
    ORDER_REFUND_STATUS,
    ORDER_STATUS,
    REFUND_STATUS,
    REFUND_TYPE,
    carrierName,
    refundableQty,
    shipAction,
} from "../../api/orderRules.ts";
import { can } from "../../auth/permissions.ts";
import { datetime, yuan } from "../../ui/format.ts";
import { notifyOk } from "../../ui/notify.ts";
import ProblemAlert from "../../components/ProblemAlert.vue";
import ShipDialog from "./ShipDialog.vue";

const props = defineProps<{ orderNo: string | null; stores: Map<number, AdminStore> }>();
const emit = defineEmits<{ close: []; changed: [] }>();
const router = useRouter();

const visible = computed({
    get: () => props.orderNo !== null,
    set: (v: boolean) => {
        if (!v) emit("close");
    },
});

const loading = ref(false);
const error = ref<unknown>(null);
const order = ref<AdminOrderDetail | null>(null);

async function load(): Promise<void> {
    const no = props.orderNo;
    if (no === null) return;
    loading.value = true;
    error.value = null;
    try {
        order.value = await keel.get("/admin/orders/{order_no}", { path: { order_no: no } });
    } catch (err) {
        order.value = null;
        error.value = err;
    } finally {
        loading.value = false;
    }
}

watch(
    () => props.orderNo,
    () => {
        order.value = null;
        void load();
    },
    { immediate: true },
);

/** 门店此刻的大区从门店列表取（判权口径与服务端一致），拿不到时退回订单上的 region_id。 */
const canOperate = computed(() => {
    const o = order.value;
    if (o === null) return false;
    const st = props.stores.get(o.store_id);
    return can.handleOrder({ id: o.store_id, region_id: st?.region_id ?? o.region_id ?? 0 });
});
const ship = computed(() => (order.value === null ? null : shipAction(order.value, canOperate.value)));

const shipVisible = ref(false);
async function onShipped(): Promise<void> {
    notifyOk("已发货");
    await load();
    emit("changed");
}

function specText(spec: Record<string, string> | undefined): string {
    return spec === undefined ? "" : Object.values(spec).join(" / ");
}

function openRefund(refundNo: string): void {
    void router.push({ name: "refunds", query: { refund_no: refundNo } });
}
</script>

<template>
    <el-drawer v-model="visible" :title="`订单 ${orderNo ?? ''}`" size="720px" destroy-on-close>
        <ProblemAlert v-if="error" :error="error" />
        <div v-loading="loading">
            <template v-if="order">
                <div class="page-toolbar">
                    <el-tag :type="ORDER_STATUS[order.status].tag">{{ ORDER_STATUS[order.status].text }}</el-tag>
                    <el-tag v-if="order.refund_status !== 0" :type="ORDER_REFUND_STATUS[order.refund_status].tag">
                        {{ ORDER_REFUND_STATUS[order.refund_status].text }}
                    </el-tag>
                    <span class="grow" />
                    <el-button
                        v-if="ship?.visible"
                        type="primary"
                        :disabled="!ship.enabled"
                        :title="ship.hint"
                        data-test="ship"
                        @click="shipVisible = true"
                    >
                        发货
                    </el-button>
                </div>
                <el-alert
                    v-if="order.status === 50"
                    type="warning"
                    :closable="false"
                    show-icon
                    class="mb12"
                    title="买家申请了整单退款，处理完退款单之前不能发货（驳回后订单回到已支付）。"
                />
                <el-alert
                    v-else-if="ship?.visible && ship.enabled && ship.hint"
                    type="info"
                    :closable="false"
                    show-icon
                    class="mb12"
                    :title="ship.hint"
                />

                <el-descriptions :column="2" border size="small" class="mb12">
                    <el-descriptions-item label="下单时间">{{ datetime(order.created_at) }}</el-descriptions-item>
                    <el-descriptions-item label="支付时间">{{ datetime(order.paid_at) }}</el-descriptions-item>
                    <el-descriptions-item label="发货时间">{{ datetime(order.shipped_at) }}</el-descriptions-item>
                    <el-descriptions-item label="完成时间">{{ datetime(order.finished_at) }}</el-descriptions-item>
                    <el-descriptions-item label="履约门店">
                        {{ order.store.store_name || `门店 #${order.store_id}` }}
                        <span v-if="order.store.region_name" class="hint">（{{ order.store.region_name }}）</span>
                    </el-descriptions-item>
                    <el-descriptions-item label="用券">{{ order.coupon_name ?? "—" }}</el-descriptions-item>
                    <el-descriptions-item label="收货人" :span="2">
                        {{ order.receiver.receiver_name }} · {{ order.receiver.phone }}
                        <div class="hint">
                            {{ order.receiver.province }}{{ order.receiver.city }}{{ order.receiver.district }}{{
                                order.receiver.street ?? ""
                            }}{{ order.receiver.detail }}
                        </div>
                    </el-descriptions-item>
                </el-descriptions>

                <h4>商品</h4>
                <el-table :data="order.items" size="small" class="mb12">
                    <el-table-column label="商品" min-width="180">
                        <template #default="{ row }">
                            {{ row.title }}
                            <div class="hint">{{ specText(row.spec_values) }}</div>
                        </template>
                    </el-table-column>
                    <el-table-column label="单价" width="90">
                        <template #default="{ row }">{{ yuan(row.price_cents) }}</template>
                    </el-table-column>
                    <el-table-column prop="quantity" label="数量" width="60" />
                    <el-table-column label="行金额" width="90">
                        <template #default="{ row }">{{ yuan(row.amount_cents ?? 0) }}</template>
                    </el-table-column>
                    <el-table-column label="分摊优惠" width="90">
                        <template #default="{ row }">{{ yuan(row.discount_cents ?? 0) }}</template>
                    </el-table-column>
                    <el-table-column label="已退 / 在途 / 可退" width="130">
                        <template #default="{ row }">
                            {{ row.refunded_qty ?? 0 }} / {{ row.refunding_qty ?? 0 }} / {{ refundableQty(row) }}
                        </template>
                    </el-table-column>
                </el-table>

                <el-descriptions :column="3" border size="small" class="mb12">
                    <el-descriptions-item label="商品金额">{{ yuan(order.goods_amount_cents ?? 0) }}</el-descriptions-item>
                    <el-descriptions-item label="运费">{{ yuan(order.freight_cents ?? 0) }}</el-descriptions-item>
                    <el-descriptions-item label="优惠">-{{ yuan(order.discount_cents ?? 0) }}</el-descriptions-item>
                    <el-descriptions-item label="应付">{{ yuan(order.payable_cents) }}</el-descriptions-item>
                    <el-descriptions-item label="实付">{{ yuan(order.paid_cents ?? 0) }}</el-descriptions-item>
                    <el-descriptions-item label="已退">{{ yuan(order.refunded_cents ?? 0) }}</el-descriptions-item>
                </el-descriptions>

                <p v-if="order.freight?.mode === 'local' && order.freight.local" class="hint mb12">
                    同城配送：距离 {{ localDistanceText(order.freight.local.distance_m) }}，配送费 {{ yuan(order.freight.local.tier_fee_cents) }}
                    <template v-if="order.freight.local.free_reason === 'threshold'">；满 {{ yuan(order.freight.local.free_over_cents) }} 免配送费</template>
                </p>

                <h4>支付</h4>
                <el-table :data="order.payments" size="small" class="mb12" empty-text="没有支付记录">
                    <el-table-column prop="payment_no" label="支付单号" min-width="200" />
                    <el-table-column prop="channel" label="渠道" width="90" />
                    <el-table-column label="金额" width="90">
                        <template #default="{ row }">{{ yuan(row.amount_cents ?? 0) }}</template>
                    </el-table-column>
                    <el-table-column label="状态" width="80">
                        <template #default="{ row }">{{ ["待支付", "成功", "失败", "已关闭"][row.status ?? 0] }}</template>
                    </el-table-column>
                    <el-table-column label="支付时间" width="160">
                        <template #default="{ row }">{{ datetime(row.paid_at) }}</template>
                    </el-table-column>
                </el-table>

                <h4>发货</h4>
                <el-table :data="order.shipments" size="small" class="mb12" empty-text="还没发货">
                    <el-table-column label="承运商" width="120">
                        <template #default="{ row }">{{ carrierName(row.carrier_code) }}</template>
                    </el-table-column>
                    <el-table-column prop="tracking_no" label="运单号" min-width="180" />
                    <el-table-column label="发出时间" width="160">
                        <template #default="{ row }">{{ datetime(row.shipped_at) }}</template>
                    </el-table-column>
                </el-table>

                <h4>售后</h4>
                <el-table :data="order.refunds" size="small" empty-text="没有退款单">
                    <el-table-column label="退款单号" min-width="200">
                        <template #default="{ row }">
                            <el-link type="primary" @click="openRefund(row.refund_no)">{{ row.refund_no }}</el-link>
                        </template>
                    </el-table-column>
                    <el-table-column label="类型" width="90">
                        <template #default="{ row }">{{ REFUND_TYPE[row.refund_type as keyof typeof REFUND_TYPE] }}</template>
                    </el-table-column>
                    <el-table-column label="状态" width="110">
                        <template #default="{ row }">
                            <el-tag size="small" :type="REFUND_STATUS[row.status as keyof typeof REFUND_STATUS].tag">
                                {{ REFUND_STATUS[row.status as keyof typeof REFUND_STATUS].text }}
                            </el-tag>
                        </template>
                    </el-table-column>
                    <el-table-column label="实退" width="90">
                        <template #default="{ row }">{{ yuan(row.amount_cents) }}</template>
                    </el-table-column>
                </el-table>
            </template>
        </div>
        <ShipDialog v-model="shipVisible" :order-no="orderNo" @shipped="onShipped" />
    </el-drawer>
</template>

<style scoped>
.mb12 {
    margin-bottom: 12px;
}
h4 {
    margin: 16px 0 8px;
}
</style>
