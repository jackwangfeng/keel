<script setup lang="ts">
// 门店列表。
//
// 两个状态必须醒目，因为它们都「不报错，只是悄悄没生意」：
//
//   · **没有默认门店**（`has_default = false`）：所有不在任何围栏内的买家
//     都会拿到 match_type = none，看到「不在服务范围」——那看起来像
//     「商品没上架」，和真因毫无关系。
//   · **非默认、没围栏**：不被任何坐标命中，也不是回落目标，一家接不到
//     任何单的店。建店按契约不收围栏，所以这是每家新店都会经过的中间态，
//     这里标成「未完成」。

import { computed, onMounted, ref } from "vue";
import { useRoute, useRouter } from "vue-router";
import { Plus, Refresh } from "@element-plus/icons-vue";
import { ElMessageBox } from "element-plus";
import { keel, type AdminRegion, type AdminStore, type AdminStoreList, type StoreCreateRequest } from "../api/client.ts";
import { IdempotentSubmission, withIdempotency } from "../api/idempotency.ts";
import { isIncomplete, listAllRegions } from "../api/stores.ts";
import { notifyError, notifyOk } from "../ui/notify.ts";
import { can, NO_PERMISSION } from "../auth/permissions.ts";
import ProblemAlert from "../components/ProblemAlert.vue";

const route = useRoute();
const router = useRouter();

const loading = ref(false);
const error = ref<unknown>(null);
const page = ref<AdminStoreList | null>(null);
const regions = ref<AdminRegion[]>([]);
const pageNo = ref(1);
const regionFilter = ref<number | undefined>(
    typeof route.query["region_id"] === "string" ? Number(route.query["region_id"]) : undefined,
);
const includeDeleted = ref(false);

async function load(): Promise<void> {
    loading.value = true;
    error.value = null;
    try {
        page.value = await keel.get("/admin/stores", {
            query: {
                page: pageNo.value,
                page_size: 20,
                ...(regionFilter.value === undefined ? {} : { region_id: regionFilter.value }),
                ...(includeDeleted.value ? { include_deleted: true } : {}),
            },
        });
    } catch (err) {
        error.value = err;
    } finally {
        loading.value = false;
    }
}

onMounted(() => {
    void load();
    listAllRegions().then(
        (rs) => (regions.value = rs),
        (e: unknown) => notifyError(e),
    );
});

const incompleteCount = computed(() => (page.value?.items ?? []).filter((s) => !s.deleted_at && isIncomplete(s)).length);

// ---------------------------------------------------------------- 设默认

async function makeDefault(row: AdminStore): Promise<void> {
    try {
        await keel.request("put", "/admin/stores/{store_id}/default", { path: { store_id: row.id } });
        notifyOk(`「${row.name}」现在是默认门店：不在任何围栏内的买家都回落到它`);
        await load();
    } catch (err) {
        notifyError(err);
    }
}

// ---------------------------------------------------------------- 新建

const createVisible = ref(false);
const createError = ref<unknown>(null);
const creating = ref(false);
const submission = new IdempotentSubmission();
const draft = ref<StoreCreateRequest>({ region_id: 0, code: "", name: "" });

function openCreate(): void {
    draft.value = {
        region_id: regionFilter.value ?? regions.value[0]?.id ?? 0,
        code: "",
        name: "",
        // 没有默认店时，第一家默认勾上：这是最常见的意图，而且不勾会立刻
        // 落进「全体访客不在服务范围」那个状态。已有默认店时不勾——勾了必 409。
        is_default: page.value?.has_default === false,
    };
    createError.value = null;
    submission.rotate();
    createVisible.value = true;
}

async function submitCreate(): Promise<void> {
    creating.value = true;
    createError.value = null;
    try {
        const body: StoreCreateRequest = { ...draft.value };
        // 空字符串的可选字段不发：服务端会把 "" 当成一个值存下来。
        for (const k of ["phone", "province", "city", "district", "address"] as const) {
            if (body[k] === "") delete body[k];
        }
        // el-input-number 清空时给 null，而契约里 lat / lng 是 number（可省略，不可为 null）。
        for (const k of ["lat", "lng"] as const) {
            if (body[k] === null || body[k] === undefined) delete body[k];
        }
        const created = await withIdempotency(submission, (key) =>
            keel.request("post", "/admin/stores", { body, headers: { "Idempotency-Key": key } }),
        );
        createVisible.value = false;
        notifyOk(
            created.is_default
                ? `已建「${created.name}」（默认门店）。还可以给它画围栏。`
                : `已建「${created.name}」。它还没有围栏，接不到任何单——去画围栏。`,
        );
        await router.push({ name: "store-detail", params: { storeId: created.id }, query: { tab: "fence" } });
    } catch (err) {
        createError.value = err;
    } finally {
        creating.value = false;
    }
}

async function remove(row: AdminStore): Promise<void> {
    try {
        await ElMessageBox.confirm(
            row.is_default
                ? `「${row.name}」是默认门店。删掉之后这家商家就没有回落目标了：不在任何围栏内的买家都会看到「不在服务范围」。确定删？`
                : `软删门店「${row.name}」？`,
            "确认删除",
            { type: "warning" },
        );
    } catch {
        return;
    }
    try {
        await keel.request("delete", "/admin/stores/{store_id}", { path: { store_id: row.id } });
        notifyOk("已软删");
        await load();
    } catch (err) {
        notifyError(err);
    }
}
</script>

<template>
    <div>
        <ProblemAlert v-if="error" :error="error" />

        <el-alert v-if="page && !page.has_default" type="error" :closable="false" show-icon class="mb12">
            <template #title>还没有默认门店</template>
            所有<b>不在任何门店围栏里</b>的买家（包括没授权定位的）现在都会看到「不在服务范围」——这看起来像商品没上架，其实是没有回落门店。
            在下面挑一家点「设为默认」，它就成为全国配送的回落目标。
        </el-alert>
        <el-alert v-if="incompleteCount > 0" type="warning" :closable="false" show-icon class="mb12">
            <template #title>这一页有 {{ incompleteCount }} 家门店「未完成」</template>
            它们没有围栏、也不是默认店：不会被任何买家坐标命中，接不到任何单。点进去画围栏。
        </el-alert>

        <div class="page-toolbar">
            <el-select v-model="regionFilter" placeholder="全部大区" clearable style="width: 180px" @change="(pageNo = 1), load()">
                <el-option v-for="r in regions" :key="r.id" :value="r.id" :label="r.name" />
            </el-select>
            <el-checkbox v-model="includeDeleted" @change="(pageNo = 1), load()">含已软删</el-checkbox>
            <span class="grow" />
            <el-button :icon="Refresh" :loading="loading" @click="load">刷新</el-button>
            <el-button type="primary" :icon="Plus" :disabled="regions.length === 0 || !regions.some((r) => can.createStoreIn(r.id))" @click="openCreate">新建门店</el-button>
        </div>
        <p v-if="regions.length === 0" class="hint">门店必须属于一个大区（stores.region_id NOT NULL）——先去「大区」建一个。</p>

        <el-table :data="page?.items ?? []" v-loading="loading" border stripe>
            <el-table-column prop="id" label="ID" width="70" />
            <el-table-column prop="code" label="编号" width="120" />
            <el-table-column label="名称" min-width="180">
                <template #default="{ row }: { row: AdminStore }">
                    <router-link :to="{ name: 'store-detail', params: { storeId: row.id } }">{{ row.name }}</router-link>
                    <el-tag v-if="row.is_default" type="primary" size="small" effect="dark" class="ml4">默认</el-tag>
                    <el-tag v-if="row.deleted_at" type="danger" size="small" class="ml4">已删</el-tag>
                </template>
            </el-table-column>
            <el-table-column label="大区" width="130">
                <template #default="{ row }: { row: AdminStore }">{{ row.region_name ?? `#${row.region_id}` }}</template>
            </el-table-column>
            <el-table-column label="状态" width="100">
                <template #default="{ row }: { row: AdminStore }">
                    <el-tag :type="row.status === 1 ? 'success' : 'info'" size="small">{{ row.status === 1 ? "营业" : "停业" }}</el-tag>
                </template>
            </el-table-column>
            <el-table-column label="围栏 / 接单" min-width="230">
                <template #default="{ row }: { row: AdminStore }">
                    <template v-if="isIncomplete(row)">
                        <el-tag type="warning" size="small" effect="dark">未完成</el-tag>
                        <span class="warn-text">没围栏也不是默认店，接不到任何单</span>
                    </template>
                    <template v-else-if="row.fence">
                        <el-tag type="success" size="small">已配围栏</el-tag>
                        <span v-if="row.is_default" class="hint"> · 并且是回落目标</span>
                    </template>
                    <template v-else>
                        <el-tag size="small">无围栏</el-tag>
                        <span class="hint"> · 只作为回落目标</span>
                    </template>
                </template>
            </el-table-column>
            <el-table-column label="地址" min-width="180">
                <template #default="{ row }: { row: AdminStore }">
                    {{ [row.city, row.district, row.address].filter(Boolean).join(" ") || "—" }}
                </template>
            </el-table-column>
            <el-table-column label="操作" width="250" fixed="right">
                <template #default="{ row }: { row: AdminStore }">
                    <el-button link type="primary" @click="router.push({ name: 'store-detail', params: { storeId: row.id } })">
                        管理
                    </el-button>
                    <el-button
                        link
                        type="primary"
                        :disabled="row.is_default || !!row.deleted_at || row.status !== 1 || !can.setDefaultStore()"
                        :title="!can.setDefaultStore() ? NO_PERMISSION : row.status !== 1 ? '停业的门店不能作为回落目标' : ''"
                        @click="makeDefault(row)"
                    >
                        设为默认
                    </el-button>
                    <el-button link type="danger" :disabled="!!row.deleted_at || !can.manageStore(row)" @click="remove(row)">删除</el-button>
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

        <el-dialog v-model="createVisible" title="新建门店" width="600px">
            <ProblemAlert v-if="createError" :error="createError" />
            <p class="hint">围栏不在这里填——建好之后在门店页的地图上画。先建店、再画围栏，是契约定的两步。</p>
            <el-form label-width="100px" @submit.prevent>
                <el-form-item label="大区" required>
                    <el-select v-model="draft.region_id">
                        <el-option v-for="r in regions" :key="r.id" :value="r.id" :label="r.name" />
                    </el-select>
                </el-form-item>
                <el-form-item label="编号" required><el-input v-model="draft.code" placeholder="租户内唯一" /></el-form-item>
                <el-form-item label="名称" required><el-input v-model="draft.name" /></el-form-item>
                <el-form-item label="电话"><el-input v-model="draft.phone" /></el-form-item>
                <el-form-item label="省 / 市 / 区">
                    <div class="row3">
                        <el-input v-model="draft.province" placeholder="省" />
                        <el-input v-model="draft.city" placeholder="市" />
                        <el-input v-model="draft.district" placeholder="区" />
                    </div>
                </el-form-item>
                <el-form-item label="地址"><el-input v-model="draft.address" /></el-form-item>
                <el-form-item label="坐标">
                    <div class="row3">
                        <el-input-number v-model="draft.lat" :precision="6" :step="0.001" :min="-90" :max="90" placeholder="纬度" controls-position="right" />
                        <el-input-number v-model="draft.lng" :precision="6" :step="0.001" :min="-180" :max="180" placeholder="经度" controls-position="right" />
                    </div>
                    <p class="hint">WGS-84（GPS 原始坐标）。别从高德 / 百度地图上抄——那是偏过的坐标。可以不填。</p>
                </el-form-item>
                <el-form-item label="默认门店">
                    <el-checkbox v-model="draft.is_default" :disabled="page?.has_default === true">
                        设为全国配送的回落门店
                    </el-checkbox>
                    <p v-if="page?.has_default" class="hint">
                        已经有默认门店了。在这里勾上会被服务端拒绝（default-store-conflict）——要换默认店，建好之后在列表里点「设为默认」。
                    </p>
                </el-form-item>
            </el-form>
            <template #footer>
                <el-button @click="createVisible = false">取消</el-button>
                <el-button
                    type="primary"
                    :loading="creating"
                    :disabled="draft.code.trim() === '' || draft.name.trim() === '' || draft.region_id === 0"
                    @click="submitCreate"
                >
                    创建
                </el-button>
            </template>
        </el-dialog>
    </div>
</template>

<style scoped>
.mb12 {
    margin-bottom: 12px;
}
.ml4 {
    margin-left: 4px;
}
.warn-text {
    margin-left: 6px;
    font-size: 12px;
    color: var(--el-color-warning-dark-2);
}
.row3 {
    display: flex;
    gap: 8px;
    width: 100%;
}
.pager {
    margin-top: 12px;
    justify-content: flex-end;
}
</style>
