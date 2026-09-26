<script setup lang="ts">
// 一张退款单的后台详情（GET /admin/refunds/{refund_no}）与它的三个动作：
//
//   10 待审核       → 同意 / 驳回（POST .../audit）
//   20 待买家退货   → 确认收到退货（POST .../receipt）
//
// **每一行的退款金额是服务端算好的**（按优惠分摊倒算，契约 RefundItem.amount_cents），
// 这里只展示，不给任何地方填金额。唯一能填的是退货退款同意时裁定的运费（按元填，
// 留空 = 保持申请时的值），上限由服务端判（订单实收运费减去别的退款单已占的，
// 超了 422 refund-freight-exceeded）。
//
// 三个动作各自一把幂等键：超时重发不会审两次；被拒之后改了再提交换新钥匙。

import { computed, onBeforeUnmount, reactive, ref, watch } from "vue";
import { useRouter } from "vue-router";
import {
    fetchAdminUploadObjectUrl,
    keel,
    type AdminRefundDetail,
    type AdminStore,
    type StaffRef,
} from "../../api/client.ts";
import { IdempotentSubmission, withIdempotency } from "../../api/idempotency.ts";
import {
    ORDER_STATUS,
    REFUND_REASON,
    REFUND_STATUS,
    REFUND_TYPE,
    adminUploadPath,
    buildAudit,
    carrierName,
    refundActions,
} from "../../api/orderRules.ts";
import { can } from "../../auth/permissions.ts";
import { datetime, yuan } from "../../ui/format.ts";
import { notifyOk } from "../../ui/notify.ts";
import ProblemAlert from "../../components/ProblemAlert.vue";

const props = defineProps<{ refundNo: string | null; stores: Map<number, AdminStore> }>();
const emit = defineEmits<{ close: []; changed: [] }>();
const router = useRouter();

const visible = computed({
    get: () => props.refundNo !== null,
    set: (v: boolean) => {
        if (!v) emit("close");
    },
});

const loading = ref(false);
const error = ref<unknown>(null);
const refund = ref<AdminRefundDetail | null>(null);

async function load(): Promise<void> {
    const no = props.refundNo;
    if (no === null) return;
    loading.value = true;
    error.value = null;
    try {
        refund.value = await keel.get("/admin/refunds/{refund_no}", { path: { refund_no: no } });
    } catch (err) {
        refund.value = null;
        error.value = err;
    } finally {
        loading.value = false;
    }
}

watch(
    () => props.refundNo,
    () => {
        refund.value = null;
        void load();
    },
    { immediate: true },
);

// ------------------------------------------------------------------ 凭证图
//
// 凭证是买家的隐私（契约：仅上传者本人与后台客服可读），`<img>` 直接用 evidence_urls 会 403。
// 逐张经 GET /admin/uploads/{id} 取成 object URL（api/client.ts 的 fetchAdminUploadObjectUrl）；
// 换单或关掉抽屉时全部 revoke。老数据里形状不对的地址原样显示。
const evidenceSrc = ref<string[]>([]);
let evidenceObjectUrls: string[] = [];

function releaseEvidence(): void {
    for (const u of evidenceObjectUrls) URL.revokeObjectURL(u);
    evidenceObjectUrls = [];
    evidenceSrc.value = [];
}

watch(refund, async (r) => {
    releaseEvidence();
    const urls = r?.evidence_urls ?? [];
    const loaded = await Promise.all(
        urls.map(async (u) => {
            const path = adminUploadPath(u);
            if (path === null) return u;
            try {
                const obj = await fetchAdminUploadObjectUrl(path);
                evidenceObjectUrls.push(obj);
                return obj;
            } catch {
                return ""; // 读不到（403 / 404）：el-image 显示加载失败的占位
            }
        }),
    );
    if (refund.value === r) evidenceSrc.value = loaded;
});
onBeforeUnmount(releaseEvidence);

const canOperate = computed(() => {
    const r = refund.value;
    if (r === null) return false;
    const st = props.stores.get(r.store_id);
    return can.handleOrder({ id: r.store_id, region_id: st?.region_id ?? r.order.region_id ?? 0 });
});
const actions = computed(() => (refund.value === null ? null : refundActions(refund.value, canOperate.value)));

function who(s: StaffRef | undefined): string {
    if (s === undefined) return "";
    return s.name !== undefined ? `${s.name}（#${s.id}）` : `员工 #${s.id}（平台级或已不在本店）`;
}

function openOrder(orderNo: string): void {
    void router.push({ name: "orders", query: { order_no: orderNo } });
}

// ------------------------------------------------------------------ 审核

const audit = reactive({
    visible: false,
    action: "approve" as "approve" | "reject",
    rejectReason: "",
    freightYuan: "",
    localError: "",
    error: null as unknown,
    submitting: false,
});
const auditSubmission = new IdempotentSubmission();

function openAudit(action: "approve" | "reject"): void {
    audit.action = action;
    audit.rejectReason = "";
    audit.freightYuan = "";
    audit.localError = "";
    audit.error = null;
    audit.visible = true;
    auditSubmission.rotate();
}

async function submitAudit(): Promise<void> {
    const r = refund.value;
    if (r === null) return;
    const built = buildAudit(audit.action, r.refund_type, {
        rejectReason: audit.rejectReason,
        freightYuan: audit.freightYuan,
    });
    if ("error" in built) {
        audit.localError = built.error;
        return;
    }
    audit.localError = "";
    audit.error = null;
    audit.submitting = true;
    try {
        await withIdempotency(auditSubmission, (key) =>
            keel.request("post", "/admin/refunds/{refund_no}/audit", {
                path: { refund_no: r.refund_no },
                body: built.body,
                headers: { "Idempotency-Key": key },
            }),
        );
        audit.visible = false;
        notifyOk(audit.action === "approve" ? "已同意" : "已驳回");
        await load();
        emit("changed");
    } catch (err) {
        audit.error = err;
    } finally {
        audit.submitting = false;
    }
}

// ------------------------------------------------------------------ 确认收到退货

const receive = reactive({ visible: false, error: null as unknown, submitting: false });
const receiveSubmission = new IdempotentSubmission();

function openReceive(): void {
    receive.error = null;
    receive.visible = true;
    receiveSubmission.rotate();
}

async function submitReceive(): Promise<void> {
    const r = refund.value;
    if (r === null) return;
    receive.submitting = true;
    receive.error = null;
    try {
        await withIdempotency(receiveSubmission, (key) =>
            keel.request("post", "/admin/refunds/{refund_no}/receipt", {
                path: { refund_no: r.refund_no },
                headers: { "Idempotency-Key": key },
            }),
        );
        receive.visible = false;
        notifyOk("已确认收到退货");
        await load();
        emit("changed");
    } catch (err) {
        receive.error = err;
    } finally {
        receive.submitting = false;
    }
}
</script>

<template>
    <el-drawer v-model="visible" :title="`退款单 ${refundNo ?? ''}`" size="680px" destroy-on-close>
        <ProblemAlert v-if="error" :error="error" />
        <div v-loading="loading">
            <template v-if="refund && actions">
                <div class="page-toolbar">
                    <el-tag :type="REFUND_STATUS[refund.status].tag">{{ REFUND_STATUS[refund.status].text }}</el-tag>
                    <el-tag type="info">{{ REFUND_TYPE[refund.refund_type] }}</el-tag>
                    <span class="grow" />
                    <el-button
                        v-if="actions.approve.visible"
                        type="primary"
                        :disabled="!actions.approve.enabled"
                        :title="actions.approve.hint"
                        data-test="approve"
                        @click="openAudit('approve')"
                    >
                        同意
                    </el-button>
                    <el-button
                        v-if="actions.reject.visible"
                        type="danger"
                        plain
                        :disabled="!actions.reject.enabled"
                        :title="actions.reject.hint"
                        data-test="reject"
                        @click="openAudit('reject')"
                    >
                        驳回
                    </el-button>
                    <el-button
                        v-if="actions.receive.visible"
                        type="primary"
                        :disabled="!actions.receive.enabled"
                        :title="actions.receive.hint"
                        data-test="receive"
                        @click="openReceive"
                    >
                        确认收到退货
                    </el-button>
                </div>

                <el-descriptions :column="2" border size="small" class="mb12">
                    <el-descriptions-item label="订单">
                        <el-link type="primary" @click="openOrder(refund.order_no)">{{ refund.order_no }}</el-link>
                        <el-tag size="small" class="ml4" :type="ORDER_STATUS[refund.order_status].tag">
                            {{ ORDER_STATUS[refund.order_status].text }}
                        </el-tag>
                    </el-descriptions-item>
                    <el-descriptions-item label="门店">
                        {{ refund.store.store_name || `门店 #${refund.store_id}` }}
                    </el-descriptions-item>
                    <el-descriptions-item label="原因">
                        {{ refund.reason_code ? REFUND_REASON[refund.reason_code] : "—" }}
                    </el-descriptions-item>
                    <el-descriptions-item label="退款渠道">{{ refund.channel ?? "—" }}</el-descriptions-item>
                    <el-descriptions-item label="买家说明" :span="2">{{ refund.reason_text || "—" }}</el-descriptions-item>
                </el-descriptions>

                <h4>退哪些（每一行的金额由服务端按优惠分摊算好）</h4>
                <el-table :data="refund.items" size="small" class="mb12">
                    <el-table-column prop="title" label="商品" min-width="200" />
                    <el-table-column prop="quantity" label="件数" width="70" />
                    <el-table-column label="实退" width="110">
                        <template #default="{ row }">{{ yuan(row.amount_cents) }}</template>
                    </el-table-column>
                </el-table>

                <el-descriptions :column="3" border size="small" class="mb12">
                    <el-descriptions-item label="货款">{{ yuan(refund.goods_amount_cents ?? 0) }}</el-descriptions-item>
                    <el-descriptions-item label="运费">{{ yuan(refund.freight_cents ?? 0) }}</el-descriptions-item>
                    <el-descriptions-item label="实退合计">
                        <strong>{{ yuan(refund.amount_cents) }}</strong>
                    </el-descriptions-item>
                    <el-descriptions-item label="订单实付">{{ yuan(refund.order.paid_cents ?? 0) }}</el-descriptions-item>
                    <el-descriptions-item label="订单运费">{{ yuan(refund.order.freight_cents ?? 0) }}</el-descriptions-item>
                    <el-descriptions-item label="订单已退">{{ yuan(refund.order.refunded_cents ?? 0) }}</el-descriptions-item>
                </el-descriptions>

                <template v-if="refund.refund_type === 2">
                    <h4>寄回物流</h4>
                    <el-descriptions
                        v-if="refund.return_shipment"
                        :column="3"
                        border
                        size="small"
                        class="mb12"
                        data-test="return-shipment"
                    >
                        <el-descriptions-item label="承运商">
                            {{ carrierName(refund.return_shipment.carrier_code) }}
                        </el-descriptions-item>
                        <el-descriptions-item label="运单号">{{ refund.return_shipment.tracking_no }}</el-descriptions-item>
                        <el-descriptions-item label="买家填写于">
                            {{ datetime(refund.return_shipment.submitted_at) }}
                        </el-descriptions-item>
                    </el-descriptions>
                    <p v-else class="hint mb12" data-test="return-shipment-missing">
                        {{
                            refund.status === 20
                                ? "买家还没有填写寄回物流。没填也可以在收到货后确认收到退货。"
                                : "买家没有填写寄回物流。"
                        }}
                    </p>
                </template>

                <template v-if="(refund.evidence_urls ?? []).length > 0">
                    <h4>凭证</h4>
                    <div class="evidence mb12">
                        <el-image
                            v-for="(u, i) in evidenceSrc"
                            :key="i"
                            :src="u"
                            :preview-src-list="evidenceSrc"
                            :initial-index="i"
                            fit="cover"
                            class="evidence-img"
                        />
                    </div>
                </template>

                <h4>审核记录</h4>
                <el-timeline>
                    <el-timeline-item :timestamp="datetime(refund.created_at)">买家申请</el-timeline-item>
                    <el-timeline-item
                        v-if="refund.audited_at"
                        :timestamp="datetime(refund.audited_at)"
                        :type="refund.status === 50 ? 'danger' : 'primary'"
                    >
                        {{ refund.status === 50 ? "驳回" : "同意" }}
                        <span v-if="refund.audited_by" class="hint">· {{ who(refund.audited_by) }}</span>
                        <div v-if="refund.reject_reason" class="hint">理由：{{ refund.reject_reason }}</div>
                        <div v-if="refund.refund_type === 2 && refund.status !== 50" class="hint">
                            裁定退运费：{{ yuan(refund.freight_cents ?? 0) }}
                        </div>
                    </el-timeline-item>
                    <el-timeline-item
                        v-if="refund.return_shipment"
                        :timestamp="datetime(refund.return_shipment.submitted_at)"
                    >
                        买家填写寄回物流
                        <span class="hint">
                            · {{ carrierName(refund.return_shipment.carrier_code) }} {{ refund.return_shipment.tracking_no }}
                        </span>
                    </el-timeline-item>
                    <el-timeline-item v-if="refund.received_at" :timestamp="datetime(refund.received_at)" type="primary">
                        确认收到退货
                        <span v-if="refund.received_by" class="hint">· {{ who(refund.received_by) }}</span>
                    </el-timeline-item>
                    <el-timeline-item v-if="refund.refunded_at" :timestamp="datetime(refund.refunded_at)" type="success">
                        已到账
                        <span v-if="refund.channel_refund_id" class="hint">· 渠道流水 {{ refund.channel_refund_id }}</span>
                    </el-timeline-item>
                    <el-timeline-item v-if="refund.status === 60" type="info">买家撤回</el-timeline-item>
                    <el-timeline-item v-if="refund.status === 30" type="warning">
                        退款中：等渠道回调到账（30 没有失败态，渠道失败是重试）
                    </el-timeline-item>
                </el-timeline>
            </template>
        </div>

        <el-dialog
            v-model="audit.visible"
            :title="audit.action === 'approve' ? '同意退款' : '驳回退款'"
            width="460px"
            append-to-body
        >
            <ProblemAlert v-if="audit.error" :error="audit.error" />
            <el-form label-width="96px" @submit.prevent="submitAudit">
                <template v-if="audit.action === 'reject'">
                    <el-form-item label="驳回理由" required :error="audit.localError">
                        <el-input
                            v-model="audit.rejectReason"
                            type="textarea"
                            :rows="3"
                            maxlength="200"
                            show-word-limit
                            placeholder="买家会看到这段话，写清楚为什么、改什么可以重新申请"
                            data-test="reject-reason"
                        />
                    </el-form-item>
                </template>
                <template v-else>
                    <p class="hint">
                        实退 {{ yuan(refund?.goods_amount_cents ?? 0) }} 货款（服务端已按优惠分摊算好）。
                        <template v-if="refund?.refund_type === 2">
                            退货退款：同意后进入「待买家退货」，收到货再点「确认收到退货」才退钱。
                        </template>
                        <template v-else>仅退款：同意后直接进入退款。</template>
                    </p>
                    <el-form-item v-if="actions?.freightEditable" label="退运费（元）" :error="audit.localError">
                        <el-input
                            v-model="audit.freightYuan"
                            :placeholder="`留空保持 ${yuan(refund?.freight_cents ?? 0)}；订单运费 ${yuan(refund?.order.freight_cents ?? 0)}`"
                            data-test="freight"
                        />
                    </el-form-item>
                </template>
            </el-form>
            <template #footer>
                <el-button @click="audit.visible = false">取消</el-button>
                <el-button
                    :type="audit.action === 'approve' ? 'primary' : 'danger'"
                    :loading="audit.submitting"
                    @click="submitAudit"
                >
                    {{ audit.action === "approve" ? "确认同意" : "确认驳回" }}
                </el-button>
            </template>
        </el-dialog>

        <el-dialog v-model="receive.visible" title="确认收到退货" width="420px" append-to-body>
            <ProblemAlert v-if="receive.error" :error="receive.error" />
            <p>确认已经收到买家寄回的货？确认之后进入退款，钱会原路退回。</p>
            <p class="hint">不会自动加回库存：退回来的货成色未知，验货后请到门店库存里手工调整。</p>
            <template #footer>
                <el-button @click="receive.visible = false">取消</el-button>
                <el-button type="primary" :loading="receive.submitting" @click="submitReceive">确认收到</el-button>
            </template>
        </el-dialog>
    </el-drawer>
</template>

<style scoped>
.mb12 {
    margin-bottom: 12px;
}
.ml4 {
    margin-left: 4px;
}
h4 {
    margin: 16px 0 8px;
}
.evidence {
    display: flex;
    gap: 8px;
    flex-wrap: wrap;
}
.evidence-img {
    width: 88px;
    height: 88px;
    border-radius: 4px;
}
</style>
