<script setup lang="ts">
// 同城配送模板（00111）。列表 + 新建 / 编辑对话框 + 删除 + 设为默认。
//
// 只有全店范围的员工（管理员、操作员）能写，任何员工都能读（门店管理员在自己
// 门店的「同城配送」页要能挑模板）——can.editLocalDeliveryTemplates（auth/permissions.ts）。

import { onMounted, ref } from "vue";
import { Plus, Refresh } from "@element-plus/icons-vue";
import { ElMessageBox } from "element-plus";
import { keel } from "../../api/client.ts";
import { centsToYuanInput } from "../../api/money.ts";
import { tiersSummary } from "../../api/localDeliveryRules.ts";
import { listLocalDeliveryTemplates, type LocalDeliveryTemplate, type LocalDeliveryTemplateInput } from "../../api/localDeliveryTemplates.ts";
import { can, NO_PERMISSION } from "../../auth/permissions.ts";
import { datetime } from "../../ui/format.ts";
import { notifyError, notifyOk } from "../../ui/notify.ts";
import ProblemAlert from "../../components/ProblemAlert.vue";
import LocalDeliveryTemplateDialog from "./LocalDeliveryTemplateDialog.vue";

const loading = ref(false);
const error = ref<unknown>(null);
const templates = ref<LocalDeliveryTemplate[]>([]);

async function load(): Promise<void> {
    loading.value = true;
    error.value = null;
    try {
        templates.value = await listLocalDeliveryTemplates();
    } catch (err) {
        error.value = err;
    } finally {
        loading.value = false;
    }
}

onMounted(load);

// ---------------------------------------------------------------- 对话框

const dialogVisible = ref(false);
const current = ref<LocalDeliveryTemplate | null>(null);

function openCreate(): void {
    current.value = null;
    dialogVisible.value = true;
}
function openEdit(t: LocalDeliveryTemplate): void {
    current.value = t;
    dialogVisible.value = true;
}
async function onSaved(t: LocalDeliveryTemplate): Promise<void> {
    notifyOk(`已保存「${t.name}」`);
    await load();
}

async function setDefault(t: LocalDeliveryTemplate): Promise<void> {
    const body: LocalDeliveryTemplateInput = {
        name: t.name,
        is_default: true,
        min_order_cents: t.min_order_cents,
        free_over_cents: t.free_over_cents,
        fee_tiers: t.fee_tiers,
    };
    try {
        const saved = await keel.request("put", "/admin/local-delivery-templates/{template_id}", { path: { template_id: t.id }, body });
        notifyOk(`已把「${saved.name}」设为默认模板`);
        await load();
    } catch (err) {
        notifyError(err);
    }
}

async function remove(t: LocalDeliveryTemplate): Promise<void> {
    try {
        await ElMessageBox.confirm(
            t.is_default
                ? "它是默认模板，删不掉——先把别的模板设为默认，或者先确认没有围栏店需要靠它兜底。"
                : (t.store_count ?? 0) > 0
                  ? `还有 ${t.store_count} 家门店在引用它，删不掉——先去这些门店的「同城配送」页改挂别的模板或自定义。`
                  : `删除「${t.name}」？`,
            "确认删除",
            { type: "warning" },
        );
    } catch {
        return;
    }
    try {
        await keel.request("delete", "/admin/local-delivery-templates/{template_id}", { path: { template_id: t.id } });
        notifyOk("已删除");
        await load();
    } catch (err) {
        notifyError(err);
    }
}
</script>

<template>
    <div>
        <ProblemAlert v-if="error" :error="error" />
        <el-alert type="info" :closable="false" show-icon class="mb12">
            新开的围栏门店不用配置，自动按默认模板收费；改模板对所有使用它的门店立即生效。
        </el-alert>
        <div class="page-toolbar">
            <span class="grow" />
            <el-button :icon="Refresh" :loading="loading" @click="load">刷新</el-button>
            <el-button
                type="primary"
                :icon="Plus"
                :disabled="!can.editLocalDeliveryTemplates()"
                :title="can.editLocalDeliveryTemplates() ? '' : NO_PERMISSION"
                @click="openCreate"
            >新建模板</el-button>
        </div>

        <el-table :data="templates" v-loading="loading" border stripe>
            <el-table-column prop="id" label="ID" width="64" />
            <el-table-column label="名称" min-width="160">
                <template #default="{ row }: { row: LocalDeliveryTemplate }">
                    {{ row.name }}
                    <el-tag v-if="row.is_default" size="small" type="success" class="ml4">默认</el-tag>
                </template>
            </el-table-column>
            <el-table-column label="起送价" width="110">
                <template #default="{ row }: { row: LocalDeliveryTemplate }">
                    {{ row.min_order_cents > 0 ? `${centsToYuanInput(row.min_order_cents)} 元` : "不设" }}
                </template>
            </el-table-column>
            <el-table-column label="免配送费门槛" width="130">
                <template #default="{ row }: { row: LocalDeliveryTemplate }">
                    {{ row.free_over_cents > 0 ? `${centsToYuanInput(row.free_over_cents)} 元` : "不设" }}
                </template>
            </el-table-column>
            <el-table-column label="距离分档" min-width="260">
                <template #default="{ row }: { row: LocalDeliveryTemplate }">{{ tiersSummary(row.fee_tiers) }}</template>
            </el-table-column>
            <el-table-column label="在用门店数" width="100">
                <template #default="{ row }: { row: LocalDeliveryTemplate }">{{ row.store_count ?? 0 }}</template>
            </el-table-column>
            <el-table-column label="更新时间" width="170">
                <template #default="{ row }: { row: LocalDeliveryTemplate }">
                    <span class="sub">{{ datetime(row.updated_at) }}</span>
                </template>
            </el-table-column>
            <el-table-column label="操作" width="200" fixed="right">
                <template #default="{ row }: { row: LocalDeliveryTemplate }">
                    <el-button
                        link
                        type="primary"
                        :disabled="!can.editLocalDeliveryTemplates()"
                        :title="can.editLocalDeliveryTemplates() ? '' : NO_PERMISSION"
                        @click="openEdit(row)"
                    >编辑</el-button>
                    <el-button
                        link
                        type="primary"
                        :disabled="!can.editLocalDeliveryTemplates() || row.is_default"
                        :title="can.editLocalDeliveryTemplates() ? '' : NO_PERMISSION"
                        @click="setDefault(row)"
                    >设为默认</el-button>
                    <el-button
                        link
                        type="danger"
                        :disabled="!can.editLocalDeliveryTemplates()"
                        :title="can.editLocalDeliveryTemplates() ? '' : NO_PERMISSION"
                        @click="remove(row)"
                    >删除</el-button>
                </template>
            </el-table-column>
        </el-table>
        <el-empty v-if="!loading && templates.length === 0" description="还没有同城配送模板" :image-size="70" />

        <LocalDeliveryTemplateDialog v-model="dialogVisible" :template="current" @saved="onSaved" />
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
</style>
