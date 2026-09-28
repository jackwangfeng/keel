<script setup lang="ts">
// 新建 / 编辑同城配送模板（整体替换，契约 PUT /admin/local-delivery-templates/{id}）。
//
// 名称 + 是否默认 + 一份规则（起送价 / 满额免配送费 / 距离分档，交给
// components/LocalDeliveryRuleForm.vue——门店「自定义」用的同一份表单）。

import { computed, ref, watch } from "vue";
import { keel } from "../../api/client.ts";
import { IdempotentSubmission, withIdempotency } from "../../api/idempotency.ts";
import type { RuleConfig } from "../../api/localDeliveryRules.ts";
import { type LocalDeliveryTemplate, type LocalDeliveryTemplateInput } from "../../api/localDeliveryTemplates.ts";
import ProblemAlert from "../../components/ProblemAlert.vue";
import LocalDeliveryRuleForm from "../../components/LocalDeliveryRuleForm.vue";

const props = defineProps<{ modelValue: boolean; template: LocalDeliveryTemplate | null }>();
const emit = defineEmits<{ "update:modelValue": [boolean]; saved: [LocalDeliveryTemplate] }>();

const visible = computed({
    get: () => props.modelValue,
    set: (v: boolean) => emit("update:modelValue", v),
});
const editing = computed(() => props.template !== null);

const name = ref("");
const isDefault = ref(false);
const localError = ref("");
const error = ref<unknown>(null);
const saving = ref(false);
const submission = new IdempotentSubmission();
const ruleFormRef = ref<InstanceType<typeof LocalDeliveryRuleForm> | null>(null);

/** 编辑时把当前模板的规则喂给规则表单；新建时空白。 */
const ruleInitial = computed<RuleConfig | null>(() => {
    const t = props.template;
    return t === null ? null : { min_order_cents: t.min_order_cents, free_over_cents: t.free_over_cents, fee_tiers: t.fee_tiers };
});

watch(
    () => props.modelValue,
    (open) => {
        if (!open) return;
        name.value = props.template?.name ?? "";
        isDefault.value = props.template?.is_default ?? false;
        localError.value = "";
        error.value = null;
        submission.rotate();
    },
);

async function submit(): Promise<void> {
    localError.value = "";
    error.value = null;
    if (name.value.trim() === "") {
        localError.value = "请填模板名称";
        return;
    }
    const cfg = ruleFormRef.value?.submit() ?? null;
    if (cfg === null) return; // 规则表单已经把错误显示在自己的错误条上

    const body: LocalDeliveryTemplateInput = {
        name: name.value.trim(),
        is_default: isDefault.value,
        min_order_cents: cfg.min_order_cents,
        free_over_cents: cfg.free_over_cents,
        fee_tiers: cfg.fee_tiers,
    };
    saving.value = true;
    try {
        let saved: LocalDeliveryTemplate;
        if (props.template === null) {
            saved = await withIdempotency(submission, (key) =>
                keel.request("post", "/admin/local-delivery-templates", { body, headers: { "Idempotency-Key": key } }),
            );
        } else {
            saved = await keel.request("put", "/admin/local-delivery-templates/{template_id}", {
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
    <el-dialog v-model="visible" :title="editing ? `编辑同城配送模板 #${template?.id}` : '新建同城配送模板'" width="620px">
        <ProblemAlert v-if="error" :error="error" />
        <el-alert v-if="localError" :title="localError" type="error" :closable="false" show-icon class="mb8" />
        <el-form label-width="140px" @submit.prevent>
            <el-form-item label="名称" required>
                <el-input v-model="name" maxlength="50" show-word-limit style="width: 320px" />
                <span class="hint ml8">本店内不重名</span>
            </el-form-item>
            <el-form-item label="设为默认模板">
                <el-switch v-model="isDefault" />
                <span class="hint ml8">新开的围栏门店没配过时按它收费；设了新的，旧的默认自动取消</span>
            </el-form-item>
            <LocalDeliveryRuleForm ref="ruleFormRef" :initial="ruleInitial" />
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
