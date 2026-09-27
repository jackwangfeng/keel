# Flutter 买家端第一阶段（第 2–5 步）Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: superpowers:executing-plans. Steps use checkbox (`- [ ]`) syntax.

**Goal:** 把 uni-app x 买家端（`app/`，已冻结）第一阶段范围内的功能移植到 `flutter_app/`：浏览（分类 / 搜索含回传 / 详情 / 「＋」加购）、购物车、地址簿、结算下单、沙箱支付、订单列表 / 详情；e2e 移植齐；小程序能编、能在开发者工具里走主链路。

**Architecture:** 与第 1 步相同：契约字段只在 `lib/api/`（按领域分文件：`catalog.dart`、`cart.dart`、`address.dart`、`order.dart`），页面只拿行模型；全局状态三个 `ChangeNotifier`（会话、门店、购物车件数）；新页面按 uni-app x 对应页面逐条移植行为与文案（行为的出处就是 `app/src/pages/**` 与 `app/src/api/view.uts`，不再在本计划里重抄一遍代码）。

**Tech Stack:** Flutter 3.41.9、go_router 17、http、shared_preferences；integration_test + chromedriver（Web 无头）。

**Spec:** `docs/superpowers/specs/2026-09-27-flutter-buyer-app-design.md`（§4 页面与错误处理规矩、§5 测试与交付顺序）

## 计划的写法（与第 1 步不同）

第 1 步的计划把每行代码都写出来，因为那一步定下了结构。第 2–5 步是**同一结构下的逐页移植**，行为的权威出处是 uni-app x 的源码（已经过真机与 e2e 验证）。本计划每个任务给：要移植的源文件、产出的 Dart 接口、必须有的测试。执行者就是写计划的人，重抄代码只会让两份不一致。

## Global Constraints

- 页面（`lib/pages/`、`lib/widgets/`）不许 import `schema.g.dart`（`scripts/check_flutter_pages.py`）。
- 金额只显示服务端算好的数，客户端不复算。
- 写操作按契约带 `Idempotency-Key`；同一次操作重试复用，内容变了换键。
- 依赖只用 mp-flutter 能接管的（`http`、`shared_preferences`）；不用原生插件。
- 登录页一律 `push`，成功 `pop`（见第 1 步 Task 8 的 ruling）。
- e2e 用 e2e 专用买家（`~/.config/keel/e2e.env`），不碰演示买家与公开访客的数据；后台前置状态请服务端会话代做。
- 每步验收：`make flutter-analyze flutter-test`、`scripts/check-all.sh`、Web 无头 e2e、`make flutter-build-mp`。

## Review Focus

1. 连点两个分类 / 连搜两次：只认最后一次请求的结果（旧结果晚到不能覆盖）。
2. 搜索回传：`trace_id` 缺席时整批不报；只有从搜索点进去 / 在结果里原地加购的才归因。
3. 购物车改数量超库存：显示服务端原因，数量回到服务端的值。
4. 结算：试算后自动选最省的券只发生一次，之后以用户选择为准（含「不使用」）；`price-changed` / `promotion-sold-out` 要用户重新确认。
5. 下单重放（`Idempotency-Replayed`）不跳转、写明「之前已提交过」；`in-flight` 按 `Retry-After` 退避。

---

## 第 2 步：浏览

### Task 1: 目录层 `lib/api/catalog.dart` + 购物车件数 + 搜索归因

- 移植：`view.uts` 的 `productRow`（`priceText` 改为完整区间，另加 `minPriceText`）、`searchHitRow`、`skuRow`、`productDetailView`（有货在前的稳定分区）、`categoryChips`、`productListQuery`、`searchRequest`、`searchTraceId`；`search-trace.uts` 整个（最近 50 条、最近一次为准）；`badge.uts` 的 `cartQuantity`。
- 产出：`fetchProducts(c, storeId:, categoryId:)`、`fetchCategories(c)`（失败返回空表）、`fetchProduct(c, id, storeId)`、`searchProducts(c, q, storeId) -> SearchResult(rows, traceId)`、`addToCart(c, skuId, qty, storeId) -> int 件数`（问题类型翻成「当前门店不卖这个规格」「购物车里这个规格已经到上限了」「库存不够了」）；`class CartCount extends ChangeNotifier { int n; refresh(); set(int) }`；`class SearchTrace { clicked / converted / addedFromList }`；`Services` 加 `cart`、`trace`。
- 测试（`test/catalog_test.dart`，假 HTTP）：区间价与「起」、缺货标记只在 `in_stock == false`、特价划线、有货在前的稳定分区、规格名 / 值拼接、`trace_id` 缺席不回传、从列表加购归因后下单回传同一 trace、归因表上限 50、加购的三种问题文案。

### Task 2: 首页分类 + 搜索入口 + 「＋」加购浮层 + 详情页 + 搜索页

- 移植：`pages/products/list.uvue`（分类 chip、连点只认最后一次、选中分类被删回到全部）、`components/quick-cart`（单规格直接加、多规格底部浮层、浮层里显示错误）、`pages/products/detail.uvue`（默认选第一个有货规格、无货规格点了提示、加购后「已加入购物车，去结算 ›」、底栏购物车角标）、`pages/search/index.uvue`（最后一次为准、空结果文案、缺货压暗、点击回传）。
- 路由：`/search`、`/product/:id` 压在外壳上（push）。
- Key：`home.search`、`home.cat.<id|all>`、`product.add.<id>`、`quick.sku.<id>`、`quick.plus`、`quick.confirm`、`detail.sku.<id>`、`detail.add`、`detail.buy`、`detail.added`、`detail.cart`、`search.input`、`search.submit`、`search.row.<id>`、`search.count`。
- 测试（`test/browse_pages_test.dart`）：分类切换请求带 `category_id`；多规格点「＋」弹浮层、单规格直接加并更新件数；详情无货规格不可选；搜索点击发 click 回传（假 HTTP 记录 `/search/events`）。
- e2e：移植 `app/e2e/category.test.js`、`search.test.js`、`searchtrace.test.js`（能在无头 Web 上观察到的部分）、`quickcart.test.js`。

## 第 3 步：购物车、地址簿

### Task 3: `lib/api/cart.dart` + 购物车 tab

- 移植：`view.uts` 的 `cartRow`、`cartStatusText`、`cartView`、`checkoutLines`、各 cart 请求；`pages/cart/index.uvue`（勾选 / 全选、改数量超库存显示原因、不能买的行写原因且不能勾、合计用 `selected_total_cents`、预估运费没地址不显示、凑单提示原样、批量删除带幂等键、去结算）。
- 底部 tab 变 4 个：首页 / 购物车 / 订单 / 我的；购物车 tab 角标跟 `CartCount`。
- 测试：`test/cart_test.dart`（行模型、不可买行不能勾选、改数量 409 回到服务端数量）、页面测试。
- e2e：移植 `app/e2e/cart.test.js`。

### Task 4: `lib/api/address.dart` + 地址簿

- 移植：`addressRow`、`addressForm`、`addressFormField`、`addressRequest`、`fieldErrors`；`pages/address/list.uvue`、`pages/address/edit.uvue`（新建幂等键；PUT 不切默认、切默认走专用接口；422 按 `errors[].field` 标红；删除点两下）。
- 测试：422 字段映射、默认排第一、编辑不带 is_default。
- e2e：移植 `app/e2e/address.test.js`（临时收件人「e2e 临时收件人」，删除只删自己建的）。

## 第 4 步：结算、下单、支付、订单

### Task 5: `lib/api/order.dart`（试算 / 下单 / 支付 / 订单读写）

- 移植：`previewView`、`orderRequest`、`couponOption`、`freightNote`、`undeliverableLines`、`promotionNotes`、`orderRow`、`orderItemRow`、`paymentRow`、`orderDetailView`、`orderStatusText/Tone`、`refundStatusText`；`client.uts` 的 `previewOrder`、`createOrder`（`in-flight` 按 `Retry-After` 退避，`reused` 抛给页面）、`listOrders`、`getOrder`、`createPayment`、`sandboxPayloadOf`、`settleSandbox`、`cancelOrder`、`confirmOrder`。
- 测试：金额拆行、券自动选最省只一次、各问题类型的映射、in-flight 退避重试、重放标记。

### Task 6: 结算页、订单列表、订单详情

- 移植：`pages/order/create.uvue`、`pages/order/list.uvue`、`pages/order/detail.uvue`（沙箱支付、取消、确认收货点两下）；我的页加订单 / 地址入口。
- e2e：移植 `checkout.test.js`、`freight.test.js`、`promotion.test.js`、`coupon.test.js` 里结算用券的部分、release 以外的下单付款、取消。

## 第 5 步：收尾

### Task 7: e2e 齐、小程序开发者工具走主链路、README、推送

- 在开发者工具里（先跟 mp-flutter 会话打招呼）走「浏览 → 加购 → 结算 → 下单 → 沙箱支付」；问题发给 mp-flutter 会话。
- README 补页面清单与 e2e 覆盖；`PROGRESS.md` 记一笔。
