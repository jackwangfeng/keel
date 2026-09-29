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
import { useMobile } from "../../ui/useMobile.ts";
import ProblemAlert from "../../components/ProblemAlert.vue";

const mobile = useMobile();

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

        <el-table v-if="!mobile" :data="page?.items ?? []" v-loading="loading" border stripe @row-click="openDetail" class="clickable">
            <el-table-column label="标题" min-width="240" show-overflow-tooltip>
                <template #default="{ row }: { row: AgentBrief }">
                    <span class="muted">#{{ row.id }}</span>
                    <!-- 更正（00141）：旧的原样保留，标「已更正」并指向新的那份 -->
                    <el-tag v-if="row.corrected_by_brief_id" type="warning" size="small" class="tag">已更正，见 #{{ row.corrected_by_brief_id }}</el-tag>
                    <el-tag v-else-if="row.corrects_brief_id" type="info" size="small" class="tag">更正 #{{ row.corrects_brief_id }}</el-tag>
                    {{ row.title }}
                </template>
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

        <!-- 手机：卡片列表，点开看全文 -->
        <div v-else class="brief-cards" v-loading="loading">
            <el-card v-for="row in page?.items ?? []" :key="row.id" shadow="never" class="brief-card" @click="openDetail(row)">
                <div class="card-title">
                    <span class="muted">#{{ row.id }}</span>
                    {{ row.title }}
                </div>
                <div class="card-tags" v-if="row.corrected_by_brief_id || row.corrects_brief_id">
                    <el-tag v-if="row.corrected_by_brief_id" type="warning" size="small">已更正，见 #{{ row.corrected_by_brief_id }}</el-tag>
                    <el-tag v-else-if="row.corrects_brief_id" type="info" size="small">更正 #{{ row.corrects_brief_id }}</el-tag>
                </div>
                <div class="card-meta muted">
                    <span>{{ row.agent_name }}</span>
                    <span>{{ row.period_start }} ~ {{ row.period_end }}</span>
                    <span>{{ datetime(row.created_at) }}</span>
                </div>
            </el-card>
        </div>

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
            <el-alert v-if="detail?.corrected_by_brief_id" type="warning" :closable="false" show-icon
                :title="`这份简报有错，已由 #${detail.corrected_by_brief_id} 更正（下面是原文，保留不改）`" />
            <el-alert v-else-if="detail?.corrects_brief_id" type="info" :closable="false" show-icon
                :title="`这份是对 #${detail.corrects_brief_id} 的更正`" />
            <p class="body">{{ detail?.body }}</p>
        </el-dialog>
    </div>
</template>

<style scoped>
.tag {
    margin: 0 6px;
}
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

/* 手机卡片列表 */
.brief-cards {
    display: flex;
    flex-direction: column;
    gap: 10px;
    min-height: 60px;
}
.brief-card {
    cursor: pointer;
}
.brief-card :deep(.el-card__body) {
    padding: 12px 14px;
}
.card-title {
    font-size: 15px;
    font-weight: 600;
    word-break: break-word;
}
.card-title .muted {
    font-weight: 400;
    margin-right: 4px;
}
.card-tags {
    margin-top: 4px;
}
.card-meta {
    display: flex;
    flex-wrap: wrap;
    gap: 8px;
    margin-top: 6px;
    font-size: 12px;
}
</style>
