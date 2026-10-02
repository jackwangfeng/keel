# AI 调渠道库存分配（设计）

日期：2026-10-03 · 状态：用户 2026-10-02 晚授权「一路写下去」，按推荐方案定稿（目标 D、方案 C 分两步）；未逐段确认，留给用户事后审阅

## 1. 目标与定位

AI 员工按各渠道的卖货速度、挂零时长、缺货拒单和单件净收入，提出调整「对外可售数」规则（`channel_stock_rules` 的比例 / 安全库存 / 上限）的提案；商家批准（或按自动执行策略在上限内自动执行）后生效；7 天后按同一口径复盘，结果进成绩单，AI 写进简报。

- **用户原话**：「keel 还是要进一步放大 ai 的优势」「C，按你说的分两步走」「你一路写下去吧」。
- **目标优先级（D）**：先不超卖，再少缺货，货不够分时按单件净收入排先后。
- **两步**：第一步按真实渠道把功能做对（自营 + 已接的渠道）；第二步在演示站造一个「演示外卖（模拟）」渠道和它的订单流，让 AI 在演示站跑出完整的一圈。**对外不声称接了美团 / 饿了么**。
- **非目标**：不让 AI 直接改库存数（那是补货提案的事）；不改价格（以后另一种提案）；不做跨门店调拨；不预测（不建模，只看过去 N 天的事实，判断交给 AI）。

## 2. 现状缺口（2026-10-03 摸底）

1. AI 看不到渠道数据：没有渠道相关的 MCP 工具；`agent_ro` 只读视图（00131）早于渠道层，`agent_ro.orders` 没有 `source` 列，渠道表一张都没有。
2. `channel_listings` 只存最近一次推送，没有历史，算不出「这个渠道这周有多少小时挂着 0」。
3. 补货用的断货天数（`inventory.StockoutDays`，日粒度）按门店，不分渠道。
4. 渠道佣金率没有地方配（Shopify 订单快照里有实际佣金，自营为 0）。
5. 提案种类没有插件接口：加一种 = `Propose*` / `exec*` 一对方法 + `runClaimed`、`withinPolicy`、`outcomePlan`、`computeOutcome` 各加一个 case + OpenAPI 枚举 + 后台 `agentProposalRules.ts`。

## 3. 数据

### 3.1 迁移号段

main 上 core 最高 `00326`。本期 core 用 **`00330`–`00339`**（`00340` 起仍留给对账）；库存库不加。

### 3.2 挂零时段 `channel_listing_zero_spans`（00330）

| 列 | 说明 |
|---|---|
| `merchant_id`, `binding_id`, `store_id`, `sku_id` | 复合外键到 binding / 门店 / SKU |
| `held` | `true` = keel 有货但规则算出来是 0（**分配造成的**）；`false` = keel 自己就没货 |
| `started_at`, `ended_at` | `ended_at` 为空 = 还挂着 |

- 写入点：推送成功回写 `channel_listings` 的同一事务里，比较新旧 `published_qty`：>0 → 0 开一段；0 → >0 关一段；一直是 0 但 `held` 变了（keel 补了货但规则仍算 0，或反过来）关旧开新。推送时手里有当时的 keel 可售数（`pushStore` 本来就重算一遍），`held = available > 0`。
- 唯一约束：每个（binding, store, sku）至多一段 `ended_at IS NULL`（部分唯一索引）。
- 首次上线：已有的 0 格子不补历史（从上线时刻起量），文档写明「上线后满 7 天数据才完整」。
  - **执行中偏离（审查修复）**：「上次成功推的是 0、这次还是 0、却没有挂着的段」的格子（上线前就是 0、比例 0、安全库存 ≥ 可售）重算时补推一次同样的 0，由推送成功的事务开段 —— 不然它们永远不会再推、永远没有段。仍只在推送成功的事务里写。
  - **执行中偏离（审查修复）**：格子不再算的那一刻关段（`ended_at = now()`，同一事务）：binding 不再是启用中的销售渠道（`UpdateBinding`）、删门店映射（`DeleteChannelStoreLink`）、删 SKU 映射（`DeleteChannelItemLink`）。这是推送事务之外唯一的写入点。已知小缝：关段时恰好在途的那次推送成功后仍可能再开一段，下一次重算 / 重新映射会纠正。
- 清理：保留 180 天（housekeep 里按批删）。

### 3.3 只读视图（00331）

`agent_ro.orders` 重建，加 `source`、`channel_order_id`；新增：
- `agent_ro.channel_bindings`：`id, channel, name, status, roles`（**不含** `config` / `secrets`）。
- `agent_ro.channel_orders`：`id, binding_id, store_id, order_no, status, (exception IS NOT NULL) AS has_exception, amounts->>'commission'...` 金额列、`created_at`（**不含** `receiver`、`last_payload`、`lines` 里的标题以外字段）。
- `agent_ro.channel_stock_rules`、`agent_ro.channel_listing_zero_spans`。
- `query_sql` 工具说明里的可读视图清单同步。

### 3.4 渠道佣金率

binding `config.commission_bp`（万分比，可选，缺省 0；后台写 config 时校验是 0–10000 的整数，读出时夹在 0–10000）。单件净收入 = 渠道价 × (1 − 佣金率) − SKU 成本价（`skus.cost_cents`，为 0 时不减，并在结果里标「没填成本价」）。自营渠道佣金 0、渠道价 = 门店价。

## 4. 给 AI 的工具

### 4.1 `channel_allocation_review`（MCP 计算工具，只读）

入参：`store_id`（必填）、`sku_ids`（可选，缺省 = 这家门店最近 N 天在任一渠道卖过、或任一渠道挂零过的 SKU，至多 50 个）、`days`（7–30，默认 14）。

每个 SKU 返回：
- keel 当前可售、断货天数（`StockoutDays`）。
- 每个销售渠道一行（自营一行 + 这家门店映射了的每个启用 binding 一行）：当前生效规则（比例 / 安全库存 / 上限 + 来自哪一级）、当前对外可售数、N 天卖出件数、**有货时的日均销量**（卖出 / (N − 这个渠道挂零的小时数 / 24)，分母下限 1；挂零小时按 `held` 与否分开统计）、`held` 挂零小时数、缺货拒单件数（渠道单 SAGA 缺货关单、或渠道单异常为缺货的件数）、单件净收入。
- **基线建议**（keel 用固定规则算的，AI 可以照用、修改或不用）：
  - 某渠道 `held` 挂零 ≥ 24 小时且该渠道日均销量 > 0 → 比例上调 10 个百分点（不超过 100%）或安全库存降到「其它渠道日均销量 × 0.5」取整。
  - 某渠道近 N 天有缺货拒单 → 安全库存 +ceil(拒单件数 / 周数)。
  - 货不够分（各渠道按日均销量 × 3 天的需求合计 > keel 可售）→ 按单件净收入从高到低保证需求，最低的那个渠道比例下调，下调幅度使总需求 ≤ 可售。**执行中偏离（审查修复）**：比例至多降到下限 10%（`allocationRatioFloorBP`），不建议调到 0；降到下限时理由写明「要不要再降（等于下架这个渠道）交给人决定」；已在下限不再建议。
  - 每条建议带「为什么」一句话。
- 自营渠道的「对外可售数」就是 keel 可售（自营没有规则），只给数，不给建议。

### 4.2 提案 `propose_channel_stock_rule`

载荷：`{binding_id, store_id, changes: [{sku_id|null, ratio_bp, safety_qty, cap_qty|null, prev: {ratio_bp, safety_qty, cap_qty, level}}]}`，至多 20 条；`sku_id` 为空 = 改「渠道 × 门店」那一级。附带通用的 `evidence`、`expected_impact`。

- 校验：AI 员工是全店范围；binding 启用中且是销售渠道；门店有映射；SKU 存在；比例 0–10000；安全库存 0–100000；上限为空或 ≥ 0；`prev` 与当前生效规则一致（不一致 = AI 看的是旧数，拒收 `409`，让它重新调工具）。
- 去重键 `chstock:<binding>:<store>:<排序后的 sku>`（同一组格子有待处理的提案时不重复提）。
- 提案标题由 keel 生成，例：「Shopify 开发店 · 示例小店：3 个商品调分配（比例 80%→90%…）」。
- **试算**：提案详情显示每条改动在当前 keel 可售数下，对外可售数从几变成几。门店级改动只试算这家门店在这个渠道上**没有单独（门店 × SKU 级）规则**的已推格子（执行中偏离，审查修复）：试算就是复盘量的格子，不受影响的格子不进去。

### 4.3 执行

`execChannelStockRule`：同一事务里先拿 binding 的规则锁（`channel_bindings` 那一行 `FOR NO KEY UPDATE`；后台 `UpsertStockRule` / `DeleteStockRule` 拿同一把，执行中偏离，审查修复），拿到之后再逐条核对 `prev` 仍与当前规则一致（有人在后台改过 → 整条提案执行失败、结果写清哪一条被改过，不覆盖人的修改）→ `UpsertStockRule`（已有的重算与推送链路照常触发）。幂等键 `proposalIdemKey(p.ID)`：重放时发现规则已经是目标值就当成功。

### 4.4 自动执行策略

`agent_auto_policies` 加一列 `max_ratio_step_bp`（00332，默认 0 = 不自动执行这种提案）：每条改动的比例变化绝对值 ≤ 它、安全库存变化 ≤ `max_units`（沿用现有列）、**不许自动把比例调到 0**（那等于下架这个渠道），才算在上限内。执行中偏离（审查修复）：封顶是 0、或试算里有格子从有货变成 0（安全库存 ≥ 可售）同样不自动执行。后台自动执行策略页加这一项。

### 4.5 复盘

窗口 7 天（沿用 `outcomeWindow`）。对提案里每个（binding, store, sku）格子，比较执行前 7 天与执行后 7 天：`held` 挂零小时、缺货拒单件数、卖出件数、单件净收入 × 卖出（渠道净收入）。

- **negative**：任一改动涉及的渠道缺货拒单增加；或被**下调**的渠道 `held` 挂零小时增加超过 24 小时且它的卖出下降。
- **positive**：缺货拒单没增加，且被**上调**的渠道 `held` 挂零小时减少 ≥ 30% 或卖出增加 ≥ 20%。
- 其它 **neutral**。keel 可售在窗口里断货超过 2 天的格子不计（缺的是货，不是分配）；全部格子都不计 → neutral 并写明原因。
- 结果 JSON 里列每个格子的前后数字，后台「复盘」与 AI 的 `my_scorecard` 都能看到；AI 在简报里引用。

## 5. 剧本与运行器

- 新剧本 `agent/skills/渠道库存分配.md`：什么时候看（每日巡店时，若这家店有启用的销售渠道）；怎么读 `channel_allocation_review`；什么时候提、什么时候不提（数据不足 7 天、keel 本身缺货、变化太小）；证据怎么写（引用具体数字）；一天至多一条、每条至多 20 个格子；简报里怎么写复盘。
- `巡店日报.md` 加一步指向它；`claude-daily.sh` 的提示词带上这本剧本。

## 6. 后台

- 提案种类标签「调渠道分配」；载荷视图（每条改动：渠道、门店、商品、旧规则 → 新规则、试算的对外可售数前后）；结果视图；复盘视图（每格前后数字表）。
- 自动执行策略页加「渠道分配单次比例变化上限」。
- 渠道详情「库存规则」页签：被 AI 提案改过的规则标「AI 提案 #id」（读提案执行结果反查，不加列）。

## 7. 第二步：演示站的「演示外卖（模拟）」渠道

### 7.1 渠道层补「从回调载荷规整」（通用，第四期本来就要）

`Caps.OutOfOrderInbound = false` 的渠道（推送带完整状态），适配器在 `ParseInbound` 里把规整好的订单放进 `Event.Order *ChannelOrder`；收单时有它就直接用，不调 `FetchOrder`。版本守卫照旧。

### 7.2 适配器 `internal/channel/demotakeout`

- `Kind = "demo_takeout"`，后台显示名「演示外卖（模拟）」。**只在 `KEEL_CHANNEL_DEMO=on` 时登记**（默认不编进注册表），README 不提它是美团。
- Caps：销售渠道、商品方向无、配送平台骑手、`AcceptRequired = true`（接单时限 5 分钟）、`OutOfOrderInbound = false`、`PricePerStore = true`。
- 回调验签：`X-Demo-Signature = hex(HMAC-SHA256(secret, body))`；正文就是一张规整好的订单（JSON）。
- `PushListings`：直接成功（keel 的 `channel_listings` 就是这个模拟平台上的数）；`Act`：接单 / 拒单 / 发货都直接成功；`FetchOrder`：`ErrUnsupported`（不会被调）。
- binding `config`：`auto_accept: true`、`commission_bp: 1800`。

### 7.3 模拟订单流（`~/.local/share/keel-eshop/bin/simulate.py` 扩展）

每轮：读演示外卖 binding 的当前对外可售数（后台接口 `listings`，用演示会话）→ 按每个 SKU 的需求率（Poisson，按商品热度设）生成下单意向 → 平台上有货就签名推一张订单回调，没货就记一次「流失」（只记在模拟器自己的日志里，keel 看不到——真实平台上也看不到）。需求率让演示外卖比自营更旺、Shopify 更淡，并让两三个热门 SKU 在当前规则（比例 60%、安全库存 5）下经常挂零，好让 AI 有东西可调。

### 7.4 演示站配置

建 binding「演示外卖（模拟）」，映射演示的 2 家门店，设初始规则（比例 60%、安全库存 5）；`KEEL_CHANNEL_DEMO=on`；AI 员工每日巡店照常跑。上线 7 天后才有第一批复盘，PROGRESS 记下日期。

## 8. 不变量

- `KEEL_CHANNELS` 关着：挂零时段不写、新工具在渠道层关着时返回「没有启用的销售渠道」、提案种类拒收；第一期的零开销不变量测试照旧。
- 挂零时段的写入只在推送成功的事务里，不在下单热路径上。
- AI 不能绕过 `prev` 核对覆盖人的修改；自动执行不能把比例调到 0。
- 只读视图不含收货人、凭据、渠道配置。

## 9. 测试

- 挂零时段：0 ↔ 非 0、`held` 切换、推送失败不写、重放不重复开段。
- `channel_allocation_review`：每条基线规则一个用例（挂零上调、拒单加安全库存、货不够分按净收入）、没填成本价的标记、keel 缺货时不给建议、门店范围员工调不了（全店范围）。
- 提案：校验（`prev` 不一致 409、比例越界、binding 停用）、去重、试算、执行（规则生效、推送入队）、执行时规则被人改过 → 失败且不覆盖、重放幂等、自动执行上限（含「不许调到 0」）。
- 复盘：positive / negative / neutral / 断货格子不计，各一条；用固定时钟造前后 7 天的订单与挂零时段。
- 只读视图：`query_sql` 读得到 `orders.source` 与渠道视图、读不到 `receiver` / `secrets`。
- 第二步：载荷规整路径（不调 `FetchOrder`、版本守卫）、演示适配器验签、开关关着时注册表里没有它。
- 通知策略测试、租户上下文测试、契约测试照旧全绿。

## 10. 分工与顺序

1. 迁移 00330–00332 + 挂零时段写入。
2. 只读视图 + `channel_allocation_review` 工具。
3. 提案种类（校验、试算、执行、自动执行上限、复盘）+ MCP 工具。
4. 后台界面。
5. 剧本与运行器提示词。
6. 第二步：载荷规整路径 + 演示适配器 + 模拟器 + 演示站配置。
7. 独立审查、全量测试、合并、上演示站。
