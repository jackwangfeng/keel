<script setup lang="ts">
// 券管理（数据模型 §7）。列表带发放与核销统计；新建 / 编辑、适用范围、
// 定向发券各一个对话框，「可领取」与启停在行上直接切。
//
// 权限：本期只对商家级的管理员与操作员开放（服务端 403 staff-forbidden）。

import { onMounted, ref } from "vue";
import { Plus, Refresh } from "@element-plus/icons-vue";
import { ElMessageBox } from "element-plus";
import { keel } from "../../api/client.ts";
import { COUPON_TYPE, scopeLabel, type AdminCouponTemplate, type CouponTemplatePage } from "../../api/coupons.ts";
import { rateLabel } from "../../api/money.ts";
import { datetime, yuan } from "../../ui/format.ts";
import { notifyError, notifyOk } from "../../ui/notify.ts";
import ProblemAlert from "../../components/ProblemAlert.vue";
import CouponTemplateDialog from "./CouponTemplateDialog.vue";
import CouponScopeDialog from "./CouponScopeDialog.vue";
import CouponGrantDialog from "./CouponGrantDialog.vue";

const loading = ref(false);
const error = ref<unknown>(null);
const page = ref<CouponTemplatePage | null>(null);
const pageNo = ref(1);
const statusFilter = ref<"" | 0 | 1>("");

async function load(): Promise<void> {
    loading.value = true;
    error.value = null;
    try {
        page.value = await keel.get("/admin/coupon-templates", {
            query: { page: pageNo.value, page_size: 20, ...(statusFilter.value === "" ? {} : { status: statusFilter.value }) },
        });
    } catch (err) {
        error.value = err;
    } finally {
        loading.value = false;
    }
}
onMounted(() => void load());

/** 券面一句话：「满 100 减 20」「8.5 折，满 50 可用，最多减 30」「立减 5」。 */
function faceText(t: AdminCouponTemplate): string {
    switch (t.coupon_type) {
        case 1:
            return `满 ${yuan(t.threshold_cents)} 减 ${yuan(t.discount_cents)}`;
        case 2: {
            const parts = [rateLabel(t.discount_rate)];
            if (t.threshold_cents > 0) parts.push(`满 ${yuan(t.threshold_cents)} 可用`);
            if (t.max_discount_cents > 0) parts.push(`最多减 ${yuan(t.max_discount_cents)}`);
            return parts.join("，");
        }
        case 3:
            return `立减 ${yuan(t.discount_cents)}`;
        default: {
            // 包邮券（00056）：抵运费，封顶 0 = 运费全免。
            const parts = [t.max_discount_cents > 0 ? `运费最多抵 ${yuan(t.max_discount_cents)}` : "包邮（运费全免）"];
            if (t.threshold_cents > 0) parts.push(`满 ${yuan(t.threshold_cents)} 可用`);
            return parts.join("，");
        }
    }
}

function validText(t: AdminCouponTemplate): string {
    return t.valid_mode === 1 ? `${datetime(t.valid_start_at)} ~ ${datetime(t.valid_end_at)}` : `领取后 ${t.valid_days} 天`;
}

// ---------------------------------------------------------------- 对话框

const tplVisible = ref(false);
const scopeVisible = ref(false);
const grantVisible = ref(false);
const current = ref<AdminCouponTemplate | null>(null);

function openCreate(): void {
    current.value = null;
    tplVisible.value = true;
}
function openEdit(t: AdminCouponTemplate): void {
    current.value = t;
    tplVisible.value = true;
}
function openScopes(t: AdminCouponTemplate): void {
    current.value = t;
    scopeVisible.value = true;
}
function openGrant(t: AdminCouponTemplate): void {
    current.value = t;
    grantVisible.value = true;
}

async function onSaved(t: AdminCouponTemplate): Promise<void> {
    notifyOk(`已保存「${t.name}」`);
    await load();
}

// ---------------------------------------------------------------- 行上的开关

async function patch(t: AdminCouponTemplate, body: { claimable?: boolean; status?: 0 | 1 }, ok: string): Promise<void> {
    try {
        await keel.request("patch", "/admin/coupon-templates/{template_id}", { path: { template_id: t.id }, body });
        notifyOk(ok);
    } catch (err) {
        notifyError(err);
    }
    await load();
}

async function toggleClaimable(t: AdminCouponTemplate, v: boolean): Promise<void> {
    await patch(t, { claimable: v }, v ? "已开放领取" : "已关闭领取");
}

async function toggleStatus(t: AdminCouponTemplate): Promise<void> {
    if (t.status === 1) {
        try {
            await ElMessageBox.confirm(
                `停用「${t.name}」之后，领券中心不再展示、也不能再定向发放。已经发出的 ${t.issued_count} 张不回收，照常可用到过期。`,
                "确认停用",
                { type: "warning" },
            );
        } catch {
            return;
        }
        await patch(t, { status: 0 }, "已停用");
    } else {
        await patch(t, { status: 1 }, "已启用");
    }
}
</script>

<template>
    <div>
        <ProblemAlert v-if="error" :error="error" />
        <div class="page-toolbar">
            <span class="hint">一单只能用一张券；券按下单门店的生效价计算。停用不回收已发出的券。</span>
            <el-select v-model="statusFilter" style="width: 110px" @change="(pageNo = 1), load()">
                <el-option value="" label="全部" />
                <el-option :value="1" label="启用中" />
                <el-option :value="0" label="已停用" />
            </el-select>
            <span class="grow" />
            <el-button :icon="Refresh" :loading="loading" @click="load">刷新</el-button>
            <el-button type="primary" :icon="Plus" @click="openCreate">新建券</el-button>
        </div>

        <el-table :data="page?.items ?? []" v-loading="loading" border stripe>
            <el-table-column prop="id" label="ID" width="64" />
            <el-table-column label="名称 / 券面" min-width="220">
                <template #default="{ row }: { row: AdminCouponTemplate }">
                    <div>
                        {{ row.name }}
                        <el-tag size="small" class="ml4">{{ COUPON_TYPE[row.coupon_type] }}</el-tag>
                        <el-tooltip v-if="row.locked" content="已发出过券：券面与范围不能再改">
                            <el-tag size="small" type="info" class="ml4">已锁定</el-tag>
                        </el-tooltip>
                    </div>
                    <div class="sub">{{ faceText(row) }}</div>
                </template>
            </el-table-column>
            <el-table-column label="有效期" min-width="200">
                <template #default="{ row }: { row: AdminCouponTemplate }">
                    <span class="sub">{{ validText(row) }}</span>
                </template>
            </el-table-column>
            <el-table-column label="适用范围" min-width="200">
                <template #default="{ row }: { row: AdminCouponTemplate }">
                    <span v-if="row.scopes.length === 0" class="sub">全场、全店</span>
                    <el-tag
                        v-for="(s, i) in row.scopes"
                        :key="i"
                        size="small"
                        :type="s.include ? 'success' : 'danger'"
                        class="scope-tag"
                    >
                        {{ scopeLabel(s) }}
                    </el-tag>
                </template>
            </el-table-column>
            <el-table-column label="发放 / 核销" min-width="230">
                <template #default="{ row }: { row: AdminCouponTemplate }">
                    <div>
                        已发 {{ row.stats.issued }}<span v-if="row.total_count > 0"> / {{ row.total_count }}</span>
                        <span class="sub">（领取 {{ row.stats.claimed }} · 定向 {{ row.stats.granted }}）</span>
                    </div>
                    <div class="sub">
                        未用 {{ row.stats.unused }} · 锁定 {{ row.stats.locked }} · 已核销 {{ row.stats.used }} · 过期 {{ row.stats.expired }}
                    </div>
                </template>
            </el-table-column>
            <el-table-column label="可领取" width="90">
                <template #default="{ row }: { row: AdminCouponTemplate }">
                    <el-switch
                        :model-value="row.claimable"
                        :disabled="row.status === 0"
                        @change="(v: string | number | boolean) => toggleClaimable(row, v === true)"
                    />
                </template>
            </el-table-column>
            <el-table-column label="状态" width="80">
                <template #default="{ row }: { row: AdminCouponTemplate }">
                    <el-tag :type="row.status === 1 ? 'success' : 'info'" size="small">{{ row.status === 1 ? "启用" : "停用" }}</el-tag>
                </template>
            </el-table-column>
            <el-table-column label="操作" width="250" fixed="right">
                <template #default="{ row }: { row: AdminCouponTemplate }">
                    <el-button link type="primary" @click="openEdit(row)">编辑</el-button>
                    <el-button link type="primary" @click="openScopes(row)">适用范围</el-button>
                    <el-button link type="primary" :disabled="row.status === 0" @click="openGrant(row)">发券</el-button>
                    <el-button link :type="row.status === 1 ? 'danger' : 'success'" @click="toggleStatus(row)">
                        {{ row.status === 1 ? "停用" : "启用" }}
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

        <CouponTemplateDialog v-model="tplVisible" :template="current" @saved="onSaved" />
        <CouponScopeDialog v-model="scopeVisible" :template="current" @saved="onSaved" />
        <CouponGrantDialog v-model="grantVisible" :template="current" @granted="load" />
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
