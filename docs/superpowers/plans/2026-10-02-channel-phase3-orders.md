# 渠道适配层 · 第三期：渠道订单 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 渠道上的订单进 keel：`orders` 放开买家可空并标来源，渠道单（`channel_orders`）收单后经 SAGA 生成一张已支付的 keel 订单（扣同一份库存），进现有履约队列；keel 发货回传平台（Shopify `fulfillmentCreate` 带物流单号）；平台上的取消 / 退款 / 发货转成 keel 订单上的动作；平台发起的申请（`channel_order_requests`）有通用的处理流（美团在第四期用上）。

**Architecture:** 适配器补 `FetchOrder` / `Act` 与订单回调解析，产出规整的 `channel.ChannelOrder`（行、金额快照、收货人、规整状态、单调版本）。core 新增 `service/channel_order.go`（收单、版本守卫、接单 SAGA、平台事实 → keel 动作）、`service/channel_order_saga.go`（三个 core 分支 + 复用库存的 `inventory_deduct/restore`）、`service/channel_order_action.go`（`channel.order.action` 队列：接单 / 拒单 / 发货回传）、`service/channel_order_request.go`（申请流 + 截止扫描）。履约仍走 `AdminOrderService.Ship`，渠道单在同一事务里多入队一条回传任务。

**Tech Stack:** Go、PostgreSQL（RLS、sqlc、goose）、dtm SAGA（单体 local:// / 拆分 http）、jobs 表、Shopify Admin GraphQL `2026-10`、`httptest` 模拟平台、Vue 3 后台。

**Spec:** `docs/superpowers/specs/2026-10-02-channel-adapter-design.md`（§4.4、§5.2、§5.3、§7.1、§7.2、§9、§11）

## 已核实的事实（2026-10-02）

- **keel `orders`**：`user_id NOT NULL` 且有复合外键 `fk_orders_user (user_id, merchant_id) → users`（00009）；外键是 MATCH SIMPLE，`user_id` 为空时不检查，放开 NOT NULL 即可。`chk_amount`：`payable = goods + freight − discount`、`refunded ≤ paid`；`chk_discount_sources`（00058）：没有券时 `discount = promotion_discount`；`chk_fulfillment_timestamps`：20 起要 `paid_at`。状态边只在 `order_status_transitions` 表 + 触发器 `guard_order_status_transition`：(0,10)(0,90)(10,20)(10,90)(20,30)(20,50)(30,40)(50,60)(50,20)——**没有 0→20**，渠道单走 0→10→20（同一事务两次 UPDATE，触发器逐次检查）。
- **keel 下单 SAGA**（`order_saga.go`）：core 分支只拿到 `(gid, branchID, op)`，租户与订单号从 gid 来；库存分支 `inventory.BranchDeduct/BranchRestore` 的扣减行随步骤载荷（`inventory.EncodeDeductPayload`）带过去。`settleFreeOrder`（payment.go:410）是「不经支付渠道直接到 20」的先例。
- **退款**：`RefundService.Create` 要买家身份和 payments 行；整单未发货退款是 `tx.StartWholeOrderRefund`（20→50）+ `tx.FinishWholeOrderRefund`（50→60），回补库存 `enqueueRefundRestock`（outbox）。渠道单退款不调支付渠道，直接走仓储写退款行。
- **通知**：状态变化与通知同事务；`TestEveryStateTransitionNotifiesOrSaysWhyNot`（notification_policy_test.go）要求每个改状态的调用点登记「发哪条通知 / 为什么不发」。
- **Shopify（开发店 2026-10-02 只读核实）**：token scope 有 `write_orders`、`write_assigned_fulfillment_orders`，**没有** `read/write_merchant_managed_fulfillment_orders`（商家自管 location 上的 `fulfillmentCreate` 一般要它，未实测，联调时确认，见 Task 9）。店铺币种 USD、时区 America/New_York。`FulfillmentInput{trackingInfo{company,number,url}, notifyCustomer, lineItemsByFulfillmentOrder:[{fulfillmentOrderId, fulfillmentOrderLineItems}]}`；`FulfillmentOrderStatus` = OPEN/IN_PROGRESS/CANCELLED/INCOMPLETE/CLOSED/SCHEDULED/ON_HOLD；`OrderDisplayFinancialStatus` = PENDING/AUTHORIZED/PARTIALLY_PAID/PARTIALLY_REFUNDED/VOIDED/PAID/REFUNDED/EXPIRED。Order 字段有 `cancelledAt`、`updatedAt`、`test`、`currentTotalPriceSet`、`totalShippingPriceSet`、`totalDiscountsSet`、`totalTaxSet`、`totalRefundedSet`、`refunds`、`fulfillmentOrders`、`fulfillments`、`shippingAddress`、`phone`、`email`。回调主题有 `ORDERS_CREATE/UPDATED/CANCELLED/PAID/FULFILLED`、`REFUNDS_CREATE`、`FULFILLMENTS_CREATE`。
- **补充只读探查（2026-10-02）**：没有 `read_customers`（查 customers 回 ACCESS_DENIED）；两个 location 都是商家自管（`fulfillmentService` 为空）；`fulfillmentCreate` 用不存在的 FO 试回 userError「Fulfillment order does not exist.」（不是 ACCESS_DENIED），接受 `@idempotent(key:)`，`fulfillmentOrderLineItems` 可省（= 剩余全部行）；单张订单完整取单查询成本 149 点；`orderCancel` 异步、`reason` 与 `restock` 必填；退款行 `restockType` = RETURN / CANCEL / LEGACY_RESTOCK / NO_RESTOCK；`write_orders` 下有 `orderCreate`（可 `test:true`、`financialStatus:PAID`、`options.inventoryBehaviour`），联调可用它建测试单——**写操作，要用户同意**。开发店现有 0 张订单。
- **未核实、按文档写、联调确认**：在开发店下单（被权限拦了，要用户在开发店下一单或批准用 Admin API 建测试单）；`refunds/create` 与 `fulfillments/create` 回调体里订单 ID 字段为 `order_id`（数字）；受保护客户数据没开时 `shippingAddress` 为 null 还是报错。

## Global Constraints

- 迁移号段：core **`00320`–`00329`**（只有 Task 1 写迁移）；库存库本期不加。第五期 `00340` 起不得占。
- 不新增订单状态边；渠道单 0→10→20、整单退款 20→50→60、发货 20→30、完成 30→40 全用现有边。
- **开关关着（`KEEL_CHANNELS` 未开）时自营下单 / 退款 / 发货的写路径不加分支**：`source` 列默认 0；发货里「是不是渠道单」的判断只读已经取出来的订单行（`order.Source`），不多一次查询；第一期的不变量测试照旧通过且不改。
- 买家侧的所有查询都带 `user_id = 我`，渠道单（`user_id` 空）自然查不到；**不建「渠道假买家」**。
- 渠道单金额只用平台快照，不重新算价（不变量 5）。keel 订单：`goods` = Σ 行价 × 数量；`freight` = 买家付的运费；`discount = promotion_discount` = 平台补贴 + 商家补贴（没有券）；`payable = paid = goods + freight − discount`。**税不进 keel 订单**（美国店价外税）：税额记在 `channel_orders.amounts.tax_cents`，平台总价 = keel 实付 + 税。
- 渠道层通用代码按 `Caps` 分支，不写 `if kind == "shopify"`；`internal/channel/shopify` 只 import 标准库与 `internal/channel`。
- core 的 SQL 不写 `merchant_id`（只靠 RLS）；手建租户上下文只在 `tenant_context_test.go` 放行清单里的文件（渠道 worker、SAGA 分支从 gid 建）。
- 错误文本写库 / 日志前过 `channel.RedactError`。
- 每个改状态的调用点进 `notificationCallSites`；渠道单没有买家，买家通知一律不发（登记理由「渠道单无 keel 买家，平台自己通知顾客」），员工侧通知照发。

## Review Focus

1. **重复 / 乱序回调建出两张 keel 订单**：`orders/create`、`orders/paid`、`orders/updated` 几乎同时到，且被并发处理。必须只有一张 keel 订单、只扣一次库存。手段：`channel_orders`（binding, 外部单号）唯一 + 建 keel 草稿与写 `channel_orders.order_no` 同一事务（`SELECT … FOR UPDATE` 锁渠道单行）+ SAGA gid 由渠道单 id 决定。测试：同一张单的三条回调并发进 `Inbound` 再 `Drain` → 1 张 keel 订单、库存扣 1 次。（Task 4）
2. **旧状态覆盖新状态**：先处理到「已取消」、再处理到迟到的「新单」快照。`channel_orders.version` 单调（Shopify 用 `updatedAt` 毫秒），旧版本只留档不改状态、不触发动作。测试：先喂 version 2 的取消、再喂 version 1 的新单 → 不建 keel 订单。（Task 4）
3. **每张平台单都触发一次 CAS 冲突**：平台卖出时自己先减了平台上的数，keel 接单扣库存后推送时 `changeFromQuantity` 还是旧基线 → 冲突 → 记差异。接单成功时按（门店, SKU）把 `channel_listings.published_qty` 基线减去这单的数量（下限 0），推送就是正常 CAS。测试：sim 上 A=7，平台下 2 件 → sim 6→5（sim 自己减）；keel 接单、`Drain` → sim 上是 keel 的对外可售数、`Calls` 里没有冲突、`channel_listings.last_error` 为空。（Task 4）
4. **扣库存失败**：keel 没货。不得出现 20 的 keel 订单；草稿关到 90 且不回补库存（没扣过）；渠道单标异常「缺货」并发员工通知；`AcceptRequired` 的渠道入队 `Act(拒单)`，Shopify（不需接单、不能拒）不调任何动作。重新处理（后台「重试」）在补了库存之后能成单，且不重复建单。（Task 4、6）
5. **平台在 keel 发货前取消**：keel 订单 20→50→60、`refunded = paid`、库存回补一次、不调支付渠道、不入队发货回传；**keel 已发货后才取消**：keel 订单不动，渠道单标异常让人处理。重放同一取消事件不重复退款。（Task 5）
6. **发货回传重放**：`fulfillmentCreate` 超时后重试，不得在 Shopify 上建两条 fulfillment：适配器先读 fulfillment orders，已经没有 OPEN / IN_PROGRESS 的就当成功。测试：sim 第一次 fulfillmentCreate 成功但回 5xx → 重试 → sim 上 1 条 fulfillment。（Task 3、5）
7. **映射不全**：行里有没链到 keel SKU 的变体、或订单分到了没映射的 location → 不建 keel 订单，渠道单标异常（原因写清是哪一行 / 哪个 location），补了映射后「重试」能成单。（Task 4）
8. **只收已付款的单**：`AUTHORIZED` / `PENDING` 的不接单；之后 `orders/paid` → 接单。（Task 4）

---

### Task 1: 迁移 00320 与 user_id 可空

**Files:**
- Create: `db/migrations/00320_channel_orders.sql`
- Modify: `db/queries/orders*.sql`（只在需要读 `source` 的查询里加列）、`db/tenancy.json`（新表登记）、`internal/repository/internal/db/*`（`make generate-sql` 生成）、`internal/repository/order.go` 等（`UserID` 变 `*int64` 的连锁）、所有编译报错的 service 调用点、`docs/电商系统-数据模型设计.md`（orders 两列、两张新表）

**迁移内容：**
```sql
-- +goose Up
ALTER TABLE orders ADD COLUMN source SMALLINT NOT NULL DEFAULT 0 CHECK (source IN (0, 1));
ALTER TABLE orders ADD COLUMN channel_order_id BIGINT NULL;
ALTER TABLE orders ALTER COLUMN user_id DROP NOT NULL;
ALTER TABLE orders ADD CONSTRAINT chk_order_buyer CHECK (source <> 0 OR user_id IS NOT NULL) NOT VALID;
ALTER TABLE orders VALIDATE CONSTRAINT chk_order_buyer;
-- 渠道单的优惠来自平台（没有券、也不是 keel 的活动）：放开来源约束，仅对 source = 1
ALTER TABLE orders DROP CONSTRAINT chk_discount_sources;
ALTER TABLE orders ADD CONSTRAINT chk_discount_sources CHECK (
    promotion_discount_cents >= 0 AND promotion_discount_cents <= discount_cents
    AND (user_coupon_id IS NOT NULL OR discount_cents = promotion_discount_cents)) NOT VALID;
ALTER TABLE orders VALIDATE CONSTRAINT chk_discount_sources;
CREATE INDEX idx_orders_channel ON orders(channel_order_id) WHERE source = 1;

CREATE TABLE channel_orders (
  id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  merchant_id BIGINT NOT NULL DEFAULT current_merchant() REFERENCES merchants(id) ON DELETE CASCADE,
  binding_id BIGINT NOT NULL,              -- FK (binding_id, merchant_id) → channel_bindings ON DELETE RESTRICT
  external_order_id TEXT NOT NULL,         -- 平台订单 ID（Shopify gid）
  external_order_name TEXT NOT NULL DEFAULT '', -- 给人看的单号（Shopify #1001）
  store_id BIGINT NULL,                    -- 映射到的 keel 门店（映射不全时为空）
  order_no TEXT NULL,                      -- 关联的 keel 订单号（接单前为空）
  platform_status TEXT NOT NULL,           -- 平台状态原文（Shopify：financial/fulfillment/cancelled 拼起来）
  status SMALLINT NOT NULL,                -- 规整状态：1 待付款 2 新单 3 已接单 4 已发货 5 已完成 6 已取消 7 已拒单
  exception TEXT NULL,                     -- 需要人处理的原因（缺货 / 行没映射 / 发货后被取消……），处理掉清空
  accept_deadline TIMESTAMPTZ NULL, pick_deadline TIMESTAMPTZ NULL,
  delivery_mode SMALLINT NOT NULL DEFAULT 0, rider JSONB NOT NULL DEFAULT '{}',
  amounts JSONB NOT NULL,                  -- {goods,freight,platform_subsidy,merchant_subsidy,commission,tax,merchant_receivable,buyer_paid,refunded}（分）
  lines JSONB NOT NULL,                    -- [{external_line_id, external_sku_id, sku_id|null, title, qty, price_cents, refunded_qty}]
  receiver JSONB NOT NULL DEFAULT '{}',    -- {name, phone, phone_kind(0 真实 1 隐私号), address{...}}
  version BIGINT NOT NULL,                 -- 单调：旧版本不覆盖新版本
  last_payload JSONB NOT NULL,
  test BOOLEAN NOT NULL DEFAULT false,
  created_at/updated_at TIMESTAMPTZ（touch_updated_at 触发器）,
  UNIQUE (binding_id, external_order_id), UNIQUE (id, merchant_id),
  FK (store_id, merchant_id) → stores, FK (order_no, merchant_id)? → 若 orders 有 (order_no, merchant_id) 唯一键就加，否则只靠 order_no 全局唯一的 FK
);
-- orders.channel_order_id 的复合 FK → channel_orders(id, merchant_id)
CREATE TABLE channel_order_requests (
  id, merchant_id, channel_order_id（FK 复合，ON DELETE CASCADE）, external_request_id TEXT NOT NULL,
  kind SMALLINT NOT NULL,      -- 1 取消 2 部分退款 3 缺货调整
  lines JSONB NOT NULL DEFAULT '[]', amount_cents BIGINT NOT NULL DEFAULT 0 CHECK (>= 0), reason TEXT NOT NULL DEFAULT '',
  status SMALLINT NOT NULL DEFAULT 1, -- 1 待处理 2 已同意 3 已拒绝 4 超时自动同意 5 平台已撤销
  deadline TIMESTAMPTZ NULL, decided_by BIGINT NULL（员工）, decided_at TIMESTAMPTZ NULL,
  created_at/updated_at, UNIQUE (channel_order_id, external_request_id)
);
-- 两张表 ENABLE + FORCE RLS（照 00301 的写法），索引：channel_orders(binding_id, status)、channel_orders(merchant_id) WHERE exception IS NOT NULL、
-- channel_order_requests(status, deadline) WHERE status = 1
```
**执行中修正（审查发现）**：`scripts/check_migrations.py` 规定 00151 之后在大表（含 orders）建索引必须 `CONCURRENTLY` + `-- +goose NO TRANSACTION`，且 `NOT VALID` 后同一事务 `VALIDATE` 等于白写。拆成三份：**00320** 建表、加列、约束 `NOT VALID`（含 `chk_discount_sources` 重建为「`source = 1` 或原条件」）；**00321** `idx_orders_channel` CONCURRENTLY；**00322** 单独 VALIDATE。另：`ConfirmOrderReceipt` 的 WHERE 带 `user_id`，渠道单永远匹配不上 → 渠道单另写一条 30→40 语句（`WHERE source = 1`），登记进通知策略；`ListAutoConfirmReminders` 排除 `user_id IS NULL`。后台订单列表**不加 JOIN、不加可空筛选参数**（generic plan 退化，压测文档 ⑨），只多带 `source`、`channel_order_id` 两列；渠道信息在这一页有 `source = 1` 的行时按 id 补查一次。

Down 反向（先删 orders 的 FK 与列、恢复 NOT NULL 前先确认没有 source=1 的行——Down 里 `DELETE` 不做，直接失败即可，写注释）。

- [ ] **Step 1**：写迁移；`make migrate-check`（或 Makefile 里等价的迁移检查 / 租户检查脚本，看 `make help`）通过；`db/tenancy.json` 登记两张新表。
- [ ] **Step 2**：`make generate-sql`；`go build ./...` 列出 `UserID` 由 `int64` 变 `*int64`（或 `pgtype.Int8`，按 sqlc 配置）的全部编译错误，逐个改：
  - 仓储领域结构 `Order.UserID` 改 `*int64`；自营下单路径照旧传非空。
  - 用 userID 的地方（限购累计 / 放回、锁券 / 解券、取消、确认收货、自动确认、买家通知、报表里的「下单人数」）：`nil` 时跳过买家相关部分（限购、券、买家通知），报表人数用 `COUNT(DISTINCT user_id)`（空值天然不计）。
  - 仓储 `Order` 加 `Source int16`、`ChannelOrderID *int64`，`GetOrder*` / 后台订单查询带上这两列。
- [ ] **Step 3**：`make test-db 2>&1 | grep -B2 -A8 -E "FAIL|panic:"` 全绿（现有测试一条不改）；`go vet ./...`。
- [ ] **Step 4**：数据模型文档补 orders 两列与两张表；提交 `渠道订单 · 迁移 00320：orders 加来源与渠道单关联、user_id 放开可空（自营仍必填）、渠道单与平台申请两张表`

### Task 2: 渠道类型与 Shopify 模拟平台的订单部分

**Files:**
- Modify: `internal/channel/channel.go`（充实 `ChannelOrder`）、`internal/channel/channeltest/fake.go`（假适配器能喂订单、记录 Act）
- Modify: `internal/channel/shopify/shopifytest/*`（订单、fulfillment orders、fulfillmentCreate、按订单发回调）

**Interfaces（Produces）：**
```go
type OrderStatus int8 // 与 channel_orders.status 同一套数
const ( OrderPendingPayment OrderStatus = iota + 1; OrderNew; OrderAccepted; OrderShipped; OrderCompleted; OrderCancelled; OrderRejected )

type ChannelOrder struct {
    ExternalOrderID, ExternalOrderName, ExternalStoreID string // ExternalStoreID：订单所在的渠道门店；分到多个门店时为空并在 StoreError 说明
    StoreError      string
    PlatformStatus  string
    Status          OrderStatus
    Version         int64 // 单调；Shopify = updatedAt 的 Unix 毫秒
    Test            bool
    PlacedAt        time.Time
    AcceptDeadline  *time.Time
    Delivery        DeliveryMode
    Lines           []OrderLine
    Amounts         OrderAmounts
    Receiver        Receiver
    Shipments       []Shipment // 平台上已有的发货（Shopify fulfillments 的物流单号）
    Refunds         []Refund   // 平台上已发生的退款（累计）
    Raw             json.RawMessage
}
type OrderLine struct { ExternalLineID, ExternalSKUID, Title string; Qty int32; PriceCents int64; RefundedQty int32 }
type OrderAmounts struct { GoodsCents, FreightCents, PlatformSubsidyCents, MerchantSubsidyCents, CommissionCents, TaxCents, MerchantReceivableCents, BuyerPaidCents, RefundedCents int64 }
type Receiver struct { Name, Phone string; PhoneVirtual bool; Province, City, District, Address, Zip, Country string }
type Shipment struct { Company, TrackingNo string; At time.Time }
type Refund struct { ExternalID string; AmountCents int64; Lines []ActionLine; Restock bool; At time.Time }
```
`BuyerPaidCents` 不含税（= Goods + Freight − PlatformSubsidy − MerchantSubsidy）；平台总价 = BuyerPaid + Tax。Shopify 没有平台补贴与佣金：Platform/Commission 为 0，折扣全记 MerchantSubsidy。

- [ ] **Step 1：模拟平台**（`shopifytest`）：`AddOrder(OrderSpec)` 建一张单（行引用已有变体、location、financial status、运费、折扣、税、收货人），**照真实平台从该 location 的 available 里减掉数量**；`SetFinancial / Cancel(restock bool) / Refund(lines, amount, restock) / FulfillInShopify(company, no)`；GraphQL 子集：`order(id:)`（Task 3 用到的字段）、`fulfillmentCreate`（校验 FO 存在且 OPEN/IN_PROGRESS、行数量不超、建 fulfillment、FO 变 CLOSED）；`FailNext("FulfillmentCreate", afterApply bool)`（成功落地但回 5xx，Review Focus 6）；每次变化 `updatedAt` 加 1 秒；`WebhookFor(orderID, topic)` 生成签名过的回调请求（头与正文照真实平台：`orders/*` 正文含 `admin_graphql_api_id`；`refunds/create`、`fulfillments/create` 正文含数字 `order_id`）。
- [ ] **Step 2：假适配器**（`channeltest`）：可配 Caps（含 `AcceptRequired`、`RefundNeedsApproval`），`PutOrder(ChannelOrder)` 供 `FetchOrder`，记录每次 `Act`，可让 `Act` 失败 N 次。
- [ ] **Step 3**：`go test ./internal/channel/...`；提交 `渠道层：渠道订单的规整类型；Shopify 模拟平台加订单、fulfillment 与订单回调；假适配器能喂单、记动作`

### Task 3: Shopify 适配器——取单、发货回传、订单回调

**Files:**
- Create: `internal/channel/shopify/order.go`、`order_test.go`
- Modify: `shopify.go`（删掉 `FetchOrder` / `Act` 的 ErrUnsupported 桩）、`webhook.go`（`refunds/create`、`fulfillments/create` 取 `order_id` 拼 gid）、`webhooks_install.go`（主题加 `ORDERS_CREATE`、`ORDERS_UPDATED`、`ORDERS_CANCELLED`、`ORDERS_PAID`、`REFUNDS_CREATE`、`FULFILLMENTS_CREATE`）、`live_test.go`（只读：取最近一张单并规整，没有单就 Skip）

- [ ] **Step 1：写失败测试**（打 sim）：
  - `FetchOrder` 规整：金额（字符串元 → 分，`"785.95"` → 78595）、行（变体 gid → `ExternalSKUID`）、`ExternalStoreID` = 唯一的 fulfillment order 的 `assignedLocation`（多个不同 location → 空 + `StoreError`）、收货人（`shippingAddress` 为 null 时 Receiver 空、不报错）、`Version` = updatedAt 毫秒、`Shipments` 来自 `fulfillments.trackingInfo`、`Refunds` 来自 `refunds`。
  - 规整状态：`cancelledAt` 非空 → Cancelled；financial PENDING / AUTHORIZED → PendingPayment；PAID / PARTIALLY_REFUNDED 且未发货 → New；全部 FO CLOSED 且有 fulfillment → Shipped；REFUNDED 且未发货 → Cancelled。
  - `Act(ActShip{TrackingCompany, TrackingNo})`：对所有 OPEN / IN_PROGRESS 的 FO 一次 `fulfillmentCreate`（全部剩余行），`notifyCustomer: true`；没有可发的 FO → 成功（幂等，Review Focus 6）；`userErrors` → 普通错误（不可重试的写进错误文本）；限流 → `RetryableError{RateLimited}`。
  - 其他动作（accept / reject / picked / request 类）→ `ErrUnsupported`（Caps 不声明，渠道层不会调）。
  - 回调：`orders/paid` 用 `admin_graphql_api_id`；`refunds/create` 的 `order_id: 123` → `gid://shopify/Order/123`；都是 `EventOrderChanged`。
  - 装回调：主题清单含 6 个订单主题，已装的不重复装。
- [ ] **Step 2–4**：跑失败 → 实现 → 通过；`go list -deps` 断言（已有 `deps_test.go`）照旧通过。
- [ ] **Step 5**：提交 `Shopify 适配器：取单规整（金额快照不含税、门店取 fulfillment order 的 location、单调版本）、发货回传 fulfillmentCreate（没有可发的就当成功）、订单 / 退款 / 发货回调`

### Task 4: core 收单与接单 SAGA

**Files:**
- Create: `internal/service/channel_order.go`、`internal/service/channel_order_saga.go`、`internal/handler/channel_order_test.go`（testdb，sim + 真实适配器，照 `newShopifyRig`）
- Modify: `internal/repository/channel.go` + `db/queries/channels.sql`（渠道单 upsert / 按 id 锁 / 列表）、`internal/repository/order.go`（`CreateChannelOrderDraft`：status 0、source 1、user_id 空、金额快照、行、`receiver_snapshot`、`store_snapshot`）、`internal/service/channel.go`（`NewChannelService` 登记 `OnInbound(EventOrderChanged, s.orderChanged)`）、`internal/app/*`（SAGA 分支注册：单体 local://、拆分 http，照 `OrderService.Branches()` 的接法）、`tenant_context_test.go`（若分支文件要手建租户——从 gid 建，照 order_saga.go）、`notification_policy_test.go`（新调用点）

**Produces：**
```go
func (s *ChannelService) orderChanged(ctx context.Context, b repository.ChannelBinding, ev channel.Event) error
func (s *ChannelService) applyChannelOrder(ctx context.Context, b repository.ChannelBinding, o channel.ChannelOrder) error // 版本守卫 + 状态转动作
func (s *ChannelService) RetryChannelOrder(ctx context.Context, id int64) error // 后台「重试」：清异常、重新回读、重走接单
// SAGA 分支：channel_order_open(0→10) / channel_order_open_undo(关到 90，渠道单标异常，AcceptRequired 入队拒单) /
//            inventory_deduct / inventory_restore（复用）/ channel_order_finish(10→20 settle + 基线扣减 + AcceptRequired 入队接单)
const BranchChannelOrderOpen, BranchChannelOrderOpenUndo, BranchChannelOrderFinish = "channel_order_open", "channel_order_open_undo", "channel_order_finish"
```

流程：
1. `orderChanged`：`Caps.OutOfOrderInbound` → `FetchOrder` 取权威状态；否则从事件载荷规整（第四期）。→ `applyChannelOrder`。
2. `applyChannelOrder`（一个事务，`FOR UPDATE` 锁渠道单行）：`version ≤` 已存 → 只更新 `last_payload` 不动作（Review Focus 2）。否则 upsert，按（旧状态, 新状态, 是否已有 order_no）决定动作：
   - 新 / 待付款 → 新单，且还没有 keel 订单：`AcceptRequired` 且没配 `config.auto_accept` → 写 `accept_deadline`，等人；否则**同一事务**建 keel 草稿（映射校验在这里：每行能按 `channel_item_links` 找到 SKU、`ExternalStoreID` 有门店映射，否则标异常不建，Review Focus 7）、写 `channel_orders.order_no` 与 `orders.channel_order_id`，提交后提交 SAGA（gid 照 `orderGID` 的规则由订单号定，重复提交被协调器去重）。
   - 已取消 / 已发货 / 退款：交给 Task 5 的 `applyPlatformFacts`（本任务先留空函数，只把状态写进渠道单）。
3. `channel_order_finish`：锁订单，0/10 → 10 → 20（`SettleOrder` 同款字段：`paid_cents = payable`、`paid_at = 平台下单时间`），渠道单 → 已接单；**按行把 `channel_listings.published_qty` 减数量（下限 0）**（Review Focus 3）；`AcceptRequired` → 入队 `channel.order.action`（accept，job_key `act:<channel_order_id>:accept`）；员工通知「渠道新订单」（复用 `notifyOrderPaid` 的员工侧，买家侧跳过）。
4. 扣库存失败 → 协调器回滚 → `channel_order_open_undo`：订单 10/0 → 90（不回补，库存分支的 restore 按流水是空操作），渠道单 `exception = "缺货：<SKU 标题> 要 N 件"`、`order_no` **清空**（下次重试建新单号；旧 90 单保留留痕），员工通知；`AcceptRequired` → 入队拒单。
5. `RetryChannelOrder`：只对 `exception` 非空且没有活着（非 90）的 keel 订单的渠道单；清 exception → `FetchOrder` → `applyChannelOrder`（强制，不受版本守卫）。

- [ ] **Step 1：写失败测试**（Review Focus 1、2、3、4、7、8 各一条，加：正常单金额对上——keel `payable = 平台总价 − 税`，`orders.user_id` 空、`source = 1`、`receiver_snapshot` 有收货人；后台订单列表 `status=20` 能看到它；买家 `GET /orders` 看不到；拆分形态（库存独立进程）跑一遍正常单）。
- [ ] **Step 2–4**：失败 → 实现 → 通过；再跑全部渠道测试、`TestEveryStateTransitionNotifiesOrSaysWhyNot`、`tenant_context_test.go`、第一期不变量测试。
- [ ] **Step 5**：提交 `渠道订单：收单（回读权威状态、版本单调）、接单 SAGA（建已支付的 keel 订单 + 扣同一份库存；缺货关单标异常、映射不全不建单）、接单后推送基线扣掉平台已减的数`

### Task 5: 履约回传与平台事实转 keel 动作

**Files:**
- Create: `internal/service/channel_order_action.go`、`internal/handler/channel_order_fulfil_test.go`
- Modify: `internal/service/order_fulfillment.go`（`Ship`：`order.Source == 1` 时同一事务入队回传，**不多查一次**）、`internal/service/channel_order.go`（`applyPlatformFacts`）、`internal/repository/refund.go` 或新文件（渠道退款：直接写成功的退款行 + 行退款数，不要 payments 行）、`internal/service/auto_confirm.go`（渠道单 30→40 不需要买家）、`channel_worker.go`（新队列 `channel.order.action` 进 `WorkOnce` / `housekeep`）、`notification_policy_test.go`

**Produces：**
```go
const QueueChannelOrderAction = "channel.order.action"
type channelActionJob struct { ChannelOrderID int64 `json:"channel_order_id"`; Action channel.Action `json:"action"` }
func (s *ChannelService) EnqueueShipTx(ctx context.Context, tx repository.Tx, order repository.Order, carrierName, trackingNo string) error
```
- 发货回传：`Ship` 写完发货行后 `s.channels.EnqueueShipTx`（`AdminOrderService.WithChannels` 已有；nil 接收者直接返回——开关关着时零开销）。worker：取渠道单与 binding → `Act` → 成功标完成；`ErrUnsupported` → 完成并 Warn；凭据失效 → `markCredentialsBroken`；其余退避（20 次进死信、渠道单标异常「发货没回传上」）。
- `applyPlatformFacts(o)`（`applyChannelOrder` 里调用，同一事务）：
  - **取消**：keel 订单 20 → 整单退款（退款行状态成功、金额 = paid、`StartWholeOrderRefund` + `FinishWholeOrderRefund`、`enqueueRefundRestock`），渠道单 → 已取消；keel 订单 30 / 40 → 不动，`exception = "平台在 keel 发货后取消了订单"`；keel 订单还在 0 / 10（SAGA 中）→ 渠道单记已取消，finish 分支看到渠道单已取消就转去关单 + 回补（finish 里锁渠道单检查）。重放：退款行带 `channel_refund:<external_order_id>:cancel` 的幂等键（唯一）。（Review Focus 5）
  - **平台上的部分退款**（`Refunds` 里新出现的 ExternalID）：记一条成功退款（金额、行退款数），`refund_status` 照现有约束 2 / 3；行标 `Restock` 且 keel 订单未发货 → 回补对应件数。按 ExternalID 幂等。
  - **平台上发了货**（`Shipments` 非空、keel 订单 20）：keel 发货 20→30（承运商写平台给的 company、单号），**不入队回传**（避免回声）。
- 自动确认收货：渠道单与自营同一扫描（`shipped_at + auto_confirm_days`），`user_id` 为空时不发买家通知。
- [ ] **Step 1：写失败测试**：keel 发货 → sim 上有 fulfillment 带单号、只建一条（含 Review Focus 6 的 5xx 重放）；开关关着的进程里 `Ship` 自营单不入任何渠道队列（并入第一期不变量测试）；Review Focus 5 两条 + 重放；部分退款（含 restock）；在 Shopify 后台发货 → keel 订单 30、jobs 里没有回传任务；渠道单 30 超过自动确认天数 → 40。
- [ ] **Step 2–4**：失败 → 实现 → 通过；全部渠道测试 + 通知策略测试。
- [ ] **Step 5**：提交 `渠道订单：keel 发货回传平台（幂等）、平台取消 / 退款 / 发货转 keel 动作（发货前取消整单退款回补、发货后取消标异常、平台发货不回声）`

### Task 6: 接单 / 拒单与平台申请流（通用，用假适配器测）

**Files:**
- Create: `internal/service/channel_order_request.go`、`internal/handler/channel_order_request_test.go`
- Modify: `channel_worker.go`（截止扫描进 `housekeep`：每分钟）、`channel_order.go`（`EventOrderRequest` 处理器登记）

**Produces：**
```go
func (s *ChannelService) AcceptChannelOrder(ctx context.Context, id int64) error  // AcceptRequired 且在等人：走 Task 4 的建单 + SAGA
func (s *ChannelService) RejectChannelOrder(ctx context.Context, id int64, reason string) error // 入队 Act(拒单)，渠道单 → 已拒单
func (s *ChannelService) DecideRequest(ctx context.Context, requestID int64, agree bool, staffID int64) error
func (s *ChannelService) sweepChannelDeadlines(ctx context.Context) // 接单截止前 N 分钟提醒、申请超时记「超时自动同意」
```
- 申请事件 → `channel_order_requests`（幂等）；binding `config.request_policy`（`manual` 默认 / `auto_agree_unshipped`：keel 订单未发货的取消自动同意）；同意 → 入队 `Act(agree_request)`，**等平台确认**（后续订单事件里的取消 / 退款）才动 keel 订单（不变量 2）；拒绝 → `Act(reject_request)`。
- 截止：`accept_deadline − config.accept_remind_minutes`（默认 3）发员工通知一次；申请 `deadline` 过了 → 状态 4、员工通知（平台侧自己会自动同意，keel 等平台事实）。
- [ ] **Step 1：写失败测试**（假适配器 `AcceptRequired`、`RefundNeedsApproval`）：人工接单 → 建单 + `Act(accept)`；超时前提醒只发一次；拒单 → `Act(reject)` 不建单；缺货 → 自动 `Act(reject)`（Review Focus 4 后半）；申请同意 → `Act(agree_request)`、keel 订单不动，之后平台取消事件到 → 整单退款；自动策略；超时记 4。
- [ ] **Step 2–5**：失败 → 实现 → 通过 → 提交 `渠道订单：人工 / 自动接单与拒单、平台申请流（同意后等平台确认才动 keel 订单）、接单与申请截止扫描`

### Task 7: 后台接口与界面

**Files:**
- Modify: `docs/电商系统-OpenAPI.yaml`（`GET /admin/channel-orders`（binding_id、status、exception_only、分页）、`GET /admin/channel-orders/{id}`（含申请）、`POST /admin/channel-orders/{id}/retry`、`/accept`、`/reject`、`POST /admin/channel-order-requests/{id}/decision`；后台订单返回加 `source`、`channel`（`{kind, binding_name, external_order_name}`，非渠道单为空）），`make generate`
- Create: `internal/handler/admin_channel_order.go`、契约测试按现有规矩
- Modify: `web/admin`：渠道详情加「订单」页签（列表：平台单号、状态、keel 订单号链接、异常标红、操作重试 / 接单 / 拒单）；订单列表与详情显示「来自 Shopify #1001」、买家显示「渠道顾客」；`make admin-responsive-check` 把新页签算进去
- 权限：查看同渠道页；重试 / 接单 / 拒单 / 申请决定需「订单处理」权限（照 `Ship` 的门店范围校验）。
- [ ] **Step 1**：契约 + handler 测试；**Step 2**：前端；**Step 3**：`make check-all`、`make admin-responsive-check`（手机 / 电脑全过并**看截图内容**）；**Step 4**：提交 `渠道订单后台：渠道单列表与详情（异常、重试、接单 / 拒单、申请决定）、订单列表标来源`

### Task 8: 压测对照（第一期挪过来的）

- [ ] 拿 `v0.7.0`（`b9a372a`，第一期合并之前）与第三期合并后、`KEEL_CHANNELS` 不开的版本，在同一套拆分形态的栈上跑 `keel-loadtest` 的 `order`、`list-default`、`admin-orders` 三个场景，p95 差距应在噪声内；结果写进 `docs/性能压测-2026-10.md` 新一节。差距超出噪声就先查原因再合并。

### Task 9: 装配、不变量、联调、文档

- [ ] **Step 1**：`internal/app/channels.go` 装配新处理器与 SAGA 分支；开关关着时不注册分支、不登记处理器；第一期不变量测试补断言「`orders.source` 全 0、`channel_orders` 空」。
- [ ] **Step 2**：`make test-db`（全量）、`make check-all`。
- [ ] **Step 3**：只读联调 `KEEL_SHOPIFY_LIVE=1`：适配器取单（开发店有单才跑）。**下单联调需要用户**：在开发店下一张测试单（或批准用 Admin API `orderCreate` 建 test 单）→ 演示站收单、接单、扣库存 → 后台发货 → Shopify 上看到单号；确认 fulfillment 的 scope 够不够（大概率要请用户在 Dev Dashboard 加 `read/write_merchant_managed_fulfillment_orders`、勾受保护客户数据字段并重装；spec §15 的 `write_fulfillments` 是旧写法，一并改）。
- [ ] **Step 4**：spec §13 补订单部分实测结论；CHANGELOG；PROGRESS；数据模型文档。
- [ ] **Step 4b（用户要求）**：写 `docs/渠道接入-Shopify.md` 给商家看：开店（Partner + 开发店 / 正式店）→ Dev Dashboard 建应用、scope 清单（`read/write_products`、`write_inventory`、`read_locations`、`read/write_orders`、`read/write_merchant_managed_fulfillment_orders`）、发新版本 + 重装才生效 → 受保护客户数据（Dev Dashboard 里没有入口，要到 Partner Dashboard 的「API access requests」申请，勾姓名 / 地址 / 电话 / 邮箱；开发店不用审核）→ keel 后台建渠道账号（店铺域名、Client ID / Secret、默认类目、回调地址）、门店映射、库存与价格规则、启用首拉 → 日常：订单进来、发货、异常单重试、已知限制（币种不换算、一单不拆多店、税不进 keel 订单）。docs 索引里加一行。
- [ ] **Step 4c（用户要求）**：README（中英两份）加「第三方渠道」：Shopify 已接（商品进、库存与价格出、订单进、发货回传），渠道接口按美团闪购 / 饿了么零售的能力设计（**不声称已对接**），链到接入指南与设计 spec；功能列表 / 架构图里有渠道层的位置。
- [ ] **Step 5**：演示站：备份 → 迁移副本试跑 → 部署 core / inventory / console + `publish-frontend.sh admin` → binding 1 点「重新同步商品」装订单回调。

## 不在本期

- 美团模拟适配器（第四期）、对账与一键补单（第五期）。
- 订单编辑（Shopify `orders/edited` 加减行）：只留档，渠道单标异常让人看。
- 一张平台单分到多个门店：标异常，不拆单。
- 币种换算（USD 按分原样记）。
