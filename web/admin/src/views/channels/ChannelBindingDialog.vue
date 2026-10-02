<script setup lang="ts">
// 新建 / 编辑渠道账号（POST /admin/channel-bindings、PATCH /admin/channel-bindings/{id}）。
//
// 新建一律停用（契约 ChannelBindingInput.status 不给 = 2）：配好凭据和门店映射之后再在详情页启用。
// 渠道与店铺账号建好就不能改（ChannelBindingPatch 里没有这两个字段）。
// config 是自由对象，渠道层认 default_category_id / price_store_id / webhook_base_url 三个键；
// 编辑时原有的其他键原样带回，不丢。

import { computed, ref, watch } from "vue";
import { keel, type AdminCategory, type AdminStore, type ChannelBinding, type ChannelKind } from "../../api/client.ts";
import { indentedLabel, listCategories } from "../../api/catalog.ts";
import { bitsFromRoles, channelLabel, deepEqual, roleOptions, rolesFromBits, shopifyDomainOk } from "../../api/channelRules.ts";
import { IdempotentSubmission, withIdempotency } from "../../api/idempotency.ts";
import { listAllStores } from "../../api/stores.ts";
import { useMobile } from "../../ui/useMobile.ts";
import ProblemAlert from "../../components/ProblemAlert.vue";

const props = defineProps<{ modelValue: boolean; binding: ChannelBinding | null; kinds: ChannelKind[] }>();
const emit = defineEmits<{ "update:modelValue": [boolean]; saved: [ChannelBinding, boolean] }>();

const mobile = useMobile();
const visible = computed({
    get: () => props.modelValue,
    set: (v: boolean) => emit("update:modelValue", v),
});
const editing = computed(() => props.binding !== null);

interface Form {
    channel: string;
    externalAccount: string;
    name: string;
    roleBits: number[];
    defaultCategoryId: number | null;
    priceStoreId: number | null;
    webhookBaseUrl: string;
}

function origin(): string {
    return typeof location === "undefined" ? "" : location.origin;
}

function numOrNull(v: unknown): number | null {
    return typeof v === "number" && Number.isFinite(v) && v > 0 ? v : null;
}

function emptyForm(): Form {
    const first = props.kinds[0];
    return {
        channel: first?.channel ?? "",
        externalAccount: "",
        name: "",
        roleBits: first === undefined ? [] : bitsFromRoles(first.roles),
        defaultCategoryId: null,
        priceStoreId: null,
        webhookBaseUrl: origin(),
    };
}

function formOf(b: ChannelBinding): Form {
    const c = b.config;
    return {
        channel: b.channel,
        externalAccount: b.external_account,
        name: b.name,
        roleBits: bitsFromRoles(b.roles),
        defaultCategoryId: numOrNull(c["default_category_id"]),
        priceStoreId: numOrNull(c["price_store_id"]),
        webhookBaseUrl: typeof c["webhook_base_url"] === "string" ? c["webhook_base_url"] : "",
    };
}

const form = ref<Form>(emptyForm());
const error = ref<unknown>(null);
const localErrors = ref<string[]>([]);
const saving = ref(false);
const submission = new IdempotentSubmission();
const categories = ref<AdminCategory[]>([]);
const stores = ref<AdminStore[]>([]);

const kind = computed(() => props.kinds.find((k) => k.channel === form.value.channel) ?? null);
const roleChoices = computed(() => roleOptions(kind.value?.roles ?? 7));
const isShopify = computed(() => form.value.channel === "shopify");
const isCatalogSource = computed(() => form.value.roleBits.includes(1));

watch(
    () => props.modelValue,
    async (open) => {
        if (!open) return;
        form.value = props.binding === null ? emptyForm() : formOf(props.binding);
        error.value = null;
        localErrors.value = [];
        submission.rotate();
        try {
            const [c, s] = await Promise.all([listCategories(), listAllStores()]);
            categories.value = c.filter((x) => x.status === 1);
            stores.value = s.stores.filter((x) => !x.deleted_at);
        } catch (err) {
            error.value = err;
        }
    },
);

function onChannelChange(ch: string): void {
    const k = props.kinds.find((x) => x.channel === ch);
    form.value.roleBits = k === undefined ? [] : bitsFromRoles(k.roles);
}

function validate(): string[] {
    const f = form.value;
    const errs: string[] = [];
    if (!editing.value) {
        if (f.channel === "") errs.push("选一个渠道");
        if (f.externalAccount.trim() === "") errs.push("填店铺账号");
        else if (isShopify.value && !shopifyDomainOk(f.externalAccount)) errs.push("Shopify 店铺账号要是 xxx.myshopify.com 的形式（小写，不带 https://）");
    }
    const n = f.name.trim().length;
    if (n < 1 || n > 60) errs.push("名称 1 到 60 个字");
    if (f.roleBits.length === 0) errs.push("至少勾一个角色");
    if (isCatalogSource.value && f.defaultCategoryId === null) errs.push("商品源要选默认类目：拉进来的新商品挂在这里");
    if (f.webhookBaseUrl.trim() !== "" && !/^https?:\/\/[^/\s]+$/.test(f.webhookBaseUrl.trim())) {
        errs.push("回调地址前缀形如 https://shop.example.com（不带路径、不带末尾斜杠）");
    }
    return errs;
}

function buildConfig(): Record<string, unknown> {
    const f = form.value;
    // 编辑时保留原 config 里的其他键；这三个键按表单重写（空 = 去掉）。
    const out: Record<string, unknown> = { ...(props.binding?.config ?? {}) };
    delete out["default_category_id"];
    delete out["price_store_id"];
    delete out["webhook_base_url"];
    if (f.defaultCategoryId !== null) out["default_category_id"] = f.defaultCategoryId;
    if (f.priceStoreId !== null) out["price_store_id"] = f.priceStoreId;
    if (f.webhookBaseUrl.trim() !== "") out["webhook_base_url"] = f.webhookBaseUrl.trim();
    return out;
}

async function save(): Promise<void> {
    localErrors.value = validate();
    if (localErrors.value.length > 0) return;
    saving.value = true;
    error.value = null;
    const f = form.value;
    try {
        if (props.binding === null) {
            const body = {
                channel: f.channel,
                external_account: f.externalAccount.trim(),
                name: f.name.trim(),
                roles: rolesFromBits(f.roleBits),
                config: buildConfig(),
            };
            const b = await withIdempotency(submission, (key) =>
                keel.request("post", "/admin/channel-bindings", { body, headers: { "Idempotency-Key": key } }),
            );
            emit("saved", b, true);
        } else {
            // config 只在真变了（深比较，键序无关）才带上：buildConfig() 是「拆开原 config
            // 再按表单拼回去」，哪怕三个键的值和原来一模一样，拼出来的键序也可能跟原来不同，
            // 带一个「看起来变了、其实没变」的 config 上去会让后端把整店重算一遍
            // （只改名字这种事也会触发）。
            const config = buildConfig();
            const configChanged = !deepEqual(config, props.binding.config);
            const b = await keel.request("patch", "/admin/channel-bindings/{binding_id}", {
                path: { binding_id: props.binding.id },
                body: {
                    name: f.name.trim(),
                    roles: rolesFromBits(f.roleBits),
                    ...(configChanged ? { config } : {}),
                },
            });
            emit("saved", b, false);
        }
        visible.value = false;
    } catch (err) {
        error.value = err;
    } finally {
        saving.value = false;
    }
}
</script>

<template>
    <el-dialog
        v-model="visible"
        :title="editing ? `编辑渠道账号 #${binding?.id}` : '新建渠道账号'"
        width="640px"
        :fullscreen="mobile"
    >
        <ProblemAlert v-if="error" :error="error" />
        <el-alert v-for="(e, i) in localErrors" :key="i" :title="e" type="error" :closable="false" show-icon class="mb8" />
        <el-alert v-if="!editing" type="info" :closable="false" show-icon class="mb8" title="新建的账号一律是停用的">
            配好凭据和门店映射后，再在详情页点「启用」。
        </el-alert>

        <el-form label-width="110px" @submit.prevent>
            <el-form-item label="渠道" required>
                <el-select v-model="form.channel" :disabled="editing" @change="onChannelChange">
                    <el-option v-for="k in kinds" :key="k.channel" :value="k.channel" :label="channelLabel(k.channel)" />
                </el-select>
            </el-form-item>
            <el-form-item label="店铺账号" required>
                <el-input
                    v-model="form.externalAccount"
                    :disabled="editing"
                    :placeholder="isShopify ? 'your-shop.myshopify.com' : '渠道上的账号'"
                />
                <p v-if="editing" class="hint">建好之后不能改；换店铺请新建一个账号。</p>
            </el-form-item>
            <el-form-item label="名称" required>
                <el-input v-model="form.name" maxlength="60" placeholder="后台里看的名字，如「Shopify 海外店」" />
            </el-form-item>
            <el-form-item label="角色" required>
                <el-checkbox-group v-model="form.roleBits">
                    <el-checkbox v-for="r in roleChoices" :key="r.bit" :value="r.bit">{{ r.text }}</el-checkbox>
                </el-checkbox-group>
                <p class="hint">商品源：商品从渠道同步进来；库存源：库存以渠道为准；销售渠道：keel 往渠道推库存与价格。</p>
            </el-form-item>
            <el-form-item label="默认类目" :required="isCatalogSource">
                <el-select v-model="form.defaultCategoryId" clearable filterable placeholder="拉进来的新商品挂在哪个类目">
                    <el-option v-for="c in categories" :key="c.id" :value="c.id" :label="indentedLabel(c)" />
                </el-select>
            </el-form-item>
            <el-form-item label="价格源门店">
                <el-select v-model="form.priceStoreId" clearable filterable placeholder="不选 = 映射门店里 id 最小的那家">
                    <el-option v-for="s in stores" :key="s.id" :value="s.id" :label="s.name" />
                </el-select>
                <p class="hint">渠道上全店一个价时，价格从这家门店出（要在门店映射里）。</p>
            </el-form-item>
            <el-form-item label="回调地址前缀">
                <el-input v-model="form.webhookBaseUrl" placeholder="https://shop.example.com" />
                <p class="hint">首次拉商品时按它在渠道上装回调订阅；留空则不装。</p>
            </el-form-item>
        </el-form>

        <template #footer>
            <el-button @click="visible = false">取消</el-button>
            <el-button type="primary" :loading="saving" @click="save">{{ editing ? "保存" : "新建" }}</el-button>
        </template>
    </el-dialog>
</template>

<style scoped>
.mb8 {
    margin-bottom: 8px;
}
.hint {
    margin: 4px 0 0;
    font-size: 12px;
    line-height: 1.5;
    color: var(--el-text-color-secondary);
    width: 100%;
}
</style>
