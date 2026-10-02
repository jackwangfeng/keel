# 渠道适配层 · 第二期：Shopify 商品与库存 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 接上第一个真实渠道 Shopify：把开发店的商品拉进 keel（建 / 更新商品与 SKU、写 `channel_item_links`、带图），把 keel 的对外可售数与价格推出去（`inventorySetQuantities` CAS + `productVariantsBulkUpdate`），并补上第一期留下的两件前置事（限流不计失败次数、错误文本不含凭据）。

**Architecture:** 适配器 `internal/channel/shopify` 只依赖 `internal/channel`：client credentials 换 token（24 小时，进程内缓存、提前续）、GraphQL 客户端（按 `throttleStatus` 退避）、回调验签、拉商品、推库存价格、装 webhook。进程内模拟平台 `shopify/shopifytest` 实现用到的 GraphQL 子集，适配器单测与渠道层集成测试都打它。core 侧新增 `service/channel_catalog.go`（`channel.catalog.pull` 队列 + 商品回调处理器：按 `channel_item_links` 建或更新 keel 商品），推送侧按新能力 `Caps.PricePerStore` 决定价格从哪家门店出。

**Tech Stack:** Go、Shopify Admin GraphQL `2026-10`、PostgreSQL（RLS、sqlc）、jobs 表、`httptest`。

**Spec:** `docs/superpowers/specs/2026-10-02-channel-adapter-design.md`（§6、§7.3、§7.4、§9、§11、§13、§15）

## 已实测的 Shopify 事实（2026-10-02，开发店 keel测试，API 2026-10）

写代码以这些为准，不再按二手资料：

- `POST https://{shop}/admin/oauth/access_token`，JSON `{grant_type: client_credentials, client_id, client_secret}` → `{access_token, scope, expires_in: 86399}`：**token 24 小时过期**。
- GraphQL `POST https://{shop}/admin/api/2026-10/graphql.json`，头 `X-Shopify-Access-Token`。每次响应带 `extensions.cost.throttleStatus {maximumAvailable: 4000, currentlyAvailable, restoreRate: 200}`。
- `inventorySetQuantities(input:{name:"available", reason:"correction", referenceDocumentUri, quantities:[{inventoryItemId, locationId, quantity, changeFromQuantity}]}) @idempotent(key:)`：
  - `changeFromQuantity` 对不上 → `userErrors[{field:["input","quantities","<i>","changeFromQuantity"], code:"CHANGE_FROM_QUANTITY_STALE"}]`；
  - **一批里有一条冲突，整批都不生效**（实测另一条没变）；
  - `changeFromQuantity: null` 被接受（跳过比对）；值没变时 `inventoryAdjustmentGroup` 为 null、无错误。
- 有 `inventoryItem.tracked = false` 的变体（不跟踪库存）和礼品卡（`product.isGiftCard`）；变体 `sku` 可能为 null。
- 店铺币种是 USD（开发店），keel 不做汇率：价格按分原样推，联调用的 binding 由商家自己保证同币种（写进 PROGRESS「已知限制」）。
- `webhookSubscriptionCreate(topic:, webhookSubscription:{uri:})`（字段是 `uri`）；主题枚举有 `PRODUCTS_CREATE/UPDATE/DELETE`、`INVENTORY_LEVELS_UPDATE`、`APP_UNINSTALLED`。
- 开发店有两个 location：`gid://shopify/Location/87516708954`（Shop location）、`…/87516741722`（My Custom Location）。

## Global Constraints

- 本期**不加迁移**（`channel_item_links.extra`、`channel_listings` 够用）；真要加，core 用 `00302`–`00309`，不得占 `00320` 起（第三期）与 `00340` 起（第五期）。
- `internal/channel/shopify` 只 import 标准库与 `internal/channel`（`go list -deps` 断言，见 Task 4）。
- 适配器返回的错误文本**不得包含** client secret / access token；渠道层写 `last_error`、jobs.last_error、日志之前再过一遍 `channel.Redact`（双保险）。
- 限流（`RetryableError.RateLimited`）不计入 jobs 的失败次数；其余失败照旧计次、封顶退避、20 次进死信。
- 渠道层通用代码按 `Caps` 分支，**不写 `if kind == "shopify"`**。
- 手建租户上下文只在登记过的文件里（`channel_worker.go`；新文件 `channel_catalog.go` 若要手建，必须进 `tenant_context_test.go` 放行清单——优先在 worker 里建好再传进去）。
- 库存服务的查询只能写在 `inventory_svc.sql`；core 的 SQL 里不写 `merchant_id`（只靠 RLS）。
- 回调地址：`{webhook_base_url}/api/v1/webhooks/channels/{binding_id}`，Host 定租户。
- binding 约定：`external_account` = 店铺域名（`xxx.myshopify.com`）；`secrets` = `{"client_id","client_secret"}`；`config` = `{"default_category_id": <int>, "price_store_id": <int, 可选>, "webhook_base_url": "<https://…>, 可选"}`。

## Review Focus

1. **第一次接上就把 Shopify 库存清零**：新建的 keel SKU 初始库存为 0，若直接推，Shopify 上的现货全变 0。必须：新建的 SKU 用 Shopify 当时的 available 作初始库存（按门店映射），且把那一刻的值记为 `channel_listings` 基线，首推是空操作。测试：sim 上某变体 available=7，拉完商品、跑完 worker，sim 上仍是 7，keel 库存是 7。（Task 7）
2. **keel 里已有同货号的 SKU（商家先在 keel 建过）**：不得新建重复商品、不得用 Shopify 的数覆盖 keel 库存；应认领（adopt）该 SKU，基线记 Shopify 现值，随后按 keel 的数推出去覆盖。测试：keel 先有货号 A 库存 3，Shopify 同货号 available 9 → 拉完后 keel 商品数不变、keel 库存仍 3、sim 上变成 3。（Task 7）
3. **一批里一条 CAS 冲突导致整批不生效**：适配器必须把冲突那条单拎出来回 `Conflict`+`ObservedQty`，其余的**同一次调用里重提**成功，而不是整批报错。测试：sim 一批 3 条、第 2 条被店员改过 → 1、3 成功，2 冲突带现值；随后渠道层重推覆盖成功。（Task 6）
4. **限流期间任务被耗进死信**：sim 连续返回 THROTTLED 30 次（超过 max_attempts=20），任务不得进死信、attempts 不增长，限流解除后推成功。（Task 1 单测 + Task 6 集成）
5. **一个 binding 两家门店、Shopify 价格只有一个**：两店 keel 价不同，只能有一家的价推出去，且另一家改价不得引起价格来回跳。测试：价格源门店改价 → sim 价格变；另一家改价 → sim 价格不变、也不入推送任务。（Task 2）

---

### Task 1: 前置——限流不计失败次数、错误文本脱敏

**Files:**
- Modify: `internal/repository/jobs.go`（加 `DeferJob`）、`internal/service/channel_worker.go`（`retry` 分流）
- Create: `internal/channel/redact.go`、`internal/channel/redact_test.go`
- Test: `internal/repository/jobs_test.go`（testdb，已有文件则追加）、`internal/handler/channel_listing_test.go`（追加限流用例）

**Interfaces:**
- Produces:
  ```go
  // repository
  // DeferJob 把占着的任务放回队列、run_after = now()+after，并撤回占位时加的那一次 attempts（限流不算失败）。
  func (r *Repo) DeferJob(ctx context.Context, id int64, reason string, after time.Duration) error
  // channel
  // Redact 把 secrets（JSON 对象）里所有长度 ≥ 6 的字符串值在 msg 里替换成 "***"。secrets 解不开时原样返回 msg。
  func Redact(msg string, secrets json.RawMessage) string
  // RedactError 返回一个 Error() 已脱敏、但 errors.Is/As 仍能穿透到原错误的包装。err 为 nil 返回 nil。
  func RedactError(err error, secrets json.RawMessage) error
  ```

- [ ] **Step 1: 写失败测试**
  - `redact_test.go`：`Redact("bad token shpat_abcdef123 for client xyz", {"client_id":"xyz","client_secret":"shpat_abcdef123"})` → `"bad token *** for client xyz"`（`xyz` 不足 6 字符不替换）；嵌套对象里的字符串也替换；`RedactError(&RetryableError{RateLimited:true, Err: errors.New("secret123456")}, …)` 的 `Error()` 不含 `secret123456` 且 `errors.As(…, *RetryableError)` 为真、`errors.Is(RedactError(ErrCredentials,…), ErrCredentials)` 为真。
  - `jobs_test.go`：入队 `max_attempts=2` 的任务，循环 5 次「DequeueJobs → DeferJob(…, 0)」，每次都能再取出，attempts 始终 ≤ 1，status 从不为 3；再 `RetryJobCapped` 两次才进死信。
- [ ] **Step 2: 跑测试确认失败**：`go test ./internal/channel/ -run Redact` 与 `make test-db ARGS='-run TestDeferJob ./internal/repository/'`（按 Makefile 实际写法；看 `make help`）→ 编译失败。
- [ ] **Step 3: 实现**
  - `DeferJob`：
    ```sql
    UPDATE jobs SET status = 0, attempts = GREATEST(attempts - 1, 0),
           run_after = now() + ($3::float8 * interval '1 second'),
           last_error = $2, locked_by = NULL, locked_at = NULL
     WHERE id = $1
    ```
    写在 `jobs.go`（与 `RetryJobCapped` 同处、同一种原生 SQL 写法），注释说明「撤回占位时那次 +1」。
  - `retry`：
    ```go
    func (s *ChannelService) retry(ctx context.Context, j repository.Job, cause error) {
        var re *channel.RetryableError
        if errors.As(cause, &re) && re.RateLimited {
            after := re.After
            if after <= 0 { after = time.Second }
            if err := s.repo.DeferJob(ctx, j.ID, cause.Error(), min(after, channelMaxBackoff)); err != nil { … log … }
            return
        }
        … 原有逻辑 …
    }
    ```
  - 渠道层所有把适配器错误写进 `last_error` / 日志 / `retry` 的地方，先 `err = channel.RedactError(err, ab.Secrets)`：`pushStore`（整批错误与每条 `r.Err`）、`markCredentialsBroken` 的日志、`handleInbound` 的 `mark`（处理器错误）。
- [ ] **Step 4: 集成用例**（`channel_listing_test.go` 追加 `TestChannelPushRateLimitDoesNotDeadLetter`）：给 `channeltest.Adapter` 加 `FailNext(n int, err error)`（前 n 次 `PushListings` 返回 err），设 25 次 `&channel.RetryableError{RateLimited:true, After: time.Millisecond}`；改库存后反复 `Drain` 直到推成功；断言任务从未进死信（`SELECT count(*) FROM jobs WHERE queue='channel.listing.push' AND status=3` = 0）。再加一条：`FailNext(1, errors.New("boom client_secret_value_xyz"))`，binding secrets 含该值 → `channel_listings.last_error` 与 `jobs.last_error` 都不含它。
- [ ] **Step 5: 跑测试通过**；`go vet ./internal/...`。
- [ ] **Step 6: 提交** `渠道层前置：限流退避不计失败次数（DeferJob）、适配器错误写库前脱敏凭据`

### Task 2: 渠道类型充实与「价格从哪家门店出」

**Files:**
- Modify: `internal/channel/channel.go`、`internal/channel/adapter.go`、`internal/channel/channeltest/fake.go`、`internal/service/channel_listing.go`、`internal/service/channel_worker.go`
- Test: `internal/handler/channel_listing_test.go`（追加）

**Interfaces:**
- Produces（`internal/channel`）：
  ```go
  // Caps 新增
  PricePerStore bool // 渠道上的价格按门店分（美团）；false = 全渠道一个价（Shopify），由价格源门店出

  // Listing 新增
  PushPrice bool // 这一条要不要推价格（PricePerStore=false 时只有价格源门店的那条为真，且价格与上次不同）

  // CatalogItem 充实（替换现在的 ExternalID/Title/Raw 三字段版本，保留这三个）
  type CatalogItem struct {
      ExternalID  string
      Title       string
      Description string
      Status      CatalogStatus     // CatalogActive / CatalogDraft / CatalogArchived
      ImageURLs   []string          // 按顺序，第一张是主图
      Variants    []CatalogVariant
      Raw         json.RawMessage
  }
  type CatalogStatus int8
  const ( CatalogActive CatalogStatus = iota + 1; CatalogDraft; CatalogArchived )
  type CatalogVariant struct {
      ExternalID string
      SKUCode    string            // 渠道上的货号，可能为空
      Options    map[string]string // 规格名 → 值（Shopify selectedOptions；单规格 "Title"→"Default Title" 时为空 map）
      PriceCents int64
      ImageURL   string
      Extra      json.RawMessage   // 存进 channel_item_links.extra（Shopify: {"inventory_item_id","product_id","tracked"}）
      Levels     []StockLevel      // 渠道上各门店的当前可售数（首接时作初始库存与基线）
  }
  type StockLevel struct { ExternalStoreID string; Qty int32 }

  // 可选接口（adapter.go）
  // CatalogItemSource：按外部 ID 拉一件商品（回调只给了 ID）。found=false 表示渠道上已经没有了（删了）。
  type CatalogItemSource interface {
      PullItem(ctx context.Context, b Binding, externalID string) (item CatalogItem, found bool, err error)
  }
  // WebhookInstaller：把这个 binding 需要的回调订阅装到渠道上（幂等：已有同主题同地址的不重复建）。
  type WebhookInstaller interface {
      EnsureWebhooks(ctx context.Context, b Binding, callbackURL string) error
  }
  ```
- Produces（service）：`listingTarget` 加 `carriesPrice bool`；`unchanged()` 只在 `carriesPrice` 时比价格。
- Produces（channeltest）：`Adapter.SetCaps(func(*channel.Caps))`、`Adapter.LastPrice(store, sku int64) (int64, bool)`（只记 `PushPrice` 为真的那些）、`FailNext`（Task 1 已加）。

- [ ] **Step 1: 写失败测试** `TestChannelPriceFromPriceStoreOnly`：假渠道 `PricePerStore=false`；binding 映射门店 S1、S2，config `{"price_store_id": S1}`；SKU 在 S1 价 1000、S2 价 1200。断言：推完后 `LastPrice(S1, sku)=1000`、S2 那条 `PushPrice=false`；改 S2 价为 1300 → 重算后**不入**推送任务（`jobs` 里没有 `binding:S2:sku` 的待办）；改 S1 价为 1100 → `LastPrice` 变 1100。再一条：config 不给 `price_store_id` → 价格源是映射门店里 store_id 最小的那家。再一条：`PricePerStore=true` → 两家都 `PushPrice`。
- [ ] **Step 2: 跑测试确认失败**。
- [ ] **Step 3: 实现**
  - `computeTargets` 里按 binding 求价格源门店：`Caps.PricePerStore` 为真 → 每家都是；否则 `config.price_store_id`（若它不在这个 binding 的门店映射里，退回最小 store_id，并 Warn 一次）。适配器 Caps 从 `s.reg.Lookup(b.Channel)` 取；`OutletBinding` 若没带 Channel/Config，补进 `ListActiveOutletBindingsForStore` 的 SELECT（`db/queries/channels.sql` + `make generate-sql`）。
  - `pushStore` 组 `Listing` 时 `PushPrice = t.carriesPrice && (t.prev == nil || t.prev.PublishedCents != t.price || t.prev.LastError != nil)`。
  - 不推价格的格子回写 `channel_listings` 时 `PublishedCents` 记 `t.price`（只作记录，不参与比较）。
  - 渠道 `Caps` 的 `ListingBatch` 改由适配器自己分批（已是），这里不动。
  - `channeltest.Adapter` 实现 `LastPrice`、`SetCaps`。
- [ ] **Step 4: 跑测试通过**（含第一期全部渠道测试：`make test-db ARGS='-run Channel ./internal/handler/'` 或等价）。
- [ ] **Step 5: 提交** `渠道层：CatalogItem / 变体 / 库存水位类型，Caps.PricePerStore 与价格源门店（全渠道一个价时只从一家门店推价）`

### Task 3: Shopify 模拟平台 `shopifytest`

**Files:**
- Create: `internal/channel/shopify/shopifytest/server.go`、`internal/channel/shopify/shopifytest/server_test.go`

**Interfaces:**
- Produces:
  ```go
  type Server struct { URL string; … }           // httptest.Server 包装
  func New(t testing.TB, shop, clientID, clientSecret string) *Server
  func (s *Server) AddProduct(p Product) (productGID string)
  type Product struct { Title, Description, Status string; GiftCard bool; Images []string; Variants []Variant }
  type Variant struct { SKU string; Price string; Options map[string]string; Tracked bool; Levels map[string]int32 /* location gid → available */ }
  func (s *Server) AddLocation(name string) (gid string)
  func (s *Server) Available(inventoryItemGID, locationGID string) (int32, bool)
  func (s *Server) SetAvailable(inventoryItemGID, locationGID string, q int32) // 模拟店员在 Shopify 后台改数
  func (s *Server) Price(variantGID string) string
  func (s *Server) DeleteProduct(gid string)
  func (s *Server) ThrottleNext(n int)       // 之后 n 次 GraphQL 调用回 THROTTLED
  func (s *Server) ExpireTokens()            // 已发出的 token 全部失效（下一次调用 401）
  func (s *Server) RotateSecret(newSecret string) // 换 client secret：旧 secret 换 token 回 400 invalid_client
  func (s *Server) Webhooks() []WebhookSub   // 已装的订阅 {Topic, URI}
  func (s *Server) Calls(op string) int      // 按操作名（products / inventorySetQuantities / productVariantsBulkUpdate / tokens …）计数
  func (s *Server) SignWebhook(body []byte) string // 用当前 secret 算 X-Shopify-Hmac-Sha256
  ```
- 适配器 `shopify.Options.BaseURL` 指向 `Server.URL`（见 Task 4）。

- [ ] **Step 1: 写测试** `server_test.go`：换 token 成功 / 错 secret 400；GraphQL 无 token 401；`inventorySetQuantities` 三条里一条 `changeFromQuantity` 不对 → 回 `CHANGE_FROM_QUANTITY_STALE`（field 路径带下标）且**三条都不生效**；`changeFromQuantity:null` 生效；同一 `@idempotent` key 第二次调用不重复生效；`ThrottleNext(1)` 回 `{"errors":[{"message":"Throttled","extensions":{"code":"THROTTLED"}}],"extensions":{"cost":{…,"throttleStatus":{"maximumAvailable":4000,"currentlyAvailable":0,"restoreRate":200}}}}`。
- [ ] **Step 2: 实现**：不做 GraphQL 解析器——按请求体里的**操作名**分发（适配器的每条查询都写 `query Products(...)` / `mutation SetQty(...)` 这样带名字，sim 用正则取操作名），变量按固定形状解 JSON。支持的操作：`Products`（分页 `first/after`，`pageInfo{hasNextPage endCursor}`）、`Product`（按 id）、`Levels`（按 inventoryItem id 列各 location available，冲突回读用）、`SetQty`、`SetPrices`（`productVariantsBulkUpdate`）、`Webhooks`（列订阅）、`CreateWebhook`、`Locations`。响应形状照上面「已实测的 Shopify 事实」与 Task 5 / 6 的查询字段。每次响应带 `extensions.cost.throttleStatus`。
- [ ] **Step 3: 跑测试通过**：`go test ./internal/channel/shopify/shopifytest/`。
- [ ] **Step 4: 提交** `Shopify 模拟平台：token、商品分页、库存 CAS（整批原子 + 幂等键）、改价、限流、webhook 订阅`

### Task 4: Shopify 适配器——token、GraphQL 客户端、回调验签

**Files:**
- Create: `internal/channel/shopify/shopify.go`、`token.go`、`graphql.go`、`webhook.go`、对应 `_test.go`、`deps_test.go`

**Interfaces:**
- Consumes: `channel.Binding/Caps/Event/RetryableError/ErrCredentials/ErrBadSignature`。
- Produces:
  ```go
  const Kind = "shopify"
  const APIVersion = "2026-10"
  type Options struct {
      BaseURL    func(shop string) string // 默认 "https://"+shop；测试指向 shopifytest
      HTTPClient *http.Client             // 默认 15 秒超时
      Now        func() time.Time
  }
  func New(o Options) *Adapter
  func (a *Adapter) Kind() string  // "shopify"
  func (a *Adapter) Caps() channel.Caps
      // Roles: RoleCatalogSource|RoleOutlet; CatalogDirection: DirIn; Delivery: DeliveryExpress|DeliveryLocal;
      // OutOfOrderInbound: true; PricePerStore: false; ListingBatch: 250
  func (a *Adapter) ParseInbound(b channel.Binding, r *http.Request, body []byte) ([]channel.Event, []byte, error)
  // 内部：
  func (a *Adapter) gql(ctx context.Context, b channel.Binding, opName, query string, vars any, out any) error
  ```

- [ ] **Step 1: 写失败测试**（打 `shopifytest`）
  - token：两次调用只换一次 token（`Calls("tokens")==1`）；`Now` 拨到 expires_in×0.8 之后再调用 → 换第二次；`ExpireTokens()` 后调用 → 自动重换并成功（只重试一次）；`RotateSecret` 后 → 错误 `errors.Is(err, channel.ErrCredentials)`，且 `err.Error()` 不含旧 / 新 secret、不含 token。
  - 限流：`ThrottleNext(1)` → `errors.As(err, *RetryableError)` 且 `RateLimited`、`After > 0`（按 `(requestedQueryCost − currentlyAvailable)/restoreRate` 秒，下限 1 秒、上限 60 秒；拿不到 cost 时 2 秒）；HTTP 429 带 `Retry-After: 3` → After = 3s；5xx / 连接断 → `RetryableError{RateLimited:false}`。
  - 回调：正确 HMAC（`base64(hmac_sha256(client_secret, body))`，头 `X-Shopify-Hmac-Sha256`）+ `X-Shopify-Shop-Domain` 等于 `b.ExternalAccount` → 解出事件；HMAC 错 / 域名不符 / secrets 里没 client_secret → `ErrBadSignature`；`X-Shopify-Topic`：`products/create|update` → `EventCatalogChanged`，`ExternalItemIDs=[body.admin_graphql_api_id]`；`products/delete` → `EventCatalogChanged`，`ExternalItemIDs=["gid://shopify/Product/"+body.id]`；`inventory_levels/update` → `EventStockChanged`；`orders/*` → `EventOrderChanged`（`ExternalOrderID` = `admin_graphql_api_id`）；其他 → `EventIgnored`。`ExternalID` 取 `X-Shopify-Event-Id`，没有则 `X-Shopify-Webhook-Id`，都没有返回空（入口会丢弃并记日志）。ack 为 nil（入口回 200 空体）。
  - `deps_test.go`：`go list -deps ./internal/channel/shopify` 的结果里 `github.com/keel/keel/` 开头的只有 `internal/channel`（用 `golang.org/x/tools/go/packages` 不引新依赖——直接 `exec.Command("go","list","-deps",…)`）。
- [ ] **Step 2: 跑测试确认失败**：`go test ./internal/channel/shopify/`。
- [ ] **Step 3: 实现**
  - `token.go`：按 binding id 缓存 `{token, expiresAt}`（`sync.Mutex` + map）；`expiresAt = now + expires_in`，取用时若 `now > issued + 0.8×expires_in` 就重换；同一 binding 并发只换一次（锁内换）。换 token 回 400/401 → `fmt.Errorf("%w：Shopify 拒绝了 client credentials（HTTP %d）", channel.ErrCredentials, code)`——**不拼响应体**（可能回显 client_id 之外的东西也不要）；其余失败 → `RetryableError`。
  - `graphql.go`：请求体 `{"query", "variables", "operationName"}`；401 → 清缓存重换一次再发；仍 401 → `ErrCredentials`；429 → 限流；5xx → 可重试；200 但 `errors[].extensions.code == "THROTTLED"` → 限流；其他 `errors` → 普通错误（不可重试也照样由 jobs 计次重试），错误文本只带 `message`，截断 300 字符。解出 `data` 进 `out`。
  - `webhook.go`：`hmac.Equal` 比较；先验签再解 body。
  - `shopify.go`：secrets 解析 `{"client_id","client_secret"}`；店铺域名取 `b.ExternalAccount`，必须匹配 `^[a-z0-9][a-z0-9-]*\.myshopify\.com$`，否则 `ErrCredentials`（配错了不该无限重试）。`Outlet` 里本期不做的方法（`PushCatalog`、`Act`、`FetchOrder`、`ListOrders`、`ListListings`）返回 `channel.ErrUnsupported`（`ListOrders/ListListings` 用一个只 yield 一次 `ErrUnsupported` 的迭代器）。
- [ ] **Step 4: 跑测试通过**。
- [ ] **Step 5: 提交** `Shopify 适配器：client credentials token（24 小时，提前续）、GraphQL 客户端按 throttleStatus 退避、回调验签与主题规整`

### Task 5: Shopify 适配器——拉商品

**Files:**
- Create: `internal/channel/shopify/catalog.go`、`catalog_test.go`

**Interfaces:**
- Produces: `(*Adapter).PullCatalog(ctx, b, cursor) (channel.CatalogPage, error)`、`(*Adapter).PullItem(ctx, b, externalID) (channel.CatalogItem, bool, error)`；内部 `func toItem(p productNode) (channel.CatalogItem, bool)`（礼品卡返回 false）；`func parsePriceCents(s string) (int64, error)`。

- [ ] **Step 1: 写失败测试**
  - `parsePriceCents`：`"949.95"→94995`、`"10"→1000`、`"0.5"→50`、`"1.005"`→错误、`"-1"`→错误、`""`→错误。不走 float。
  - 打 sim：3 件商品（含一件礼品卡、一件 DRAFT、一件两规格 + 一个 `Tracked:false` 变体、一个 sku 为空的变体），`PullCatalog` 每页 2 件（测试里把页大小设成 2：`Options.PageSize`，默认 50）走完所有页 → 共 2 件（礼品卡被滤掉），DRAFT 的 `Status=CatalogDraft`；两规格的 `Options` 正确；单规格 `Default Title` 的 `Options` 为空 map；变体 `Extra` 解出 `{"inventory_item_id":…,"product_id":…,"tracked":false}`；`Levels` 列出各 location 的 available；`ImageURLs` 顺序与 sim 一致。
  - `PullItem`：存在 → found；`DeleteProduct` 后 → `found=false, err=nil`。
- [ ] **Step 2: 跑测试确认失败**。
- [ ] **Step 3: 实现**：查询（变体每件最多 100、location 最多 10，超出时只取前面并在 `Raw` 里不展开——联调发现不够再分页）：
  ```graphql
  query Products($first:Int!,$after:String){ products(first:$first, after:$after){
    pageInfo{hasNextPage endCursor}
    nodes{ id title descriptionHtml status isGiftCard
      media(first:20){nodes{ ... on MediaImage { image{url} } }}
      variants(first:100){nodes{ id sku price selectedOptions{name value} image{url}
        inventoryItem{ id tracked inventoryLevels(first:10){nodes{ location{id} quantities(names:["available"]){quantity} }} } }} } } }
  query Product($id:ID!){ product(id:$id){ …同上 nodes 内字段… } }
  ```
  `descriptionHtml` 原样作 Description（keel 详情就是富文本）。`NextCursor` 只在 `hasNextPage` 时给。
- [ ] **Step 4: 跑测试通过**。
- [ ] **Step 5: 提交** `Shopify 适配器：分页拉商品与按 ID 拉单件（滤礼品卡、规格 / 图片 / 各门店可售数、价格按十进制串精确换分）`

### Task 6: Shopify 适配器——推库存与价格、装 webhook

**Files:**
- Create: `internal/channel/shopify/listing.go`、`listing_test.go`、`webhooks_install.go`、`webhooks_install_test.go`

**Interfaces:**
- Produces: `(*Adapter).PushListings(ctx, b, ls) ([]channel.ListingResult, error)`、`(*Adapter).EnsureWebhooks(ctx, b, callbackURL) error`、`(*Adapter).Available(ctx, b, inventoryItemGID, locationGID string) (int32, bool, error)`（冲突回读用，导出给联调测试）。

- [ ] **Step 1: 写失败测试**（打 sim）
  - 3 条、PrevQty 全对 → 全成功，sim 上数对；`Calls("SetQty")==1`。
  - 3 条、第 2 条被 `SetAvailable` 改过 → 结果 1、3 `Err==nil`，2 `Conflict && *ObservedQty == 改后的值`；sim 上 1、3 已是新值（同一次 `PushListings` 里重提了剩下的）；`Calls("SetQty")==2`。
  - PrevQty 为 nil → 发 `changeFromQuantity:null`，成功。
  - `Tracked:false` 的条目 → 不发库存、直接成功（价格照推）。
  - 600 条 → 分 3 批（250/250/100）。
  - 幂等键：同一组 `IdemKey` 推两次 → 两次的 `@idempotent(key:)` 相同（sim 记录下来比对）；组成员不同 → 不同。key = `"keel-" + hex(sha256(排序后的 IdemKey 用 \n 连接))[:40]`。
  - 价格：`PushPrice` 为真的条目按 `extra.product_id` 分组，每个商品一次 `productVariantsBulkUpdate`，sim 上价格变成 `"12.34"`（分 → 两位小数串）；`PushPrice` 为假的不调。某变体改价 userErrors → 只有那条 `Err` 非空。
  - 限流：`ThrottleNext(1)` → 整批返回 `RetryableError{RateLimited:true}`（不是逐条）。
  - 凭据：`RotateSecret` → 整批返回 `ErrCredentials`。
  - `EnsureWebhooks`：首次装 `PRODUCTS_CREATE`、`PRODUCTS_UPDATE`、`PRODUCTS_DELETE`、`INVENTORY_LEVELS_UPDATE`、`APP_UNINSTALLED` 五个，uri 都是 callbackURL；再调一次不重复建（`len(Webhooks())==5`）；已有同主题但 uri 不同的 → 新建一个指向新地址的（旧的不删，交给人）。
- [ ] **Step 2: 跑测试确认失败**。
- [ ] **Step 3: 实现**
  - 分批：按 250 切；每批 `mutation SetQty($in:InventorySetQuantitiesInput!,$k:String!){ inventorySetQuantities(input:$in) @idempotent(key:$k){ inventoryAdjustmentGroup{id} userErrors{field message code} } }`，`name:"available"`、`reason:"correction"`、`referenceDocumentUri:"keel://channel/<binding_id>"`。
  - userErrors：`code == "CHANGE_FROM_QUANTITY_STALE"` 且 `field[2]` 是下标 → 记为冲突；其他带下标的 → 那条 `Err`；不带下标的 → 整批 `Err`。有冲突时：对冲突条目 `Available` 回读（`query Levels($id:ID!){ inventoryItem(id:$id){ inventoryLevels(first:10){nodes{location{id} quantities(names:["available"]){quantity}}} } }`），把非冲突、非出错的条目**重提一次**（新组成员 → 新幂等键）；重提又冲突的同样回读、标冲突，不再循环。
  - 价格：`mutation SetPrices($pid:ID!,$vs:[ProductVariantsBulkInput!]!){ productVariantsBulkUpdate(productId:$pid, variants:$vs){ userErrors{field message} } }`；`field` 形如 `["variants","<i>","price"]` 定位到条目。
  - 一条的最终 `Err` = 库存错误 ∪ 价格错误（`errors.Join`）。
  - `EnsureWebhooks`：先 `query Webhooks{ webhookSubscriptions(first:100){ nodes{ topic uri } } }`，缺哪个建哪个：`mutation CreateWebhook($t:WebhookSubscriptionTopic!,$u:String!){ webhookSubscriptionCreate(topic:$t, webhookSubscription:{uri:$u}){ userErrors{field message} } }`。
- [ ] **Step 4: 跑测试通过**。
- [ ] **Step 5: 提交** `Shopify 适配器：库存 CAS 推送（冲突单拎出回读、其余同次重提、按组成员定幂等键）、按商品批量改价、幂等装 webhook`

### Task 7: core 商品同步（`channel.catalog.pull` + 商品回调）

**Files:**
- Create: `internal/service/channel_catalog.go`、`internal/handler/channel_catalog_test.go`（testdb，打 sim + 真实适配器）
- Modify: `internal/service/channel.go`（启用时入队首拉；`QueueChannelCatalogPull` 常量；`IsActiveCatalogSource`）、`internal/service/channel_worker.go`（`WorkOnce` / `housekeep` 加新队列）、`internal/repository/channel.go` + `db/queries/channels.sql`（按外部 ID 反查 link、按 product 列 SKU link、按货号找 SKU 连带 product_id）、`internal/repository/admin_sku.go` 或 `product_import.go`（若缺「按货号取 SKU + product_id」）、`tenant_context_test.go`（仅当新文件手建了租户上下文）

**Interfaces:**
- Consumes: `channel.CatalogSource`、`channel.CatalogItemSource`、`channel.WebhookInstaller`、`channel.CatalogItem/CatalogVariant/StockLevel`；`tx.CreateProduct`、`tx.UpdateProduct`、`tx.CreateSKU`、`tx.UpdateSKU`、`tx.SetProductPublication`（下架）、`tx.RecordChannelListing`、`tx.ListChannelStoreLinks`、`s.inv.InitSKUs`、`seedProductStockFlags`、`s.RecomputeListings`。
- Produces:
  ```go
  const QueueChannelCatalogPull = "channel.catalog.pull"
  type channelCatalogJob struct { BindingID int64 `json:"binding_id"`; Cursor string `json:"cursor,omitempty"`; First bool `json:"first,omitempty"` }
  type channelBindingConfig struct { DefaultCategoryID int64 `json:"default_category_id"`; PriceStoreID int64 `json:"price_store_id"`; WebhookBaseURL string `json:"webhook_base_url"` }
  func (s *ChannelService) syncCatalogItem(ctx context.Context, b repository.ChannelBinding, ab channel.Binding, item channel.CatalogItem) error
  func (s *ChannelService) catalogChanged(ctx context.Context, b repository.ChannelBinding, ev channel.Event) error // OnInbound(EventCatalogChanged)
  var ErrChannelNoDefaultCategory = errors.New("binding 没配 default_category_id，商品拉不进来")
  ```
  `NewChannelService` 末尾 `s.OnInbound(channel.EventCatalogChanged, s.catalogChanged)`。

- [ ] **Step 1: 写失败测试**（`channel_catalog_test.go`，sim + `shopify.New(Options{BaseURL: sim})` 登记进 rig 的注册表；rig 照 `newChannelRig` 改一个能注入注册表的版本）
  - **首接不清零**（Review Focus 1）：sim 一件两规格商品，变体 A 在 location L1 available=7，变体 B 在 L1=2；binding 映射 keel 默认门店 ↔ L1、`default_category_id` 有效；启用 → `Drain` → keel 多 1 件商品（status 草稿 0）、2 个 SKU，keel 库存 7 / 2；`channel_item_links` product 1 条、sku 2 条（extra 含 inventory_item_id）；`channel_listings` 基线 7 / 2；sim 上仍是 7 / 2，`Calls("SetQty")==0`。
  - **认领已有 SKU**（Review Focus 2）：keel 先建商品 P（货号 A，库存 3）；sim 同货号 A available=9 → 拉完 keel 商品数不变、A 链到 P、keel 库存仍 3、`Drain` 后 sim 上 A 变 3。
  - **回调更新**：sim 改标题、加一个变体 C（available 4）→ 发签名过的 `products/update` 回调到入口（`testChannels` 路由或直接 `svc.Inbound`）→ `Drain` → keel 标题更新、多一个 SKU C、库存 4。
  - **变体删除**：sim 删掉变体 B → `products/update` → keel 的 SKU B 停售（status 0），link 保留。
  - **商品删除**：`products/delete` → keel 商品下架，links 保留。
  - **没配类目**：config 无 `default_category_id` → 任务失败，jobs.last_error 含「default_category_id」，keel 无新商品。
  - **货号为空**：变体 sku 为 null → keel 货号 `shopify-<变体数字 id>`。
  - **装 webhook**：config 有 `webhook_base_url=https://x.test` → 首拉之后 sim 上 5 个订阅，uri = `https://x.test/api/v1/webhooks/channels/<id>`；没配就不装、不报错。
  - **重复首拉幂等**：停用再启用 → 再拉一遍，商品 / SKU / link 数不变，库存不被 Shopify 的数覆盖（此时 Shopify 上的已经是 keel 推出去的）。
- [ ] **Step 2: 跑测试确认失败**。
- [ ] **Step 3: 实现**
  - 触发：`ChannelBinding` 加 `IsActiveCatalogSource()`（启用中 + roles 含 1）；`CreateBinding` / `UpdateBinding` 里它从假翻到真时，同一事务 `EnqueueJob(QueueChannelCatalogPull, job_key="pull:<id>:", payload {binding_id, First:true})`。
  - `workCatalog`（worker 文件里出队、建租户上下文，调 `s.pullCatalogPage(ctx, j)`）：取 binding（不在启用中 → 标完成）；`First` 且适配器是 `WebhookInstaller` 且配了 `webhook_base_url` → `EnsureWebhooks`（失败只 Warn，不挡拉商品）；`PullCatalog(cursor)`；逐件 `syncCatalogItem`；有 `NextCursor` → 入队下一页（job_key `pull:<id>:<sha1(cursor)前 16 位>`）；标完成。任何错误 `retry`（脱敏）。
  - `syncCatalogItem`（一件商品一个事务，事务外再做库存初始化与重算）：
    1. 解 config，`DefaultCategoryID == 0` → `ErrChannelNoDefaultCategory`。
    2. 找 product link（外部 ID）→ keel 商品 id。没有：按变体货号（空货号用 `shopify-<id 数字段>`）找 keel 已有、且**没链到这个 binding 别的变体**的 SKU；有则取第一个的 product_id 作这件商品（认领），否则 `CreateProduct{CategoryID, Title, Description}`（草稿）。写 product link（extra `{}`）。
    3. 有 link 的商品：`UpdateProduct` 改 Title / Description（与当前不同才改）。`Status == CatalogArchived` → 下架（`SetProductPublication(false)`；已下架则不动）。新建时不上架，上架是 keel 的决定。
    4. 逐变体：有 sku link → `UpdateSKU` 改规格（不同才改）、更新 link extra；没有 → 认领同货号且属于这件 keel 商品的 SKU，或 `CreateSKU{ProductID, SKUCode, SpecValues: Options, PriceCents, Status:1}`；同货号但属于别的商品 → 跳过这个变体并 `Warn`（不让整件失败）；写 sku link（extra = 变体 Extra）。
    5. 本次新建的 SKU：对这个 binding 的每条门店映射，若变体有那个 location 的 Level → 记 `inventory.InitRow{SKUID, StoreID, Available: Level.Qty}`。
    6. 本次新链上的 SKU（新建或认领）：对每条门店映射有 Level 的 → `RecordChannelListing{PublishedQty: Level.Qty, PublishedCents: 变体价}`（基线）。
    7. 以前链着、这次变体里没有的 sku link → `UpdateSKU(status 0)`。
    8. 提交后：`s.inv.InitSKUs(rows)`（已有库存行的会被库存服务跳过）、`seedProductStockFlags(ctx, s.repo, s.inv, []int64{productID})`，再对每条门店映射 `RecomputeListings(store, 本次链上的 sku, binding)`——认领的 SKU 于是把 keel 的数推出去覆盖。
  - `catalogChanged`：适配器须是 `CatalogItemSource`，否则返回 `ErrUnsupported`（不重试——直接 `nil` 并 Warn，免得死循环；写清理由）；对每个 `ExternalItemIDs`：`PullItem` → found 则 `syncCatalogItem`；not found → 按 link 找 keel 商品下架。
- [ ] **Step 4: 跑测试通过**；再跑第一期全部渠道测试与 `TestEveryStateTransitionNotifiesOrSaysWhyNot`、`tenant_context_test.go`、契约测试（本期不加路由，应不受影响）。
- [ ] **Step 5: 提交** `渠道层：Shopify 商品进 keel（首拉 + 回调；认领同货号 SKU、新 SKU 以平台现值作初始库存与推送基线、变体删除停售、商品删除下架、首拉时装 webhook）`

### Task 8: 商品图片

**Files:**
- Create: `internal/service/channel_images.go`、测试并入 `internal/handler/channel_catalog_test.go`
- Modify: `internal/service/channel.go`（`WithUploadStore(UploadStore)`）、`internal/app/channels.go`（装配时把现有 upload store 传进来）

**Interfaces:**
- Produces: `func (s *ChannelService) WithUploadStore(st UploadStore, client *http.Client, allowHost func(string) bool) *ChannelService`；`func (s *ChannelService) syncImages(ctx context.Context, productID int64, urls []string, linkExtra json.RawMessage) (json.RawMessage, error)`（返回新的 product link extra，含 `images_sha`）。

- [ ] **Step 1: 写失败测试**：sim 商品两张图（sim 同时起一个图片 handler 回 PNG 字节，`allowHost` 测试里放行 127.0.0.1）→ 拉完 keel 商品有 2 张 `product_images`，顺序一致，`uploads.purpose = 1`、`referenced = true`；再拉一次（图没变）→ 不再下载（图片 handler 计数不变）；sim 换一张图 → 重新下载并替换；图片 URL 主机不在白名单 / 回 404 / 非 image 类型 / 超过 `MaxUploadBytes` → 商品照样同步成功，图片保持原样，日志 Warn。没配 upload store → 跳过图片。
- [ ] **Step 2: 跑测试确认失败**。
- [ ] **Step 3: 实现**：`images_sha = sha256(urls 用 \n 连接)`，与 product link extra 里的相同就跳过。下载：只 `https`（测试放行 http://127.0.0.1）、主机 `allowHost`（生产：`cdn.shopify.com`）、`io.LimitReader(MaxUploadBytes+1)`、按响应 `Content-Type` 过 `uploadExtensionFor`。落盘 `store.Put` → 事务里登记 upload（复用 `AdminCatalogService.CreateUpload` 用的那条仓储写法，`staff_id` 为空、purpose 1）→ `ReplaceProductImages`。任何一张失败：整组放弃（删掉已落盘的，`discardStoredUpload`），返回错误由调用方 Warn，不让商品同步失败。图片在 `syncCatalogItem` 第 8 步之后做（事务外，下载不攥连接）。
- [ ] **Step 4: 跑测试通过**。
- [ ] **Step 5: 提交** `渠道层：Shopify 商品图下载进 uploads（主机白名单、限大小、图没变不重下、失败不挡商品同步）`

### Task 9: 装配、开发店联调、文档

**Files:**
- Modify: `internal/app/channels.go`（`channelRegistry` 登记 `shopify.New(shopify.Options{})`）、`internal/app/channels_test.go`
- Create: `internal/channel/shopify/live_test.go`、`internal/handler/channel_shopify_live_test.go`
- Modify: `docs/superpowers/specs/2026-10-02-channel-adapter-design.md`（§13 改成已实测、§15 第 5 步给出回调路径）、`CHANGELOG.md`、`PROGRESS.md`、`docs/电商系统-数据模型设计.md`（若 `channel_item_links.extra` / `channel_bindings.config` 的约定字段要记）

- [ ] **Step 1: 装配测试**：`KEEL_CHANNELS=on` 时 `/admin/channel-kinds` 列出 `shopify`，Caps 与 Task 4 一致；关着时注册表不建（第一期不变量测试照旧通过）。
- [ ] **Step 2: 适配器联调测试**（`live_test.go`，`KEEL_SHOPIFY_LIVE=1` 才跑，从 `~/.config/keel/shopify-dev` 读 `SHOPIFY_SHOP/CLIENT_ID/CLIENT_SECRET`，不打印任何凭据）：换 token；`PullCatalog` 走完全部页，断言礼品卡被滤掉、至少一件商品有 Levels；挑一个 tracked 变体：`PushListings` 用 `PrevQty = 现值+7` → `Conflict` 且 `ObservedQty == 现值`、Shopify 上的数不变；`PrevQty = 现值`、`Qty = 现值` → 成功（空操作）。**不改开发店的任何数据**。
- [ ] **Step 3: 全链路联调**（`channel_shopify_live_test.go`，同一开关，testdb）：建 binding（真实开发店、映射 keel 默认门店 ↔ `Shop location`、`default_category_id`）→ 启用 → 拉商品 → 断言 keel 里的 SKU 库存等于 Shopify 上的数；挑一个 SKU 把 keel 库存 +1 → `Drain` → 回读 Shopify 上是现值 +1 → 再 −1 还原 → 回读还原。测试结束删 binding（links 级联）。图片不在联调里测（不装 upload store）。
- [ ] **Step 4: 跑**：`go test ./internal/channel/...`；`make test-db`（全量，只看失败项）；`make check-all` 或 Makefile 里等价的总闸（看 `make help`）；`KEEL_SHOPIFY_LIVE=1` 跑两条联调测试并把结果写进 PROGRESS。
- [ ] **Step 5: 文档**：spec §13 Shopify 一条改为「已实测」并列上面的事实；§15 第 5 步：回调地址 `https://<演示站域名>/api/v1/webhooks/channels/<binding_id>`，在 binding config 里配 `webhook_base_url` 后首拉时自动装；CHANGELOG 加第二期条目；PROGRESS 更新渠道层那一条（已做 / 没做：后台「由 Shopify 管理」字段锁定与界面、`inventory_levels/update` 只留档不处理、`app/uninstalled` 未处理（应标凭据失效）、币种不换算、订单在第三期——**第三期之前 Shopify 上卖出的件 keel 不知道，会被 keel 的数推回去，联调时不要在 Shopify 下单**）。
- [ ] **Step 6: 提交** `渠道层第二期收尾：登记 Shopify 适配器、开发店联调测试（只读 + 可还原）、spec 改实测结论、CHANGELOG 与 PROGRESS`

---

## 不在本期

- 后台界面与「由 Shopify 管理」的字段锁定（后台改了标题，下次同步会被 Shopify 的覆盖——写进 PROGRESS 已知限制）。
- 定时全量拉商品（只有启用时首拉 + 回调；回调丢了靠停用再启用补，第五期对账再做定时）。
- `inventory_levels/update`（店员在 Shopify 改数）：只留档；CAS 冲突时会被 keel 覆盖，差异记在 `last_error`。
- 订单、履约（第三期）。

## 执行中偏离计划的地方（2026-10-02）

- **商品源 binding 推送不看 keel 的上架状态**（Task 2 补）：`ChannelSKUOffers` 原先把「商品已上架」算进能不能卖，而拉进来的新商品在 keel 是草稿 —— 第一次推送就会把 Shopify 现货推成 0。改为返回三个因素（SKU 启用 / 商品没删 / 已上架），`SKUOffer.Sellable(catalogOwned)` 对商品源 binding 不看上架。keel 的上架仍由 keel 决定（上架要过广告法检查，不能被同步绕过）；Shopify 归档 / 删除 → keel 下架。
- **改价 / 停售 / 上下架触发重算**（Task 2 补）：第一期只有改渠道规则会重算，keel 里改门店价、大区价、基准价、SKU 停售、商品上下架或删除都不会。加 `ChannelService.SKUsChanged / ProductChanged`，`AdminStoreService` / `AdminCatalogService` 经 `WithChannels` 接上（KEEL_CHANNELS 关着时为 nil，nil 接收者直接返回）。启用中的 binding 改 config 也整店重算（价格源门店可能变了）。
- **各门店可售数单独查**（Task 5 补）：实测 products 带 inventoryLevels 一页 10 件就 710 点（单条上限 1000），改为 products 不带水位（25 件 308 点）+ `nodes(ids:)` 每 50 个 item 一条（15 点）。默认页大小 25。
- **迁移 00302**（Task 8 补）：`uploads.chk_upload_owner` 只允许员工 / 买家二选一，系统下载的图没有上传者。加 `channel_binding_id`（复合外键），约束改为 `num_nonnulls(...) = 1`；数据模型文档同步。
- 模拟平台没有单独的 `server_test.go`：它被适配器测试全覆盖（每个操作都有用例）。
- 改价同组有一条出错时按整组没生效处理（同库存），其余条目记「同批出错」重推。
