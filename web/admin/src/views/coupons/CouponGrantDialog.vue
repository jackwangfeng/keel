<script setup lang="ts">
// 按手机号定向发券（POST /admin/coupon-templates/{id}/grants）。
//
// 整批全有或全无：有一个手机号在本店查不到买家，整批 422、一张都不发，
// 服务端在 detail 里列出是哪几个 —— 原样显示，改完再发。
// 带 Idempotency-Key：网络断了重发不会给同一批人再发一遍。

import { computed, ref, watch } from "vue";
import { keel } from "../../api/client.ts";
import type { AdminCouponTemplate, CouponGrantResult } from "../../api/coupons.ts";
import { IdempotentSubmission, withIdempotency } from "../../api/idempotency.ts";
import ProblemAlert from "../../components/ProblemAlert.vue";

const props = defineProps<{ modelValue: boolean; template: AdminCouponTemplate | null }>();
const emit = defineEmits<{ "update:modelValue": [boolean]; granted: [] }>();

const visible = computed({
    get: () => props.modelValue,
    set: (v: boolean) => emit("update:modelValue", v),
});

const text = ref("");
const error = ref<unknown>(null);
const sending = ref(false);
const result = ref<CouponGrantResult | null>(null);
const submission = new IdempotentSubmission();

const phones = computed(() =>
    text.value
        .split(/[\s,，;；、]+/)
        .map((s) => s.trim())
        .filter((s) => s !== ""),
);

const remaining = computed(() => {
    const t = props.template;
    if (t === null || t.total_count === 0) return null;
    return Math.max(0, t.total_count - t.issued_count);
});

watch(
    () => props.modelValue,
    (open) => {
        if (!open) return;
        text.value = "";
        error.value = null;
        result.value = null;
        submission.rotate();
    },
);

// 改了名单就是另一次提交，换钥匙（否则会撞 idempotency-key-reused）。
watch(text, () => submission.rotate());

async function send(): Promise<void> {
    const t = props.template;
    if (t === null || phones.value.length === 0) return;
    sending.value = true;
    error.value = null;
    try {
        result.value = await withIdempotency(submission, (key) =>
            keel.request("post", "/admin/coupon-templates/{template_id}/grants", {
                path: { template_id: t.id },
                body: { phones: phones.value },
                headers: { "Idempotency-Key": key },
            }),
        );
        emit("granted");
    } catch (err) {
        error.value = err;
    } finally {
        sending.value = false;
    }
}
</script>

<template>
    <el-dialog v-model="visible" :title="`定向发券：${template?.name ?? ''}`" width="620px">
        <ProblemAlert v-if="error" :error="error" />
        <template v-if="result === null">
            <p class="hint">
                每行一个手机号（也可用逗号、空格分隔），每个号发一张，同一个号出现两次发两张。
                整批全有或全无：有号码查不到买家时一张都不发。不受「每人限领」约束，受总量约束。
            </p>
            <el-input v-model="text" type="textarea" :rows="8" placeholder="13800000001&#10;13800000002" />
            <p class="hint">
                共 {{ phones.length }} 个号码<span v-if="remaining !== null">，这批券还剩 {{ remaining }} 张</span>
                <span v-if="phones.length > 200" class="danger">（一批至多 200 个）</span>
            </p>
        </template>
        <template v-else>
            <el-alert :title="`已发出 ${result.granted} 张`" type="success" :closable="false" show-icon class="mb8" />
            <el-table :data="result.coupons" border size="small" max-height="320">
                <el-table-column prop="phone" label="手机号" width="160" />
                <el-table-column prop="user_id" label="买家 ID" width="100" />
                <el-table-column prop="coupon_code" label="券码" />
            </el-table>
        </template>
        <template #footer>
            <el-button @click="visible = false">{{ result === null ? "取消" : "关闭" }}</el-button>
            <el-button
                v-if="result === null"
                type="primary"
                :loading="sending"
                :disabled="phones.length === 0 || phones.length > 200 || template?.status === 0"
                @click="send"
            >
                发放
            </el-button>
        </template>
    </el-dialog>
</template>

<style scoped>
.hint {
    color: var(--el-text-color-secondary);
    font-size: 12px;
}
.danger {
    color: var(--el-color-danger);
}
.mb8 {
    margin-bottom: 8px;
}
</style>
