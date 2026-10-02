<script setup lang="ts">
// 一个渠道账号：概况（状态、启停、编辑、凭据、重新同步商品）+ 门店映射 / 库存规则 / 价格规则 / 推送状态。
//
// 凭据只显示「已配置 / 未配置」。写按钮只给管理员（服务端 requireMerchantAdmin，403 为准）。
// 「重新同步商品」只对启用中的商品源出现：POST …/catalog-pulls（Idempotency-Key 必填），202 = 已排队，
// 别的状态服务端回 409。

import { computed, onMounted, ref } from "vue";
import { useRoute, useRouter } from "vue-router";
import { ArrowLeft, CopyDocument, Edit, Key, Refresh } from "@element-plus/icons-vue";
import { ElMessageBox } from "element-plus";
import { keel, type AdminCategory, type AdminStore, type ChannelBinding, type ChannelKind } from "../../api/client.ts";
import { listCategories } from "../../api/catalog.ts";
import { bindingStatusLabel, channelLabel, rolesLabel } from "../../api/channelRules.ts";
import { IdempotentSubmission, withIdempotency } from "../../api/idempotency.ts";
import { listAllStores } from "../../api/stores.ts";
import { can } from "../../auth/permissions.ts";
import { datetime } from "../../ui/format.ts";
import { notifyError, notifyOk } from "../../ui/notify.ts";
import ProblemAlert from "../../components/ProblemAlert.vue";
import ChannelBindingDialog from "./ChannelBindingDialog.vue";
import ChannelSecretsDialog from "./ChannelSecretsDialog.vue";
import ChannelStoreLinks from "./ChannelStoreLinks.vue";
import ChannelRules from "./ChannelRules.vue";
import ChannelListings from "./ChannelListings.vue";
import ChannelOrders from "./ChannelOrders.vue";
import { useMobile } from "../../ui/useMobile.ts";

const props = defineProps<{ bindingId: string }>();
const router = useRouter();
const route = useRoute();
const id = computed(() => Number(props.bindingId));
const canWrite = computed(() => can.manageChannels());
const mobile = useMobile();

const loading = ref(false);
const loadError = ref<unknown>(null);
const binding = ref<ChannelBinding | null>(null);
const kinds = ref<ChannelKind[]>([]);
const stores = ref<AdminStore[]>([]);
const categories = ref<AdminCategory[]>([]);
const linkCount = ref<number | null>(null);
// ?tab=orders 直接打开订单页签（从渠道单相关的链接跳过来）。
const tab = ref(route.query.tab === "orders" ? "orders" : "stores");

async function load(): Promise<void> {
    loading.value = true;
    loadError.value = null;
    try {
        const [b, k, s, c] = await Promise.all([
            keel.request("get", "/admin/channel-bindings/{binding_id}", { path: { binding_id: id.value } }),
            keel.get("/admin/channel-kinds", {}),
            listAllStores(),
            listCategories(),
        ]);
        binding.value = b;
        kinds.value = k.items;
        stores.value = s.stores.filter((x) => !x.deleted_at);
        categories.value = c;
    } catch (err) {
        loadError.value = err;
    } finally {
        loading.value = false;
    }
}
onMounted(() => void load());

const status = computed(() => (binding.value === null ? null : bindingStatusLabel(binding.value.status)));
const isCatalogSource = computed(() => ((binding.value?.roles ?? 0) & 1) !== 0);
const webhookUrl = computed(() => (binding.value === null ? "" : `${location.origin}${binding.value.webhook_path}`));

function configNum(key: string): number | null {
    const v = binding.value?.config[key];
    return typeof v === "number" ? v : null;
}
const defaultCategory = computed(() => {
    const cid = configNum("default_category_id");
    if (cid === null) return "未设";
    return categories.value.find((c) => c.id === cid)?.name ?? `类目 #${cid}`;
});
const priceStore = computed(() => {
    const sid = configNum("price_store_id");
    if (sid === null) return "未设（用映射门店里 id 最小的那家）";
    return stores.value.find((s) => s.id === sid)?.name ?? `门店 #${sid}`;
});
const webhookBase = computed(() => {
    const v = binding.value?.config["webhook_base_url"];
    return typeof v === "string" && v !== "" ? v : "未设（不装回调订阅）";
});

function copy(text: string): void {
    navigator.clipboard.writeText(text).then(
        () => notifyOk("已复制"),
        (err: unknown) => notifyError(err),
    );
}

// ------------------------------------------------------------ 启用 / 停用
async function setStatus(next: 1 | 2): Promise<void> {
    const b = binding.value;
    if (b === null) return;
    const warn: string[] = [];
    if (next === 1) {
        if (!b.has_secrets) warn.push("还没配凭据，启用后调渠道接口会失败。");
        if (linkCount.value === 0) warn.push("还没映射任何门店，启用后不会推库存与价格。");
    }
    const msg =
        next === 1
            ? `启用「${b.name}」之后开始同步：商品源会拉商品，销售渠道开始推库存与价格。${warn.join("")}`
            : `停用「${b.name}」之后不再同步；由它管理的商品字段在后台放开可改。`;
    try {
        await ElMessageBox.confirm(msg, next === 1 ? "确认启用" : "确认停用", { type: warn.length > 0 || next === 2 ? "warning" : "info" });
    } catch {
        return;
    }
    try {
        binding.value = await keel.request("patch", "/admin/channel-bindings/{binding_id}", {
            path: { binding_id: b.id },
            body: { status: next },
        });
        notifyOk(next === 1 ? "已启用" : "已停用");
    } catch (err) {
        notifyError(err);
    }
}

// ------------------------------------------------------------ 重新同步商品
const pulling = ref(false);
const pullSubmission = new IdempotentSubmission();
async function catalogPull(): Promise<void> {
    const b = binding.value;
    if (b === null) return;
    pulling.value = true;
    try {
        await withIdempotency(pullSubmission, (key) =>
            keel.request("post", "/admin/channel-bindings/{binding_id}/catalog-pulls", {
                path: { binding_id: b.id },
                headers: { "Idempotency-Key": key },
            }),
        );
        notifyOk("已排队重新拉取商品，稍后到「商品」里看结果");
    } catch (err) {
        notifyError(err);
    } finally {
        pulling.value = false;
    }
}

// ------------------------------------------------------------ 对话框
const editVisible = ref(false);
const secretsVisible = ref(false);
function onEdited(b: ChannelBinding): void {
    binding.value = b;
    notifyOk("已保存");
}
async function onSecretsSaved(): Promise<void> {
    // 只为刷新 has_secrets；凭据本身不回显。
    try {
        binding.value = await keel.request("get", "/admin/channel-bindings/{binding_id}", { path: { binding_id: id.value } });
    } catch (err) {
        notifyError(err);
    }
}
</script>

<template>
    <div v-loading="loading">
        <ProblemAlert v-if="loadError" :error="loadError" />

        <template v-if="binding && status">
            <div class="page-toolbar">
                <el-button :icon="ArrowLeft" @click="router.push({ name: 'channels' })">返回渠道</el-button>
                <span class="title">{{ binding.name }}</span>
                <el-tag :type="status.type" size="small">{{ status.text }}</el-tag>
                <el-tag :type="binding.has_secrets ? 'success' : 'warning'" size="small">
                    凭据{{ binding.has_secrets ? "已配置" : "未配置" }}
                </el-tag>
                <span class="grow" />
                <template v-if="canWrite">
                    <el-button v-if="binding.status !== 1" type="success" @click="setStatus(1)">启用</el-button>
                    <el-button v-else type="danger" plain @click="setStatus(2)">停用</el-button>
                    <el-button :icon="Edit" @click="editVisible = true">编辑</el-button>
                    <el-button :icon="Key" @click="secretsVisible = true">配置凭据</el-button>
                    <el-button v-if="binding.status === 1 && isCatalogSource" :icon="Refresh" :loading="pulling" @click="catalogPull">
                        重新同步商品
                    </el-button>
                </template>
            </div>

            <el-alert v-if="binding.status === 3" type="error" :closable="false" show-icon class="mb12" title="凭据失效，已停止推送">
                续期失败（渠道上的应用被卸载或密钥被换掉）。重新配置凭据后再点「启用」。
            </el-alert>
            <el-alert
                v-else-if="binding.status === 2 && (!binding.has_secrets || linkCount === 0)"
                type="info"
                :closable="false"
                show-icon
                class="mb12"
                title="还没准备好启用"
            >
                {{ binding.has_secrets ? "" : "先「配置凭据」；" }}{{ linkCount === 0 ? "在「门店映射」里至少映射一家门店；" : "" }}然后再启用。
            </el-alert>

            <el-descriptions :column="mobile ? 1 : 2" border class="mb12">
                <el-descriptions-item label="渠道">{{ channelLabel(binding.channel) }}</el-descriptions-item>
                <el-descriptions-item label="店铺账号">{{ binding.external_account }}</el-descriptions-item>
                <el-descriptions-item label="角色">{{ rolesLabel(binding.roles) }}</el-descriptions-item>
                <el-descriptions-item label="默认类目">{{ defaultCategory }}</el-descriptions-item>
                <el-descriptions-item label="价格源门店">{{ priceStore }}</el-descriptions-item>
                <el-descriptions-item label="回调地址前缀">{{ webhookBase }}</el-descriptions-item>
                <el-descriptions-item label="回调地址" :span="mobile ? 1 : 2">
                    <code class="path">{{ webhookUrl }}</code>
                    <el-button link type="primary" :icon="CopyDocument" title="复制" @click="copy(webhookUrl)" />
                </el-descriptions-item>
                <el-descriptions-item label="创建">{{ datetime(binding.created_at) }}</el-descriptions-item>
                <el-descriptions-item label="更新">{{ datetime(binding.updated_at) }}</el-descriptions-item>
            </el-descriptions>

            <el-tabs v-model="tab" type="border-card">
                <el-tab-pane label="门店映射" name="stores">
                    <ChannelStoreLinks
                        :binding-id="binding.id"
                        :channel="binding.channel"
                        :stores="stores"
                        :can-write="canWrite"
                        @changed="(n: number) => (linkCount = n)"
                    />
                </el-tab-pane>
                <el-tab-pane label="库存规则" name="stock" lazy>
                    <ChannelRules kind="stock" :binding-id="binding.id" :stores="stores" :can-write="canWrite" />
                </el-tab-pane>
                <el-tab-pane label="价格规则" name="price" lazy>
                    <ChannelRules kind="price" :binding-id="binding.id" :stores="stores" :can-write="canWrite" />
                </el-tab-pane>
                <el-tab-pane label="推送状态" name="listings" lazy>
                    <ChannelListings :binding-id="binding.id" :stores="stores" />
                </el-tab-pane>
                <el-tab-pane label="订单" name="orders" lazy>
                    <ChannelOrders :binding-id="binding.id" :bindings="[binding]" :kinds="kinds" :stores="stores" />
                </el-tab-pane>
            </el-tabs>

            <ChannelBindingDialog v-model="editVisible" :binding="binding" :kinds="kinds" @saved="onEdited" />
            <ChannelSecretsDialog v-model="secretsVisible" :binding="binding" @saved="onSecretsSaved" />
        </template>
    </div>
</template>

<style scoped>
.title {
    font-size: 16px;
    font-weight: 600;
}
.mb12 {
    margin-bottom: 12px;
}
.path {
    font-size: 12px;
    word-break: break-all;
}

/* 手机：标签页横向滑动，不截断（同门店详情） */
@media (max-width: 768px) {
    :deep(.el-tabs__nav-wrap) {
        overflow-x: auto;
    }
    :deep(.el-tabs__nav-wrap)::after {
        display: none;
    }
    :deep(.el-tabs__nav) {
        flex-wrap: nowrap;
    }
    :deep(.el-tabs__item) {
        flex-shrink: 0;
    }
}
</style>
