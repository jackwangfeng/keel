<script setup lang="ts">
// 店铺设置（契约 GET / PUT /admin/shop-settings，数据模型 §2 shop_preferences）。
//
// 只有管理员能进（菜单对别的角色隐藏；服务端同样 403 role-forbidden）。
// 整体替换：保存时把整张表单发过去，客服电话留空即清空。
//
// 每一项旁边写清楚「改了之后什么时候、对谁生效」—— 这几项改的是全店每一单的时效，
// 商家最常问的就是「已经发货的单算不算」。

import { computed, onMounted, reactive, ref } from "vue";
import { keel } from "../api/client.ts";
import {
    DAYS_MAX,
    DAYS_MIN,
    PHONE_MAX,
    formOf,
    shopSettingsBody,
    timezoneOptions,
    validateShopSettings,
    type ShopSettings,
    type ShopSettingsForm,
} from "../api/shopSettings.ts";
import { datetime } from "../ui/format.ts";
import { notifyOk } from "../ui/notify.ts";
import ProblemAlert from "../components/ProblemAlert.vue";

const loading = ref(false);
const saving = ref(false);
const loadError = ref<unknown>(null);
const saveError = ref<unknown>(null);
const current = ref<ShopSettings | null>(null);
const form = reactive<ShopSettingsForm>({ timezone: "", autoConfirmDays: 7, returnShipDays: 7, afterSaleDays: 15, servicePhone: "" });

const zones = computed(() => {
    let all: string[] = [];
    try {
        all = Intl.supportedValuesOf("timeZone");
    } catch {
        // 老浏览器没有这个 API：只给常用的几个，照样能手输。
    }
    return timezoneOptions(all);
});

const problems = computed(() => validateShopSettings(form));
const dirty = computed(() => {
    if (current.value === null) return false;
    return JSON.stringify(shopSettingsBody(form)) !== JSON.stringify(shopSettingsBody(formOf(current.value)));
});

async function load(): Promise<void> {
    loading.value = true;
    loadError.value = null;
    try {
        current.value = await keel.get("/admin/shop-settings", {});
        Object.assign(form, formOf(current.value));
    } catch (err) {
        loadError.value = err;
    } finally {
        loading.value = false;
    }
}

async function save(): Promise<void> {
    if (Object.keys(problems.value).length > 0) return;
    saving.value = true;
    saveError.value = null;
    try {
        // PUT 天然幂等，不带 Idempotency-Key（契约）。
        current.value = await keel.request("put", "/admin/shop-settings", { body: shopSettingsBody(form) });
        Object.assign(form, formOf(current.value));
        notifyOk("店铺设置已保存");
    } catch (err) {
        saveError.value = err;
    } finally {
        saving.value = false;
    }
}

function reset(): void {
    if (current.value !== null) Object.assign(form, formOf(current.value));
    saveError.value = null;
}

onMounted(() => void load());
</script>

<template>
    <div class="page" v-loading="loading">
        <div class="page-head">
            <h2>店铺设置</h2>
            <span v-if="current" class="muted">
                {{ current.updated_at === null ? "从没改过，下面都是默认值" : `最近修改：${datetime(current.updated_at)}` }}
            </span>
        </div>

        <ProblemAlert v-if="loadError" :error="loadError" />

        <el-form v-if="current" label-width="160px" class="settings-form" @submit.prevent="save">
            <el-form-item label="店铺名称">
                <span>{{ current.shop_name }}</span>
                <span class="hint">店铺名称由平台修改（商家管理），这里只读。</span>
            </el-form-item>

            <el-form-item label="客服电话" :error="problems.servicePhone">
                <el-input v-model="form.servicePhone" :maxlength="PHONE_MAX" clearable placeholder="不填即不设" style="width: 260px" />
            </el-form-item>

            <el-form-item label="店铺时区" :error="problems.timezone">
                <el-select v-model="form.timezone" filterable allow-create default-first-option style="width: 260px">
                    <el-option v-for="z in zones" :key="z" :label="z" :value="z" />
                </el-select>
                <span class="hint">经营报表按它切「今天」「按天」。改了之后下一次打开报表就按新时区算，历史数据不用重算。</span>
            </el-form-item>

            <el-form-item label="自动确认收货" :error="problems.autoConfirmDays">
                <span>发货后</span>
                <el-input-number v-model="form.autoConfirmDays" :min="DAYS_MIN" :max="DAYS_MAX" :step="1" step-strictly />
                <span>天</span>
                <span class="hint">
                    到期由系统替买家确认收货。按「此刻」的天数判断：改短之后，已发货超过新天数的单会在十分钟内被确认；
                    有售后在途的单暂停，售后结束后再确认。
                </span>
            </el-form-item>

            <el-form-item label="售后期" :error="problems.afterSaleDays">
                <span>订单完成后</span>
                <el-input-number v-model="form.afterSaleDays" :min="DAYS_MIN" :max="DAYS_MAX" :step="1" step-strictly />
                <span>天内可申请售后</span>
                <span class="hint">
                    从买家确认收货（或系统自动确认）算起，过了这个天数买家就不能再申请退款 / 退货。还没完成的订单不受限制。
                    按「此刻」的天数判断：改长之后，已过期的单重新可以申请。
                </span>
            </el-form-item>

            <el-form-item label="退货寄回时限" :error="problems.returnShipDays">
                <span>审核通过后</span>
                <el-input-number v-model="form.returnShipDays" :min="DAYS_MIN" :max="DAYS_MAX" :step="1" step-strictly />
                <span>天</span>
                <span class="hint">
                    退货退款审核通过后，买家超过这个天数还没填寄回物流，售后单自动关闭（买家会收到通知，可以重新申请）。
                    已经填了物流的不关。同样按「此刻」的天数判断。
                </span>
            </el-form-item>

            <el-form-item>
                <el-button type="primary" native-type="submit" :loading="saving"
                    :disabled="!dirty || Object.keys(problems).length > 0">保存</el-button>
                <el-button :disabled="!dirty" @click="reset">撤销修改</el-button>
            </el-form-item>
        </el-form>

        <ProblemAlert v-if="saveError" :error="saveError" />
    </div>
</template>

<style scoped>
.page-head {
    display: flex;
    align-items: baseline;
    gap: 16px;
}
.muted {
    color: var(--el-text-color-secondary);
    font-size: 13px;
}
.settings-form {
    max-width: 880px;
    margin-top: 16px;
}
.settings-form :deep(.el-form-item__content) {
    gap: 8px;
}
.hint {
    flex-basis: 100%;
    color: var(--el-text-color-secondary);
    font-size: 12px;
    line-height: 1.6;
}
</style>
