<script setup lang="ts">
// 新建 / 编辑运费模板（整体替换，契约 PUT /admin/freight-templates/{id}）。
//
// 一个模板 = 计费方式 + 按省分组的规则 + 包邮条件 + 不配送地区（数据模型 §7）。
// 地区按省多选：一个省只能出现在一条规则里、且不能同时是不配送 —— 别处已经选了的省
// 在多选框里置灰（api/freightRules.ts 的 takenElsewhere），提交前再本地校验一遍
// （checkTemplateDraft），服务端仍会判第三遍。
//
// 金额按「元」输入、按字符串换算成整数分（api/money.ts）；首重 / 续重按克输入。

import { computed, ref, watch } from "vue";
import { Delete, Plus } from "@element-plus/icons-vue";
import { keel, type AdminStore } from "../../api/client.ts";
import { type AdminFreightTemplate, type FreightChargeMode, type FreightTemplateInput } from "../../api/freight.ts";
import { checkTemplateDraft, PROVINCES, takenElsewhere, type RuleLike } from "../../api/freightRules.ts";
import { IdempotentSubmission, withIdempotency } from "../../api/idempotency.ts";
import { centsToYuanInput, yuanToCents } from "../../api/money.ts";
import { can } from "../../auth/permissions.ts";
import ProblemAlert from "../../components/ProblemAlert.vue";

const props = defineProps<{ modelValue: boolean; template: AdminFreightTemplate | null; stores: AdminStore[] }>();
const emit = defineEmits<{ "update:modelValue": [boolean]; saved: [AdminFreightTemplate] }>();

const visible = computed({
    get: () => props.modelValue,
    set: (v: boolean) => emit("update:modelValue", v),
});
const editing = computed(() => props.template !== null);

interface RuleForm {
    regions: string[];
    firstUnit: number;
    firstFee: string;
    addUnit: number;
    addFee: string;
    freeThreshold: string;
    freeQuantity: number;
}

interface Form {
    name: string;
    /** null = 全店模板。 */
    storeId: number | null;
    chargeMode: FreightChargeMode;
    isDefault: boolean;
    rules: RuleForm[];
    undeliverable: string[];
}

function defaultRule(mode: FreightChargeMode): RuleForm {
    return mode === 1
        ? { regions: [], firstUnit: 1, firstFee: "8", addUnit: 1, addFee: "2", freeThreshold: "", freeQuantity: 0 }
        : { regions: [], firstUnit: 1000, firstFee: "10", addUnit: 1000, addFee: "5", freeThreshold: "", freeQuantity: 0 };
}

function emptyForm(): Form {
    return { name: "", storeId: null, chargeMode: 1, isDefault: false, rules: [defaultRule(1)], undeliverable: [] };
}

function formOf(t: AdminFreightTemplate): Form {
    return {
        name: t.name,
        storeId: t.store_id,
        chargeMode: t.charge_mode,
        isDefault: t.is_default,
        rules: t.rules.map((r) => ({
            regions: [...r.region_codes],
            firstUnit: r.first_unit,
            firstFee: centsToYuanInput(r.first_fee_cents),
            addUnit: r.additional_unit,
            addFee: centsToYuanInput(r.additional_fee_cents),
            freeThreshold: r.free_threshold_cents > 0 ? centsToYuanInput(r.free_threshold_cents) : "",
            freeQuantity: r.free_quantity,
        })),
        undeliverable: [...t.undeliverable_region_codes],
    };
}

const form = ref<Form>(emptyForm());
const localError = ref("");
const error = ref<unknown>(null);
const saving = ref(false);
const submission = new IdempotentSubmission();

watch(
    () => props.modelValue,
    (open) => {
        if (!open) return;
        form.value = props.template === null ? emptyForm() : formOf(props.template);
        localError.value = "";
        error.value = null;
        submission.rotate();
    },
);

const unit = computed(() => (form.value.chargeMode === 1 ? "件" : "克"));

/** 当前身份能挑哪些门店（界面体验；服务端按门店价的判据再判）。 */
const ownerOptions = computed(() => props.stores.filter((s) => can.editFreight(s)));

/** 把表单里的规则换成「分」的形状，给本地校验与提交共用。金额不合法时返回错误文案。 */
function rulesOut(): { ok: true; rules: RuleLike[] } | { ok: false; msg: string } {
    const out: RuleLike[] = [];
    for (let i = 0; i < form.value.rules.length; i += 1) {
        const r = form.value.rules[i]!;
        const money = (label: string, s: string, required: boolean): number | string => {
            if (s.trim() === "") return required ? `第 ${i + 1} 条规则请填${label}` : 0;
            const v = yuanToCents(s);
            return v === null ? `第 ${i + 1} 条规则的${label}「${s}」不是合法金额（元，至多两位小数）` : v;
        };
        const first = money("首费", r.firstFee, true);
        const add = money("续费", r.addFee, true);
        const th = money("满额包邮门槛", r.freeThreshold, false);
        for (const v of [first, add, th]) if (typeof v === "string") return { ok: false, msg: v };
        out.push({
            region_codes: [...r.regions],
            first_unit: r.firstUnit,
            first_fee_cents: first as number,
            additional_unit: r.addUnit,
            additional_fee_cents: add as number,
            free_threshold_cents: th as number,
            free_quantity: r.freeQuantity,
        });
    }
    return { ok: true, rules: out };
}

/** 某一条规则（或不配送）的多选框里要置灰的省：别处已经选了。 */
function disabledFor(self: number | "undeliverable"): Set<string> {
    const rules = form.value.rules.map((r) => ({ region_codes: r.regions }) as RuleLike);
    return takenElsewhere(rules, form.value.undeliverable, self);
}

function addRule(): void {
    const r = defaultRule(form.value.chargeMode);
    // 新加的规则默认是「指定地区」，留一个空的待选；默认规则只能有一条。
    form.value.rules.splice(form.value.rules.length - 1, 0, { ...r, regions: [] });
}

function removeRule(i: number): void {
    form.value.rules.splice(i, 1);
}

function onModeChange(mode: FreightChargeMode): void {
    // 换计费方式时首件 / 续件的单位变了（件 ↔ 克），数值原样留着会是荒唐的「首重 1 克」。
    const d = defaultRule(mode);
    for (const r of form.value.rules) {
        r.firstUnit = d.firstUnit;
        r.addUnit = d.addUnit;
    }
}

async function submit(): Promise<void> {
    localError.value = "";
    error.value = null;
    const f = form.value;
    if (f.name.trim() === "") {
        localError.value = "请填模板名称";
        return;
    }
    const rules = rulesOut();
    if (!rules.ok) {
        localError.value = rules.msg;
        return;
    }
    const bad = checkTemplateDraft(rules.rules, f.undeliverable);
    if (bad !== null) {
        localError.value = bad;
        return;
    }
    if (f.isDefault && f.storeId !== null) {
        localError.value = "门店模板不能设成全店默认";
        return;
    }
    const body: FreightTemplateInput = {
        name: f.name.trim(),
        store_id: f.storeId,
        charge_mode: f.chargeMode,
        is_default: f.isDefault,
        rules: rules.rules,
        undeliverable_region_codes: [...f.undeliverable],
    };
    saving.value = true;
    try {
        let saved: AdminFreightTemplate;
        if (props.template === null) {
            saved = await withIdempotency(submission, (key) =>
                keel.request("post", "/admin/freight-templates", { body, headers: { "Idempotency-Key": key } }),
            );
        } else {
            saved = await keel.request("put", "/admin/freight-templates/{template_id}", {
                path: { template_id: props.template.id },
                body,
            });
        }
        emit("saved", saved);
        visible.value = false;
    } catch (err) {
        error.value = err;
    } finally {
        saving.value = false;
    }
}
</script>

<template>
    <el-dialog v-model="visible" :title="editing ? `编辑运费模板 #${template?.id}` : '新建运费模板'" width="900px">
        <ProblemAlert v-if="error" :error="error" />
        <el-alert v-if="localError" :title="localError" type="error" :closable="false" show-icon class="mb8" />
        <el-form label-width="96px" @submit.prevent>
            <el-form-item label="名称" required><el-input v-model="form.name" maxlength="60" show-word-limit /></el-form-item>
            <el-form-item label="归属">
                <el-select v-model="form.storeId" style="width: 280px" placeholder="全店模板">
                    <el-option :value="null" label="全店模板（商品可以单独挂它）" :disabled="!can.editFreight(null)" />
                    <el-option v-for="s in ownerOptions" :key="s.id" :value="s.id" :label="`门店模板：${s.name}`" />
                </el-select>
                <span class="hint ml8">门店模板管这家店发货、没单独挂模板的商品；每家店至多一个</span>
            </el-form-item>
            <el-form-item label="全店默认">
                <el-switch v-model="form.isDefault" :disabled="form.storeId !== null" />
                <span class="hint ml8">没单独挂模板、门店也没有门店模板的商品按它算；同一时刻只有一个</span>
            </el-form-item>
            <el-form-item label="计费方式" required>
                <el-radio-group v-model="form.chargeMode" @change="(v: string | number | boolean | undefined) => onModeChange(v === 2 ? 2 : 1)">
                    <el-radio :value="1">按件</el-radio>
                    <el-radio :value="2">按重量（取 SKU 的重量，单位克）</el-radio>
                </el-radio-group>
            </el-form-item>

            <el-form-item label="计费规则" required>
                <div class="rules">
                    <div v-for="(r, i) in form.rules" :key="i" class="rule">
                        <div class="rule-head">
                            <strong>{{ r.regions.length === 0 ? "其余地区（默认规则）" : `指定地区 #${i + 1}` }}</strong>
                            <el-button
                                v-if="form.rules.length > 1"
                                link
                                type="danger"
                                :icon="Delete"
                                @click="removeRule(i)"
                            >删除</el-button>
                        </div>
                        <el-select
                            v-model="r.regions"
                            multiple
                            filterable
                            collapse-tags
                            collapse-tags-tooltip
                            :max-collapse-tags="8"
                            placeholder="不选任何省 = 其余地区（默认规则，恰好一条）"
                            style="width: 100%"
                        >
                            <el-option
                                v-for="p in PROVINCES"
                                :key="p.code"
                                :value="p.code"
                                :label="p.name"
                                :disabled="disabledFor(i).has(p.code)"
                            />
                        </el-select>
                        <div class="rule-row">
                            <span>首{{ form.chargeMode === 1 ? "件" : "重" }}</span>
                            <el-input-number v-model="r.firstUnit" :min="1" :max="1000000" size="small" />
                            <span>{{ unit }}，</span>
                            <el-input v-model="r.firstFee" size="small" class="money"><template #append>元</template></el-input>
                            <span>；每续</span>
                            <el-input-number v-model="r.addUnit" :min="1" :max="1000000" size="small" />
                            <span>{{ unit }}，加</span>
                            <el-input v-model="r.addFee" size="small" class="money"><template #append>元</template></el-input>
                        </div>
                        <div class="rule-row">
                            <span>满</span>
                            <el-input v-model="r.freeThreshold" size="small" class="money" placeholder="不设">
                                <template #append>元</template>
                            </el-input>
                            <span>包邮，或满</span>
                            <el-input-number v-model="r.freeQuantity" :min="0" :max="999999" size="small" />
                            <span>件包邮（0 = 不设）</span>
                        </div>
                    </div>
                    <el-button :icon="Plus" size="small" @click="addRule">加一条指定地区的规则</el-button>
                    <p class="hint">
                        满额包邮比的是<strong>优惠后</strong>的应付商品金额（营销活动、优惠券都减完之后），满件比的是整单件数。
                        偏远地区另设一条规则、不填包邮门槛，就是「偏远地区不包邮」。
                    </p>
                </div>
            </el-form-item>

            <el-form-item label="不配送地区">
                <el-select v-model="form.undeliverable" multiple filterable placeholder="不选即全国配送" style="width: 100%">
                    <el-option
                        v-for="p in PROVINCES"
                        :key="p.code"
                        :value="p.code"
                        :label="p.name"
                        :disabled="disabledFor('undeliverable').has(p.code)"
                    />
                </el-select>
                <p class="hint">收货地址在这些省时，下单会被拒（逐行告诉买家哪几件送不到）。</p>
            </el-form-item>
        </el-form>
        <template #footer>
            <el-button @click="visible = false">取消</el-button>
            <el-button type="primary" :loading="saving" @click="submit">{{ editing ? "保存" : "创建" }}</el-button>
        </template>
    </el-dialog>
</template>

<style scoped>
.mb8 {
    margin-bottom: 8px;
}
.ml8 {
    margin-left: 8px;
}
.hint {
    color: var(--el-text-color-secondary);
    font-size: 12px;
    margin: 4px 0 0;
}
.rules {
    width: 100%;
}
.rule {
    border: 1px solid var(--el-border-color-lighter);
    border-radius: 4px;
    padding: 8px 12px;
    margin-bottom: 8px;
}
.rule-head {
    display: flex;
    justify-content: space-between;
    align-items: center;
    margin-bottom: 6px;
}
.rule-row {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: 6px;
    margin-top: 6px;
}
.money {
    width: 130px;
}
</style>
