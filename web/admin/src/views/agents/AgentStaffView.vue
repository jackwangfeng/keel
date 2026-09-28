<script setup lang="ts">
// AI 员工：提案 / 简报 / 员工与密钥，三个 tab（AI 经营 M9）。
//
// tab 状态同步进 URL 的 `?tab=`，与门店详情页同一个做法（StoreDetailView.vue）——
// 首页横幅链过来的就是带着 `?tab=proposals` 的这一条路由。
import { ref } from "vue";
import { useRoute, useRouter } from "vue-router";
import { can } from "../../auth/permissions.ts";
import ProposalsTab from "./ProposalsTab.vue";
import BriefsTab from "./BriefsTab.vue";
import AgentsTab from "./AgentsTab.vue";

const route = useRoute();
const router = useRouter();

const TABS = ["proposals", "briefs", "staff"] as const;
const tabFromQuery = typeof route.query["tab"] === "string" ? route.query["tab"] : "proposals";
const tab = ref<(typeof TABS)[number]>(TABS.includes(tabFromQuery as (typeof TABS)[number]) ? (tabFromQuery as (typeof TABS)[number]) : "proposals");

function onTabChange(name: string | number): void {
    void router.replace({ query: { ...route.query, tab: String(name) } });
}

const canSeeBriefs = can.seeAgentBriefs();
const canManageAgents = can.manageAgents();
</script>

<template>
    <div>
        <el-tabs v-model="tab" type="border-card" @tab-change="onTabChange">
            <el-tab-pane label="提案" name="proposals">
                <ProposalsTab />
            </el-tab-pane>
            <el-tab-pane v-if="canSeeBriefs" label="简报" name="briefs" lazy>
                <BriefsTab />
            </el-tab-pane>
            <el-tab-pane v-if="canManageAgents" label="AI 员工与密钥" name="staff" lazy>
                <AgentsTab />
            </el-tab-pane>
        </el-tabs>
        <p v-if="!canSeeBriefs || !canManageAgents" class="hint foot">
            <span v-if="!canSeeBriefs">简报只有全店范围的人（管理员 / 操作员）能看。</span>
            <span v-if="!canManageAgents">员工与密钥的管理只有本店管理员能做。</span>
        </p>
    </div>
</template>

<style scoped>
.foot {
    margin-top: 8px;
}
</style>
