<script setup lang="ts">
// 发货：填承运商与运单号，调 POST /admin/orders/{order_no}/shipments。
//
// 一次提交一把幂等键（IdempotentSubmission）：超时重发用同一把，服务端回放第一次的结果，
// 不会登记出两个包裹；明确被拒（409 状态不对 / 运单号重复、422）之后改了再提交是另一次，
// 换新钥匙。规则写在 api/idempotency.ts 的文件头。
//
// 一期整单发货，请求体里没有明细（契约 ShipmentCreateRequest）。

import { ref, watch } from "vue";
import { keel, type Shipment } from "../../api/client.ts";
import { IdempotentSubmission, withIdempotency } from "../../api/idempotency.ts";
import { buildShipment, CARRIERS } from "../../api/orderRules.ts";
import ProblemAlert from "../../components/ProblemAlert.vue";

const props = defineProps<{ orderNo: string | null }>();
const visible = defineModel<boolean>({ required: true });
const emit = defineEmits<{ shipped: [shipment: Shipment] }>();

const carrier = ref("sf");
const trackingNo = ref("");
const localError = ref("");
const error = ref<unknown>(null);
const submitting = ref(false);
const submission = new IdempotentSubmission();

watch(visible, (open) => {
    if (!open) return;
    carrier.value = "sf";
    trackingNo.value = "";
    localError.value = "";
    error.value = null;
    submission.rotate();
});

async function submit(): Promise<void> {
    const orderNo = props.orderNo;
    if (orderNo === null) return;
    const built = buildShipment(carrier.value, trackingNo.value);
    if ("error" in built) {
        localError.value = built.error;
        return;
    }
    localError.value = "";
    error.value = null;
    submitting.value = true;
    try {
        const shipment = await withIdempotency(submission, (key) =>
            keel.request("post", "/admin/orders/{order_no}/shipments", {
                path: { order_no: orderNo },
                body: built.body,
                headers: { "Idempotency-Key": key },
            }),
        );
        visible.value = false;
        emit("shipped", shipment);
    } catch (err) {
        error.value = err;
    } finally {
        submitting.value = false;
    }
}
</script>

<template>
    <el-dialog v-model="visible" :title="`发货 · ${orderNo ?? ''}`" width="480px" append-to-body>
        <ProblemAlert v-if="error" :error="error" />
        <el-form label-width="84px" @submit.prevent="submit">
            <el-form-item label="承运商" required>
                <el-select v-model="carrier" filterable allow-create default-first-option style="width: 100%">
                    <el-option v-for="c in CARRIERS" :key="c.code" :label="`${c.name}（${c.code}）`" :value="c.code" />
                </el-select>
            </el-form-item>
            <el-form-item label="运单号" required :error="localError">
                <el-input v-model="trackingNo" maxlength="64" placeholder="例如 SF1234567890" data-test="tracking-no" />
            </el-form-item>
            <p class="hint">
                整单发货：一单只发一次，发出后订单变为「已发货」。不扣库存（下单时已经扣过）。
                同一个运单号在本店只能登记一次。
            </p>
        </el-form>
        <template #footer>
            <el-button @click="visible = false">取消</el-button>
            <el-button type="primary" :loading="submitting" @click="submit">确认发货</el-button>
        </template>
    </el-dialog>
</template>
