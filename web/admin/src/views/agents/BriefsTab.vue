<script setup lang="ts">
// AI 员工写的经营简报（post_brief）。全店口径，只给全店范围的人看——
// 服务端对其他角色回 403 role-forbidden，这里在能进这个 tab 的前提下才会调它
// （AgentStaffView 按 can.seeAgentBriefs() 决定要不要渲染这个 tab）。
//
// body 是 markdown，但**按不可信输入渲染，不渲染 HTML**：只用 white-space: pre-wrap，
// 不接 markdown 解析器（解析器一样能被喂出 XSS 相关的边角情况）。
import { ref, onMounted } from "vue";
import { Refresh } from "@element-plus/icons-vue";
import { keel, type AgentBrief, type AgentBriefPage } from "../../api/client.ts";
import { datetime } from "../../ui/format.ts";
import ProblemAlert from "../../components/ProblemAlert.vue";

const loading = ref(false);
const error = ref<unknown>(null);
const page = ref<AgentBriefPage | null>(null);
const pageNo = ref(1);
const pageSize = ref(20);

async function load(): Promise<void> {
    loading.value = true;
    error.value = null;
    try {
        page.value = await keel.get("/admin/agent-briefs", { query: { page: pageNo.value, page_size: pageSize.value } });
    } catch (err) {
        error.value = err;
    } finally {
        loading.value = false;
    }
}

onMounted(() => void load());
defineExpose({ reload: load });

// 列表项与详情是契约里同一个形状（AgentBrief 一个类型两处都用），body 已经在列表里，
// 不用为了「看全文」再打一次 GET /admin/agent-briefs/{id}。
const detailVisible = ref(false);
const detail = ref<AgentBrief | null>(null);
function openDetail(row: AgentBrief): void {
    detail.value = row;
    detailVisible.value = true;
}
</script>

<template>
    <div>
        <ProblemAlert v-if="error" :error="error" />

        <div class="page-toolbar">
            <span class="hint">新的在前。点一行看全文。</span>
            <span class="grow" />
            <el-button :icon="Refresh" :loading="loading" @click="load">刷新</el-button>
        </div>

        <el-table :data="page?.items ?? []" v-loading="loading" border stripe @row-click="openDetail" class="clickable">
            <el-table-column label="标题" min-width="240" show-overflow-tooltip>
                <template #default="{ row }: { row: AgentBrief }">{{ row.title }}</template>
            </el-table-column>
            <el-table-column label="AI 员工" width="120" show-overflow-tooltip>
                <template #default="{ row }: { row: AgentBrief }">{{ row.agent_name }}</template>
            </el-table-column>
            <el-table-column label="覆盖周期" width="200">
                <template #default="{ row }: { row: AgentBrief }">{{ row.period_start }} ~ {{ row.period_end }}</template>
            </el-table-column>
            <el-table-column label="生成时间" width="170">
                <template #default="{ row }: { row: AgentBrief }">{{ datetime(row.created_at) }}</template>
            </el-table-column>
        </el-table>

        <el-empty v-if="!loading && (page?.items.length ?? 0) === 0" description="还没有简报" />

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

        <el-dialog v-model="detailVisible" :title="detail?.title ?? ''" width="640px">
            <p class="muted">{{ detail?.agent_name }} · {{ detail?.period_start }} ~ {{ detail?.period_end }} · {{ datetime(detail?.created_at) }}</p>
            <p class="body">{{ detail?.body }}</p>
        </el-dialog>
    </div>
</template>

<style scoped>
.pager {
    margin-top: 12px;
    justify-content: flex-end;
}
.clickable :deep(.el-table__row) {
    cursor: pointer;
}
.muted {
    color: var(--el-text-color-secondary);
    font-size: 12px;
    margin: 0 0 12px;
}
.body {
    white-space: pre-wrap;
    word-break: break-word;
    margin: 0;
    max-height: 60vh;
    overflow: auto;
}
</style>
