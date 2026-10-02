<script setup lang="ts">
// 配置凭据（PUT /admin/channel-bindings/{id}/secrets）。**只写不读**：
//   · 服务端任何响应都不带凭据，界面只知道 has_secrets（已配置 / 未配置）；
//   · 输入框是 password，对话框一关就把输入清空（下次打开是空的），组件状态里不留；
//   · 保存成功只提示「已保存」，不回显。
// Shopify 的凭据是自建应用的 client_id / client_secret；别的渠道按「键 / 值」逐项填。

import { computed, reactive, ref, watch } from "vue";
import { Delete, Plus } from "@element-plus/icons-vue";
import { keel, type ChannelBinding } from "../../api/client.ts";
import { channelLabel } from "../../api/channelRules.ts";
import { notifyOk } from "../../ui/notify.ts";
import { useMobile } from "../../ui/useMobile.ts";
import ProblemAlert from "../../components/ProblemAlert.vue";

const props = defineProps<{ modelValue: boolean; binding: ChannelBinding }>();
const emit = defineEmits<{ "update:modelValue": [boolean]; saved: [] }>();

const mobile = useMobile();
const visible = computed({
    get: () => props.modelValue,
    set: (v: boolean) => emit("update:modelValue", v),
});
const isShopify = computed(() => props.binding.channel === "shopify");

const shopify = reactive({ clientId: "", clientSecret: "" });
const pairs = ref<{ key: string; value: string }[]>([{ key: "", value: "" }]);
const error = ref<unknown>(null);
const localError = ref("");
const saving = ref(false);

function clear(): void {
    shopify.clientId = "";
    shopify.clientSecret = "";
    pairs.value = [{ key: "", value: "" }];
    error.value = null;
    localError.value = "";
}

// 打开与关闭都清一次：关闭时清是为了不让输入留在内存里，打开时清是兜底。
watch(
    () => props.modelValue,
    () => clear(),
);

function body(): Record<string, string> | null {
    if (isShopify.value) {
        if (shopify.clientId.trim() === "" || shopify.clientSecret === "") return null;
        return { client_id: shopify.clientId.trim(), client_secret: shopify.clientSecret };
    }
    const out: Record<string, string> = {};
    for (const p of pairs.value) {
        if (p.key.trim() === "") continue;
        out[p.key.trim()] = p.value;
    }
    return Object.keys(out).length === 0 ? null : out;
}

async function save(): Promise<void> {
    const b = body();
    if (b === null) {
        localError.value = isShopify.value ? "client_id 与 client_secret 都要填" : "至少填一项";
        return;
    }
    localError.value = "";
    saving.value = true;
    error.value = null;
    try {
        await keel.request("put", "/admin/channel-bindings/{binding_id}/secrets", {
            path: { binding_id: props.binding.id },
            body: b,
        });
        notifyOk("已保存");
        emit("saved");
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
        :title="`配置凭据 · ${binding.name}`"
        width="560px"
        :fullscreen="mobile"
        destroy-on-close
        @closed="clear"
    >
        <ProblemAlert v-if="error" :error="error" />
        <el-alert v-if="localError" :title="localError" type="error" :closable="false" show-icon class="mb8" />
        <el-alert type="info" :closable="false" show-icon class="mb8">
            <template #title>
                现在：{{ binding.has_secrets ? "已配置" : "未配置" }}。保存会整体替换原来的凭据。
            </template>
            凭据只写不读：保存之后这里和任何接口都不会再显示它。
        </el-alert>

        <el-form v-if="isShopify" label-width="110px" autocomplete="off" @submit.prevent>
            <el-form-item label="client_id" required>
                <el-input v-model="shopify.clientId" autocomplete="off" placeholder="Shopify 自建应用的 Client ID" />
            </el-form-item>
            <el-form-item label="client_secret" required>
                <el-input
                    v-model="shopify.clientSecret"
                    type="password"
                    autocomplete="new-password"
                    placeholder="Shopify 自建应用的 Client secret"
                />
            </el-form-item>
        </el-form>

        <div v-else>
            <p class="hint">{{ channelLabel(binding.channel) }} 的凭据按键 / 值逐项填。</p>
            <div v-for="(p, i) in pairs" :key="i" class="pair">
                <el-input v-model="p.key" placeholder="键" autocomplete="off" />
                <el-input v-model="p.value" type="password" placeholder="值" autocomplete="new-password" />
                <el-button :icon="Delete" :disabled="pairs.length === 1" @click="pairs.splice(i, 1)" />
            </div>
            <el-button :icon="Plus" @click="pairs.push({ key: '', value: '' })">加一项</el-button>
        </div>

        <template #footer>
            <el-button @click="visible = false">取消</el-button>
            <el-button type="primary" :loading="saving" @click="save">保存</el-button>
        </template>
    </el-dialog>
</template>

<style scoped>
.mb8 {
    margin-bottom: 8px;
}
.pair {
    display: flex;
    gap: 8px;
    margin-bottom: 8px;
}
</style>
