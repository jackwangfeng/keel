<script setup lang="ts">
// 左侧菜单 + 右侧内容。菜单项从 src/router/modules 的声明里来，
// 不是另抄一份 —— 两份清单迟早对不上，而对不上的症状是「路由能到、
// 菜单上没有」，一个没人会主动去看的状态。
import { computed } from "vue";
import { useRoute, useRouter } from "vue-router";
import { SwitchButton } from "@element-plus/icons-vue";
import { currentSession, keel, setSession } from "../api/client.ts";
import { notifyError } from "../ui/notify.ts";
import { sections } from "../router/modules/index.ts";

const route = useRoute();
const router = useRouter();

const session = computed(() => currentSession());

/** 平台级操作员：`staff.merchant_id` 为 null（数据模型 §14 的两级身份）。 */
const isPlatform = computed(() => session.value?.staff.merchant_id === null);

const roleText = computed(() => {
    const staff = session.value?.staff;
    if (staff === undefined) return "";
    const role = staff.role === 1 ? "管理员" : "操作员";
    return isPlatform.value ? `平台级${role}` : `商家级${role}`;
});

interface MenuItem {
    path: string;
    title: string;
    icon: (typeof sections)[number]["icon"];
}

const menu = computed<MenuItem[]>(() =>
    sections
        .filter((s) => s.platformOnly !== true || isPlatform.value)
        .flatMap((s) =>
            s.routes
                .filter((r) => r.meta?.menu === true)
                .map((r) => ({ path: `/${String(r.path)}`, title: r.meta?.title ?? s.title, icon: s.icon })),
        ),
);

/** 当前高亮哪一项。详情页要高亮它所属的列表项，所以用前缀匹配。 */
const activeMenu = computed(() => {
    const here = route.path;
    const hit = menu.value
        .filter((m) => here === m.path || here.startsWith(`${m.path}/`))
        .sort((a, b) => b.path.length - a.path.length)[0];
    return hit?.path ?? here;
});

async function logout(): Promise<void> {
    // 契约里**没有** `POST /admin/auth/logout`（买家侧有，后台侧没有）。
    // 所以这里只清本地会话——服务端那条 staff_tokens 会自己到期。
    // 不假装调用一个不存在的接口，也不在界面上说「已登出所有设备」。
    void keel;
    setSession(null);
    await router.push({ name: "login" });
}

function copyToken(): void {
    const token = session.value?.token;
    if (token === undefined) return;
    navigator.clipboard.writeText(token).then(
        () => undefined,
        (err: unknown) => notifyError(err),
    );
}
</script>

<template>
    <el-container class="shell">
        <el-aside width="200px" class="aside">
            <div class="brand">
                <span class="brand-name">Keel</span>
                <span class="brand-sub">商家后台</span>
            </div>
            <el-menu :default-active="activeMenu" router class="menu">
                <el-menu-item v-for="item in menu" :key="item.path" :index="item.path">
                    <el-icon><component :is="item.icon" /></el-icon>
                    <span>{{ item.title }}</span>
                </el-menu-item>
            </el-menu>
        </el-aside>

        <el-container>
            <el-header class="header">
                <div class="crumb">{{ route.meta.title ?? "" }}</div>
                <div class="who">
                    <el-tag v-if="isPlatform" type="warning" size="small" effect="dark">平台级</el-tag>
                    <span class="who-text">{{ session?.staff.email }}（{{ roleText }}）</span>
                    <el-button link size="small" @click="copyToken">复制会话 token</el-button>
                    <el-button link size="small" :icon="SwitchButton" @click="logout">退出</el-button>
                </div>
            </el-header>
            <el-main class="main">
                <router-view />
            </el-main>
        </el-container>
    </el-container>
</template>

<style scoped>
.shell {
    height: 100vh;
}
.aside {
    background: #20222a;
    display: flex;
    flex-direction: column;
}
.brand {
    height: 60px;
    display: flex;
    align-items: baseline;
    gap: 6px;
    padding: 0 18px;
    color: #fff;
}
.brand-name {
    font-size: 20px;
    font-weight: 700;
    letter-spacing: 1px;
}
.brand-sub {
    font-size: 12px;
    opacity: 0.6;
}
.menu {
    flex: 1;
    border-right: none;
    background: transparent;
    --el-menu-bg-color: transparent;
    --el-menu-text-color: #c9ccd4;
    --el-menu-hover-bg-color: #2b2e38;
    --el-menu-active-color: #ffd04b;
}
.header {
    display: flex;
    align-items: center;
    justify-content: space-between;
    border-bottom: 1px solid var(--el-border-color-light);
    background: #fff;
}
.crumb {
    font-size: 16px;
    font-weight: 600;
}
.who {
    display: flex;
    align-items: center;
    gap: 8px;
    font-size: 13px;
    color: var(--el-text-color-regular);
}
.main {
    background: var(--el-bg-color-page);
}
</style>
