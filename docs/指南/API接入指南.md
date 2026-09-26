# API 接入指南

写客户端、对接 ERP 或者自己写脚本时看这篇。它讲的是**所有接口共用的约定**；
每个接口的字段细节以契约为准：[`docs/电商系统-OpenAPI.yaml`](../电商系统-OpenAPI.yaml)（OpenAPI 3.1）。

> 契约是这个项目**唯一的真相源**：服务端的 Go 代码、后台界面的 TypeScript 类型、
> 买家端 App 的 UTS 类型，全部从它生成。契约改了而实现没跟上，CI 会红。

---

## 地址

| 部署形态 | 基础地址 |
|---|---|
| 单商家（本地默认） | `http://localhost:8080/api/v1` |
| 多商家 | `https://<商家code>.<平台域名>/api/v1`，或商家绑定的自有域名 |

**租户（哪一家店）由部署和域名决定，请求里不带任何标识租户的参数。**
唯一的例外是后台的平台级会话，见下文 `X-Keel-Merchant`。

运维用的两个路径不在 `/api/v1` 下：`GET /healthz`（健康检查）、`GET /version`（版本信息）。

---

## 数据约定

- **金额一律是整数，单位「分」**，字段名以 `_cents` 结尾。`12900` 就是 129.00 元。
  永远不要用浮点数处理金额。
- **时间一律是带时区的 RFC 3339 字符串（UTC）**，字段名以 `_at` 结尾。
- **订单、支付、退款用业务编号**（`order_no` / `payment_no` / `refund_no`），其余对象用整数 `id`。
  编号不可枚举，但越权防护不靠它：服务端按当前用户强制过滤，别人的订单一律 404。
- **没有 `{code, message, data}` 信封**。成功就是 2xx 加资源本身，失败看状态码和错误体。
- **分页**：列表接口用 `page`（从 1 开始）和 `page_size`，响应里带 `page`、`page_size`、`total`、`items`。

---

## 鉴权

### 买家

```bash
curl -s $B/auth/login -H 'Content-Type: application/json' \
     -d '{"phone":"13800000000","password":"keel-demo-2026"}'
```

响应里有 `access_token`、`refresh_token`、`expires_in`（秒）和 `user`。之后的请求带上：

```
Authorization: Bearer <access_token>
```

- 过期前用 `POST /auth/refresh` 换新的，`POST /auth/logout` 注销。
- 目前**只有手机号 + 密码**这一种登录方式可用。契约里的短信验证码登录、微信登录需要接短信服务、
  微信开放平台，本项目还没接，服务端会回 **501**（明确表示「这条路没开」，而不是 401「验证码错了」）。

### 后台员工

后台会话的获取方式见 [后台使用手册 · 登录](./后台使用手册.md#登录)。拿到的会话 token 同样放在
`Authorization: Bearer` 头里，7 天有效。后台接口都在 `/admin/` 前缀下。

### 平台级会话切换商家：`X-Keel-Merchant`

平台级员工管理某一家店时，在请求里带上这家店的 code：

```
X-Keel-Merchant: shop1
```

- 只有**平台级会话**可以带。商家级员工带了会被拒绝（403），不会被悄悄忽略。
- code 不存在返回 422。不会猜，也不会回落到某个默认店。
- 买家接口、公开接口一律不读这个头。

---

## 写操作幂等：`Idempotency-Key`

下单、发起支付、后台的创建类操作等写接口要求带 `Idempotency-Key` 头，值是客户端生成的 UUID。
网络超时后**用同一个 key 重试**，服务端保证只执行一次。

| 情况 | 服务端的行为 |
|---|---|
| 同一个 key 已经成功过 | 返回第一次的响应（状态码和响应体都一样），并带 `Idempotency-Replayed: true` |
| 同一个 key 正在处理中 | `409` + `Retry-After`，退避后重试即可，**不要当成业务失败** |
| 同一个 key 但请求体不同 | `422`，类型是 `idempotency-key-reused`。宁可显式失败，也不把不同的请求当成重放吞掉 |
| 第一次执行就失败了 | 重放时返回同一个失败；确实要重试，换一个新 key |

key 的有效期是 24 小时，作用域是「接口 + 用户 + key」。哪些接口要求带它，看契约里对应操作的参数。

---

## 错误

错误体遵循 [RFC 9457 Problem Details](https://www.rfc-editor.org/rfc/rfc9457)，`Content-Type: application/problem+json`：

```json
{
  "type": "https://keel.dev/problems/insufficient-stock",
  "title": "库存不足",
  "status": 409,
  "detail": "……",
  "errors": [{ "field": "items[0].quantity", "message": "……" }]
}
```

- **程序里按 `type` 判断错误种类**，不要按 `title` 或 `detail` 的文字判断，那些是给人看的。
- `errors[]` 是字段级的校验错误。
- 同一个状态码可能对应好几种 `type`，契约里每个操作的响应描述都逐条列出来了。

几个常见的：

| 状态码 | 常见 `type` | 含义 |
|---|---|---|
| 401 | — | 没登录或会话过期 |
| 403 | `role-forbidden` / `out-of-scope` | 后台：角色不允许 / 超出管辖范围 |
| 404 | `not-found` | 不存在、不属于你，或者**接口本身还没实现** |
| 409 | `idempotency-key-in-flight` | 同一个幂等 key 正在处理 |
| 422 | `idempotency-key-reused` / `compliance-rejected` | 幂等 key 被复用 / 商品文案命中违禁词 |
| 422 | `region-not-deliverable` | 试算 / 下单时有商品送不到这个收货地址，`undeliverable_items` 逐行给出原因 |
| 429 | — | 触发限流（目前只有 `/search` 按 IP 限流） |
| 501 | — | 这条路依赖的外部服务没接（短信、微信、邮件），明确告诉你没开 |

---

## 下单与支付

一次完整的购买是这几步：

1. `GET /stores/resolve?lat=&lng=` —— 按买家位置解析服务门店。不带坐标时回落到默认门店。
   门店决定了价格和库存。
2. `POST /orders/preview` —— 试算：价格、运费、可用的券、优惠分摊。不落库，可以反复调。
   运费按请求里的 `address_id` 算（见下面「运费」）。
3. `POST /orders` —— 下单，**必须带 `Idempotency-Key`**。扣库存、锁券、建订单三步由分布式事务
   协调器（dtmrs 的 SAGA）编排，任何一步失败，已经执行的步骤都会被补偿回去。
4. `POST /orders/{order_no}/payments` —— 发起支付，返回渠道需要的支付参数。
5. 渠道回调 `POST /webhooks/payments/{channel}` 到账后，订单变成已支付，券被核销。
   超时未支付的订单会被自动关闭，库存和券退回。

`scripts/smoke.sh` 把这几步完整跑了一遍（用沙箱支付），可以直接照着读。

### 之后的流程

| 买家 | 接口 |
|---|---|
| 取消未支付的订单（库存和券退回） | `POST /orders/{order_no}/cancel` |
| 确认收货 | `POST /orders/{order_no}/confirm` |
| 申请售后（不传金额，按行和件数） | `POST /orders/{order_no}/refunds` |
| 查看、撤回售后 | `GET /refunds`、`GET /refunds/{refund_no}`、`POST /refunds/{refund_no}/cancel` |
| 上传退款凭证 / 头像 | `POST /uploads`（multipart：`purpose` = `3` 凭证 / `2` 头像，`file`） |
| 退货退款：填寄回的物流 | `POST /refunds/{refund_no}/return-shipment`（`carrier_code`、`tracking_no`） |

写操作都要带 `Idempotency-Key`。订单详情里每一行有 `refunded_qty` 和 `refunding_qty`，
还可以退的件数 = `quantity − refunded_qty − refunding_qty`。

**售后凭证**：先 `POST /uploads`（`purpose=3`，单文件 ≤ 10 MB，只收 jpeg / png / webp）拿到
`url`（形如 `/api/v1/uploads/{id}`），再把它**原样**放进申请售后的 `evidence_urls`。服务端只收
你自己用 `purpose=3` 传的地址，外链、别人的文件、商品图都回 422。凭证不公开：
`GET /uploads/{id}` 读凭证要带**上传者本人**的 `Authorization: Bearer`，否则 403
（商品图、头像不用带令牌）。小程序的 `<image>` 带不了请求头，先用 `uni.downloadFile`
（带 `header`）取到文件再显示。

**寄回物流**：退货退款审核通过后退款单是 `20 待买家退货`，买家寄出后调
`return-shipment` 填物流公司代码（`sf` / `jd` / `yto`……）与运单号。状态不变，
商家收到货确认后才进入退款；`20` 期间可以再调一次改掉填错的单号。
填过的物流在退款单的 `return_shipment` 里。

**自动确认收货**：发货后买家不点确认，满店铺设置的天数（默认 7 天）系统替他确认，
订单到 `40 已完成`；这一单有进行中的售后时暂停，售后结束后再确认。
客户端要展示倒计时的话按 `shipped_at` + 7 天估算（天数目前没有对外接口）。

购物车（`/cart`）按门店计价，请求时带上和商品页、下单页相同的 `store_id`。
购物车金额和试算用的是同一条价格查询，两边逐分一致。

### 运费

运费由商家在后台配的**运费模板**决定（按件或按重量、按省设价、满额 / 满件包邮、指定地区不配送），
服务端按收货地址算好返回，客户端**不要自己算**：

- `POST /orders/preview` 必返 `freight_cents`（运费）、`freight_discount_cents`（包邮券抵掉的运费）
  和 `freight` 明细（按模板分组：命中哪条规则、计费量、为什么包邮）。
  **应付 = `goods_amount_cents` + `freight_cents` − `discount_cents`**，`discount_cents` 已经包含
  包邮券抵掉的运费。商家没配任何模板时运费是 0，明细里 `free_reason = no_template`。
- 计价顺序固定：商品原价 → 营销活动 → 优惠券（门槛按活动后金额判）→ 运费（**满额包邮按优惠后
  应付商品金额判**）→ 包邮券抵运费。所以「满 99 包邮」的单用了一张满减券之后可能不再包邮，
  试算会如实算出来 —— 结算页照着 `freight_cents` 显示即可。
- **送不到**：收货省在模板的不配送地区里时，试算与下单都回 422 `region-not-deliverable`，
  `undeliverable_items` 是 `[{sku_id, reason_code, reason}]`，把这几行标出来让买家去掉或换地址。
  `reason_code = province_unknown` 表示地址归不到省（没有 `region_code`、省名也认不出），
  请引导买家补全地址的省份。
- **包邮券**（`coupon_type = 4`）抵运费、最多抵到 0；这一单运费为 0 时用不了（409 `coupon-not-applicable`）。
  `POST /coupons/applicable` 要带 `address_id` 才会列出包邮券。
- **购物车**：`GET /cart`（以及另外四条返回 `Cart` 的接口）收可选的 `address_id`，不传用买家的默认地址；
  有地址时返回 `freight`（预估运费：按已勾选、送得到的行算，不含券）与 `address_id`，
  送不到的行带 `undeliverable`。没有地址时这两个字段整个不出现（不是「包邮」）。最终以试算为准。
- 订单上：`freight_cents` 是下单时算好的运费，`freight_discount_cents` 是包邮券抵掉的部分，
  订单详情的 `freight` 是下单那一刻的规则快照（之后商家改模板不影响它）。
  **实收运费 = `freight_cents − freight_discount_cents`**，售后退运费的上限按它算。

---

## 商品批量导入

三条接口，全部要后台会话（管理员 / 操作员），平台级会话可以带 `X-Keel-Merchant`：

```
GET  /api/v1/admin/product-imports/template?format=xlsx|csv   下载模板
POST /api/v1/admin/product-imports/preview                    预检（不写库）
POST /api/v1/admin/product-imports                            确认导入（Idempotency-Key 必填）
```

后两条是 `multipart/form-data`，文件放在 `file` 那一项，≤ 5 MB、≤ 2000 行
（超出分别是 413 `import-file-too-large` 与 422 `import-file-invalid`）。
格式按内容判断：xlsx 或 csv（UTF-8 可带 BOM，GBK 也认）；老式 `.xls` 与加密工作簿是 415。

```bash
# 预检：返回逐行结果（rows）、每件商品的类目决定（products[].category）
curl -s -H "Authorization: Bearer $STAFF_TOKEN" \
     -F file=@商品.xlsx \
     https://shop.example.com/api/v1/admin/product-imports/preview

# 确认：同一份文件 + 每件商品选定的类目（按预检结果里的 first_row 对应）
curl -s -H "Authorization: Bearer $STAFF_TOKEN" \
     -H "Idempotency-Key: $(uuidgen)" \
     -F file=@商品.xlsx \
     -F 'categories=[{"first_row":2,"category_id":12},{"first_row":5,"category_id":7}];type=application/json' \
     https://shop.example.com/api/v1/admin/product-imports
```

接入时要注意的几条：

- **预检不落库，确认时重传同一份文件。** 服务端重新解析、重新校验，不信任客户端回传的预检结果。
- **推荐的类目要自己带回去。** 预检里 `status = recommended` 的商品带着 `category_id`，
  确认时请放进 `categories`——确认这一步不调推理引擎，也不会自动采用推荐。
  `needs_review` / `unavailable` 的商品必须由人选；没选的那件在回执里是 `failed`。
  文件里类目列对上了（`matched`）的可以不带。
- **有错的商品整件跳过**，一件都导不了时是 422 `import-nothing-to-import`，什么都不写。
- **幂等两层**：同一把 `Idempotency-Key` 重放返回首次结果（带 `Idempotency-Replayed: true`）；
  同一份文件（按 sha256）在本店已经确认过时，换钥匙也不会再建，返回那一次的回执并带
  `already_imported: true`。
- 导入的商品一律是**草稿**（`status = 0`）；图片 URL 只出现在回执的 `image_urls` 里，不会下载。
- 违禁词命中在 `rows[].violations`（与发布时拒绝的 `errors[]` 同一个形状），只提示不阻断。

---

## 经营报表

六条只读接口，都在 `/admin/reports/` 下，要后台会话（`Authorization: Bearer <会话 token>`），
平台级会话同样可以带 `X-Keel-Merchant` 切店。口径的唯一真相源是契约里的 `ReportWindow`
与 `ReportMetrics`，这里只列要点：

| 接口 | 用途 | 特有参数 |
|---|---|---|
| `GET /admin/reports/overview` | 指标卡：本期与上一周期的 `ReportMetrics` | — |
| `GET /admin/reports/trend` | 按小时 / 按天的支付、退款、净销售额、订单数 | — |
| `GET /admin/reports/products` | 商品排行 Top N | `sort_by`（`amount` / `quantity`）、`category_id`（含子孙）、`limit`（1–50） |
| `GET /admin/reports/stores` | 门店与大区对比 | — |
| `GET /admin/reports/inventory-alerts` | `available_qty <= warning_qty` 的门店 SKU | `limit`（1–200），没有时间参数 |
| `GET /admin/reports/search` | 搜索次数、无结果率、热门词、无结果词 | `limit`（1–50） |

**时间窗口**（前五条里除库存预警外都收）：`period` = `today`（默认）/ `yesterday` /
`last_7_days` / `last_30_days` / `custom`；`custom` 必须同时给 `start_date` 与 `end_date`
（`YYYY-MM-DD`，**都含**），最多跨 **366 天**。窗口写错（不认识的 `period`、缺日期、起晚于止、
超过 366 天）一律 `422 invalid-request`，不会悄悄回退成今天。

- **时区**：按店铺时区（`shop_settings.timezone`，没有就是 `Asia/Shanghai`）切自然日与整点，
  响应的 `window.timezone` 回显实际用的那一个。`window.current` / `window.previous` 给出
  半开区间 `[start_at, end_at)`（UTC）和店铺时区里的 `start_date` / `end_date`（都含）。
- **`last_7_days` / `last_30_days` 不含今天**；`today` 的上一周期是**昨天的同一时段**。
- **归属时间**：销售（支付金额、订单数、买家数、商品排行）按 `orders.paid_at`；退款按
  `refunds.refunded_at`（只算已到账的 `40`）。已支付的订单指状态 `20/30/40/50/60`，
  草稿 `0`、待支付 `10`、已关闭 `90` 不计。
- **金额**一律整数分；`refund_rate`、`zero_result_rate` 在分母为 0 时是 `null`（不是 0）。
- **范围**：与 `GET /admin/orders` 同一个判据，按订单的履约门店收窄；`store_id` / `region_id`
  与范围取交集，越出范围得到全零而不是 403。搜索概况只放管理员、操作员，其他角色
  `403 role-forbidden`（检索日志没有门店维度）。

```bash
curl -H "Authorization: Bearer $STAFF_TOKEN" \
  "http://localhost:8080/api/v1/admin/reports/overview?period=custom&start_date=2026-09-01&end_date=2026-09-26"
```

---

## 消息中心（站内通知）

订单与售后的关键状态变化会给买家发一条站内消息：支付成功、已发货（带物流）、
自动确认收货即将到期（到期前一天）、系统自动确认收货、超时未支付被关闭、
售后审核通过 / 驳回（带理由）、退款到账。买家**自己**做的动作（取消、确认收货、撤回售后）不发。
通知与状态变化在同一个数据库事务里写入：状态改了消息一定在，回滚了消息一定不在。

| 买家 | 接口 |
|---|---|
| 消息列表（分页，`unread_only=true` 只看未读；响应里带 `unread_count`） | `GET /me/notifications` |
| 未读数（角标用） | `GET /me/notifications/unread-count` |
| 标一条已读 | `POST /me/notifications/{notification_id}/read` |
| 全部已读 | `POST /me/notifications/read-all` |

两条标已读的接口**不需要** `Idempotency-Key`：已读是一次设置，重复调用结果不变；
响应是标完之后的未读数，直接拿来刷新角标。不是自己的通知（或不存在）一律 404。

每一条 `Notification`：

| 字段 | 说明 |
|---|---|
| `kind` | 种类（`order_paid`、`order_shipped`、`refund_rejected`……，完整枚举见契约 `NotificationKind`）。**只用来挑图标**，不要按它拼文案 |
| `title` / `body` | 服务端渲染好的中文标题与正文，**原样展示** |
| `target` | 点了跳哪里：`type` 为 `order`（看 `order_no`）/ `refund`（看 `refund_no`，`order_no` 是所属订单）/ `inventory`（只在后台出现，`store_id` + `sku_id`）。四个定位字段都一定出现，用不上的是 `null` |
| `read_at` | 已读时间，`null` 即未读 |
| `created_at` | 产生时间，列表按它倒序 |

客户端没有推送通道（本期没接微信订阅消息、短信），在「我的」页或底栏角标上
**打开页面时拉一次未读数**、前台停留时每 30～60 秒轮询一次就够了。通知保留 90 天。

后台员工有同构的四条：`GET /admin/notifications`、`GET /admin/notifications/unread-count`、
`POST /admin/notifications/{notification_id}/read`、`POST /admin/notifications/read-all`。
内容是商家侧的待办（新订单待发货、新的待审核售后、买家已寄回退货、库存预警），
按员工的门店范围收窄（与 `GET /admin/orders` 同一个判据），已读状态每个员工各一份，
范围外的通知标已读回 404。平台级会话同样可以用 `X-Keel-Merchant` 切到某家店读它的提醒。

---

## SDK 与代码生成

| 语言 | 位置 | 说明 |
|---|---|---|
| TypeScript | `web/src/api/client.mts` + `schema.d.ts` | 一个泛型的 `request` 原语，路径、参数、请求体、响应体的类型全部从契约推导，没有手写的 interface |
| UTS（uni-app x） | `app/src/api/schema.uts` | 买家端 App 用，由 `scripts/gen_uts_schema.py` 从同一份契约生成 |
| Go（服务端） | `internal/api/` | oapi-codegen 生成，handler 实现的就是它的接口 |

其他语言可以直接用契约跑 [OpenAPI Generator](https://openapi-generator.tech/) 之类的工具生成客户端。

---

## 实现进度

契约描述的是**完整的接口面**，其中一部分还没有实现，调用会得到 404「接口不存在」。
后台接口的实现进度由 `internal/handler/contract_test.go` 里的锁表守着；
总体进度见 [README 的路线图](../../README.zh-CN.md#路线图) 和 [CHANGELOG](../../CHANGELOG.md)。
