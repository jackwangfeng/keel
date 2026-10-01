<script setup lang="ts">
// 新建 / 编辑券模板。按券型显示不同字段（数据模型 §7 的 chk_coupon_rule：
// 每种券型只有一种写法），金额按「元」输入、按字符串换算成整数分提交（api/money.ts）。
//
// 已发出过券（locked）的模板，券面字段与有效期只读：券实例不快照规则，
// 改模板等于悄悄改掉买家手里的券（服务端同样会 409 coupon-template-locked）。

import { computed, ref, watch } from "vue";
import { keel } from "../../api/client.ts";
import {
    FREE_SHIPPING_HINT,
    type AdminCouponTemplate,
    type CouponTemplateCreateRequest,
    type CouponTemplatePatchRequest,
} from "../../api/coupons.ts";
import { IdempotentSubmission, withIdempotency } from "../../api/idempotency.ts";
import { centsToYuanInput, rateToZheInput, yuanToCents, zheToRate } from "../../api/money.ts";
import ProblemAlert from "../../components/ProblemAlert.vue";
import { rangeDefaultTime } from "../../ui/format.ts";

const props = defineProps<{ modelValue: boolean; template: AdminCouponTemplate | null }>();
const emit = defineEmits<{ "update:modelValue": [boolean]; saved: [AdminCouponTemplate] }>();

const visible = computed({
    get: () => props.modelValue,
    set: (v: boolean) => emit("update:modelValue", v),
});
const editing = computed(() => props.template !== null);
const locked = computed(() => props.template?.locked === true);

interface Form {
    name: string;
    couponType: 1 | 2 | 3 | 4;
    threshold: string;
    discount: string;
    zhe: string;
    maxDiscount: string;
    validMode: 1 | 2;
    range: [Date, Date] | null;
    validDays: number;
    totalCount: number;
    perUserLimit: number;
    claimable: boolean;
}

function emptyForm(): Form {
    return {
        name: "",
        couponType: 1,
        threshold: "",
        discount: "",
        zhe: "",
        maxDiscount: "",
        validMode: 1,
        range: null,
        validDays: 7,
        totalCount: 0,
        perUserLimit: 1,
        claimable: false,
    };
}

function formOf(t: AdminCouponTemplate): Form {
    return {
        name: t.name,
        couponType: t.coupon_type,
        threshold: centsToYuanInput(t.threshold_cents),
        discount: centsToYuanInput(t.discount_cents),
        zhe: rateToZheInput(t.discount_rate),
        maxDiscount: t.max_discount_cents > 0 ? centsToYuanInput(t.max_discount_cents) : "",
        validMode: t.valid_mode,
        range:
            t.valid_start_at !== undefined && t.valid_end_at !== undefined
                ? [new Date(t.valid_start_at), new Date(t.valid_end_at)]
                : null,
        validDays: t.valid_days > 0 ? t.valid_days : 7,
        totalCount: t.total_count,
        perUserLimit: t.per_user_limit,
        claimable: t.claimable,
    };
}

const form = ref<Form>(emptyForm());
const localError = ref("");
const error = ref<unknown>(null);
const saving = ref(false);
const submission = new IdempotentSubmission();

watch(
    () => props.modelValue,
    (open) => {
        if (!open) return;
        form.value = props.template === null ? emptyForm() : formOf(props.template);
        localError.value = "";
        error.value = null;
        submission.rotate();
    },
);

/** 券面与有效期那一半（名称、总量、限领、可领取另外拼）。 */
type FaceBody = Omit<CouponTemplateCreateRequest, "name" | "valid_mode" | "claimable" | "per_user_limit" | "total_count">;

/** 把表单换算成券面字段。金额一律字符串解析，任何一项不合法都返回错误文案。 */
function faceFields(): { ok: true; body: FaceBody } | { ok: false; msg: string } {
    const f = form.value;
    const money = (label: string, s: string, required: boolean): number | string => {
        if (s.trim() === "") return required ? `请填${label}` : 0;
        const v = yuanToCents(s);
        return v === null ? `${label}「${s}」不是合法金额（元，至多两位小数）` : v;
    };
    const body: FaceBody = { coupon_type: f.couponType };
    if (f.couponType === 1) {
        const th = money("门槛", f.threshold, true);
        const d = money("减免", f.discount, true);
        if (typeof th === "string") return { ok: false, msg: th };
        if (typeof d === "string") return { ok: false, msg: d };
        if (d <= 0) return { ok: false, msg: "减免必须大于 0" };
        if (th < d) return { ok: false, msg: `满 ${f.threshold} 减 ${f.discount}：门槛小于减免是配置错误` };
        body.threshold_cents = th;
        body.discount_cents = d;
    } else if (f.couponType === 2) {
        const rate = zheToRate(f.zhe);
        if (rate === null) return { ok: false, msg: "折扣填 0.1 到 9.99 之间的「折」数，如 8.5 表示 8.5 折" };
        const th = money("门槛", f.threshold, false);
        const max = money("封顶", f.maxDiscount, false);
        if (typeof th === "string") return { ok: false, msg: th };
        if (typeof max === "string") return { ok: false, msg: max };
        body.discount_rate = rate;
        body.threshold_cents = th;
        body.max_discount_cents = max;
    } else if (f.couponType === 3) {
        const d = money("减免", f.discount, true);
        if (typeof d === "string") return { ok: false, msg: d };
        if (d <= 0) return { ok: false, msg: "减免必须大于 0" };
        body.discount_cents = d;
    } else {
        // 包邮券（00056）：门槛可选，「封顶」是最多抵多少运费，留空即全免。
        // 不带减免额与折扣率（chk_coupon_rule 那一支）。
        const th = money("门槛", f.threshold, false);
        const max = money("封顶", f.maxDiscount, false);
        if (typeof th === "string") return { ok: false, msg: th };
        if (typeof max === "string") return { ok: false, msg: max };
        body.threshold_cents = th;
        body.max_discount_cents = max;
    }
    if (f.validMode === 1) {
        if (f.range === null) return { ok: false, msg: "请选择有效期起止时间" };
        body.valid_start_at = f.range[0].toISOString();
        body.valid_end_at = f.range[1].toISOString();
    } else {
        body.valid_days = f.validDays;
    }
    return { ok: true, body };
}

async function submit(): Promise<void> {
    localError.value = "";
    error.value = null;
    const f = form.value;
    if (f.name.trim() === "") {
        localError.value = "请填券名";
        return;
    }
    saving.value = true;
    try {
        let saved: AdminCouponTemplate;
        const common = {
            name: f.name.trim(),
            total_count: f.totalCount,
            per_user_limit: f.perUserLimit,
            claimable: f.claimable,
        };
        if (props.template === null) {
            const face = faceFields();
            if (!face.ok) {
                localError.value = face.msg;
                return;
            }
            const body: CouponTemplateCreateRequest = { ...face.body, ...common, valid_mode: f.validMode };
            saved = await withIdempotency(submission, (key) =>
                keel.request("post", "/admin/coupon-templates", { body, headers: { "Idempotency-Key": key } }),
            );
        } else {
            let body: CouponTemplatePatchRequest = { ...common };
            if (!locked.value) {
                const face = faceFields();
                if (!face.ok) {
                    localError.value = face.msg;
                    return;
                }
                // 换券型时，别的券型的字段显式归零：PATCH 只改传了的字段，
                // 不归零的话旧券型的值会留在行里、撞上 chk_coupon_rule。
                body = {
                    threshold_cents: 0,
                    discount_cents: 0,
                    discount_rate: 0,
                    max_discount_cents: 0,
                    ...face.body,
                    ...body,
                    valid_mode: f.validMode,
                };
            }
            saved = await keel.request("patch", "/admin/coupon-templates/{template_id}", {
                path: { template_id: props.template.id },
                body,
            });
        }
        emit("saved", saved);
        visible.value = false;
    } catch (err) {
        error.value = err;
    } finally {
        saving.value = false;
    }
}
</script>

<template>
    <el-dialog v-model="visible" :title="editing ? `编辑券模板 #${template?.id}` : '新建券模板'" width="620px">
        <ProblemAlert v-if="error" :error="error" />
        <el-alert v-if="localError" :title="localError" type="error" :closable="false" show-icon class="mb8" />
        <el-alert
            v-if="locked"
            type="info"
            :closable="false"
            show-icon
            class="mb8"
            title="这批券已经发出过，券面（券型、门槛、减免、折扣、封顶）与有效期不能再改"
            description="券实例不快照规则，改模板等于悄悄改掉买家手里的券。要换面值请新建一批。能改的是名称、总量、每人限领、可领取。"
        />
        <el-form label-width="96px" @submit.prevent>
            <el-form-item label="名称" required><el-input v-model="form.name" maxlength="60" show-word-limit /></el-form-item>
            <el-form-item label="券型" required>
                <el-radio-group v-model="form.couponType" :disabled="locked">
                    <el-radio :value="1">满减</el-radio>
                    <el-radio :value="2">折扣</el-radio>
                    <el-radio :value="3">立减</el-radio>
                    <el-radio :value="4">包邮</el-radio>
                </el-radio-group>
            </el-form-item>

            <template v-if="form.couponType === 1">
                <el-form-item label="满（元）" required>
                    <el-input v-model="form.threshold" :disabled="locked" placeholder="如 100" />
                </el-form-item>
                <el-form-item label="减（元）" required>
                    <el-input v-model="form.discount" :disabled="locked" placeholder="如 20，不能大于门槛" />
                </el-form-item>
            </template>
            <template v-else-if="form.couponType === 2">
                <el-form-item label="折扣" required>
                    <el-input v-model="form.zhe" :disabled="locked" placeholder="如 8.5（表示 8.5 折）">
                        <template #append>折</template>
                    </el-input>
                </el-form-item>
                <el-form-item label="门槛（元）">
                    <el-input v-model="form.threshold" :disabled="locked" placeholder="留空即无门槛" />
                </el-form-item>
                <el-form-item label="封顶（元）">
                    <el-input v-model="form.maxDiscount" :disabled="locked" placeholder="留空即不封顶" />
                </el-form-item>
                <p class="hint">折扣减免向下取整到分（对商家有利，每单至多少减不到 1 分）。</p>
            </template>
            <template v-else-if="form.couponType === 3">
                <el-form-item label="减（元）" required>
                    <el-input v-model="form.discount" :disabled="locked" placeholder="无门槛立减" />
                </el-form-item>
            </template>
            <template v-else>
                <el-form-item label="门槛（元）">
                    <el-input v-model="form.threshold" :disabled="locked" placeholder="留空即无门槛（比适用商品小计）" />
                </el-form-item>
                <el-form-item label="封顶（元）">
                    <el-input v-model="form.maxDiscount" :disabled="locked" placeholder="最多抵多少运费，留空即运费全免" />
                </el-form-item>
                <p class="hint">{{ FREE_SHIPPING_HINT }}</p>
            </template>

            <el-form-item label="有效期" required>
                <el-radio-group v-model="form.validMode" :disabled="locked">
                    <el-radio :value="1">固定时间段</el-radio>
                    <el-radio :value="2">领取后 N 天</el-radio>
                </el-radio-group>
            </el-form-item>
            <el-form-item v-if="form.validMode === 1" label="起止">
                <el-date-picker
                    v-model="form.range"
                    type="datetimerange"
                    :default-time="rangeDefaultTime"
                    :disabled="locked"
                    start-placeholder="开始"
                    end-placeholder="结束"
                />
            </el-form-item>
            <el-form-item v-else label="天数">
                <el-input-number v-model="form.validDays" :min="1" :max="3650" :step="1" :precision="0" step-strictly :disabled="locked" />
            </el-form-item>

            <el-form-item label="总量">
                <el-input-number v-model="form.totalCount" :min="0" :step="1" :precision="0" step-strictly />
                <span class="hint ml8">0 表示不限；不能低于已发出数</span>
            </el-form-item>
            <el-form-item label="每人限领">
                <el-input-number v-model="form.perUserLimit" :min="1" :step="1" :precision="0" step-strictly />
                <span class="hint ml8">只约束领券中心；定向发放不受它限制</span>
            </el-form-item>
            <el-form-item label="可领取">
                <el-switch v-model="form.claimable" />
                <span class="hint ml8">打开后买家能在领券中心自己领（建议先配好适用范围）</span>
            </el-form-item>
        </el-form>
        <template #footer>
            <el-button @click="visible = false">取消</el-button>
            <el-button type="primary" :loading="saving" @click="submit">{{ editing ? "保存" : "创建" }}</el-button>
        </template>
    </el-dialog>
</template>

<style scoped>
.mb8 {
    margin-bottom: 8px;
}
.ml8 {
    margin-left: 8px;
}
.hint {
    color: var(--el-text-color-secondary);
    font-size: 12px;
}
</style>
