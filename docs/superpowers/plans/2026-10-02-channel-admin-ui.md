# 渠道管理页 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 在商家后台（`web/admin`，Vue 3 + Element Plus）加「渠道」菜单：渠道账号的增改启停与凭据、门店映射、库存与价格规则、推送状态、手动重新同步商品；商品详情对商品源管理的字段标「由 Shopify 管理」并锁住（服务端同样拒绝）。

**Architecture:** 后台接口第一期已齐，本期只补四处契约缺口（binding 是否已配凭据、推送状态带 SKU 名称并可只看出错的、手动重拉商品、商品的 `managed_by` 与对应的 409），其余全是前端。前端照现有惯例：类型全从契约生成（`@contract/schema.js`）、请求走 `src/api/client.ts`、纯逻辑放 `src/api/channelRules.ts` 配 `node --test` 单测、菜单与路由同一份声明（`src/router/modules/channels.ts`）。

**Tech Stack:** Go（gin handler + service）、OpenAPI 契约 → `make generate`、Vue 3.5 + Element Plus 2.14 + vue-router 5、`node --test`、`make admin-responsive-check`。

**Spec:** `docs/superpowers/specs/2026-10-02-channel-adapter-design.md`（§7.4「由 Shopify 管理」、§8「后台菜单不出现渠道」）。

## Global Constraints

- **不加迁移。**
- 新 problem type 只有一个：`https://keel.dev/problems/managed-by-channel`（409），登记进契约的 problem 类型清单与 `internal/problem`（照现有类型的写法）。
- 凭据只写不读：任何响应、任何前端状态都不出现 secret 的值；界面只显示「已配置 / 未配置」。
- 渠道写接口只有管理员能调（第一期已定）：前端对非管理员隐藏写按钮，但以服务端 403 为准。
- `KEEL_CHANNELS` 关闭时：菜单不出现「渠道」（前端探测 `GET /admin/channel-kinds`，404 = 关），商品详情不出现「由 … 管理」（`managed_by` 恒为 null），零新增查询（`managed_by` 的查询只在渠道服务存在时做）。
- 每个新路由进 `contract_test.go` 的 `routes` 表且在契约里。
- 前端不手写请求 / 响应类型；金额用 `src/api/money.ts`，万分比与百分数的换算写在 `channelRules.ts` 并有单测。
- 手机宽度（~400px）可用：表格放进 `overflow-x: auto`，对话框用现有的 `useMobile` 全屏模式。

## Review Focus

1. **凭据回显**：建账号 / 改账号 / 列表 / 详情任何一个响应里出现 `client_secret`，或者前端把用户输入的 secret 留在组件状态里（关掉对话框再打开还在）。测试：handler 测试断言响应体不含 secret 字符串；前端对话框关闭时清空表单（Task 4 手测清单）。
2. **商品被锁但没有提示 / 有提示但没锁住**：前端禁用了标题输入框，但 `PATCH /admin/products/{id}` 带 title 仍能改掉 Shopify 管的字段（下次同步又被覆盖，商家以为改成了）。测试：handler 测试 PATCH title / description、PATCH sku spec_values、PUT images 对 Shopify 商品回 409 `managed-by-channel`，改类目 / 价格 / 状态照常 200。（Task 2）
3. **停用的渠道账号仍然锁商品**：binding 停用后不再同步，字段应当放开。测试：停用后 PATCH title 200、`managed_by` 为 null。（Task 2）
4. **开关关闭时前端报错或闪一下菜单**：`/admin/channel-kinds` 404 时菜单静默不出现，不弹错误提示。（Task 3 单测探测函数 + 手测）
5. **万分比换算误差**：比例 33.33% ↔ 3333 bp、加价 −15% ↔ −1500 bp、固定价 12.30 元 ↔ 1230 分，往返不漂。（Task 3 单测）

---

### Task 1: 契约与服务端补口（凭据标志、推送状态、手动重拉）

**Files:**
- Modify: `docs/电商系统-OpenAPI.yaml`（`ChannelBinding.has_secrets`；`ChannelListing.sku_code` / `product_title`；`GET …/listings` 加 `errors_only` 查询参数；新路径 `POST /admin/channel-bindings/{binding_id}/catalog-pulls`）
- Modify: `db/queries/channels.sql`（listings 分页 join skus / products、`errors_only` 条件；binding 查询带 `secrets <> '{}'`）、`internal/repository/channel.go`、`internal/service/channel.go`（`RequestCatalogPull`）、`internal/service/admin_channel.go`、`internal/handler/admin_channel.go`（读 query 的接口按契约测试规矩放单独文件）、`internal/handler/contract_test.go`
- Test: `internal/handler/admin_channel_test.go`（追加）

**Interfaces:**
- Produces（契约）：
  - `ChannelBinding.has_secrets: boolean`（必填）
  - `ChannelListing.sku_code: string`、`ChannelListing.product_title: string`（必填）
  - `GET /admin/channel-bindings/{binding_id}/listings?errors_only=true`
  - `POST /admin/channel-bindings/{binding_id}/catalog-pulls` → 202（无体）；binding 不是启用中的商品源 → 409 `https://keel.dev/problems/conflict`（现有类型）；非管理员 403；不存在 404。带 Idempotency-Key（与其他后台写一致）。
- Produces（Go）：`func (s *ChannelService) RequestCatalogPull(ctx context.Context, bindingID int64) error`（`ErrChannelNotCatalogSource` 映射 409）。

- [ ] **Step 1: 写失败测试**：建 binding 之后 `has_secrets=false`，`PUT secrets` 之后 `true`，响应体不含 secret 字符串；listings 有 `sku_code` / `product_title`，`errors_only=true` 只回 `last_error` 非空的行；`catalog-pulls` 对启用中的商品源 202 并入队（jobs 里有 `channel.catalog.pull`），对假渠道（只有销售渠道角色）409，对操作员 403。
- [ ] **Step 2: 跑测试确认失败**。
- [ ] **Step 3: 改契约 → `make generate`（只确认退出码）→ 实现**。
- [ ] **Step 4: 跑测试通过**，含 `contract_test.go`、`go vet ./internal/...`。
- [ ] **Step 5: 提交** `渠道后台接口补口：binding 是否已配凭据、推送状态带 SKU 名称并可只看出错、手动重拉商品`

### Task 2: 「由渠道管理」的商品字段（服务端）

**Files:**
- Modify: 契约（`AdminProduct.managed_by: string | null`，`AdminProductDetail` 同；`PATCH /admin/products/{id}`、`PATCH /admin/skus/{id}`、`PUT /admin/products/{id}/images` 加 409 `managed-by-channel` 响应）、`internal/problem`（新类型）、`db/queries/channels.sql`（`ChannelManagedProducts(product_ids) → (product_id, channel)`：启用中的商品源 binding 的 product link）、`internal/repository/channel.go`、`internal/service/admin_catalog.go`（`UpdateProduct` / `UpdateSKU` / `ReplaceImages` 前检查；列表与详情填 `managed_by`）、`internal/handler`（错误映射）
- Test: `internal/handler/channel_managed_test.go`（新）

**Interfaces:**
- Produces（Go）：
  ```go
  var ErrManagedByChannel = errors.New("这个字段由渠道管理") // → 409 managed-by-channel，detail 带渠道名
  // ChannelService：nil 接收者返回空（开关关闭零开销）
  func (s *ChannelService) ManagedBy(ctx context.Context, tx repository.Tx, productIDs []int64) (map[int64]string, error)
  ```
- 被锁的字段：商品 `title`、`description`、图片；SKU `spec_values`。不锁：类目、品牌、副标题、运费模板、上下架、SKU 价格 / 成本 / 重量 / 状态 / 货号、库存。
- 渠道服务自己的同步（`syncCatalogItem` 直接用 tx，不经 `AdminCatalogService`）不受影响。

- [ ] **Step 1: 写失败测试**（用第二期的 `newShopifyRig` 拉一件商品进来）：`PATCH title` → 409 `managed-by-channel`，detail 含 shopify；`PATCH category_id` → 200；`PATCH sku spec_values` → 409；`PATCH sku price_cents` → 200；`PUT images` → 409；商品详情与列表 `managed_by = "shopify"`；停用 binding 之后 `PATCH title` → 200、`managed_by = null`；keel 自建商品 `managed_by = null`、照常可改。
- [ ] **Step 2: 跑测试确认失败**。
- [ ] **Step 3: 实现**：检查放在各写事务里、先于写入（同一个事务读 link 与 binding 状态）；列表填 `managed_by` 一次批量查询（不逐件）。
- [ ] **Step 4: 跑测试通过**，再跑 `go test ./internal/handler/ -run 'Product|SKU|Catalog|Channel|Shopify'` 与契约测试。
- [ ] **Step 5: 提交** `商品源管理的字段（标题、详情、图片、规格）在后台标出并锁住：改动回 409 managed-by-channel，停用渠道即放开`

### Task 3: 前端骨架——API 封装、纯逻辑、菜单

**Files:**
- Create: `web/admin/src/api/channels.ts`（对 `client.ts` 的薄封装 + 类型别名）、`web/admin/src/api/channelRules.ts`、`web/admin/src/api/channelRules.test.ts`、`web/admin/src/router/modules/channels.ts`
- Modify: `web/admin/src/api/client.ts`（导出 `ChannelBinding` / `ChannelKind` / `ChannelStoreLink` / `ChannelStockRule` / `ChannelPriceRule` / `ChannelListing` 类型别名）、`web/admin/src/router/modules/index.ts`、`web/admin/src/router/section.ts` + `web/admin/src/layouts/MainLayout.vue`（分区可选的 `available?: () => Promise<boolean>`，菜单渲染前求一次、失败按不可用）
- Modify: `Makefile` 的 `admin-test` 若是按文件列举则加上新测试

**Interfaces:**
- Produces（`channelRules.ts`，全部纯函数）：
  ```ts
  export function percentToBp(p: number): number          // 33.33 → 3333，四舍五入到整数 bp
  export function bpToPercent(bp: number): number         // 3333 → 33.33
  export function bindingStatusLabel(s: number): { text: string; type: "success" | "info" | "danger" } // 1 启用 / 2 停用 / 3 凭据失效
  export function rolesLabel(roles: number): string        // 5 → "商品源、销售渠道"
  export function validateStockRule(r: { storeId: number | null; skuId: number | null; ratioPercent: number; safetyQty: number; capQty: number | null }): string[] // 中文错误列表
  export function validatePriceRule(r: { skuId: number | null; markupPercent: number; fixedYuan: string | null }): string[]
  export function channelsAvailable(probe: () => Promise<{ status: number }>): Promise<boolean> // 200 → true，404 → false，别的 → false 且不抛
  export function shopifyDomainOk(s: string): boolean      // xxx.myshopify.com
  ```
- 菜单：「渠道」，图标 `Connection`，`order` 放在多收款退回（33）之后、留间隔（例如 36）。路由：`channels`（列表，menu）、`channels/:id`（详情，不进菜单）。

- [ ] **Step 1: 写失败测试** `channelRules.test.ts`：Review Focus 5 的换算往返；`validateStockRule` 只给 SKU 不给门店 → 报错，比例 > 100 → 报错；`validatePriceRule` 固定价只能配 SKU；`channelsAvailable` 对 200 / 404 / 500 / 抛异常分别返回 true / false / false / false；`shopifyDomainOk`。
- [ ] **Step 2: `make admin-test` 确认失败**。
- [ ] **Step 3: 实现 `channelRules.ts`、`channels.ts`、路由与菜单探测**（列表页先放占位组件）。
- [ ] **Step 4: `make admin-test`、`make admin-type-check` 通过**。
- [ ] **Step 5: 提交** `后台渠道：API 封装与纯逻辑（万分比换算、规则校验、开关探测），「渠道」菜单按开关出现`

### Task 4: 渠道账号列表与详情页

**Files:**
- Create: `web/admin/src/views/channels/ChannelListView.vue`、`ChannelDetailView.vue`、`ChannelBindingDialog.vue`（新建 / 编辑账号）、`ChannelSecretsDialog.vue`、`ChannelStoreLinks.vue`、`ChannelRules.vue`、`ChannelListings.vue`
- Modify: `web/admin/README.md` 第五节页面表加一行

**页面内容：**
- **列表**：名称、渠道、店铺账号、角色、状态标签、是否已配凭据、回调路径（`webhook_path`，可复制）；「新建账号」按钮（管理员）。
- **新建 / 编辑对话框**：渠道（下拉，来自 channel-kinds）、店铺账号（Shopify 校验域名格式）、名称、角色（按所选渠道能当的角色勾选）、默认类目（类目下拉，商品源必填）、价格源门店（门店下拉，可空）、回调地址前缀（默认填当前站点 `location.origin`）。新建一律停用，提示「配好凭据和门店映射后再启用」。
- **详情页**：顶部概况（状态、启用 / 停用按钮、编辑、配置凭据、重新同步商品〔仅启用中的商品源〕）；页签：门店映射（门店下拉 + 外部门店 ID，可删）、库存规则（渠道级 / 门店级 / SKU 级，比例用百分数输入）、价格规则（渠道级加价 / SKU 固定价，元输入）、推送状态（门店筛选、只看出错、分页；列：SKU 货号、商品名、门店、对外可售、价格、推送时间、错误）。
- **凭据对话框**：Shopify 两个字段 `client_id`、`client_secret`（password 输入），只写；关闭即清空；保存成功只提示「已保存」。
- 所有写操作带 Idempotency-Key（用 `src/api/idempotency.ts`）；Problem 错误用现有 `src/api/errors.ts` 的呈现方式；非管理员隐藏写按钮。

- [ ] **Step 1: 实现列表与新建 / 编辑 / 凭据对话框**，`make admin-type-check`。
- [ ] **Step 2: 实现详情页四个页签**，`make admin-type-check`。
- [ ] **Step 3: 本机起栈手测**（`KEEL_CHANNELS=on`，对开发店建一个账号走完：建 → 配凭据 → 映射门店 → 启用 → 看推送状态 → 重新同步 → 停用；凭据对话框关闭再开是空的；开关关闭时没有菜单），记录结果。
- [ ] **Step 4: `make admin-responsive-check`**（手机宽度不横向滚动），截图看一遍。
- [ ] **Step 5: 提交** `后台渠道：账号列表、新建与凭据、详情（门店映射、库存与价格规则、推送状态、重新同步商品）`

### Task 5: 商品详情标注「由 Shopify 管理」

**Files:**
- Modify: `web/admin/src/views/ProductDetailView.vue`（与它用到的 SKU / 图片组件）、`web/admin/src/api/channelRules.ts`（`managedLabel(channel: string | null): string | null`，"shopify" → "由 Shopify 管理"，未知渠道 → "由 <渠道> 管理"）+ 测试；`ProductListView.vue` 标题旁小标签

- [ ] **Step 1: 写失败测试** `managedLabel`。
- [ ] **Step 2: 实现**：`managed_by` 非空时标题、详情、图片、SKU 规格输入禁用并显示说明（「这些字段从 Shopify 同步，在 Shopify 后台修改；价格、库存、类目仍在这里改」）；万一仍收到 409 `managed-by-channel`（并发同步），显示 detail 并刷新详情。
- [ ] **Step 3: `make admin-test`、`make admin-type-check`、本机手测一件 Shopify 商品与一件自建商品。**
- [ ] **Step 4: 提交** `后台商品：由渠道管理的字段标出并禁用`

### Task 6: 收尾

- [ ] 全量 `make test-db`、`./scripts/check-all.sh`（不同时跑）。
- [ ] 独立 agent 审查整个分支（前端 + 契约 + 服务端补口）。
- [ ] CHANGELOG、PROGRESS（含页面截图路径或手测记录）、`web/admin/README.md` 页面表。
- [ ] 合并、部署演示站（备份、构建 app 与 console、外网验证），在演示站后台看一遍渠道账号 1。
