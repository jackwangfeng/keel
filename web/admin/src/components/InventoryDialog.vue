<script setup lang="ts">
// 库存：比较并设置（CAS）。
//
// 这是后端刻意设计过、而界面最容易白费的一处。契约里 `expected_available_qty`
// 是**必填**的，它的含义是「我看到的那个值」；服务端执行
// `UPDATE ... WHERE sku_id = $1 AND available_qty = $expected`。
// 对不上就 409，并把**当前真实值**放在 Problem 的 `current` 里一起回来。
//
// 那个 `current` 存在的全部理由是：页面能直接刷新那一格并重试，
// 而不是让人再点一次查询才知道发生了什么。所以这里：
//
//   · 409 时把「你看到的」和「现在是」并排显示，并说清多半是并发下单扣掉了；
//   · 给一个「用新值重试」的按钮 —— 它把 expected 换成 current.available_qty
//     并原样重发，新值不动。补货场景（进货 100 件）因此要按「新值 = 当前 + 100」
//     来想，所以按钮旁边还有一个「按差额重算新值」的选项。
//
// 这条接口**没有**幂等键（契约的 parameters 里只有 SkuId）：PUT + CAS 本身
// 就是幂等的，重发同一个请求要么成功一次要么 409，不会写出第二笔。

import { computed, ref, watch } from "vue";
import { asInventoryConflict, keel, type AdminInventory, type AdminSku } from "../api/client.ts";
import ProblemAlert from "./ProblemAlert.vue";
import { notifyOk } from "../ui/notify.ts";

const props = defineProps<{ modelValue: boolean; sku: AdminSku | null }>();
const emit = defineEmits<{
    "update:modelValue": [value: boolean];
    updated: [inventory: AdminInventory];
}>();

const expected = ref(0);
const target = ref(0);
const warning = ref(0);
const busy = ref(false);
const error = ref<unknown>(null);
/** 409 回来的当前真实值。非 null 时界面进入「重试」形态。 */
const conflictCurrent = ref<AdminInventory | null>(null);
/** 冲突发生时，用户原本想加/减多少。用于「按差额重算」。 */
const intendedDelta = ref(0);

watch(
    () => [props.modelValue, props.sku] as const,
    ([open, sku]) => {
        if (!open || sku === null) return;
        expected.value = sku.available_qty;
        target.value = sku.available_qty;
        warning.value = sku.warning_qty ?? 0;
        error.value = null;
        conflictCurrent.value = null;
        intendedDelta.value = 0;
    },
    { immediate: true },
);

const skuLabel = computed(() => {
    const sku = props.sku;
    if (sku === null) return "";
    const spec = Object.entries(sku.spec_values ?? {})
        .map(([k, v]) => `${k}:${v}`)
        .join(" / ");
    return spec === "" ? sku.sku_code : `${sku.sku_code}（${spec}）`;
});

function close(): void {
    emit("update:modelValue", false);
}

async function submit(): Promise<void> {
    const sku = props.sku;
    if (sku === null) return;
    busy.value = true;
    error.value = null;
    try {
        const updated = await keel.request("put", "/admin/skus/{sku_id}/inventory", {
            path: { sku_id: sku.id },
            body: {
                expected_available_qty: expected.value,
                available_qty: target.value,
                warning_qty: warning.value,
            },
        });
        notifyOk(`库存已改为 ${updated.available_qty}`);
        emit("updated", updated);
        close();
    } catch (err) {
        const conflict = asInventoryConflict(err);
        if (conflict !== null) {
            // 记下「用户本来想改多少」，供「按差额重算」用。
            intendedDelta.value = target.value - expected.value;
            conflictCurrent.value = conflict.current;
        }
        error.value = err;
    } finally {
        busy.value = false;
    }
}

/** 用服务端回来的当前值当作新的 expected，新值不动，直接重发。 */
function retryWithCurrent(): void {
    const current = conflictCurrent.value;
    if (current === null) return;
    expected.value = current.available_qty;
    void submit();
}

/** 保持「我本来想加/减多少」，在当前真实值上重算新值。补货场景走这条。 */
function retryWithDelta(): void {
    const current = conflictCurrent.value;
    if (current === null) return;
    expected.value = current.available_qty;
    target.value = Math.max(0, current.available_qty + intendedDelta.value);
    void submit();
}
</script>

<template>
    <el-dialog
        :model-value="modelValue"
        title="改库存（比较并设置）"
        width="560px"
        @update:model-value="emit('update:modelValue', $event)"
    >
        <p class="hint">
            {{ skuLabel }}
        </p>
        <p class="hint">
            这条接口和下单抢同一行。<strong>「我看到的值」是必填的</strong>——服务端拿它当
            UPDATE 的条件，对不上就拒绝，于是「页面停了三分钟、期间卖掉 4 件」不会被一次后台覆盖抹掉。
        </p>

        <ProblemAlert v-if="error" :error="error" />

        <el-alert v-if="conflictCurrent" type="warning" :closable="false" show-icon class="conflict">
            <template #title>库存在你读到它之后被改过</template>
            <template #default>
                <p>
                    你看到的是 <b>{{ expected }}</b
                    >，服务端现在是 <b>{{ conflictCurrent.available_qty }}</b
                    >（多半是并发下单扣减）。
                </p>
                <p>
                    <el-button type="primary" size="small" :loading="busy" @click="retryWithCurrent">
                        用新值重试（仍写 {{ target }}）
                    </el-button>
                    <el-button
                        v-if="intendedDelta !== 0"
                        size="small"
                        :loading="busy"
                        @click="retryWithDelta"
                    >
                        按差额重算（{{ intendedDelta > 0 ? "+" : "" }}{{ intendedDelta }} →
                        {{ Math.max(0, conflictCurrent.available_qty + intendedDelta) }}）
                    </el-button>
                </p>
            </template>
        </el-alert>

        <el-form label-width="140px" @submit.prevent>
            <el-form-item label="我看到的值">
                <el-input-number v-model="expected" :min="0" />
                <span class="hint ml8">expected_available_qty</span>
            </el-form-item>
            <el-form-item label="要写进去的新值">
                <el-input-number v-model="target" :min="0" />
                <span class="hint ml8">绝对值，不是增量</span>
            </el-form-item>
            <el-form-item label="低库存预警线">
                <el-input-number v-model="warning" :min="0" />
                <span class="hint ml8">一期只是一个存着的数，没有接到任何告警</span>
            </el-form-item>
        </el-form>

        <template #footer>
            <el-button @click="close">取消</el-button>
            <el-button type="primary" :loading="busy" @click="submit">提交</el-button>
        </template>
    </el-dialog>
</template>

<style scoped>
.conflict {
    margin-bottom: 12px;
}
.ml8 {
    margin-left: 8px;
}
</style>
