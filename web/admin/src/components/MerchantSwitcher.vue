<script setup lang="ts">
// 顶栏的「当前管理：×× ▾」。**只对平台级会话渲染**（商家级员工看不到它，
// 他的请求也不会带 X-Keel-Merchant，见 api/merchantScope.ts）。
//
// 选中之后整页重新加载：每一页的数据都是按租户取的，只重取当前页的话，
// 侧边栏跳到别的页、缓存在组件里的类目下拉……都可能还是上一家店的。
// 整页重载是唯一不会漏的做法，而切换是个低频动作。

import { computed, ref } from "vue";
import { ArrowDown } from "@element-plus/icons-vue";
import { currentSession, keel, type Merchant } from "../api/client.ts";
import { currentMerchantScope, setMerchantScope } from "../api/merchantScope.ts";
import { notifyError } from "../ui/notify.ts";

const session = computed(() => currentSession());
const isPlatform = computed(() => session.value?.staff.merchant_id === null);
const scope = computed(() => currentMerchantScope(session.value));

const merchants = ref<Merchant[]>([]);
const loading = ref(false);

async function load(visible: boolean): Promise<void> {
    if (!visible || loading.value) return;
    loading.value = true;
    try {
        const res = await keel.get("/admin/merchants", { query: { page: 1, page_size: 100 } });
        merchants.value = res.items;
    } catch (err) {
        notifyError(err);
    } finally {
        loading.value = false;
    }
}

function choose(cmd: string): void {
    if (cmd === "__host__") {
        setMerchantScope(session.value, null);
    } else {
        const m = merchants.value.find((x) => x.code === cmd);
        if (m === undefined) return;
        setMerchantScope(session.value, { code: m.code, name: m.name, disabled: m.status !== 1 });
    }
    globalThis.location.reload();
}
</script>

<template>
    <el-dropdown v-if="isPlatform" trigger="click" @visible-change="load" @command="choose">
        <el-button :type="scope ? 'warning' : 'default'" size="small" class="switcher">
            <span>当前管理：</span>
            <b>{{ scope ? `${scope.name}（${scope.code}）` : "按当前域名的那家店" }}</b>
            <el-tag v-if="scope?.disabled" type="danger" size="small" effect="dark" class="ml4">已停用</el-tag>
            <el-icon class="el-icon--right"><ArrowDown /></el-icon>
        </el-button>
        <template #dropdown>
            <el-dropdown-menu v-loading="loading">
                <el-dropdown-item command="__host__" :disabled="scope === null">
                    按当前域名的那家店（不切换）
                </el-dropdown-item>
                <el-dropdown-item
                    v-for="m in merchants"
                    :key="m.id"
                    :command="m.code"
                    :disabled="scope?.code === m.code"
                    divided
                >
                    {{ m.name }}（{{ m.code }}）
                    <el-tag v-if="m.status !== 1" type="danger" size="small" class="ml4">
                        {{ m.status === 2 ? "已停用" : "待审核" }}
                    </el-tag>
                </el-dropdown-item>
            </el-dropdown-menu>
        </template>
    </el-dropdown>
</template>

<style scoped>
.switcher b {
    margin-left: 2px;
}
.ml4 {
    margin-left: 4px;
}
</style>
