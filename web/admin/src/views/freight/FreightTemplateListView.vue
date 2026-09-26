<script setup lang="ts">
// 运费模板（数据模型 §7「运费模板」）。列表 + 新建 / 编辑对话框 + 删除。
//
// 一行商品用哪个模板：商品单独挂的（全店模板）→ 履约门店的门店模板 → 全店默认 →
// 都没有则不计运费。这一条在页头写给运营看，免得「我改了模板，怎么这件商品没变」。
//
// 权限：全店模板同商品目录（管理员、操作员），门店模板同门店价；四种角色都能看。
// 按钮置灰只是体验（auth/permissions.ts），服务端会再判一遍。

import { computed, onMounted, ref } from "vue";
import { Plus, Refresh } from "@element-plus/icons-vue";
import { ElMessageBox } from "element-plus";
import { keel, type AdminStore } from "../../api/client.ts";
import { CHARGE_MODE, type AdminFreightTemplate, type FreightTemplatePage } from "../../api/freight.ts";
import { regionsLabel, ruleSummary } from "../../api/freightRules.ts";
import { listAllStores } from "../../api/stores.ts";
import { can, NO_PERMISSION } from "../../auth/permissions.ts";
import { datetime } from "../../ui/format.ts";
import { notifyError, notifyOk } from "../../ui/notify.ts";
import ProblemAlert from "../../components/ProblemAlert.vue";
import FreightTemplateDialog from "./FreightTemplateDialog.vue";

const loading = ref(false);
const error = ref<unknown>(null);
const page = ref<FreightTemplatePage | null>(null);
const pageNo = ref(1);
const storeFilter = ref<number | "">("");
const stores = ref<AdminStore[]>([]);

async function load(): Promise<void> {
    loading.value = true;
    error.value = null;
    try {
        page.value = await keel.get("/admin/freight-templates", {
            query: { page: pageNo.value, page_size: 20, ...(storeFilter.value === "" ? {} : { store_id: storeFilter.value }) },
        });
    } catch (err) {
        error.value = err;
    } finally {
        loading.value = false;
    }
}

onMounted(() => {
    void load();
    listAllStores().then(
        (r) => (stores.value = r.stores),
        (err: unknown) => notifyError(err),
    );
});

const storeById = computed(() => new Map(stores.value.map((s) => [s.id, s])));

function ownerText(t: AdminFreightTemplate): string {
    if (t.store_id === null) return "全店模板";
    return `门店模板：${storeById.value.get(t.store_id)?.name ?? `#${t.store_id}`}`;
}

function canEdit(t: AdminFreightTemplate): boolean {
    if (t.store_id === null) return can.editFreight(null);
    const s = storeById.value.get(t.store_id);
    // 门店列表还没回来时放宽（界面判断只是体验，服务端会再判）。
    return s === undefined ? true : can.editFreight(s);
}

// ---------------------------------------------------------------- 对话框

const dialogVisible = ref(false);
const current = ref<AdminFreightTemplate | null>(null);

function openCreate(): void {
    current.value = null;
    dialogVisible.value = true;
}
function openEdit(t: AdminFreightTemplate): void {
    current.value = t;
    dialogVisible.value = true;
}
async function onSaved(t: AdminFreightTemplate): Promise<void> {
    notifyOk(`已保存「${t.name}」`);
    await load();
}

async function remove(t: AdminFreightTemplate): Promise<void> {
    try {
        await ElMessageBox.confirm(
            `删除「${t.name}」之后，之前按它算的订单不受影响（运费规则快照在订单上）；` +
                (t.is_default ? "它是全店默认模板，删掉之后没挂模板、门店也没有门店模板的商品将不计运费。" : ""),
            "确认删除",
            { type: "warning" },
        );
    } catch {
        return;
    }
    try {
        await keel.request("delete", "/admin/freight-templates/{template_id}", { path: { template_id: t.id } });
        notifyOk("已删除");
    } catch (err) {
        notifyError(err);
    }
    await load();
}
</script>

<template>
    <div>
        <ProblemAlert v-if="error" :error="error" />
        <el-alert type="info" :closable="false" show-icon class="mb12">
            一行商品用哪个模板：商品单独挂的（在商品详情里选）→ 发货门店的门店模板 → 全店默认模板 → 都没有则不计运费。
            满额包邮按<strong>优惠后</strong>的应付商品金额判；包邮券抵的是运费，最多抵到 0。改模板只影响之后的订单。
        </el-alert>
        <div class="page-toolbar">
            <el-select v-model="storeFilter" style="width: 200px" @change="(pageNo = 1), load()">
                <el-option value="" label="全部模板" />
                <el-option v-for="s in stores" :key="s.id" :value="s.id" :label="`门店模板：${s.name}`" />
            </el-select>
            <span class="grow" />
            <el-button :icon="Refresh" :loading="loading" @click="load">刷新</el-button>
            <el-button type="primary" :icon="Plus" @click="openCreate">新建模板</el-button>
        </div>

        <el-table :data="page?.items ?? []" v-loading="loading" border stripe>
            <el-table-column prop="id" label="ID" width="64" />
            <el-table-column label="名称 / 归属" min-width="200">
                <template #default="{ row }: { row: AdminFreightTemplate }">
                    <div>
                        {{ row.name }}
                        <el-tag v-if="row.is_default" size="small" type="success" class="ml4">全店默认</el-tag>
                        <el-tag size="small" class="ml4">{{ CHARGE_MODE[row.charge_mode] }}</el-tag>
                    </div>
                    <div class="sub">{{ ownerText(row) }}</div>
                </template>
            </el-table-column>
            <el-table-column label="计费规则" min-width="380">
                <template #default="{ row }: { row: AdminFreightTemplate }">
                    <div v-for="(r, i) in row.rules" :key="i" class="rule-line">
                        <span class="region">{{ regionsLabel(r.region_codes) }}</span>
                        <span class="sub">{{ ruleSummary(row.charge_mode, r) }}</span>
                    </div>
                </template>
            </el-table-column>
            <el-table-column label="不配送" min-width="140">
                <template #default="{ row }: { row: AdminFreightTemplate }">
                    <span v-if="row.undeliverable_region_codes.length === 0" class="sub">全国配送</span>
                    <span v-else>{{ regionsLabel(row.undeliverable_region_codes) }}</span>
                </template>
            </el-table-column>
            <el-table-column label="挂着的商品" width="100">
                <template #default="{ row }: { row: AdminFreightTemplate }">{{ row.product_count }}</template>
            </el-table-column>
            <el-table-column label="更新时间" width="170">
                <template #default="{ row }: { row: AdminFreightTemplate }">
                    <span class="sub">{{ datetime(row.updated_at) }}</span>
                </template>
            </el-table-column>
            <el-table-column label="操作" width="140" fixed="right">
                <template #default="{ row }: { row: AdminFreightTemplate }">
                    <el-button link type="primary" :disabled="!canEdit(row)" :title="canEdit(row) ? '' : NO_PERMISSION" @click="openEdit(row)">编辑</el-button>
                    <el-tooltip :disabled="row.product_count === 0" content="还有商品挂着它：先在商品详情里改挂别的模板">
                        <span>
                            <el-button
                                link
                                type="danger"
                                :disabled="!canEdit(row) || row.product_count > 0"
                                @click="remove(row)"
                            >删除</el-button>
                        </span>
                    </el-tooltip>
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

        <FreightTemplateDialog v-model="dialogVisible" :template="current" :stores="stores" @saved="onSaved" />
    </div>
</template>

<style scoped>
.mb12 {
    margin-bottom: 12px;
}
.ml4 {
    margin-left: 4px;
}
.sub {
    color: var(--el-text-color-secondary);
    font-size: 12px;
}
.rule-line {
    line-height: 1.7;
}
.region {
    margin-right: 8px;
}
.pager {
    margin-top: 12px;
    justify-content: flex-end;
}
</style>
