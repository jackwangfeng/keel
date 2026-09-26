<script setup lang="ts">
// 后台登录。
//
// 契约里后台身份有三条路，**本轮只有两条真的能走通**，界面如实反映：
//
//   1. `POST /admin/auth/bootstrap` —— 第一个平台管理员。进程首次启动时
//      发现库里没有在岗平台管理员，就建一个并把一串一次性 token
//      **明文打进容器日志**（`docker compose logs app | grep bootstrap_token`）。
//      24 小时有效，用掉即失效。为什么是 stdout 而不是邮件：全新部署时 SMTP
//      大概率还没配，而这个项目的卖点是 `docker compose up` 一条命令。
//   2. `POST /admin/auth/session` —— 拿一次性登录 token 换会话。新员工由
//      `POST /admin/staff` 创建时会生成一串，本轮同样只能进日志。
//   3. `POST /admin/auth/email-link` —— 申请邮件链接。**服务端本轮回 501**
//      （没有接邮件服务）。按钮仍然留着并把那个 Problem 原样显示出来：
//      把它藏起来会让读者以为这条路存在，而点一下看到服务端自己说
//      「还没接邮件服务」是最准确的说明。

import { computed, ref } from "vue";
import { useRoute, useRouter } from "vue-router";
import { keel, setSession, type StaffSession } from "../api/client.ts";
import ProblemAlert from "../components/ProblemAlert.vue";
import { notifyOk } from "../ui/notify.ts";

const route = useRoute();
const router = useRouter();

const tab = ref<"bootstrap" | "token">("bootstrap");

const bootstrapToken = ref("");
const bootstrapEmail = ref("");
const loginToken = ref("");
const emailForLink = ref("");

const busy = ref(false);
const error = ref<unknown>(null);
const emailLinkNote = ref<unknown>(null);

const canBootstrap = computed(() => bootstrapToken.value.trim() !== "" && bootstrapEmail.value.trim() !== "");
const canExchange = computed(() => loginToken.value.trim() !== "");

async function finish(session: StaffSession): Promise<void> {
    setSession(session);
    notifyOk(`已登录：${session.staff.email}`);
    const redirect = route.query["redirect"];
    await router.push(typeof redirect === "string" && redirect !== "" ? redirect : "/");
}

async function doBootstrap(): Promise<void> {
    busy.value = true;
    error.value = null;
    try {
        // 请求体的形状来自契约（paths["/admin/auth/bootstrap"].post.requestBody）。
        // 这里手写一个 `{token, email}` 的 interface 也能跑，但契约改了不会红。
        const session = await keel.request("post", "/admin/auth/bootstrap", {
            body: { token: bootstrapToken.value.trim(), email: bootstrapEmail.value.trim() },
        });
        await finish(session);
    } catch (err) {
        error.value = err;
    } finally {
        busy.value = false;
    }
}

async function doExchange(): Promise<void> {
    busy.value = true;
    error.value = null;
    try {
        const session = await keel.request("post", "/admin/auth/session", {
            body: { token: loginToken.value.trim() },
        });
        await finish(session);
    } catch (err) {
        error.value = err;
    } finally {
        busy.value = false;
    }
}

async function doEmailLink(): Promise<void> {
    busy.value = true;
    emailLinkNote.value = null;
    try {
        await keel.request("post", "/admin/auth/email-link", { body: { email: emailForLink.value.trim() } });
        notifyOk("已受理。按契约，邮箱存在与否不在响应里区分。");
    } catch (err) {
        // 501 走这里。**不吞掉**：服务端那句话比前端能编的任何一句都准。
        emailLinkNote.value = err;
    } finally {
        busy.value = false;
    }
}
</script>

<template>
    <div class="login-page">
        <el-card class="login-card">
            <template #header>
                <div class="login-title">
                    <span class="brand">Keel</span>
                    <span>商家后台登录</span>
                </div>
            </template>

            <ProblemAlert v-if="error" :error="error" />

            <el-tabs v-model="tab">
                <el-tab-pane label="首次进入（引导 token）" name="bootstrap">
                    <p class="hint">
                        全新部署里，第一个平台级管理员由进程自己建出来，一次性引导 token
                        明文打在容器日志里，24 小时有效、用掉即失效：<br />
                        <code>docker compose logs app | grep bootstrap_token</code>
                    </p>
                    <el-form label-width="96px" @submit.prevent>
                        <el-form-item label="引导 token">
                            <el-input v-model="bootstrapToken" placeholder="日志里 bootstrap_token= 后面那一串" clearable />
                        </el-form-item>
                        <el-form-item label="你的邮箱">
                            <el-input v-model="bootstrapEmail" placeholder="补全自己的邮箱，后续找回用" clearable />
                        </el-form-item>
                        <el-form-item>
                            <el-button type="primary" :loading="busy" :disabled="!canBootstrap" @click="doBootstrap">
                                换取会话
                            </el-button>
                        </el-form-item>
                    </el-form>
                </el-tab-pane>

                <el-tab-pane label="已有登录 token" name="token">
                    <p class="hint">
                        管理员用「员工 → 新建」加人时，服务端会生成一串 15 分钟有效的一次性登录
                        token。本轮没有接邮件服务，它同样只出现在进程日志里
                        （<code>docker compose logs app | grep 登录链接</code>）。
                    </p>
                    <el-form label-width="96px" @submit.prevent>
                        <el-form-item label="登录 token">
                            <el-input v-model="loginToken" placeholder="一次性登录 token" clearable />
                        </el-form-item>
                        <el-form-item>
                            <el-button type="primary" :loading="busy" :disabled="!canExchange" @click="doExchange">
                                换取会话
                            </el-button>
                        </el-form-item>
                    </el-form>
                </el-tab-pane>
            </el-tabs>

            <el-divider />

            <div class="email-link">
                <p class="hint">
                    契约里还有一条「申请邮箱登录链接」。它在这个版本里<strong>没有实现</strong>——
                    点一下，服务端会自己说明原因。
                </p>
                <div class="email-link-row">
                    <el-input v-model="emailForLink" placeholder="邮箱" clearable />
                    <el-button :loading="busy" @click="doEmailLink">申请邮件链接</el-button>
                </div>
                <ProblemAlert v-if="emailLinkNote" :error="emailLinkNote" />
            </div>
        </el-card>
    </div>
</template>

<style scoped>
.login-page {
    min-height: 100vh;
    display: flex;
    align-items: center;
    justify-content: center;
    background: var(--el-bg-color-page);
}
.login-card {
    width: 560px;
    max-width: calc(100vw - 32px);
}
.login-title {
    display: flex;
    align-items: baseline;
    gap: 8px;
}
.login-title .brand {
    font-size: 20px;
    font-weight: 700;
}
.email-link-row {
    display: flex;
    gap: 8px;
    margin: 8px 0;
}
code {
    background: var(--el-fill-color-light);
    padding: 1px 4px;
    border-radius: 3px;
}
</style>
