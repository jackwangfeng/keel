# 渠道适配层设计（Shopify 首个实现，接口对齐美团闪购 / 饿了么零售，预留超市 ERP）

日期：2026-10-02 · 状态：设计已确认，待写实现计划

## 1. 定位与场景

- **渠道适配层是 keel 的一个模块，不是独立产品。** Shopify 是第一个真实实现；接口按「Shopify + 美团闪购 + 饿了么零售」的能力并集设计，以后要能接超市 ERP。
- **核心场景：超市全渠道。** 超市不放弃美团、饿了么，同时有自营商城（keel 自己的小程序）。卖点是「所有渠道一个后台、一份库存」：keel 是中台，不替代平台。
- **这次的目的是开源展示可扩展性**：一套干净的渠道接口 + 一个真实参考实现（Shopify，真实开发店联调）+ 一个按美团闪购流程写的模拟适配器（证明接口装得下外卖平台，但**不声称已对接美团 / 饿了么**）。

## 2. 不变量（硬约束）

1. **不配置任何渠道时，渠道层不产生任何行为和额外开销，现有功能不受影响。** 实现方式见 §8，验证方式见 §11。
2. **平台订单以平台状态为准。** keel 不替平台做决定（接单时效、骑手状态、取消、退款都以平台回执为准），只把平台事实转成 keel 订单上的动作。
3. **一份库存。** 真实库存只有库存服务里（门店, SKU）那一份；所有渠道的订单从它扣。渠道看到的是按规则算出的「对外可售数」，不是切开的分仓。
4. **对销售渠道而言 keel 是库存与价格的权威。** 平台上的数被人改了，以 keel 覆盖并记差异。
5. **渠道订单不重新算价。** 金额一律用平台给的快照。

## 3. 非目标（这次不做）

- 美团 / 饿了么的真实对接（没有开发者账号；二手资料称美团闪购已对服务商关闭对接，未核实，见 §13）。
- 超市 ERP 适配器的实现（只定义接口与库存服务的扩展口，§6.6）。
- **导流**（引导平台顾客去自营渠道）：可能违反平台规则，做之前要先查清（§13）。
- 渠道独立部署成单独服务（接口不排斥，以后可以拆）。

## 4. 概念模型

### 4.1 三种角色

| 角色 | 谁来当 | 方向 |
|---|---|---|
| 商品主数据源 | keel（默认）/ Shopify / 超市 ERP | 进 keel |
| 库存数据源 | keel（默认）/ 超市 ERP | 进 keel |
| 销售渠道 | keel 自营小程序 / Shopify / 美团闪购 / 饿了么零售 | 商品、价格、对外可售数出 keel；订单进 keel |

适配器声明自己能当哪些角色；每个商家为每个 binding 选择实际承担的角色。keel 自营小程序也视为一个销售渠道（内建，不需要 binding），定价与库存分配规则同样适用于它。

### 4.2 对外可售数

```
对外可售数 = clamp( floor(真实可售 × 比例) − 安全库存, 0, 上限 )
```

规则粒度：渠道 → 渠道 × 门店 → 渠道 × 门店 × SKU，最具体的生效。平台缺货会被罚、被降权，一般给平台更高的比例，自营渠道可以压低把余量留给平台。

不硬切分仓的理由：切开后 A 渠道卖空、B 渠道还有货，「一份库存」不成立。代价是超卖窗口（平台上的数是之前推的），靠安全库存 + `stock.changed` 即时推送压小，对账兜底。

### 4.3 渠道定价

价格规则 = 渠道级加价（万分比，如美团 +15% 抵佣金）+ SKU 级覆盖价，最具体的生效。推给平台的是算好的价格。

### 4.4 订单：渠道单 + 关联 keel 订单（两层）

- `channel_orders` 存平台那一侧的事实。
- **接单**时生成一张关联的 keel 订单（`source = 1`），进同一个履约队列、同一套报表与 AI 经营数据。
- 渠道差异（接单时效、平台退款审批、骑手状态、催单）关在渠道层，转成 keel 订单上的动作。

## 5. 数据模型

### 5.1 迁移号段

main 上 core 与库存库当前最高都是 `00240`。

- core：**`00300`–`00349`**
- 库存库：**`00300`–`00319`**（`00300` 用于「开了渠道的商家」表，其余留给 ERP 模式）

### 5.2 core 新表（全部带 `merchant_id` + RLS）

| 表 | 内容 | 关键约束 |
|---|---|---|
| `channel_bindings` | 商家接的一个渠道账号：`channel`（`shopify` / `meituan` / `eleme` / `erp.<厂商>`）、`roles`、`status`（启用 / 停用 / 凭据失效）、`config` JSONB（接单策略、申请自动处理策略、同步方向等）、`secrets` JSONB（**接口只写不读**，不回显） | （商家, 渠道, 外部账号）唯一 |
| `channel_store_links` | keel 门店 ↔ 渠道门店（Shopify location gid / 美团 `app_poi_code` / 饿了么 shop_id） | （binding, 外部门店）唯一；（binding, 门店）唯一 |
| `channel_item_links` | keel 商品 / SKU ↔ 外部 ID；`kind`（product / sku）；`extra` 放渠道附带 ID（如 Shopify inventoryItem gid） | 两个方向都唯一（参照 `user_identities`） |
| `channel_price_rules` | 渠道级加价、SKU 级覆盖价 | 最具体的生效 |
| `channel_stock_rules` | 比例、安全库存、上限；粒度渠道 / ×门店 / ×SKU | 最具体的生效 |
| `channel_listings` | 最近一次推给渠道的状态：（binding, 门店, SKU）的对外可售数、价格、推送时间、最近错误 | 与待推值相同就不推；对账时作为 keel 侧值；Shopify CAS 的 `changeFromQuantity` |
| `channel_inbound_events` | 回调去重与留档：外部事件 ID、主题、报文、处理状态 | （binding, 外部事件 ID）唯一 |
| `channel_orders` | 外部单号、门店、关联 `order_no`（接单前为空）、平台状态原文、规整状态、接单 / 拣货截止、配送方式、骑手信息、金额快照（平台补贴、商家补贴、佣金、配送费、商家实收）、收货人（号码 + 号码类型，容纳隐私号）、最近报文、`version` | （binding, 外部单号）唯一 |
| `channel_order_requests` | 平台发起的取消 / 部分退款 / 缺货调整：涉及行、金额、状态（待处理 / 同意 / 拒绝 / 超时自动同意）、截止时间 | （channel_order, 外部请求 ID）唯一 |
| `channel_recon_runs` / `channel_recon_diffs` | 对账批次与差异（订单缺失 / 状态不一致 / 金额不一致 / 库存不一致） | |

往外的任务走现有 `jobs` 表，新增队列：`channel.listing.push`、`channel.order.action`、`channel.catalog.pull`、`channel.recon`、`channel.inbound`。

### 5.3 `orders` 的改动（只加，不改原语义）

- 加 `source SMALLINT NOT NULL DEFAULT 0`（0 自营 / 1 渠道）、`channel_order_id BIGINT NULL`。
- `user_id` 放开为可空；加 `CHECK (source <> 0 OR user_id IS NOT NULL)`，先 `NOT VALID` 再单独 `VALIDATE`。买家端查询都带 `user_id = 我`，空值自然查不到。不建「渠道假买家」，避免污染用户统计。
- 只加一个部分索引 `WHERE source = 1`。
- 金额按平台快照填：商品金额 = 渠道价 × 数量之和；运费 = 买家付的配送费；折扣 = 平台补贴 + 商家补贴；应付 = 实付 = 买家实付。`chk_amount` 不变。佣金、平台补贴结算留在 `channel_orders` 的金额快照，供报表与对账。
- 渠道订单退款不调支付渠道，按平台退款结果记账；`refund_status` / `refunded_cents` 照现有约束。平台在发货前取消 = 走现有的 20 → 50 → 60 全额退款边，**不新增状态边**。

### 5.4 库存库

- `00300`：开了渠道的商家（`merchant_id`, 启用时间），由 core 经二阶段消息维护，库存服务进程内缓存。
- `stock.changed` 复用二阶段消息的屏障表，不新建表。
- ERP 模式（以后）：门店 × SKU 的外部在手数、预占明细，用 `00301`–`00319`。

## 6. 适配器接口

### 6.1 接口（按角色拆分，适配器只实现自己能当的）

```go
// internal/channel
type Adapter interface {
    Kind() string // "shopify" / "meituan" / "eleme" / "erp.xxx"
    Caps() Caps
    // 验签 + 解析回调，产出规整事件；ack 是平台要求的应答体
    ParseInbound(b Binding, r *http.Request, body []byte) (evs []Event, ack []byte, err error)
}

type CatalogSource interface { // Shopify / ERP
    PullCatalog(ctx context.Context, b Binding, cursor string) (CatalogPage, error)
}

type StockSource interface { // ERP（本次只定义）
    PullOnHand(ctx context.Context, b Binding, stores []StoreLink) ([]OnHand, error)
}

type Outlet interface {
    PushListings(ctx context.Context, b Binding, ls []Listing) ([]ListingResult, error) // 绝对值 + 价格，适配器自己分批限流
    PushCatalog(ctx context.Context, b Binding, items []CatalogItem) error             // 美团 / 饿了么：往平台建改商品
    Act(ctx context.Context, b Binding, o OrderRef, a Action) error                     // 接单 / 拒单 / 拣货完成 / 发货(物流单号) / 自配送状态 / 同意或拒绝申请 / 缺货调整
    FetchOrder(ctx context.Context, b Binding, extID string) (ChannelOrder, error)     // 回读权威状态
    ListOrders(ctx context.Context, b Binding, day civil.Date) (Iter[ChannelOrder], error)
    ListListings(ctx context.Context, b Binding, store StoreLink) (Iter[Listing], error)
}

type SalesSink interface { // ERP 回写（本次只定义）
    PostSale(ctx context.Context, b Binding, doc SaleDoc) error
}
```

### 6.2 能力声明 `Caps`

需不需要接单、接单时限；商品同步方向（进 / 出 / 无）；配送方式（平台骑手 / 自配送 / 快递）；退款是否需商家审批；是否支持部分退款；是否支持缺货调整；有无「拣货完成」动作；回调是否可能乱序（是否需回读）；库存语义（绝对值）。

**渠道层通用代码按 `Caps` 分支，不写 `if kind == "meituan"`。**

### 6.3 三家的预期能力（依据 §14 调研，二手资料部分以联调为准）

| 能力 | Shopify | 美团闪购（模拟） | 饿了么零售（仅对齐） |
|---|---|---|---|
| 角色 | 商品源 + 销售渠道 | 销售渠道 | 销售渠道 |
| 商品方向 | 进 | 出 | 出 |
| 需接单 | 否 | 是 | 是（可自动接单） |
| 配送 | 快递 / 本地配送（fulfillment） | 平台骑手 / 自配送 | 平台骑手 / 自配送 |
| 退款需审批 | 否（商家主动） | 是 | 是（约 15 分钟超时自动同意） |
| 回调乱序需回读 | 是 | 否（推送带完整状态） | 待查 |
| 验签 | HMAC-SHA256 + Base64（`X-Shopify-Hmac-SHA256`） | MD5（URL + 排序参数 + secret） | TOP 体系 md5 / hmac |

## 7. 数据流

### 7.1 回调进来

```
POST /webhooks/channels/{binding}
  → Host 定租户（同支付回调）→ 取 binding → 适配器验签解析
  → 写 channel_inbound_events（重复：直接回 ack）→ 立刻回 ack
  → 入队 channel.inbound，异步处理
```

处理时以平台为准：`Caps` 声明会乱序的（Shopify）把事件当「有变化」的提示，调 `FetchOrder` 取权威状态；推送带完整状态的（美团）直接用。`channel_orders.version` 单调，旧状态不覆盖新状态。验签失败回 401 不落库；binding 停用时只留档不处理。

### 7.2 订单流

1. **收单**：订单事件 → 写 `channel_orders`（规整状态「新单」）。
2. **接单**：binding 配自动接单或人工接单（人工时截止前 N 分钟提醒员工）。接单 = 渠道单 SAGA：建 keel 订单（`source = 1`，直接 20 已支付，金额用快照）→ 扣库存 → 完成；成功后入队 `Act(接单)`。
   - 扣库存失败：需接单的渠道 `Act(拒单)`；不能拒单的（Shopify）标异常，员工做缺货调整或退款。
   - 扣库存在接单时、不在收单时：对得上「收单 → 拒单」流程；收单到接单之间库存未锁，平台是按对外可售数下单的，由安全库存兜。
3. **履约**：员工在现有履约队列处理（与自营同列表）。平台骑手 → 骑手事件更新 `channel_orders`，转成 keel 发货（30）/ 完成（40）；自配送 → keel 发货时推状态；Shopify → keel 发货时 `fulfillmentCreate` 带物流单号。
4. **平台申请**（取消 / 部分退款 / 缺货调整）→ `channel_order_requests`（带截止）→ 员工同意 / 拒绝或按 binding 策略自动处理，超时记自动同意 → 平台确认后 keel 订单退款或改行，未发货的回补库存（沿用 `inventory.release` outbox）。
5. **定时扫描**：接单 / 拣货 / 申请截止与催单 → 员工通知或 agent 事件。

### 7.3 库存与价格出去

```
库存服务：可售数每次变化 → 二阶段消息 stock.changed {store, skus}（只带键；仅限开了渠道的商家，§8）
core：对每个启用的销售渠道 binding 按规则算对外可售数
      与 channel_listings 相同 → 跳过
      不同 → EnqueueJob(channel.listing.push, job_key = binding:store:sku)
```

- **合并推送**：jobs「同 key 未完成不重复入队」，一阵密集变化只推一次；执行时读当时最新值。
- **Shopify CAS**：`changeFromQuantity` = `channel_listings` 里上次推送值；冲突说明平台上被改过 → 回读 → 以 keel 覆盖 → 记差异。`@idempotent` 幂等 key 用 `binding:store:sku:listing_version`。
- 价格规则或 keel 门店价变化 → 同样入队。

### 7.4 商品进来（Shopify 当商品源）

`products/*` 回调或定时全量（`channel.catalog.pull`）→ `PullCatalog` → 按 `channel_item_links` 新建 / 更新 keel 商品与 SKU；图片下载存进 uploads（按商家配置可为 S3）。由商品源管理的字段（标题、规格、图片）在后台显示「由 Shopify 管理」、不可改；门店价、库存规则仍在 keel 改。

### 7.5 ERP 模式（预留，本次不实现）

- 读：适配器只读 ERP 的从库或视图（超市书面授权），定期 `PullOnHand` 写进 keel 作为「外部在手」。
- 可售 = 外部在手 − 未确认预占。线上卖出先记预占，再经 outbox `PostSale`（虚拟收银流水 / 单据导入 / 中间表，由 ERP 自己扣库存），幂等带重试。
- 从库里看到这笔销售入账（在手相应减少）后释放预占，避免重复扣。
- 不直接写 ERP 库存表，不指望 ERP 厂商开接口。

## 8. 零开销开关（不变量 1 的实现）

**第一级：进程开关 `KEEL_CHANNELS`，默认关，部署时定。** 关闭时：

- 不注册 `/webhooks/channels/*` 与后台渠道管理接口（404）；
- 不启动任何渠道 worker、定时扫描、对账，不订阅 `stock.changed`；
- 库存服务不发 `stock.changed`；
- 后台菜单不出现「渠道」（前端从开关接口读）。

**第二级：开关打开后按商家。** 只有至少有一个启用的销售渠道 binding 的商家，库存服务才发 `stock.changed`；其他商家每笔订单的写路径与现在相同。core 启用 / 停用 binding 时经二阶段消息通知库存服务，库存服务记在库存库 `00300` 的表里、进程内缓存。

**`orders` 改动零开销**：常量默认值加列只改元数据（PG ≥ 11）；放开可空只改元数据；CHECK `NOT VALID` 后单独 `VALIDATE`（扫表不阻塞读写，合迁移规矩）；只有一个 `WHERE source = 1` 的部分索引，无渠道订单时为空；自营下单路径不加分支。

## 9. 出错处理

- 往外任务：jobs 封顶退避 + 死信（沿用库存 outbox 参数）；进死信发 agent 事件（如「美团门店 A 库存推送失败 3 小时」）。
- 平台限流：适配器按平台规则限流，Shopify 按 `extensions.cost.throttleStatus` 退避；限流算可重试，不计入失败次数。
- 凭据：Shopify token 过期自动续期；续期失败 → binding 标「凭据失效」，停推送并告警，不无限重试。
- 接单 SAGA 失败：现有 SAGA 补偿，再按 `Caps` 拒单或标异常。

## 10. 对账

- 每天定时 + 后台手动触发：按 binding 拉平台前一天订单与当前对外可售数，和 `channel_orders`、`channel_listings` 逐条比对。
- 差异四类：平台有 keel 无 / 状态不一致 / 金额不一致 / 库存不一致；写 `channel_recon_diffs` 并发 agent 事件。
- 订单缺失可一键补单（走正常收单 → 接单流程）。

## 11. 测试

- **适配器单测**：验签、解析、请求构造，用平台文档样例报文；美团 / 饿了么按二手资料造样例，文件里标明来源。
- **模拟平台**：Shopify（GraphQL 子集）与美团各一个进程内 `httptest` 服务，覆盖乱序回调、重复推送、限流、CAS 冲突、接单超时、部分退款。
- **渠道层集成测试**：真实 Postgres + 库存服务，单体与拆分两种形态，接进现有 `test-db`。
- **不变量测试**：
  - 开关关闭：现有测试一条不改全部通过；
  - 开关关闭：走完下单、改库存、退款，断言 jobs 无渠道队列任务、dtmrs 无 `stock.changed`、渠道表全空；
  - 开关打开但商家无 binding：同样断言；
  - 第一期合并前后用 `keel-loadtest` 压下单与列表，p95 差距在噪声内，结果写进压测文档。
- **真实开发店联调**（第二、三期末）：拉商品 → 改 keel 库存 → Shopify 后台看数 → Shopify 下单 → keel 发货 → Shopify 看到物流单号。

## 12. 分期

每期单独写实现计划、单独合并。

1. **渠道骨架**：角色、binding、门店 / SKU 映射、凭据、webhook 入口、outbox 队列、价格与库存规则、`stock.changed`（含按商家开关）、零开销不变量测试、压测对照。
2. **Shopify 商品与库存**：拉商品进来，推价格与对外可售数出去；开发店联调前半段。
3. **渠道订单**：`orders` 改动、渠道单 SAGA、履约与申请流；Shopify 订单与履约回传；开发店联调后半段。
4. **美团模拟适配器**：推单、接单 / 拣货时效、骑手状态、催单、取消、部分退款与缺货调整、商品往外推。
5. **对账**。
6. **以后**：ERP 适配器（从库读取、预占、回写）。

## 13. 风险与待查

- **导流规则**：引导平台顾客去自营渠道可能违反美团、饿了么规则；做导流前先查清，本设计不包含。
- **美团闪购服务商通道**：二手资料称已对服务商关闭对接，未核实；对外只说「接口对齐、有模拟适配器」。
- **美团 / 饿了么接口细节**：文档站需登录，公网拿不到正文；签名原文、回调验签字段、状态码全集、批量与 QPS 限制、部分退款 API、店铺绑定流程都待拿到账号后核实。饿了么零售已更名「淘宝闪购零售开放平台」，很多二手资料基于旧外卖 openapi。
- **美团配送回调与订单回调是两套平台**（青云聚信 dap.meituan.com vs 闪购）；如果以后接美团配送，单独一个适配器角色，不假设一家平台一套签名。
- **Shopify（2026-10-02 在开发店上实测，第二期按此实现）**：client credentials 换的 token `expires_in = 86399`（24 小时）；`inventorySetQuantities` 冲突码 `CHANGE_FROM_QUANTITY_STALE`、**一批里一条出错整批不生效**、`changeFromQuantity: null` 跳过比对；限流桶 4000 点、每秒回 200；单条查询成本上限 1000（products 带 inventoryLevels 一页 10 件就 710 点，所以水位改用 `nodes(ids:)` 单查）；webhook 订阅字段是 `uri`。仍未核实：`inventorySetQuantities` 单次条数上限（按 250）、webhook 重试次数与时长。
- **Shopify 订单部分（2026-10-02 在开发店上只读核实，第三期按此实现）**：token scope 当时有 `write_orders`、`write_assigned_fulfillment_orders`，**没有** `read/write_merchant_managed_fulfillment_orders`（两个 location 都是商家自管，`fulfillmentCreate` 一般要这一对 scope；见 §15 的更新）；没有 `read_customers`（查 customers 回 `ACCESS_DENIED`）；`fulfillmentCreate` 用不存在的 FO 试回 userError「Fulfillment order does not exist.」而不是 `ACCESS_DENIED`，接受 `@idempotent(key:)`，`fulfillmentOrderLineItems` 可省（= 剩余全部行）；单张订单完整取单查询成本 149 点；`orderCancel` 异步、`reason` 与 `restock` 必填；退款行 `restockType` = RETURN / CANCEL / LEGACY_RESTOCK / NO_RESTOCK；`write_orders` 下有 `orderCreate`（可 `test:true`、`financialStatus:PAID`、`options.inventoryBehaviour`），联调建测试单用得上，但是写操作要用户同意；`FulfillmentOrderStatus` = OPEN/IN_PROGRESS/CANCELLED/INCOMPLETE/CLOSED/SCHEDULED/ON_HOLD；`OrderDisplayFinancialStatus` = PENDING/AUTHORIZED/PARTIALLY_PAID/PARTIALLY_REFUNDED/VOIDED/PAID/REFUNDED/EXPIRED；回调主题有 `ORDERS_CREATE/UPDATED/CANCELLED/PAID/FULFILLED`、`REFUNDS_CREATE`、`FULFILLMENTS_CREATE`；开发店当时 0 张订单。**下单联调实测（2026-10-02，用 `orderCreate` 建 test 单，`internal/handler/channel_shopify_orders_live_test.go`）**：①**受保护客户数据**没开时不是字段为 null，而是整个 Order 对象 `ACCESS_DENIED`「This app is not approved to access the Order object」（连 `orders` 列表都读不了，`orderCreate` 执行成功但读不回）；Dev Dashboard 里没有申请入口，要到 Partner Dashboard → Apps → API access requests 申请，开发店保存即生效。②用户补了 `read/write_merchant_managed_fulfillment_orders` 之后 `fulfillmentCreate` 成功（#1002，顺丰单号回传、FO 全部 CLOSED）；**没有测过只有 `write_assigned_fulfillment_orders` 时行不行**，接入指南按两个都要写。③`orderCancel` 必填 `orderId`、`reason`、`restock`，可选 `refundMethod{originalPaymentMethodsRefund, storeCreditRefund}`、`notifyCustomer`、`staffNote`；异步，约 1–2 秒后 `cancelledAt` 才有值。④`orderCreate` 建的 PAID 单没有支付交易：取消后有一条 Refund 记录，但 `totalRefunded` 为 0、`displayFinancialStatus` 仍是 PAID——keel 按「已取消」整单退款，不看退款金额。⑤`inventorySetQuantities` 在 2026-10 必须带 `@idempotent`（缺了直接报错）。⑥真实店上收到的订单 `updatedAt` 只到秒（`2026-10-02T09:37:29Z`），同一秒两次变化版本号相同，收单按「同版本照样重走、只挡严格更旧」处理。**仍未核实**：真实的订单回调正文（开发店上订单回调还没装到任何对外地址，联调是自己签名造的回调）；`refunds/create` 与 `fulfillments/create` 的 `order_id` 字段按文档写。

## 14. 调研依据

- Shopify（shopify.dev 官方为主）：2026-10 为当前稳定版；2026-01-01 起后台不能新建 legacy custom app，需 Dev Dashboard 建应用、client credentials 换 token；`inventorySetQuantities` 2026-04 起 `changeFromQuantity` 与 `@idempotent` 必填；dev store 读受保护客户数据无需审核；webhook `X-Shopify-Hmac-SHA256`（Base64）、`X-Shopify-Webhook-Id` 去重、不保证顺序；2024-10 起 `fulfillmentCreate` 只能用于商家自管位置或自有履约服务。
- 美团闪购 / 饿了么零售：除美团配送回调（dap.meituan.com）外均为二手资料，结论只用于对齐接口形状。

## 15. Shopify 开发店准备（用户操作）

1. 注册 Shopify Partner（免费），建一家 development store。
2. 在 Dev Dashboard 建应用，装到这家开发店；scope：`read_products`、`write_products`、`write_inventory`、`read_locations`、`read_orders`、`write_orders`、`read_merchant_managed_fulfillment_orders`、`write_merchant_managed_fulfillment_orders`（商家自管 location 上的 `fulfillmentCreate` 要这一对；Shopify 旧文档里的 `write_fulfillments` 是过时写法，不要用）。**改了 scope 之后要在 Dev Dashboard 的 Versions 里发布新版本，再回店铺重新安装 / 更新应用，权限才生效**——只改 scope 不发布、不重装，联调时会一直卡在权限不够。
3. 受保护客户数据（订单收货人姓名 / 地址 / 电话 / 邮箱）**不在 Dev Dashboard 里配**：去 Partner Dashboard（<https://partners.shopify.com> → Apps → 选中这个应用），没选过分发方式的话先选 Custom distribution，再进 API access requests → Protected customer data access → Request access，勾选 Protected customer data 与姓名 / 地址 / 电话 / 邮箱字段，按提示填 Data protection details。**开发店不需要审核，保存即生效**；没开这一步时读订单会报 `This app is not approved to access the Order object`。
4. 把 Client ID / Client Secret / 店铺域名写进本机 `~/.config/keel/shopify-dev`（0600），不经对话传递。
5. webhook 回调地址：`https://<演示站域名>/api/v1/webhooks/channels/<binding_id>`。在 binding 的 `config` 里配 `webhook_base_url`（`https://<演示站域名>`），启用时首拉商品顺带自动装订阅（`PRODUCTS_CREATE/UPDATE/DELETE`、`INVENTORY_LEVELS_UPDATE`、
`APP_UNINSTALLED`、`ORDERS_CREATE/UPDATED/CANCELLED/PAID`、`REFUNDS_CREATE`、`FULFILLMENTS_CREATE`，共 11 个主题；
第三期之前建的 binding 升级后要点一次「重新同步商品」补装订单相关的几个）。binding 其余约定：`external_account` = 店铺域名，`secrets` = `{"client_id","client_secret"}`，`config.default_category_id` 必填（新商品挂哪个类目），`config.price_store_id` 可选（价格从哪家门店出，缺省为映射门店里 id 最小的）。
