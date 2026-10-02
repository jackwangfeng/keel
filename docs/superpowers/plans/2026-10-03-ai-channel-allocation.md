# AI 调渠道库存分配 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** AI 员工能看到各渠道的卖速 / 挂零 / 缺货拒单 / 单件净收入，提出调整 `channel_stock_rules` 的提案，批准或在上限内自动执行后生效，7 天后复盘；演示站上有一个「演示外卖（模拟）」渠道让这一圈跑起来。

**Architecture:** 推送成功的事务里记挂零时段（`channel_listing_zero_spans`）；`service/channel_allocation.go` 算每个 SKU 每个渠道的事实与基线建议，经 MCP 工具 `channel_allocation_review` 给 AI；新提案种类 `channel_stock_rule` 走现有提案机制（`Propose*`/`exec*` + `runClaimed`/`withinPolicy`/`outcomePlan`/`computeOutcome` 各一个 case），执行调 `ChannelService.UpsertStockRule`。第二步给渠道层补「从回调载荷规整」，加只在 `KEEL_CHANNEL_DEMO=on` 登记的适配器 `channel/demotakeout`，演示站模拟器推它的订单。

**Tech Stack:** Go、PostgreSQL（RLS、sqlc、goose）、MCP（现有 `mcpTool` 注册）、Vue 3 后台、Python 模拟器。

**Spec:** `docs/superpowers/specs/2026-10-03-ai-channel-allocation-design.md`

## Global Constraints

- 迁移号段 core **`00330`–`00339`**（只有 Task 1、Task 2、Task 3 各写自己那份：00330 / 00331 / 00332）；库存库不加。大表规则照 `scripts/check_migrations.py`（`orders` 上的视图重建不涉及索引；新表索引普通建即可，新表不是大表）。
- core 的 SQL 不写 `merchant_id`（只靠 RLS）；`agent_ro` 视图显式 `WHERE merchant_id = current_merchant()`（照 00131）。
- `agent_ro` 视图**不含** `channel_bindings.config` / `secrets`、`channel_orders.receiver` / `last_payload`。
- `KEEL_CHANNELS` 关着时：不写挂零时段、`channel_allocation_review` 返回空结果并说明「没有启用的销售渠道」、`propose_channel_stock_rule` 拒收（`ErrChannelsDisabled` → 409）；第一期零开销不变量测试照旧通过。
- 渠道层通用代码按 `Caps` 分支，不写 `if kind == ...`；`internal/channel/demotakeout` 只 import 标准库与 `internal/channel`。
- 演示适配器**只在 `KEEL_CHANNEL_DEMO=on` 时登记**；任何对外文档不说它是美团 / 饿了么。
- 提案执行不覆盖人的修改（`prev` 核对）；自动执行不许把比例调到 0。
- 每个改状态的调用点照 `notificationCallSites` 规矩登记（本期大概率不新增）；手建租户上下文只在放行清单文件里。
- 测试库：`PGPORT=<port> make test-db`（自起 `keel-postgres:16` 容器，见 PROGRESS「常用操作」）。

## Review Focus

1. **推送失败、重试、并发推送导致挂零时段错乱**：同一格子两个 worker 先后推 0 和 5 → 时段必须一开一关、不留两段 open。手段：部分唯一索引 + 在锁着 `channel_listings` 行的同一事务里写。测试：连续推 0、0、5、0 → 两段，第一段已关；推送失败不写。（Task 1）
2. **AI 拿旧数提案 → 执行时覆盖了店长刚改的规则**：`prev` 在提案时与执行时各核一次；执行时不一致整条失败、规则保持店长的值。测试：提案后后台改规则 → 批准 → 失败、结果说明哪一格。（Task 3）
3. **自动执行把一个渠道「关掉」**：比例调到 0 或大幅下调被自动执行。测试：策略 `max_ratio_step_bp=2000` 时 80%→0 不自动执行、80%→70% 自动执行。（Task 3）
4. **keel 自己缺货时 AI 被误导去调分配**：工具不给建议、复盘不计这些格子。测试：keel 可售 0 的 SKU 无建议；窗口里断货 3 天的格子 neutral「缺的是货」。（Task 2、Task 3）
5. **只读视图泄露**：`query_sql` 读 `agent_ro.channel_bindings` 不得有 `secrets`/`config` 列、`agent_ro.channel_orders` 不得有 `receiver`。测试：`SELECT * ` 的列名断言。（Task 2）

---

### Task 1: 挂零时段（迁移 00330 + 推送时写入）

**Files:**
- Create: `db/migrations/00330_channel_listing_zero_spans.sql`、`internal/handler/channel_zero_span_test.go`
- Modify: `db/queries/channels.sql`（开段 / 关段 / 按窗口汇总）、`internal/repository/channel.go`、`internal/service/channel_worker.go`（`pushStore` 回写成功处调用）、`internal/service/channel_worker.go` 的 `housekeep`（180 天清理）、`db/tenancy.json`（若需登记）、`docs/电商系统-数据模型设计.md`

**Interfaces — Produces:**
```go
// repository
func (tx Tx) RecordChannelListingZero(ctx context.Context, bindingID, storeID, skuID int64, prevQty *int32, newQty int32, held bool, at time.Time) error
type ChannelZeroHours struct{ BindingID, StoreID, SKUID int64; HeldHours, EmptyHours float64 }
func (tx Tx) ChannelZeroHours(ctx context.Context, storeID int64, skuIDs []int64, from, to time.Time) ([]ChannelZeroHours, error) // 时段与 [from,to) 的交集小时数；open 段截到 to
func (r *Repo) PurgeChannelZeroSpans(ctx context.Context, before time.Time, limit int) (int64, error)
```
迁移：表 `channel_listing_zero_spans(id, merchant_id DEFAULT current_merchant(), binding_id, store_id, sku_id, held BOOLEAN NOT NULL, started_at TIMESTAMPTZ NOT NULL, ended_at TIMESTAMPTZ NULL, CHECK (ended_at IS NULL OR ended_at >= started_at))`，复合外键到 binding（ON DELETE CASCADE）/ stores / skus，部分唯一索引 `(merchant_id, binding_id, store_id, sku_id) WHERE ended_at IS NULL`，索引 `(merchant_id, store_id, started_at)`，ENABLE + FORCE RLS（照 00301）。

- [ ] **Step 1: 写失败测试**（照 `internal/handler/channel_listing_test.go` 的 rig）：推 0 → 一段 open、held=keel 有货；再推 0（held 不变）→ 不新开；keel 补货但规则仍算 0 → 关 held=false 段、开 held=true 段；推 5 → 关；推送失败（`FailNext`）→ 不写；`ChannelZeroHours` 对 open 段截到 to。开关关着时（`newChannelStockRig(t,false)` 那一套）不写。
- [ ] **Step 2: 跑测试确认失败**
- [ ] **Step 3: 实现**：`pushStore` 里成功回写 `channel_listings` 的那段，拿到 prev（上次推送值）、新值与当时重算用的 `available`，调 `RecordChannelListingZero(..., held = available > 0)`；逻辑：new>0 → 关 open 段；new==0 → 有 open 段且 held 相同 → 不动；held 不同 → 关旧开新；没有 open 段 → 开。
- [ ] **Step 4: 跑测试通过**；`go vet`；迁移检查脚本。
- [ ] **Step 5: 提交** `渠道分配 · 挂零时段：推送成功时记渠道上挂 0 的起止（分「keel 有货但规则算 0」与「keel 没货」），180 天清理`

### Task 2: 只读视图（00331）+ `channel_allocation_review` 工具

**Files:**
- Create: `db/migrations/00331_agent_ro_channels.sql`、`internal/service/channel_allocation.go`、`internal/service/channel_allocation_internal_test.go`（基线规则的纯函数单测）、`internal/handler/channel_allocation_test.go`
- Modify: `internal/handler/mcp_tools_compute.go`（注册工具，照 `slow_movers`）、`internal/handler/mcp_tools_proposals.go`（`query_sql` 说明里的视图清单）、`db/queries/*.sql`（按渠道汇总卖出件数、缺货拒单件数）、`internal/app/*`（AgentService 拿到 ChannelService，开关关着为 nil）、契约（若 MCP 工具有 schema 产物）

**Interfaces — Produces:**
```go
type AllocationReviewInput struct{ StoreID int64; SKUIDs []int64; Days int } // Days 7–30，默认 14；SKUIDs 空 = 自动挑，至多 50
type AllocationReview struct{ StoreID int64; Days int; Note string; SKUs []AllocationSKU }
type AllocationSKU struct{ SKUID int64; Title string; Available int32; StockoutDays int; CostMissing bool; Channels []AllocationChannel; Suggestions []AllocationSuggestion }
type AllocationChannel struct{ BindingID *int64 /*nil = 自营*/; Name string; Rule *channel.StockRule; RuleLevel string /*binding|store|sku|default*/; PublishedQty int32; Sold int64; DailyVelocity float64; HeldZeroHours, EmptyZeroHours float64; StockoutRejects int64; UnitNetCents int64 }
type AllocationSuggestion struct{ BindingID int64; SKUID *int64; RatioBP int32; SafetyQty int32; CapQty *int32; Why string }
func (s *ChannelService) AllocationReview(ctx context.Context, in AllocationReviewInput) (AllocationReview, error)
func suggestAllocation(sku AllocationSKU) []AllocationSuggestion // 纯函数，spec §4.1 三条基线规则
```
视图：重建 `agent_ro.orders` 加 `source`、`channel_order_id`（先 `DROP VIEW` 再 `CREATE`，保持原列顺序在前、新列在后；授权照 00131）；新建 `agent_ro.channel_bindings(id, channel, name, status, roles)`、`agent_ro.channel_orders(id, binding_id, store_id, order_no, status, has_exception, goods_cents, freight_cents, commission_cents, buyer_paid_cents, created_at)`、`agent_ro.channel_stock_rules`、`agent_ro.channel_listing_zero_spans`。

- [ ] **Step 1: 写失败测试**：纯函数——挂零 ≥24h 且有卖 → 上调 10 点；有拒单 → 安全库存 +ceil(拒单/周数)；货不够分 → 净收入最低的渠道下调到总需求 ≤ 可售；keel 可售 0 → 无建议；自营不给建议。集成——造两个渠道（sim + 假适配器）的订单与挂零时段，断言每渠道的卖出、日均（分母扣挂零小时、下限 1）、净收入（佣金 `config.commission_bp`、成本 `skus.cost_cents`，成本 0 标 `CostMissing`）；`query_sql` 能读 `agent_ro.orders.source` 与渠道视图、`SELECT * FROM agent_ro.channel_bindings` 列里没有 `secrets`/`config`（Review Focus 5）；MCP 工具对门店范围的 AI 员工也可用（只读，按员工范围过滤门店）——**注意**：先查现有计算工具对员工范围的处理，照它。开关关着返回空 + Note。
- [ ] **Step 2–4**：失败 → 实现 → 通过。
- [ ] **Step 5: 提交** `渠道分配 · AI 能看到渠道：只读视图加订单来源与渠道表（不含凭据与收货人）、channel_allocation_review 工具（各渠道卖速 / 挂零 / 缺货拒单 / 单件净收入 + 基线建议）`

### Task 3: 提案种类 `channel_stock_rule`（00332 自动执行上限）

**Files:**
- Create: `db/migrations/00332_agent_policy_ratio_step.sql`、`internal/service/agent_proposal_channel.go`、`internal/handler/agent_channel_proposal_test.go`
- Modify: `internal/service/agent_proposal.go`（`runClaimed` case）、`internal/service/agent_auto_policy.go`（`autoKinds`、`withinPolicy`、策略读写含 `MaxRatioStepBP`）、`internal/service/agent_proposal_outcome.go`（`outcomePlan`、`computeOutcome`）、`internal/handler/mcp_tools_proposals.go`（`propose_channel_stock_rule`）、`docs/电商系统-OpenAPI.yaml`（提案种类枚举两处、自动执行策略字段）、`make generate`、`db/queries/agent*.sql`（策略列）

**Interfaces — Produces:**
```go
const ProposalKindChannelStockRule = "channel_stock_rule"
type ChannelStockRulePayload struct{ BindingID, StoreID int64; Changes []ChannelStockRuleChange }
type ChannelStockRuleChange struct{ SKUID *int64; RatioBP, SafetyQty int32; CapQty *int32; Prev ChannelRuleSnapshot }
type ChannelRuleSnapshot struct{ RatioBP, SafetyQty int32; CapQty *int32; Level string }
func (s *AgentProposalService) ProposeChannelStockRule(ctx context.Context, pl ChannelStockRulePayload, meta ProposalMeta) (repository.AgentProposal, error)
func (s *AgentProposalService) execChannelStockRule(ctx context.Context, p repository.AgentProposal) (ProposalResult, error)
var ErrProposalStale = errors.New("提案依据的规则已经变了") // → 409
```
复盘 outcome JSON：`{verdict, reason, cells:[{binding_id, store_id, sku_id, before:{held_zero_hours, stockout_rejects, sold, net_cents}, after:{...}, excluded_reason?}]}`。

- [ ] **Step 1: 写失败测试**：校验（`prev` 不一致 409、比例 >10000、binding 停用、门店未映射、>20 条、非全店范围）；去重键；试算（提案详情带每格前后对外可售数——放在 payload 旁的 `preview` 字段或结果里，照现有试算写法）；执行生效 + 入队重算推送；**执行时规则被人改过 → 失败、规则不变**（Review Focus 2）；重放幂等；自动执行（Review Focus 3）；复盘 positive / negative / neutral / 断货格子不计（Review Focus 4），用固定时钟造前后 7 天数据；开关关着拒收。
- [ ] **Step 2–4**：失败 → 实现 → 通过；通知策略测试、契约测试照旧绿。
- [ ] **Step 5: 提交** `渠道分配 · 提案：AI 提调渠道分配（依据规则旧值核对、试算、执行不覆盖人的修改、自动执行限单次比例变化且不许调到 0、7 天前后对比复盘）`

### Task 4: 后台界面

**Files:**
- Modify: `web/admin/src/api/agentProposalRules.ts`（`KIND_LABEL`「调渠道分配」、`channelStockRulePayload()`、`describeResult`/`describeOutcome` 分支）、`agentProposalRules.test.ts`、`web/admin/src/views/agents/ProposalPayloadView.vue` / `ProposalResultView.vue` / `ProposalOutcomeView.vue`、自动执行策略页、渠道详情「库存规则」页签（被 AI 提案改过的规则标「AI 提案 #id」：后端在规则列表接口加可空 `proposal_id`，由最近一条执行成功、覆盖该格子的提案反查——若成本高就只在详情抽屉查）
- [ ] **Step 1**：规则单测（payload 解码、结果与复盘描述）→ 失败 → 实现。
- [ ] **Step 2**：视图；`make admin-type-check`、`make admin-test`。
- [ ] **Step 3**：`make admin-responsive-check`（LOCAL_DIST + 本地后端带一条该种类的提案），看截图内容。
- [ ] **Step 4: 提交** `渠道分配 · 后台：提案种类「调渠道分配」的载荷 / 结果 / 复盘视图、自动执行上限、规则标出 AI 提案来源`

### Task 5: 剧本与运行器

**Files:**
- Create: `agent/skills/渠道库存分配.md`
- Modify: `agent/skills/巡店日报.md`（加一步）、`agent/runner/claude-daily.sh`（提示词带上新剧本）、`docs/AI接口.md`（新工具与提案）
- [ ] 剧本写清：何时看、怎么读工具结果、何时不提（数据不足 7 天、keel 缺货、变化太小）、证据写法（引用数字）、每天至多一条、复盘写进简报的格式。
- [ ] **提交** `渠道分配 · 剧本：AI 员工每日巡店时看渠道分配，何时提、怎么写证据与复盘`

### Task 6: 第二步——载荷规整路径 + 演示外卖适配器 + 模拟器

**Files:**
- Modify: `internal/channel/channel.go`（`Event.Order *ChannelOrder`）、`internal/service/channel_order.go`（`orderChanged`：`!Caps.OutOfOrderInbound && ev.Order != nil` 直接用）
- Create: `internal/channel/demotakeout/demotakeout.go`、`demotakeout_test.go`、`deps_test.go`
- Modify: `internal/app/channels.go`（`KEEL_CHANNEL_DEMO=on` 才登记）、`internal/handler/channel_order_test.go`（载荷规整路径：不调 `FetchOrder`、版本守卫）
- Modify（仓库外，演示站）：`~/.local/share/keel-eshop/bin/simulate.py`（演示外卖订单流，spec §7.3）、`cron-simulate.sh`（若需）、`demo-env.sh`（`KEEL_CHANNEL_DEMO=on`，**先备份**）

- [ ] **Step 1: 写失败测试**：适配器验签（对 / 错 / 缺头）、正文规整成 `ChannelOrder`、`Caps` 断言、`deps_test`；收单走载荷不调 `FetchOrder`（假适配器计数）；开关 `KEEL_CHANNEL_DEMO` 不开时 `/admin/channel-kinds` 没有它。
- [ ] **Step 2–4**：失败 → 实现 → 通过；全量 `make test-db`、`scripts/check-all.sh`。
- [ ] **Step 5: 提交** `渠道层：推送带完整状态的渠道直接用回调载荷收单；演示外卖（模拟）适配器（仅 KEEL_CHANNEL_DEMO=on 登记）`
- [ ] **Step 6（部署后做，见 Task 7）**：模拟器扩展与演示站配置。

### Task 7: 独立审查、合并、上演示站

- [ ] 独立审查整支分支（Opus，只读），按 Review Focus 与 Global Constraints 找真 bug；修复。
- [ ] 全量 `make test-db`、`scripts/check-all.sh`、`make admin-test`。
- [ ] 合并 main；PROGRESS、CHANGELOG（Unreleased）、README「AI 员工」一节补一句新提案种类。
- [ ] 演示站：备份两库 → 迁移在备份副本上试跑 → `migrate` → `up -d --no-deps app inventory console` → `publish-frontend.sh admin`；`demo-env.sh` 加 `KEEL_CHANNEL_DEMO=on`（备份 `.bak-demotakeout`）。
- [ ] 演示站配置：建 binding「演示外卖（模拟）」（`config {auto_accept:true, commission_bp:1800}`、密钥写 `~/.config/keel/demo-takeout-secret` 0600 并经后台只写接口设上）、映射演示的门店、初始规则比例 60% 安全库存 5；模拟器推订单，验证收单、扣库存、对外可售数变化、挂零时段在记。
- [ ] 手动跑一次 `agent/runner/claude-daily.sh`（演示站密钥由 cron 脚本提供，照 `cron-agent-daily.sh` 的方式），确认 AI 调了 `channel_allocation_review`；若数据不足 7 天它应不提，记下「第一批复盘约在 YYYY-MM-DD」。
