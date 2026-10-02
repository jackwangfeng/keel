# 渠道适配层 · 第一期：渠道骨架 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 建起渠道层的骨架：表、接口、规则计算、库存变化通知、回调入口、推送队列、后台管理接口，以及「不配渠道零开销」的开关与不变量测试。没有任何真实适配器（第二期接 Shopify），用一个测试用的假适配器跑通全链路。

**Architecture:** `internal/channel` 放与 keel 其余部分无关的纯类型、接口、注册表与规则计算；`internal/service/channel*.go` 放用到仓储与库存服务的编排；库存服务的 `StockNotifier` 增加一条只对「开了渠道的商家」发的 `stock.changed` 二阶段消息；core 启停 binding 时经二阶段消息维护库存库里的「开了渠道的商家」表。进程开关 `KEEL_CHANNELS`（默认关）在 `internal/app` 决定是否注册任何渠道路由、分支与后台任务。

**Tech Stack:** Go、PostgreSQL（goose 迁移、sqlc、RLS）、dtmrs 二阶段消息、jobs 表、gin、OpenAPI 契约（`docs/电商系统-OpenAPI.yaml` → `make generate`）。

**Spec:** `docs/superpowers/specs/2026-10-02-channel-adapter-design.md`

## Global Constraints

- 迁移号段：core `00300`–`00349`，库存库 `00300`–`00319`；本期用 core `00300`（镜像到库存库 `00300`）与 `00301`。
- 库存库的表必须同时出现在 core 迁移（同号）与 `db/migrations-inventory/`（`IF NOT EXISTS`、角色走 `KEEL_INVENTORY_ROLE`），照 `00240_activity_sync_revs.sql`。
- 新表全部 `merchant_id DEFAULT current_merchant()` + `ENABLE/FORCE ROW LEVEL SECURITY` + `tenant` 策略 + `GRANT … TO keel_app`，照 `00230`。
- 租户上下文只能从 Host / gid 来；后台任务手建上下文要进 `tenant_context_test.go` 的放行清单。
- 每个注册的路由要么进 `contract_test.go` 的 `routes` 表（且在契约里），要么进 `nonContractRoutes` 并写理由。
- `KEEL_CHANNELS` 默认关；关闭时不注册任何渠道路由 / 分支 / 后台任务，库存服务不发 `stock.changed`。
- `channel_bindings.secrets` 只写不读：任何响应都不回显。
- 新增改库存的路径都要过 `stockTx`（不另开事务外壳）。

## Review Focus

1. **开关关闭时库存写路径多了一次查询**：`stock.changed` 的判定必须在开关关闭时零查询（不碰 `channel_merchants`），测试断言 `StockNotifier` 的查询计数为 0。（Task 4）
2. **同一事务里同一 SKU 改两次又改回原值**（10 → 9 → 10）：`changed()` 用首前值 / 末后值，相等不发。（Task 4）
3. **binding 停用后库存还在发**：停用最后一个销售渠道 binding 必须同步删 `channel_merchants` 行并让库存侧缓存失效；测试断言停用后不再发。（Task 5）
4. **回调重复投递与并发投递**：同一外部事件 ID 并发到达两次，只落一行、两次都回 ack。（Task 6）
5. **secrets 泄露**：列表、详情、更新响应都不含 secrets；更新只传 `config` 时 secrets 保持不变。（Task 7）

---

### Task 1: 迁移 00300 / 00301

**Files:**
- Create: `db/migrations/00300_channel_merchants.sql`、`db/migrations-inventory/00300_channel_merchants.sql`
- Create: `db/migrations/00301_channel_core.sql`

**Produces:** 表 `channel_merchants(merchant_id PK, enabled_at)`；`channel_bindings`、`channel_store_links`、`channel_item_links`、`channel_price_rules`、`channel_stock_rules`、`channel_listings`、`channel_inbound_events`（列见 spec §5.2）。

- [ ] 写 00300（core 版 + 库存版，库存版照 00240 的 ENVSUB / DO 块）。
- [ ] 写 00301：`channel_bindings(id, merchant_id, channel TEXT CHECK (channel ~ '^[a-z][a-z0-9_.]*$'), external_account TEXT, name, roles SMALLINT CHECK (roles BETWEEN 1 AND 7)`（位：1 商品源 2 库存源 4 销售渠道）`, status SMALLINT`（1 启用 2 停用 3 凭据失效）`, config JSONB, secrets JSONB, created_at, updated_at, UNIQUE(merchant_id, channel, external_account))`；其余表按 spec，规则表用可空 `store_id` / `sku_id` + `UNIQUE NULLS NOT DISTINCT (binding_id, store_id, sku_id)`；`channel_inbound_events UNIQUE(binding_id, external_event_id)`；外键到 stores / skus / products 用 `(id, merchant_id)` 复合键并 `ON DELETE CASCADE`。
- [ ] `make migrate-check`（或 check-all 里的迁移步骤）通过；`make test-db` 里的迁移往返测试通过。
- [ ] 提交。

### Task 2: sqlc 查询与仓储

**Files:**
- Create: `db/queries/channels.sql`、`internal/repository/channel.go`、`internal/repository/channel_test.go`（testdb）
- Modify: `db/queries/inventory_svc.sql`（channel_merchants 的增删查）、`internal/repository/inventory_store.go`

**Produces（Go，tenantTx / InventoryStoreTx 上的方法）：**
```go
CreateChannelBinding(ctx, ChannelBindingInput) (ChannelBinding, error)
GetChannelBinding(ctx, id int64) (ChannelBinding, error)          // 不含 secrets
GetChannelBindingSecrets(ctx, id int64) (json.RawMessage, error)  // 只给适配器用
ListChannelBindings(ctx) ([]ChannelBinding, error)
UpdateChannelBinding(ctx, id int64, p ChannelBindingPatch) (ChannelBinding, error)
CountActiveOutletBindings(ctx) (int64, error)
ListActiveOutletBindingsForStore(ctx, storeID int64) ([]ChannelBinding, error)
UpsertChannelStoreLink / DeleteChannelStoreLink / ListChannelStoreLinks
UpsertChannelStockRule / DeleteChannelStockRule / ListChannelStockRules(ctx, bindingID)
UpsertChannelPriceRule / DeleteChannelPriceRule / ListChannelPriceRules(ctx, bindingID)
GetChannelListings(ctx, bindingID, storeID int64, skuIDs []int64) ([]ChannelListing, error)
UpsertChannelListing(ctx, ChannelListing) error
InsertChannelInboundEvent(ctx, ChannelInboundEventInput) (id int64, inserted bool, err error)
// 库存库
SetChannelMerchant(ctx, enabled bool) error
ChannelMerchantEnabled(ctx) (bool, error)
```
- [ ] 先写 testdb 测试：建 binding → 列表不含 secrets；重复外部事件 `inserted=false`；规则 NULL 唯一性；跨租户读不到。
- [ ] 写查询，`make generate-sql`，写仓储方法，测试通过。
- [ ] 提交。

### Task 3: `internal/channel` 包（纯计算）

**Files:**
- Create: `internal/channel/channel.go`（类型、角色位、`Caps`、`Event`、`Listing`、`ListingResult`、`Action` 等）、`internal/channel/adapter.go`（spec §6.1 的五个接口）、`internal/channel/registry.go`（`Register(kind string, f Factory)`、`Lookup(kind) (Adapter, bool)`）、`internal/channel/rules.go`、`internal/channel/rules_test.go`、`internal/channel/fake/fake.go`（测试用适配器：记录 PushListings 调用、ParseInbound 按 `X-Fake-Event-Id` 头解析、HMAC 验签）

**Produces:**
```go
type StockRule struct{ StoreID, SKUID *int64; RatioBP int32; SafetyQty int32; CapQty *int32 }
type PriceRule struct{ SKUID *int64; MarkupBP int32; FixedCents *int64 }
func ResolveStockRule(rules []StockRule, storeID, skuID int64) StockRule // 最具体的；没有规则 = {RatioBP:10000}
func PublishedQty(available int32, r StockRule) int32                    // clamp(floor(a*ratio/10000) - safety, 0, cap)
func ResolvePriceRule(rules []PriceRule, skuID int64) PriceRule
func PublishedPrice(baseCents int64, r PriceRule) int64                  // fixed 优先；否则 base*(10000+markup)/10000 四舍五入到分
```
- [ ] 表驱动测试：负可售（-3）→ 0；比例 7000 × 9 = 6；安全库存大于可售 → 0；上限；最具体规则胜出（店×SKU > 店 > 渠道）；加价四舍五入；固定价优先。
- [ ] 实现，`go test ./internal/channel/...` 通过。
- [ ] 提交。

### Task 4: 库存侧 `stock.changed`

**Files:**
- Modify: `internal/inventory/stock_msg.go`（`stockCrossings.changed()`；`StockNotifier` 增加 `channels *ChannelGate`、`changedAction`；`prepare` 在 gate 放行时为变化的 SKU 另登记一条消息）
- Create: `internal/inventory/channel_gate.go`（`ChannelGate`：进程开关 + 按商家缓存，TTL 30s，`Invalidate(merchantID)`；`Queries()` 计数供测试）、`internal/inventory/channel_msg.go`（接收 core 的「商家开 / 关渠道」消息分支 `BranchChannelMerchantSync`，写 `channel_merchants` 并 `Invalidate`）
- Test: `internal/inventory/stock_msg_test.go`（纯）、`internal/inventory/channel_gate_test.go`、testdb 集成测试

**Produces:** `const TopicStockChanged = "stock.changed"`、`BranchChannelStockChanged = "channel_stock_changed"`、`BranchChannelMerchantSync = "inventory_channel_merchant_sync"`；`func NewChannelGate(enabled bool, lookup func(ctx) (bool, error)) *ChannelGate`；`(*StockNotifier).WithChannels(g *ChannelGate, action string) *StockNotifier`；`ChangedSent() int64`。

- [ ] 测试：gate 关 → 不查库、不发；gate 开、商家未开 → 查一次（缓存命中后不再查）、不发；商家开 → 10→9 发一条；10→9→10 不发；跨 0 时两条都发（两个主题互不影响）。
- [ ] 实现；`make test-db` 里 inventory 相关通过。
- [ ] 提交。

### Task 5: core 侧 `ChannelService`

**Files:**
- Create: `internal/service/channel.go`（binding 生命周期、规则与映射的增删改查、启停时发「商家开 / 关渠道」二阶段消息，照 `promotion_quota_msg.go`）、`internal/service/channel_listing.go`（`StockChangedBranch()`：解码消息 → 找该店启用的销售渠道 binding → 读库存服务当前可售 → 算对外可售数与价格 → 与 `channel_listings` 比较 → `EnqueueJob(channel.listing.push, job_key = binding:store:sku)`；`RunListingPush(ctx)` worker：认领任务 → 适配器 `PushListings` → 回写 `channel_listings`）
- Test: `internal/service/channel_test.go`、`internal/service/channel_listing_test.go`（testdb + fake 适配器）

**Produces:** `NewChannelService(repo, inv inventory.Service, tc …) *ChannelService`；`(*ChannelService).StockChangedBranch() dtm.BranchFuncEx`；`MerchantSyncQueryBranch() dtm.BranchFunc`；`RunListingPush(ctx)`；`RecomputeListings(ctx, storeID int64, skuIDs []int64) error`。

- [ ] 测试：启用第一个销售渠道 binding → 库存库 `channel_merchants` 有行；停用最后一个 → 删行；改库存 → 一条 push 任务 → fake 收到绝对值；同值不重复推；推送失败按退避重试、进死信。
- [ ] 实现，通过；`TestServiceNeverBuildsATenantContextByHand` 仍通过（worker 走放行清单）。
- [ ] 提交。

### Task 6: 回调入口

**Files:**
- Create: `internal/handler/webhook_channel.go`（`POST /api/v1/webhooks/channels/:binding`）、`internal/service/channel_inbound.go`（`Inbound(ctx, bindingID, r, body) (ack []byte, status int, err error)`：取 binding → 适配器 ParseInbound → 写 inbound events（去重）→ 入队 `channel.inbound` → 回 ack；`RunInbound(ctx)`：本期只标记已处理、记录「无处理器」）
- Modify: OpenAPI 加该操作；`contract_test.go` routes 表
- Test: handler 测试（验签失败 401 不落库；重复 200；停用 binding 只留档）

- [ ] 测试 → 实现 → 通过 → 提交。

### Task 7: 后台管理接口

**Files:**
- Modify: `docs/电商系统-OpenAPI.yaml`（`/admin/channels/bindings`、`/admin/channels/bindings/{id}`、`…/{id}/secrets`（PUT，只写）、`…/{id}/store-links`、`…/{id}/stock-rules`、`…/{id}/price-rules`、`…/{id}/listings`）
- Create: `internal/handler/admin_channel.go` 与测试；权限点 `channel:manage`
- 生成：`make generate`

- [ ] 测试：secrets 不回显；只改 config 时 secrets 不变；权限；跨租户 404。
- [ ] 实现 → `make generate` → 契约测试、check-all 通过 → 提交。

### Task 8: 装配与开关

**Files:**
- Create: `internal/app/channels.go`（`EnvChannels = "KEEL_CHANNELS"`、`channelsFromEnv()`；建 `ChannelGate`、`ChannelService`；注册分支、路由、后台任务（`Go` 跑 push / inbound worker））
- Modify: `internal/app/app.go`、`internal/app/split.go`（单体与 core / inventory 角色各自装配）、`compose.yaml`（透传 `KEEL_CHANNELS`）、`docs/指南/部署与配置.md`

- [ ] 测试：开关关时路由表里没有任何 `/channels` 路由、分支表里没有渠道分支；开时都有。
- [ ] 实现 → 通过 → 提交。

### Task 9: 不变量测试与压测对照

**Files:**
- Create: `internal/app/channels_invariant_test.go`（或 service 层 testdb 测试）

- [ ] 开关关：下单、改库存、退款后断言 jobs 无 `channel.*` 队列任务、`StockNotifier.ChangedSent()==0`、`ChannelGate.Queries()==0`、渠道表全空。
- [ ] 开关开、无 binding：同样断言（`Queries()` 允许每商家一次）。
- [ ] 全量 `make test-db`、check-all 通过。
- [ ] `keel-loadtest` 下单与列表各一轮（开关关）与改动前 main 对照，写进 `docs/性能压测-2026-10.md` 新一节。
- [ ] 更新 CHANGELOG `[Unreleased]`、PROGRESS.md；提交并推送。
