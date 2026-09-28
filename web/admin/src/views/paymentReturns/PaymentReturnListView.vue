<script setup lang="ts">
// 多收款退回（GET /admin/payment-returns，00150）：订单只认一笔到账，其余的 —— 重复支付、订单取消或关闭后才到、
// 金额与应付不符 —— 系统自动原路退回，不需要审核。这一页是给人看账的：哪些钱退了、哪些还没退成（待提交且有
// 失败原因的，比如没配渠道回调密钥，每分钟重试一次）。全店范围的员工可见。

import { onMounted, ref } from "vue";
import { Refresh } from "@element-plus/icons-vue";
import { keel, type PaymentReturnPage } from "../../api/client.ts";
import { PAYMENT_RETURN_REASON, PAYMENT_RETURN_STATUS, isStuck } from "../../api/paymentReturnRules.ts";
import { datetime, yuan } from "../../ui/format.ts";
import ProblemAlert from "../../components/ProblemAlert.vue";

const loading = ref(false);
const error = ref<unknown>(null);
const page = ref<PaymentReturnPage | null>(null);
const pageNo = ref(1);
const pageSize = ref(20);
const status = ref<10 | 30 | 40 | undefined>(undefined);

async function load(): Promise<void> {
    loading.value = true;
    error.value = null;
    try {
        page.value = await keel.get("/admin/payment-returns", {
            query: { page: pageNo.value, page_size: pageSize.value, ...(status.value === undefined ? {} : { status: status.value }) },
        });
    } catch (err) {
        error.value = err;
    } finally {
        loading.value = false;
    }
}

onMounted(load);

const statusOptions = Object.entries(PAYMENT_RETURN_STATUS).map(([k, v]) => ({ value: Number(k) as 10 | 30 | 40, label: v.text }));
</script>

<template>
    <div>
        <ProblemAlert v-if="error" :error="error" />
        <p class="hint">订单只认一笔到账；重复支付、订单取消或关闭后才到账、金额与应付不符的钱，系统自动原路退回，不需要审核。</p>

        <el-form :inline="true" class="filters" @submit.prevent="(pageNo = 1), load()">
            <el-form-item label="状态">
                <el-select v-model="status" clearable placeholder="全部" style="width: 140px" @change="(pageNo = 1), load()">
                    <el-option v-for="o in statusOptions" :key="o.value" :label="o.label" :value="o.value" />
                </el-select>
            </el-form-item>
            <el-form-item>
                <el-button :icon="Refresh" :loading="loading" @click="load">刷新</el-button>
            </el-form-item>
        </el-form>

        <el-table v-loading="loading" :data="page?.items ?? []" row-key="return_no" empty-text="没有多收款">
            <el-table-column label="退回单号" min-width="220">
                <template #default="{ row }">
                    {{ row.return_no }}
                    <div class="hint">
                        订单
                        <router-link :to="{ name: 'orders', query: { order_no: row.order_no } }">{{ row.order_no }}</router-link>
                    </div>
                </template>
            </el-table-column>
            <el-table-column label="状态" width="110">
                <template #default="{ row }">
                    <el-tag size="small" :type="isStuck(row) ? 'danger' : PAYMENT_RETURN_STATUS[row.status as 10 | 30 | 40].tag">
                        {{ isStuck(row) ? "提交失败，重试中" : PAYMENT_RETURN_STATUS[row.status as 10 | 30 | 40].text }}
                    </el-tag>
                </template>
            </el-table-column>
            <el-table-column label="原因" width="180">
                <template #default="{ row }">{{ PAYMENT_RETURN_REASON[row.reason as 1 | 2 | 3] }}</template>
            </el-table-column>
            <el-table-column label="金额" width="110" align="right">
                <template #default="{ row }">{{ yuan(row.amount_cents) }}</template>
            </el-table-column>
            <el-table-column label="渠道 / 原支付流水" min-width="240">
                <template #default="{ row }">
                    {{ row.channel === "alipay" ? "支付宝" : row.channel === "wechat" ? "微信" : "—" }}
                    <div class="hint">{{ row.payment_txn_id ?? "—" }}</div>
                </template>
            </el-table-column>
            <el-table-column label="时间" width="200">
                <template #default="{ row }">
                    {{ datetime(row.created_at) }}
                    <div v-if="row.returned_at" class="hint">退回于 {{ datetime(row.returned_at) }}</div>
                    <div v-else-if="row.last_error" class="hint error">{{ row.last_error }}（已试 {{ row.attempts }} 次）</div>
                </template>
            </el-table-column>
        </el-table>

        <el-pagination
            v-if="page"
            class="pager"
            layout="total, prev, pager, next"
            :total="page.total"
            :current-page="page.page"
            :page-size="page.page_size"
            @current-change="
                (n: number) => {
                    pageNo = n;
                    load();
                }
            "
        />
    </div>
</template>

<style scoped>
.hint {
    color: var(--el-text-color-secondary);
    font-size: 12px;
}
.error {
    color: var(--el-color-danger);
}
.pager {
    margin-top: 12px;
    justify-content: flex-end;
}
</style>
