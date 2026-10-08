<script setup lang="ts">
// 商家管理（仅平台级）。列表**含停用与待审核的店**——不含的话，平台就没有入口
// 把一家停掉的店启用回来。
//
// 三件事要说清楚，因为它们都是「点了才知道」会很糟的：
//
//   · **单商家部署里不能开店**（MerchantList.single_merchant_mode）。按钮置灰并写明
//     原因：那套部署忽略 Host，新店谁也访问不到，而且下次重启启动自检会失败。
//     服务端照样会拒（409 single-merchant-mode），这里只是不让人白点一次。
//   · **停用立刻让买家打不开这家店**（最多延迟半分钟的解析缓存）。平台仍能切进去。
//   · 改名和停用在服务端是**追加一行修订**，不改 merchants 那一行
//     （keel_app 在 merchants 上没有 UPDATE）。界面上看不出差别，但「最近修改」那一列
//     就是那一行修订的时间。
//   · **开店的 201 里带着这家新店第一个管理员的一次性登录 token**，这里把它显示出来
//     一次（契约 MerchantOpened）。不显示的话，把门交给店主的唯一路子是翻 app 的日志，
//     而日志会被采集、会进索引。这一串 15 分钟后自己失效、用一次就作废，
//     所以它只出现一次；关了就没了。丢了也不慌：切进这家店，在「员工」页给他重签一串。
//   · **自有域名**走 PATCH domain（迁移 00340）：空串保存成 null 摘掉；底域之下 / 不像域名 /
//     被别家占着分别 422 / 422 / 409。单商家部署一律 409。界面只是 curl 的替代，闸门全在服务端。

import { computed, onMounted, ref } from "vue";
import { Plus, Refresh } from "@element-plus/icons-vue";
import { ElMessageBox } from "element-plus";
import {
    currentSession,
    keel,
    type Merchant,
    type MerchantCreateRequest,
    type MerchantList,
    type MerchantOpened,
} from "../api/client.ts";
import { IdempotentSubmission, withIdempotency } from "../api/idempotency.ts";
import { currentMerchantScope, setMerchantScope } from "../api/merchantScope.ts";
import { datetime } from "../ui/format.ts";
import { notifyError, notifyOk } from "../ui/notify.ts";
import ProblemAlert from "../components/ProblemAlert.vue";

const session = computed(() => currentSession());
const isPlatformAdmin = computed(
    () => session.value?.staff.merchant_id === null && session.value?.staff.role === 1,
);
const scope = computed(() => currentMerchantScope(session.value));

const loading = ref(false);
const error = ref<unknown>(null);
const page = ref<MerchantList | null>(null);
const pageNo = ref(1);

async function load(): Promise<void> {
    loading.value = true;
    error.value = null;
    try {
        page.value = await keel.get("/admin/merchants", { query: { page: pageNo.value, page_size: 20 } });
    } catch (err) {
        error.value = err;
    } finally {
        loading.value = false;
    }
}
onMounted(() => void load());

const STATUS: Record<1 | 2 | 3, { text: string; tag: "success" | "danger" | "warning" }> = {
    1: { text: "正常", tag: "success" },
    2: { text: "停用", tag: "danger" },
    3: { text: "待审核", tag: "warning" },
};

// ---------------------------------------------------------------- 开店

const openVisible = ref(false);
const openError = ref<unknown>(null);
const opening = ref(false);
const submission = new IdempotentSubmission();
const draft = ref<MerchantCreateRequest>({ code: "", name: "", admin_email: "" });

function openDialog(): void {
    draft.value = { code: "", name: "", admin_email: "" };
    openError.value = null;
    submission.rotate();
    openVisible.value = true;
}

async function submitOpen(): Promise<void> {
    opening.value = true;
    openError.value = null;
    try {
        const m = await withIdempotency(submission, (key) =>
            keel.request("post", "/admin/merchants", { body: draft.value, headers: { "Idempotency-Key": key } }),
        );
        openVisible.value = false;
        showCredential(m, String(draft.value.admin_email ?? "").trim());
        await load();
    } catch (err) {
        openError.value = err;
    } finally {
        opening.value = false;
    }
}

// ---------------------------------------------------------- 那串一次性凭据

/**
 * 开店拿到的凭据。显示一次，关掉就清 —— 留在页面状态里就等于这台机器上
 * 任何一个能读这块屏幕的人都能拿到它（会话本身放 sessionStorage 是同一个理由）。
 */
const credential = ref<{ code: string; email: string; staffId?: number; token?: string; expireAt?: string } | null>(
    null,
);
const credentialVisible = ref(false);

function showCredential(opened: MerchantOpened, adminEmail: string): void {
    credential.value = {
        code: opened.code,
        email: adminEmail,
        staffId: opened.admin_staff_id ?? undefined,
        token: opened.admin_login_token ?? undefined,
        expireAt: opened.admin_login_token_expire_at,
    };
    credentialVisible.value = true;
}

function closeCredential(): void {
    credentialVisible.value = false;
    credential.value = null;
}

// ---------------------------------------------------------------- 改名

const renameVisible = ref(false);
const renameError = ref<unknown>(null);
const renaming = ref(false);
const renameTarget = ref<Merchant | null>(null);
const newName = ref("");

function openRename(m: Merchant): void {
    renameTarget.value = m;
    newName.value = m.name;
    renameError.value = null;
    renameVisible.value = true;
}

async function submitRename(): Promise<void> {
    const m = renameTarget.value;
    if (m === null) return;
    renaming.value = true;
    renameError.value = null;
    try {
        await keel.request("patch", "/admin/merchants/{merchant_id}", {
            path: { merchant_id: m.id },
            body: { name: newName.value.trim() },
        });
        renameVisible.value = false;
        notifyOk("已改名");
        await load();
    } catch (err) {
        renameError.value = err;
    } finally {
        renaming.value = false;
    }
}

// ---------------------------------------------------------------- 停用 / 启用

async function setStatus(m: Merchant, status: 1 | 2): Promise<void> {
    if (status === 2) {
        try {
            await ElMessageBox.confirm(
                `停用「${m.name}」（${m.code}）？买家打开这家店会看到 404（最多延迟半分钟）。平台管理员仍能切进去管理、再启用。`,
                "确认停用",
                { type: "warning", confirmButtonText: "停用", cancelButtonText: "取消" },
            );
        } catch {
            return;
        }
    }
    try {
        await keel.request("patch", "/admin/merchants/{merchant_id}", {
            path: { merchant_id: m.id },
            body: { status },
        });
        notifyOk(status === 2 ? `已停用「${m.name}」` : `已启用「${m.name}」`);
        // 顶栏正选着这家店的话，把「已停用」这个标记跟着更新。
        if (scope.value?.code === m.code) {
            setMerchantScope(session.value, { code: m.code, name: m.name, disabled: status === 2 });
        }
        await load();
    } catch (err) {
        notifyError(err);
    }
}

function manage(m: Merchant): void {
    setMerchantScope(session.value, { code: m.code, name: m.name, disabled: m.status !== 1 });
    globalThis.location.assign("/products");
}

// ---------------------------------------------------------------- 自有域名

const domainVisible = ref(false);
const domainError = ref<unknown>(null);
const domainSaving = ref(false);
const domainTarget = ref<Merchant | null>(null);
const domainDraft = ref("");

function openDomain(m: Merchant): void {
    domainTarget.value = m;
    domainDraft.value = m.domain ?? "";
    domainError.value = null;
    domainVisible.value = true;
}

async function submitDomain(): Promise<void> {
    const m = domainTarget.value;
    if (m === null) return;
    domainSaving.value = true;
    domainError.value = null;
    const trimmed = domainDraft.value.trim();
    try {
        await keel.request("patch", "/admin/merchants/{merchant_id}", {
            path: { merchant_id: m.id },
            body: { domain: trimmed === "" ? null : trimmed },
        });
        domainVisible.value = false;
        notifyOk(trimmed === "" ? `已摘掉「${m.name}」的自有域名` : `已登记 ${trimmed}`);
        await load();
    } catch (err) {
        domainError.value = err;
    } finally {
        domainSaving.value = false;
    }
}
</script>

<template>
    <div>
        <ProblemAlert v-if="error" :error="error" />

        <el-alert v-if="page?.single_merchant_mode" type="info" :closable="false" show-icon class="mb12">
            <template #title>这是一套单商家部署（配了 KEEL_DEFAULT_MERCHANT），不能再开店</template>
            单商家部署忽略请求的域名，所有请求都落在默认商家上：新开的店谁也访问不到，而且启动自检（活跃商家必须恰好一家）
            会让这套部署<b>下一次重启就起不来</b>。服务端对开店回 409 <code>single-merchant-mode</code>。
            要开多家店，先切到多商家部署：清空 <code>KEEL_DEFAULT_MERCHANT</code>、配置 <code>KEEL_BASE_DOMAIN</code>。
        </el-alert>

        <div class="page-toolbar">
            <span class="hint">
                平台级视角：全部商家，含停用与待审核。「切过去管理」之后，后台每一页的读写都落在那家店上。
            </span>
            <span class="grow" />
            <el-button :icon="Refresh" :loading="loading" @click="load">刷新</el-button>
            <el-tooltip
                :disabled="!page?.single_merchant_mode && isPlatformAdmin"
                :content="page?.single_merchant_mode ? '单商家部署不能开店（见上方说明）' : '开店要平台级管理员'"
            >
                <span>
                    <el-button
                        type="primary"
                        :icon="Plus"
                        :disabled="page?.single_merchant_mode !== false || !isPlatformAdmin"
                        @click="openDialog"
                    >
                        开店
                    </el-button>
                </span>
            </el-tooltip>
        </div>

        <el-table :data="page?.items ?? []" v-loading="loading" border stripe>
            <el-table-column prop="id" label="ID" width="70" />
            <el-table-column prop="code" label="code" width="140" />
            <el-table-column label="名称" min-width="180">
                <template #default="{ row }: { row: Merchant }">
                    {{ row.name }}
                    <el-tag v-if="scope?.code === row.code" type="warning" size="small" effect="dark" class="ml4">
                        正在管理
                    </el-tag>
                </template>
            </el-table-column>
            <el-table-column label="状态" width="90">
                <template #default="{ row }: { row: Merchant }">
                    <el-tag :type="STATUS[row.status].tag" size="small">{{ STATUS[row.status].text }}</el-tag>
                </template>
            </el-table-column>
            <el-table-column label="自定义域名" min-width="160">
                <template #default="{ row }: { row: Merchant }">{{ row.domain ?? "—" }}</template>
            </el-table-column>
            <el-table-column label="开店时间" width="170">
                <template #default="{ row }: { row: Merchant }">{{ datetime(row.created_at) }}</template>
            </el-table-column>
            <el-table-column label="最近修改" width="170">
                <template #default="{ row }: { row: Merchant }">{{ datetime(row.updated_at) }}</template>
            </el-table-column>
            <el-table-column label="操作" width="320" fixed="right">
                <template #default="{ row }: { row: Merchant }">
                    <el-button link type="primary" :disabled="scope?.code === row.code" @click="manage(row)">
                        切过去管理
                    </el-button>
                    <el-button link type="primary" :disabled="!isPlatformAdmin" @click="openRename(row)">改名</el-button>
                    <el-button link type="primary" :disabled="!isPlatformAdmin" @click="openDomain(row)">域名</el-button>
                    <el-button
                        v-if="row.status === 1"
                        link
                        type="danger"
                        :disabled="!isPlatformAdmin"
                        @click="setStatus(row, 2)"
                    >
                        停用
                    </el-button>
                    <el-button v-else link type="success" :disabled="!isPlatformAdmin" @click="setStatus(row, 1)">
                        启用
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

        <el-dialog v-model="openVisible" title="开店" width="560px">
            <ProblemAlert v-if="openError" :error="openError" />
            <p class="hint">
                建商家，并在这家新店里建出它的第一个管理员。本轮没有接邮件服务，那封「发登录链接」的信不会发 ——
                他的<strong>一次性登录 token 会在这一次的响应里给你</strong>，开完店当场显示一次，请通过可信渠道转交本人。
                新店的入口是 <code>{code}.KEEL_BASE_DOMAIN</code>。
            </p>
            <el-form label-width="110px" @submit.prevent>
                <el-form-item label="code" required>
                    <el-input v-model="draft.code" placeholder="小写字母、数字、连字符，2~31 位；它会成为域名的一段" />
                </el-form-item>
                <el-form-item label="店名" required><el-input v-model="draft.name" /></el-form-item>
                <el-form-item label="管理员邮箱" required><el-input v-model="draft.admin_email" /></el-form-item>
            </el-form>
            <template #footer>
                <el-button @click="openVisible = false">取消</el-button>
                <el-button
                    type="primary"
                    :loading="opening"
                    :disabled="draft.code.trim() === '' || draft.name.trim() === '' || String(draft.admin_email).trim() === ''"
                    @click="submitOpen"
                >
                    开店
                </el-button>
            </template>
        </el-dialog>

        <el-dialog
            v-model="credentialVisible"
            :title="`新店「${credential?.code ?? ''}」的第一个管理员：登录凭据`"
            width="620px"
            :close-on-click-modal="false"
            @closed="credential = null"
        >
            <template v-if="credential?.token">
                <p class="hint">
                    把这一串交给 <strong>{{ credential?.email }}</strong>，他在登录页「已有登录 token」那一栏粘进去就进了这家新店。
                    <strong>只显示这一次</strong>：关掉窗口就没了，日志里也没有它。
                    15 分钟有效（到 {{ datetime(credential?.expireAt) }}），用掉即失效。
                </p>
                <el-input :model-value="credential?.token" readonly type="textarea" :rows="2" />
                <p class="hint">
                    staff.id = {{ credential?.staffId }}。错过这一屏不会把这家店锁死：在列表里「切过去管理」这家店，
                    到「员工」页给他重签一串 —— 平台会话切进店之后就落在这家店的租户作用域里，那条接口回明文。
                </p>
            </template>
            <p v-else class="hint">
                这次响应没有凭据 —— 这把幂等键之前已经开过一次店，回放的是那一次的
                <code>Merchant</code>。一次性凭据的明文不进数据库，所以也没法回放第一串。
                要交给本人的话：「切过去管理」这家店，到「员工」页重签登录 token（那条接口回明文）。
            </p>
            <template #footer>
                <el-button type="primary" @click="closeCredential">我已转交</el-button>
            </template>
        </el-dialog>

        <el-dialog v-model="renameVisible" :title="`改名：${renameTarget?.code ?? ''}`" width="460px">
            <ProblemAlert v-if="renameError" :error="renameError" />
            <el-form label-width="70px" @submit.prevent>
                <el-form-item label="新名字"><el-input v-model="newName" /></el-form-item>
            </el-form>
            <p class="hint">code 不能改：它是这家店域名的一段。</p>
            <template #footer>
                <el-button @click="renameVisible = false">取消</el-button>
                <el-button type="primary" :loading="renaming" :disabled="newName.trim() === ''" @click="submitRename">
                    保存
                </el-button>
            </template>
        </el-dialog>

        <el-dialog v-model="domainVisible" :title="`自有域名：${domainTarget?.code ?? ''}`" width="520px">
            <ProblemAlert v-if="domainError" :error="domainError" />
            <p class="hint">
                登记之后，买家用这个 Host 进这家店（解析器按归一化后的小写比）。
                底域之下的名字（例如 <code>foo.KEEL_BASE_DOMAIN</code>）一律拒——那片只认 code。
                清空并保存 = 摘掉入口；子域名 <code>{code}.KEEL_BASE_DOMAIN</code> 不受影响。
                单商家部署会 409。证书与 DNS 仍是运营侧的事。
            </p>
            <el-form label-width="90px" @submit.prevent>
                <el-form-item label="域名">
                    <el-input v-model="domainDraft" placeholder="例如 shop.example.org；留空即摘掉" />
                </el-form-item>
            </el-form>
            <template #footer>
                <el-button @click="domainVisible = false">取消</el-button>
                <el-button type="primary" :loading="domainSaving" :disabled="!isPlatformAdmin" @click="submitDomain">
                    保存
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
.pager {
    margin-top: 12px;
    justify-content: flex-end;
}
code {
    background: var(--el-fill-color-light);
    padding: 1px 4px;
    border-radius: 3px;
}
</style>
