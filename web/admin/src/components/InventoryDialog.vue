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
// ## 两条路径，同一个对话框
//
// 传了 `storeId` 就打门店维度那条（`PUT /admin/stores/{store_id}/skus/{sku_id}/inventory`），
// 没传就打不带门店的那条（`PUT /admin/skus/{sku_id}/inventory`）。两条语义逐字一致。
//
// **不带门店的那条在多门店之后有一个新的 409：`store-ambiguous`。**
// 契约写死：商家恰好一家门店时它就是那一家，否则 409——服务端不替你猜，
// 因为猜错一家店的后果是把另一家店的水位覆盖掉。它和 CAS 冲突**共用 409**，
// 只能靠 type 分：
//
//   · inventory-precondition-failed → 刷新那一格再试，会成功（上面那套）
//   · store-ambiguous               → 重试**永远**不会成功。要换一条路径。
//
// 所以对后者这里不显示「有人改了库存」、不给重试按钮，而是列出这家商家的
// 门店，每一家一个按钮直接跳到那家店的库存页。把它显示成冲突让人重试，
// 就是教人对着一扇永远不开的门一直敲。
//
// 这条接口**没有**幂等键（契约的 parameters 里只有 SkuId）：PUT + CAS 本身
// 就是幂等的，重发同一个请求要么成功一次要么 409，不会写出第二笔。

import { computed, ref, watch } from "vue";
import { useRouter } from "vue-router";
import {
    asInventoryConflict,
    asInventoryShortage,
    isProblemType,
    keel,
    ProblemType,
    type AdminInventory,
    type AdminStore,
} from "../api/client.ts";
import { listAllStores } from "../api/stores.ts";
import { IdempotentSubmission, withIdempotency } from "../api/idempotency.ts";
import ProblemAlert from "./ProblemAlert.vue";
import { notifyError, notifyOk } from "../ui/notify.ts";

/** 要改的那一格。两种入口（商品详情的 SKU、门店的库存行）都能给出这个形状。 */
export interface InventoryTarget {
    skuId: number;
    skuCode: string;
    /** 规格的可读描述，可空。 */
    spec?: string;
    /** 你看到的可售量（这家店缺行时就是 0）。 */
    availableQty: number;
    warningQty: number;
}

const props = defineProps<{
    modelValue: boolean;
    sku: InventoryTarget | null;
    /** 给了就走门店维度那条路径。 */
    storeId?: number | null;
    /** 门店名，只用于标题。 */
    storeName?: string;
}>();
const router = useRouter();
const emit = defineEmits<{
    "update:modelValue": [value: boolean];
    updated: [inventory: AdminInventory];
}>();

/**
 * 两种改法。「加减」是进货 / 盘亏的正确形状（POST .../inventory/adjustments）：
 * 只说加减多少，不带「我看到的值」，于是永远不会因为并发下单拿到要重读重试的 409。
 * 「设为」留给盘点得出的绝对结论与改预警线。默认「加减」——补货是最常见的那一种。
 */
const mode = ref<"adjust" | "set">("adjust");
const delta = ref(0);
const reason = ref("");
/** 一次打开对应一次提交；失败（除「处理中」与 503 外）换钥匙，判据在 api/idempotency.ts。 */
const submission = new IdempotentSubmission();
/** 「加减」扣过头时服务端回来的当前水位。 */
const shortCurrent = ref<AdminInventory | null>(null);
const expected = ref(0);
const target = ref(0);
const warning = ref(0);
const busy = ref(false);
const error = ref<unknown>(null);
/** 409 回来的当前真实值。非 null 时界面进入「重试」形态。 */
const conflictCurrent = ref<AdminInventory | null>(null);
/** 冲突发生时，用户原本想加/减多少。用于「按差额重算」。 */
const intendedDelta = ref(0);
/** store-ambiguous 时列出来让人选的门店。非 null 即进入「换路径」形态。 */
const ambiguousStores = ref<AdminStore[] | null>(null);

watch(
    () => [props.modelValue, props.sku] as const,
    ([open, sku]) => {
        if (!open || sku === null) return;
        mode.value = "adjust";
        delta.value = 0;
        reason.value = "";
        shortCurrent.value = null;
        submission.rotate();
        expected.value = sku.availableQty;
        target.value = sku.availableQty;
        warning.value = sku.warningQty;
        error.value = null;
        conflictCurrent.value = null;
        intendedDelta.value = 0;
        ambiguousStores.value = null;
    },
    { immediate: true },
);

const skuLabel = computed(() => {
    const sku = props.sku;
    if (sku === null) return "";
    const base = sku.spec === undefined || sku.spec === "" ? sku.skuCode : `${sku.skuCode}（${sku.spec}）`;
    return props.storeId ? `${base} · 门店：${props.storeName ?? `#${props.storeId}`}` : base;
});

function close(): void {
    emit("update:modelValue", false);
}

async function submit(): Promise<void> {
    if (mode.value === "adjust") {
        await submitAdjust();
        return;
    }
    const sku = props.sku;
    if (sku === null) return;
    busy.value = true;
    error.value = null;
    const body = {
        expected_available_qty: expected.value,
        available_qty: target.value,
        warning_qty: warning.value,
    };
    try {
        const storeId = props.storeId;
        const updated =
            storeId === null || storeId === undefined
                ? await keel.request("put", "/admin/skus/{sku_id}/inventory", { path: { sku_id: sku.skuId }, body })
                : await keel.request("put", "/admin/stores/{store_id}/skus/{sku_id}/inventory", {
                      path: { store_id: storeId, sku_id: sku.skuId },
                      body,
                  });
        notifyOk(`库存已改为 ${updated.available_qty}`);
        emit("updated", updated);
        close();
    } catch (err) {
        if (isProblemType(err, ProblemType.storeAmbiguous)) {
            // 不是冲突，是路径不对。列出门店让人选，不给「重试」。
            conflictCurrent.value = null;
            error.value = err;
            listAllStores().then(
                (r) => (ambiguousStores.value = r.stores),
                (e: unknown) => notifyError(e),
            );
            return;
        }
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

/** 相对调整。门店维度与单店捷径两条路径，错误的处置与「设为」同构。 */
async function submitAdjust(): Promise<void> {
    const sku = props.sku;
    if (sku === null || delta.value === 0) return;
    busy.value = true;
    error.value = null;
    shortCurrent.value = null;
    const trimmed = reason.value.trim();
    const body = trimmed === "" ? { delta: delta.value } : { delta: delta.value, reason: trimmed };
    try {
        const storeId = props.storeId;
        const updated = await withIdempotency(submission, (key) =>
            storeId === null || storeId === undefined
                ? keel.request("post", "/admin/skus/{sku_id}/inventory/adjustments", {
                      path: { sku_id: sku.skuId },
                      body,
                      headers: { "Idempotency-Key": key },
                  })
                : keel.request("post", "/admin/stores/{store_id}/skus/{sku_id}/inventory/adjustments", {
                      path: { store_id: storeId, sku_id: sku.skuId },
                      body,
                      headers: { "Idempotency-Key": key },
                  }),
        );
        notifyOk(`库存已${delta.value > 0 ? "加" : "减"} ${Math.abs(delta.value)}，现在是 ${updated.available_qty}`);
        emit("updated", updated);
        close();
    } catch (err) {
        if (isProblemType(err, ProblemType.storeAmbiguous)) {
            error.value = err;
            listAllStores().then(
                (r) => (ambiguousStores.value = r.stores),
                (e: unknown) => notifyError(e),
            );
            return;
        }
        const shortage = asInventoryShortage(err);
        if (shortage !== null) shortCurrent.value = shortage.current;
        error.value = err;
    } finally {
        busy.value = false;
    }
}

function goToStoreInventory(store: AdminStore): void {
    const sku = props.sku;
    close();
    void router.push({
        name: "store-detail",
        params: { storeId: store.id },
        query: { tab: "inventory", sku: sku === null ? undefined : String(sku.skuId) },
    });
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
        title="改库存"
        width="560px"
        @update:model-value="emit('update:modelValue', $event)"
    >
        <p class="hint">
            {{ skuLabel }}
        </p>
        <el-radio-group v-if="ambiguousStores === null" v-model="mode" size="small" class="mode">
            <el-radio-button value="adjust">加减（进货 / 盘亏）</el-radio-button>
            <el-radio-button value="set">设为（盘点结果 / 预警线）</el-radio-button>
        </el-radio-group>
        <p v-if="mode === 'adjust'" class="hint">
            只说加减多少，服务端在<strong>当前真实值</strong>上加减，和同时发生的下单扣减互不覆盖；
            扣完会变负时拒绝。网络超时重试不会加两次。
        </p>
        <p v-else class="hint">
            这条接口和下单抢同一行。<strong>「我看到的值」是必填的</strong>——服务端拿它当
            UPDATE 的条件，对不上就拒绝，于是「页面停了三分钟、期间卖掉 4 件」不会被一次后台覆盖抹掉。
        </p>

        <ProblemAlert v-if="error" :error="error" />

        <el-alert v-if="ambiguousStores !== null" type="warning" :closable="false" show-icon class="ambiguous">
            <template #title>这里改不了：商家有 {{ ambiguousStores.length }} 家门店，要指明改哪一家</template>
            <template #default>
                <p>
                    多门店之后库存是按门店分的，「这个 SKU 的库存」没有唯一答案。服务端不替你猜——猜错一家店，
                    就是把另一家店的水位覆盖掉。<b>重试没有用</b>，选一家店去改：
                </p>
                <div class="store-buttons">
                    <el-button
                        v-for="st in ambiguousStores"
                        :key="st.id"
                        size="small"
                        :type="st.is_default ? 'primary' : 'default'"
                        @click="goToStoreInventory(st)"
                    >
                        {{ st.name }}{{ st.is_default ? "（默认）" : "" }} → 库存
                    </el-button>
                </div>
            </template>
        </el-alert>

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

        <el-alert v-if="shortCurrent" type="warning" :closable="false" show-icon class="conflict">
            <template #title>不够扣：现在只有 {{ shortCurrent.available_qty }} 件</template>
            <template #default>
                <p>原样重试不会成功——改小扣减量，或者先补货。</p>
            </template>
        </el-alert>

        <el-form v-if="ambiguousStores === null && mode === 'adjust'" label-width="140px" @submit.prevent>
            <el-form-item label="加减多少">
                <el-input-number v-model="delta" :min="-1000000" :max="1000000" />
                <span class="hint ml8">正数进货，负数扣减；不能为 0</span>
            </el-form-item>
            <el-form-item label="原因">
                <el-input v-model="reason" maxlength="200" show-word-limit placeholder="例如：3 月进货、盘点盘亏（可不填）" />
            </el-form-item>
        </el-form>

        <el-form v-if="ambiguousStores === null && mode === 'set'" label-width="140px" @submit.prevent>
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
            <el-button
                v-if="ambiguousStores === null"
                type="primary"
                :loading="busy"
                :disabled="mode === 'adjust' && delta === 0"
                @click="submit"
                >提交</el-button
            >
        </template>
    </el-dialog>
</template>

<style scoped>
.ambiguous {
    margin-bottom: 12px;
}
.store-buttons {
    display: flex;
    flex-wrap: wrap;
    gap: 6px;
    margin-top: 6px;
}
.store-buttons .el-button + .el-button {
    margin-left: 0;
}
.conflict {
    margin-bottom: 12px;
}
.mode {
    margin-bottom: 8px;
}
.ml8 {
    margin-left: 8px;
}
</style>
