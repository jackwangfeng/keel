<script setup lang="ts">
// 开店。契约里只有 `POST /admin/merchants` 这一条——**没有商家列表接口**，
// 所以这一页只有一个表单，没有表格。这不是省事：画一个「商家列表」出来，
// 它的数据只能是编的。
//
// 调用者必须是平台级（merchant_id 为空）且 role = 1，否则 403。
// 菜单里对商家级操作员隐藏了这一项，但直接输 URL 还是能进来，
// 所以这里也自己说一句，并且真的调用时把服务端的 403 原样显示出来。

import { computed, ref } from "vue";
import { currentSession, keel, type Merchant, type MerchantCreateRequest } from "../api/client.ts";
import { IdempotentSubmission, withIdempotency } from "../api/idempotency.ts";
import { notifyOk } from "../ui/notify.ts";
import ProblemAlert from "../components/ProblemAlert.vue";

const session = computed(() => currentSession());
const isPlatformAdmin = computed(
    () => session.value?.staff.merchant_id === null && session.value?.staff.role === 1,
);

const draft = ref<MerchantCreateRequest>({ code: "", name: "", admin_email: "" });
const busy = ref(false);
const error = ref<unknown>(null);
const created = ref<Merchant | null>(null);
const submission = new IdempotentSubmission();

const canSubmit = computed(
    () =>
        draft.value.code.trim() !== "" &&
        draft.value.name.trim() !== "" &&
        String(draft.value.admin_email).trim() !== "",
);

async function submit(): Promise<void> {
    busy.value = true;
    error.value = null;
    created.value = null;
    try {
        created.value = await withIdempotency(submission, (key) =>
            keel.request("post", "/admin/merchants", {
                body: draft.value,
                headers: { "Idempotency-Key": key },
            }),
        );
        notifyOk("已开店，并给该邮箱创建了第一个商家级管理员");
    } catch (err) {
        error.value = err;
    } finally {
        busy.value = false;
    }
}
</script>

<template>
    <div class="wrap">
        <el-alert v-if="!isPlatformAdmin" type="warning" :closable="false" show-icon class="mb12">
            <template #title>只有平台级管理员能开店</template>
            你当前的身份是
            {{ session?.staff.merchant_id === null ? "平台级操作员" : "商家级成员" }}。
            提交会被服务端拒绝（403），下面照样能试——被拒时你会看到服务端自己的说法。
        </el-alert>

        <el-card>
            <template #header>开店</template>
            <p class="hint">
                建商家，并为其创建第一个商家级管理员，给该邮箱发登录链接。
                本轮没有接邮件服务，那串一次性登录 token 只进进程日志。
            </p>

            <ProblemAlert v-if="error" :error="error" />

            <el-result
                v-if="created"
                icon="success"
                :title="`已创建：${created.name}`"
                :sub-title="`code = ${created.code}，id = ${created.id}`"
            />

            <el-form label-width="120px" style="max-width: 560px" @submit.prevent>
                <el-form-item label="短标识 code" required>
                    <el-input v-model="draft.code" placeholder="全局唯一，用于域名或路径路由（/s/{code}）" />
                </el-form-item>
                <el-form-item label="店名" required>
                    <el-input v-model="draft.name" />
                </el-form-item>
                <el-form-item label="管理员邮箱" required>
                    <el-input v-model="draft.admin_email" placeholder="这家店第一个管理员" />
                </el-form-item>
                <el-form-item>
                    <el-button type="primary" :loading="busy" :disabled="!canSubmit" @click="submit">开店</el-button>
                </el-form-item>
            </el-form>
        </el-card>
    </div>
</template>

<style scoped>
.wrap {
    max-width: 760px;
}
.mb12 {
    margin-bottom: 12px;
}
</style>
