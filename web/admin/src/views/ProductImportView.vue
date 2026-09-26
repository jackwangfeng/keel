<script setup lang="ts">
// 商品批量导入（契约 /admin/product-imports）：下载模板 → 上传预检 → 改类目 → 确认导入 → 看结果。
//
// 两条要记住的契约语义，界面照着做：
//
//   · **预检不落库**，确认时把**同一份文件**再传一次。所以选中的 File 对象要一直留着，
//     换了文件就必须重新预检（否则确认的是另一份没被看过的文件）。
//   · **确认这一步不调推理引擎**，也不会自动采用推荐。推荐的类目要由界面原样带回
//     （importRules.ts 的 commitChoices）。
//
// 表格里一行一个 SKU；类目选择框只画在每件商品的第一行上。错误行标红，
// 违禁词在原文上标出来（HighlightedText），不阻断导入：导入的是草稿，上架时才拦。

import { computed, nextTick, onMounted, ref } from "vue";
import { useRouter } from "vue-router";
import { Back, Download, Upload } from "@element-plus/icons-vue";
import type { UploadFile } from "element-plus";
import {
    commitProductImport,
    downloadImportTemplate,
    previewProductImport,
    type AdminCategory,
    type ProductImportFormat,
    type ProductImportPreview,
    type ProductImportResult,
    type ProductImportRow,
} from "../api/client.ts";
import { indentedLabel, listCategories } from "../api/catalog.ts";
import { IdempotentSubmission, withIdempotency } from "../api/idempotency.ts";
import {
    canCommit,
    commitChoices,
    commitSummary,
    DECISION_TEXT,
    initialSelections,
    isFirstRowOfProduct,
    productOf,
    rowTone,
    scoreText,
    specText,
    violationsOf,
    type Selections,
} from "../api/importRules.ts";
import { datetime, yuan } from "../ui/format.ts";
import { notifyError, notifyOk } from "../ui/notify.ts";
import { can, NO_PERMISSION } from "../auth/permissions.ts";
import ProblemAlert from "../components/ProblemAlert.vue";
import HighlightedText from "../components/HighlightedText.vue";

const router = useRouter();

const step = ref<0 | 1 | 2>(0);
const file = ref<File | null>(null);
const categories = ref<AdminCategory[]>([]);

const previewing = ref(false);
const previewError = ref<unknown>(null);
const preview = ref<ProductImportPreview | null>(null);
const selections = ref<Selections>({});
const onlyProblems = ref(false);

const committing = ref(false);
const commitError = ref<unknown>(null);
const result = ref<ProductImportResult | null>(null);
// 结果卡片：确认之后滚过去（预检表可能有几百行高，结果在它下面，不滚的话看起来像没反应）。
const resultCard = ref<{ $el?: HTMLElement } | null>(null);
// 一次预检结果对应一次提交：换文件 / 重新预检时换钥匙。判据在 api/idempotency.ts 文件头。
const submission = new IdempotentSubmission();

onMounted(async () => {
    try {
        categories.value = await listCategories();
    } catch (err) {
        notifyError(err);
    }
});

/** 可选的类目：启用中的（停用的选了服务端也会让那件商品 failed）。 */
const activeCategories = computed(() => categories.value.filter((c) => c.status === 1));

async function download(format: ProductImportFormat): Promise<void> {
    try {
        await downloadImportTemplate(format);
    } catch (err) {
        notifyError(err);
    }
}

function onFileChange(f: UploadFile): void {
    file.value = f.raw ?? null;
    // 换了文件：旧的预检结果不再对应这份文件，清掉，免得确认了一份没看过的文件。
    preview.value = null;
    result.value = null;
    previewError.value = null;
    commitError.value = null;
    step.value = 0;
}

async function runPreview(): Promise<void> {
    if (file.value === null) return;
    previewing.value = true;
    previewError.value = null;
    try {
        const pv = await previewProductImport(file.value);
        preview.value = pv;
        selections.value = initialSelections(pv);
        submission.rotate();
        step.value = 1;
    } catch (err) {
        previewError.value = err;
    } finally {
        previewing.value = false;
    }
}

const summary = computed(() =>
    preview.value === null ? null : commitSummary(preview.value, selections.value),
);

const visibleRows = computed<ProductImportRow[]>(() => {
    const rows = preview.value?.rows ?? [];
    return onlyProblems.value ? rows.filter((r) => rowTone(r) !== "") : rows;
});

function rowClass({ row }: { row: ProductImportRow }): string {
    const tone = rowTone(row);
    return tone === "" ? "" : `import-row-${tone}`;
}

function decisionOf(row: ProductImportRow) {
    return preview.value === null ? undefined : productOf(preview.value, row.first_row);
}

async function submit(): Promise<void> {
    if (file.value === null || preview.value === null) return;
    committing.value = true;
    commitError.value = null;
    const choices = commitChoices(preview.value, selections.value);
    try {
        const res = await withIdempotency(submission, (key) => commitProductImport(file.value as File, choices, key));
        result.value = res;
        step.value = 2;
        await nextTick();
        resultCard.value?.$el?.scrollIntoView({ behavior: "smooth", block: "start" });
        notifyOk(res.already_imported
            ? "这份文件之前已经导入过，这次没有重复建商品"
            : `已导入 ${res.created_products} 件商品（${res.created_skus} 个 SKU），全部是草稿`);
    } catch (err) {
        commitError.value = err;
    } finally {
        committing.value = false;
    }
}

function restart(): void {
    file.value = null;
    preview.value = null;
    result.value = null;
    previewError.value = null;
    commitError.value = null;
    step.value = 0;
}

const failedOutcomes = computed(() => (result.value?.products ?? []).filter((o) => o.status === "failed"));
const imageOutcomes = computed(() =>
    (result.value?.products ?? []).filter((o) => o.status === "created" && (o.image_urls?.length ?? 0) > 0),
);
</script>

<template>
    <div class="import-page">
        <div class="page-toolbar">
            <el-button :icon="Back" @click="router.push({ name: 'products' })">返回商品列表</el-button>
            <span class="grow" />
        </div>

        <el-alert v-if="!can.editCatalog()" type="warning" :closable="false" show-icon :title="NO_PERMISSION" class="mb12" />

        <el-steps :active="step" finish-status="success" align-center class="mb12">
            <el-step title="选文件并预检" description="下载模板填好后上传；预检不写库" />
            <el-step title="核对与改类目" description="红色行不会导入；类目可改" />
            <el-step title="导入结果" description="全部是草稿，不会自动上架" />
        </el-steps>

        <!-- 第一步：模板与上传 -->
        <el-card shadow="never" class="mb12">
            <template #header>1. 下载模板，填好后上传</template>
            <p class="hint">
                列：商品标题（必填）、副标题、类目、规格名 / 规格值、SKU 编码（必填）、基准价（元，必填）、库存（必填）、重量（克）、图片 URL、描述。
                <strong>同一个标题的多行会合并成同一件商品</strong>（一行一个规格）。单次最多 2000 行、5 MB。
                图片 URL 本期只记录不下载，导入后请在商品页上传。
            </p>
            <div class="row-gap">
                <el-button :icon="Download" @click="download('xlsx')">下载 Excel 模板</el-button>
                <el-button :icon="Download" @click="download('csv')">下载 CSV 模板</el-button>
            </div>
            <el-upload
                drag
                :auto-upload="false"
                :limit="1"
                :show-file-list="false"
                accept=".xlsx,.csv"
                :disabled="!can.editCatalog()"
                :on-change="onFileChange"
                class="mt12"
            >
                <el-icon class="el-icon--upload"><Upload /></el-icon>
                <div class="el-upload__text">
                    <template v-if="file">已选：<strong>{{ file.name }}</strong>（再点一次或拖进来可更换）</template>
                    <template v-else>把 xlsx / csv 拖到这里，或<em>点击选择</em></template>
                </div>
            </el-upload>
            <div class="mt12">
                <el-button type="primary" :loading="previewing" :disabled="file === null || !can.editCatalog()" @click="runPreview">
                    上传并预检
                </el-button>
            </div>
            <ProblemAlert v-if="previewError" :error="previewError" class="mt12" />
        </el-card>

        <!-- 第二步：预检结果 -->
        <el-card v-if="preview && step >= 1" shadow="never" class="mb12">
            <template #header>
                2. 预检结果：{{ preview.total_rows }} 行、{{ preview.products.length }} 件商品，
                <span :class="{ danger: preview.error_rows > 0 }">{{ preview.error_rows }} 行有错</span>
            </template>

            <el-alert v-if="preview.previous_import" type="warning" :closable="false" show-icon class="mb12"
                :title="`这份文件已经在 ${datetime(preview.previous_import.created_at)} 导入过（导入记录 #${preview.previous_import.import_id}）。再确认不会重复建商品，只会返回那一次的结果。`" />
            <el-alert v-if="preview.category_engine !== 'ok'" type="info" :closable="false" show-icon class="mb12"
                :title="preview.category_engine === 'not_configured'
                    ? '这个部署没有配置推理引擎，没有类目推荐：没对上类目的商品请手选。导入本身不受影响。'
                    : '推理引擎这次没在时限内给出推荐（忙或不可用）：没对上类目的商品请手选，或稍后重新预检。'" />
            <el-alert v-for="(n, i) in preview.notices" :key="i" type="info" :closable="false" :title="n" class="mb8" />

            <p class="hint">
                类目推荐：按「标题 + 副标题」与各末级类目名的语义相似度排序，给前三个候选。
                相似度 ≥ {{ preview.category_gate.min_score }} 且比第二名高出 ≥ {{ preview.category_gate.min_margin }} 时才替你选上，
                否则标「需人工确认」。推荐只是建议，确认前都可以改。
            </p>

            <div class="page-toolbar">
                <el-checkbox v-model="onlyProblems">只看有问题的行</el-checkbox>
                <span class="grow" />
            </div>

            <el-table :data="visibleRows" border size="small" :row-class-name="rowClass" row-key="row" max-height="560">
                <el-table-column prop="row" label="行" width="56" fixed />
                <el-table-column label="商品" min-width="220">
                    <template #default="{ row }: { row: ProductImportRow }">
                        <div>{{ row.title ?? "（无标题）" }}</div>
                        <HighlightedText v-if="violationsOf(row, 'title').length > 0" :text="row.title ?? ''" :hits="violationsOf(row, 'title')" />
                        <div v-if="row.subtitle" class="hint">{{ row.subtitle }}</div>
                        <HighlightedText v-if="violationsOf(row, 'subtitle').length > 0" :text="row.subtitle ?? ''" :hits="violationsOf(row, 'subtitle')" />
                        <HighlightedText v-if="violationsOf(row, 'description').length > 0" :text="row.description ?? ''" :hits="violationsOf(row, 'description')" />
                    </template>
                </el-table-column>
                <el-table-column label="规格" width="150">
                    <template #default="{ row }: { row: ProductImportRow }">{{ specText(row.spec_values) }}</template>
                </el-table-column>
                <el-table-column prop="sku_code" label="SKU 编码" width="150" />
                <el-table-column label="基准价" width="100">
                    <template #default="{ row }: { row: ProductImportRow }">{{ row.price_cents === undefined ? "—" : yuan(row.price_cents) }}</template>
                </el-table-column>
                <el-table-column label="库存" width="70">
                    <template #default="{ row }: { row: ProductImportRow }">{{ row.stock ?? "—" }}</template>
                </el-table-column>
                <el-table-column label="类目" min-width="260">
                    <template #default="{ row }: { row: ProductImportRow }">
                        <template v-if="isFirstRowOfProduct(row) && decisionOf(row)">
                            <el-tag size="small" :type="DECISION_TEXT[decisionOf(row)!.category.status].tag" class="mb4">
                                {{ DECISION_TEXT[decisionOf(row)!.category.status].text }}
                            </el-tag>
                            <el-select
                                v-model="selections[row.first_row]"
                                filterable
                                clearable
                                placeholder="选择类目"
                                size="small"
                                style="width: 100%"
                                :disabled="!decisionOf(row)!.importable"
                            >
                                <el-option-group v-if="decisionOf(row)!.category.candidates.length > 0" label="推荐（相似度）">
                                    <el-option
                                        v-for="c in decisionOf(row)!.category.candidates"
                                        :key="`r${c.category_id}`"
                                        :value="c.category_id"
                                        :label="`${c.path_name}（${scoreText(c.score)}）`"
                                    />
                                </el-option-group>
                                <el-option-group label="全部类目">
                                    <el-option v-for="c in activeCategories" :key="c.id" :value="c.id" :label="indentedLabel(c)" />
                                </el-option-group>
                            </el-select>
                        </template>
                        <span v-else class="hint">同第 {{ row.first_row }} 行</span>
                    </template>
                </el-table-column>
                <el-table-column label="问题" min-width="260">
                    <template #default="{ row }: { row: ProductImportRow }">
                        <div v-for="(e, i) in row.errors" :key="`e${i}`" class="danger">
                            <span v-if="e.column">「{{ e.column }}」</span>{{ e.message }}
                        </div>
                        <div v-for="(w, i) in row.warnings" :key="`w${i}`" class="warn">
                            <span v-if="w.column">「{{ w.column }}」</span>{{ w.message }}
                        </div>
                        <div v-for="(v, i) in row.violations" :key="`v${i}`" class="warn">{{ v.message }}</div>
                    </template>
                </el-table-column>
            </el-table>

            <div v-if="summary" class="commit-bar">
                <span>
                    将导入 <strong>{{ summary.products }}</strong> 件商品（{{ summary.skus }} 个 SKU）
                    <template v-if="summary.blocked > 0">，跳过 <span class="danger">{{ summary.blocked }}</span> 件有错的</template>
                    <template v-if="summary.missingCategory > 0">，<span class="warn">{{ summary.missingCategory }}</span> 件还没选类目（不选会被跳过）</template>
                    。导入的商品一律是草稿。
                </span>
                <span class="grow" />
                <!-- 导入完成之后按钮置灰：同一份文件再确认只会拿回那一次的结果（服务端认得这份文件），
                     点了也不会重复建，但会让人以为又导了一遍。要再导，点结果页的「再导一份」。 -->
                <el-button type="primary" :loading="committing" :disabled="!canCommit(preview, selections) || !can.editCatalog() || result !== null" @click="submit">
                    确认导入
                </el-button>
            </div>
            <ProblemAlert v-if="commitError" :error="commitError" class="mt12" />
        </el-card>

        <!-- 第三步：结果 -->
        <el-card v-if="result && step === 2" ref="resultCard" shadow="never">
            <template #header>3. 导入结果（导入记录 #{{ result.import_id }}）</template>
            <el-result
                :icon="result.failed_rows > 0 ? 'warning' : 'success'"
                :title="result.already_imported ? '这份文件之前已经导入过' : `导入了 ${result.created_products} 件商品、${result.created_skus} 个 SKU`"
                :sub-title="`共 ${result.total_rows} 行，${result.failed_rows} 行没有导入。商品全部是草稿，到商品列表里补图、检查后再上架。`"
            >
                <template #extra>
                    <el-button type="primary" @click="router.push({ name: 'products', query: {} })">去商品列表</el-button>
                    <el-button @click="restart">再导一份</el-button>
                </template>
            </el-result>

            <el-table :data="result.products" border size="small" row-key="first_row">
                <el-table-column prop="first_row" label="首行" width="64" />
                <el-table-column prop="title" label="商品" min-width="200" />
                <el-table-column label="结果" width="110">
                    <template #default="{ row }">
                        <el-tag :type="row.status === 'created' ? 'success' : 'danger'" size="small">
                            {{ row.status === "created" ? `已建（${row.sku_count} 个 SKU）` : "未导入" }}
                        </el-tag>
                    </template>
                </el-table-column>
                <el-table-column label="说明" min-width="300">
                    <template #default="{ row }">
                        <router-link v-if="row.product_id" :to="{ name: 'product-detail', params: { productId: row.product_id } }">
                            打开商品 #{{ row.product_id }}
                        </router-link>
                        <div v-for="(r, i) in row.reasons ?? []" :key="i" class="danger">{{ r }}</div>
                    </template>
                </el-table-column>
            </el-table>

            <template v-if="imageOutcomes.length > 0">
                <h4>待上传的图片（文件里填了地址，本期没有自动下载）</h4>
                <ul class="image-list">
                    <li v-for="o in imageOutcomes" :key="o.first_row">
                        <router-link :to="{ name: 'product-detail', params: { productId: o.product_id } }">{{ o.title }}</router-link>：
                        <span v-for="(u, i) in o.image_urls ?? []" :key="i" class="url">{{ u }}</span>
                    </li>
                </ul>
            </template>
            <p v-if="failedOutcomes.length > 0" class="hint">
                没导入的商品：改好文件后重新导入即可 —— 已经建过的 SKU 编码会被识别出来跳过，不会重复建。
            </p>
        </el-card>
    </div>
</template>

<style scoped>
.mb4 {
    margin-bottom: 4px;
}
.mb8 {
    margin-bottom: 8px;
}
.mb12 {
    margin-bottom: 12px;
}
.mt12 {
    margin-top: 12px;
}
.row-gap {
    display: flex;
    gap: 8px;
    flex-wrap: wrap;
}
.danger {
    color: var(--el-color-danger);
}
.warn {
    color: var(--el-color-warning-dark-2);
}
.commit-bar {
    display: flex;
    align-items: center;
    gap: 12px;
    margin-top: 12px;
    flex-wrap: wrap;
}
.image-list .url {
    margin-right: 8px;
    word-break: break-all;
    font-family: var(--el-font-family-mono, monospace);
    font-size: 12px;
}
:deep(.import-row-error) td {
    background: var(--el-color-danger-light-9) !important;
}
:deep(.import-row-warning) td {
    background: var(--el-color-warning-light-9) !important;
}
</style>
