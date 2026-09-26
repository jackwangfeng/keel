<script setup lang="ts">
// 营销活动（数据模型 §7·二）：满减、满折、限时折扣、秒杀、新人礼。
// 列表带规则摘要与现算的阶段；新建 / 编辑一个对话框，上线 / 下线在行上直接切。
//
// 权限：与优惠券同一行，全店范围（管理员 / 操作员）。服务端 403 会原样摊开。

import { onMounted, ref } from "vue";
import { Plus, Refresh } from "@element-plus/icons-vue";
import { ElMessageBox } from "element-plus";
import { keel } from "../../api/client.ts";
import { scopeLabel } from "../../api/coupons.ts";
import { PHASE, PROMOTION_TYPE, ruleSummary, type AdminPromotion, type PromotionType } from "../../api/promotionRules.ts";
import { datetime } from "../../ui/format.ts";
import { notifyError, notifyOk } from "../../ui/notify.ts";
import ProblemAlert from "../../components/ProblemAlert.vue";
import PromotionDialog from "./PromotionDialog.vue";

interface Page {
    items: AdminPromotion[];
    total: number;
    page: number;
    page_size: number;
}

const loading = ref(false);
const error = ref<unknown>(null);
const page = ref<Page | null>(null);
const pageNo = ref(1);
const statusFilter = ref<"" | 0 | 1>("");
const typeFilter = ref<"" | PromotionType>("");

async function load(): Promise<void> {
    loading.value = true;
    error.value = null;
    try {
        page.value = await keel.get("/admin/promotions", {
            query: {
                page: pageNo.value,
                page_size: 20,
                ...(statusFilter.value === "" ? {} : { status: statusFilter.value }),
                ...(typeFilter.value === "" ? {} : { promotion_type: typeFilter.value }),
            },
        });
    } catch (err) {
        error.value = err;
    } finally {
        loading.value = false;
    }
}
onMounted(() => void load());

const dialogVisible = ref(false);
const current = ref<AdminPromotion | null>(null);

function openCreate(): void {
    current.value = null;
    dialogVisible.value = true;
}
function openEdit(p: AdminPromotion): void {
    current.value = p;
    dialogVisible.value = true;
}
async function onSaved(p: AdminPromotion): Promise<void> {
    notifyOk(p.status === 0 ? `已保存「${p.name}」（下线状态，确认无误后点「上线」）` : `已保存「${p.name}」`);
    await load();
}

async function toggle(p: AdminPromotion): Promise<void> {
    const goingOnline = p.status === 0;
    try {
        await ElMessageBox.confirm(
            goingOnline
                ? `上线「${p.name}」之后，在有效期内的试算与下单立即按它计价。上线中只能改名，改规则要先下线。`
                : `下线「${p.name}」之后，新的试算与下单不再命中它；已下的单按下单时的快照，不受影响。`,
            goingOnline ? "确认上线" : "确认下线",
            { type: goingOnline ? "info" : "warning" },
        );
    } catch {
        return;
    }
    try {
        await keel.request("patch", "/admin/promotions/{promotion_id}", {
            path: { promotion_id: p.id },
            body: { status: goingOnline ? 1 : 0 },
        });
        notifyOk(goingOnline ? "已上线" : "已下线");
    } catch (err) {
        notifyError(err);
    }
    await load();
}
</script>

<template>
    <div>
        <ProblemAlert v-if="error" :error="error" />
        <div class="page-toolbar">
            <span class="hint">计价顺序：门店价 → 限时折扣 / 秒杀改单价 → 满减满折按行分摊 → 优惠券（按活动后金额）→ 运费。</span>
            <el-select v-model="typeFilter" style="width: 120px" @change="(pageNo = 1), load()">
                <el-option value="" label="全部类型" />
                <el-option v-for="(label, k) in PROMOTION_TYPE" :key="k" :value="Number(k)" :label="label" />
            </el-select>
            <el-select v-model="statusFilter" style="width: 110px" @change="(pageNo = 1), load()">
                <el-option value="" label="全部" />
                <el-option :value="1" label="已上线" />
                <el-option :value="0" label="已下线" />
            </el-select>
            <span class="grow" />
            <el-button :icon="Refresh" :loading="loading" @click="load">刷新</el-button>
            <el-button type="primary" :icon="Plus" @click="openCreate">新建活动</el-button>
        </div>

        <el-table :data="page?.items ?? []" v-loading="loading" border stripe>
            <el-table-column prop="id" label="ID" width="64" />
            <el-table-column label="名称 / 类型" min-width="180">
                <template #default="{ row }: { row: AdminPromotion }">
                    <div>
                        {{ row.name }}
                        <el-tag size="small" class="ml4">{{ PROMOTION_TYPE[row.promotion_type] }}</el-tag>
                        <el-tooltip v-if="!row.stack_with_coupon && row.promotion_type !== 5" content="命中这个活动的订单不能再用优惠券">
                            <el-tag size="small" type="warning" class="ml4">不与券同享</el-tag>
                        </el-tooltip>
                    </div>
                </template>
            </el-table-column>
            <el-table-column label="规则" min-width="260">
                <template #default="{ row }: { row: AdminPromotion }">
                    <span class="sub">{{ ruleSummary(row) }}</span>
                </template>
            </el-table-column>
            <el-table-column label="适用范围" min-width="180">
                <template #default="{ row }: { row: AdminPromotion }">
                    <span v-if="row.promotion_type === 5" class="sub">首单前的买家</span>
                    <span v-else-if="row.scopes.length === 0" class="sub">全场、全店</span>
                    <el-tag v-for="(s, i) in row.scopes" :key="i" size="small" :type="s.include ? 'success' : 'danger'" class="scope-tag">
                        {{ scopeLabel(s) }}
                    </el-tag>
                </template>
            </el-table-column>
            <el-table-column label="活动时间" min-width="200">
                <template #default="{ row }: { row: AdminPromotion }">
                    <span class="sub">{{ datetime(row.starts_at) }} ~ {{ datetime(row.ends_at) }}</span>
                </template>
            </el-table-column>
            <el-table-column label="阶段" width="90">
                <template #default="{ row }: { row: AdminPromotion }">
                    <el-tag :type="PHASE[row.phase].tag" size="small">{{ PHASE[row.phase].text }}</el-tag>
                </template>
            </el-table-column>
            <el-table-column label="操作" width="150" fixed="right">
                <template #default="{ row }: { row: AdminPromotion }">
                    <el-button link type="primary" @click="openEdit(row)">{{ row.status === 1 ? "改名" : "编辑" }}</el-button>
                    <el-button link :type="row.status === 1 ? 'danger' : 'success'" @click="toggle(row)">
                        {{ row.status === 1 ? "下线" : "上线" }}
                    </el-button>
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
            @current-change="(n: number) => ((pageNo = n), load())"
        />

        <PromotionDialog v-model="dialogVisible" :promotion="current" @saved="onSaved" />
    </div>
</template>

<style scoped>
.ml4 {
    margin-left: 4px;
}
.sub {
    color: var(--el-text-color-secondary);
    font-size: 12px;
}
.scope-tag {
    margin: 2px 4px 2px 0;
}
.pager {
    margin-top: 12px;
    justify-content: flex-end;
}
</style>
