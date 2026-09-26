<script setup lang="ts">
// 顶栏铃铛：后台待办提醒（契约 GET /admin/notifications，数据模型 §16）。
//
// - 角标是**调用者自己**范围内的未读数（大区 / 门店管理员只算自己范围里的门店），
//   每 UNREAD_POLL_MS 轮询一次 /admin/notifications/unread-count，切页时也刷一次；
// - 点开取最近 DROPDOWN_PAGE_SIZE 条（响应里带未读数，角标顺手校准）；
// - 点一条：先标已读（响应就是新的未读数），再跳到订单 / 售后 / 门店库存；
// - 「全部已读」只动自己的已读状态，别的员工不受影响（契约原话：已读每人一份）。
//
// 标题与正文原样展示，不按 kind 拼（api/notifications.ts 文件头）。
// 取不到就安静地不显示角标：铃铛是提醒，不该因为它让整个框架报错。

import { onBeforeUnmount, onMounted, ref, watch } from "vue";
import { useRoute, useRouter } from "vue-router";
import { Bell } from "@element-plus/icons-vue";
import { keel } from "../api/client.ts";
import {
    DROPDOWN_PAGE_SIZE,
    NOTIFICATION_TAG,
    UNREAD_POLL_MS,
    badgeText,
    notificationLocation,
    type Notification,
} from "../api/notifications.ts";
import { datetime } from "../ui/format.ts";
import { notifyError } from "../ui/notify.ts";

const route = useRoute();
const router = useRouter();

const unread = ref(0);
const items = ref<Notification[]>([]);
const loading = ref(false);
const open = ref(false);

async function refreshUnread(): Promise<void> {
    try {
        const res = await keel.get("/admin/notifications/unread-count", {});
        unread.value = res.unread_count;
    } catch {
        // 见文件头：取不到就不显示，不打扰。
    }
}

async function loadList(): Promise<void> {
    loading.value = true;
    try {
        const res = await keel.get("/admin/notifications", { query: { page: 1, page_size: DROPDOWN_PAGE_SIZE } });
        items.value = res.items;
        unread.value = res.unread_count;
    } catch (err) {
        notifyError(err);
    } finally {
        loading.value = false;
    }
}

async function openItem(n: Notification): Promise<void> {
    if (n.read_at === null) {
        try {
            const res = await keel.request("post", "/admin/notifications/{notification_id}/read", {
                path: { notification_id: n.id },
            });
            unread.value = res.unread_count;
            n.read_at = new Date().toISOString();
        } catch (err) {
            notifyError(err);
        }
    }
    const to = notificationLocation(n);
    open.value = false;
    if (to !== null) await router.push(to);
}

async function readAll(): Promise<void> {
    try {
        const res = await keel.request("post", "/admin/notifications/read-all", {});
        unread.value = res.unread_count;
        const now = new Date().toISOString();
        for (const n of items.value) if (n.read_at === null) n.read_at = now;
    } catch (err) {
        notifyError(err);
    }
}

let timer: ReturnType<typeof setInterval> | undefined;
onMounted(() => {
    void refreshUnread();
    timer = setInterval(() => void refreshUnread(), UNREAD_POLL_MS);
});
onBeforeUnmount(() => {
    if (timer !== undefined) clearInterval(timer);
});
watch(() => route.fullPath, () => void refreshUnread());
watch(open, (v) => {
    if (v) void loadList();
});
</script>

<template>
    <el-popover v-model:visible="open" placement="bottom-end" :width="380" trigger="click">
        <template #reference>
            <el-badge :value="badgeText(unread)" :hidden="unread === 0" class="bell" data-test="notification-bell">
                <el-button link :icon="Bell" aria-label="待办提醒" />
            </el-badge>
        </template>
        <div class="head">
            <span class="head-title">待办提醒</span>
            <el-button link size="small" type="primary" :disabled="unread === 0" @click="readAll">全部已读</el-button>
        </div>
        <div v-loading="loading" class="list">
            <el-empty v-if="!loading && items.length === 0" description="暂时没有提醒" :image-size="60" />
            <div
                v-for="n in items"
                :key="n.id"
                class="item"
                :class="{ unread: n.read_at === null }"
                role="button"
                tabindex="0"
                @click="openItem(n)"
                @keydown.enter="openItem(n)"
            >
                <div class="item-head">
                    <el-tag size="small" :type="NOTIFICATION_TAG[n.kind]" effect="plain">{{ n.title }}</el-tag>
                    <span class="time">{{ datetime(n.created_at) }}</span>
                </div>
                <div class="body">{{ n.body }}</div>
            </div>
        </div>
    </el-popover>
</template>

<style scoped>
.bell {
    margin-right: 4px;
}
.head {
    display: flex;
    align-items: center;
    justify-content: space-between;
    padding-bottom: 6px;
    border-bottom: 1px solid var(--el-border-color-lighter);
}
.head-title {
    font-weight: 600;
}
.list {
    max-height: 420px;
    overflow-y: auto;
    min-height: 60px;
}
.item {
    padding: 8px 4px;
    border-bottom: 1px solid var(--el-border-color-extra-light);
    cursor: pointer;
    opacity: 0.65;
}
.item.unread {
    opacity: 1;
}
.item:hover {
    background: var(--el-fill-color-light);
}
.item-head {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 8px;
}
.time {
    font-size: 12px;
    color: var(--el-text-color-secondary);
}
.body {
    margin-top: 4px;
    font-size: 13px;
    line-height: 1.5;
    white-space: pre-wrap;
}
</style>
