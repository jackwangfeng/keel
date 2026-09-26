# OpenAPI 契约核对 · 逐条处置记录（2026-09-25）

契约与数据模型定稿后做过一次逐条核对，产出 14 条「必须改」（M1–M14）与
16 条「建议改」（S1–S16）。本文件记录**每一条的最终处置**。

留这份记录的原因很实际：下一个人重新发现同样的问题时，能看到上一次是怎么想的，
而不是从头再争论一遍。**判定「不做」是允许的，判定后不留痕不行。**

---

## 必须改（M1–M14）—— 全部已实现

| # | 内容 | 处置 |
|---|---|---|
| M1 | `Refund.status` 取值与状态机不符、缺 `enum` | 抽出 `RefundStatus`，枚举 `[10,20,30,40,50,60]` 与 DDL 逐值对齐 |
| M2 | `Refund` 缺明细与关键字段 | 补 `items[]` / `refund_type` / `payment_no` / `channel_refund_id` 等 |
| M3 | 申请退款缺 `refund_type` | 抽 `RefundCreateRequest`，`refund_type` 与 `reason_code` 入 required |
| M4 | 退款接口面残缺，三个状态不可达 | 补查询 / 撤回 / 审核 / 退款回调四类端点 |
| M5 | 申请退款只有 201 | 补 404 / 409 / 422，409 按 Problem `type` 细分 |
| M6 | `Order` 缺 `refund_status` | 已补并入 required |
| M7 | `OrderStatus` 未写明 50/60 仅整单退 | description 改写，列出七条合法边 |
| M8 | `ProductSummary.required` 引用不存在的 `status` | 补 `status` 属性 |
| M9 | 认证 / 用户 / 地址整块缺失 | 补 auth / me / identities / addresses 共 14 个端点 |
| M10 | 购物车缺 `selected` | 补字段 + `PUT /cart/selection` |
| M11 | 幂等语义未定义 | 补冲突响应、重放头、四条语义说明 |
| M12 | `/search` 缺 `strategy` | 请求与响应各补一处 |
| M13 | 支付回调 description 残留「库存 Confirm」 | 改为 SAGA 的实际动作 |
| M14 | 确认收货无接口 | 补 `POST /orders/{order_no}/confirm` |

---

## 建议改（S1–S16）

| # | 内容 | 处置 | 理由 |
|---|---|---|---|
| S1 | `Cart.total_cents` 语义歧义 | **已做** | 补 `selected_total_cents`，两个总额语义写进 description |
| S2 | `OrderItem` 缺在途退款数量 | **已做** | 补 `refunding_qty`，并在数据模型 §10 写出定义式与「DB 拦不住在途超退」的警告 |
| S3 | `GET /orders` 不能按售后状态筛选 | **已做** | 补 `refund_status` 查询参数 |
| S4 | `Order` 缺 `shipped_at` / `finished_at` | **已做** | 物流时间线与自动确认收货倒计时都要用 |
| S5 | 「对外一律用业务编号」自己没被遵守 | **已做（改约定）** | 收窄为「仅订单/支付/退款用不可枚举编号」。防爬单量这个理由只对这三类成立；越权靠 `user_id` 过滤，不靠编号难猜 |
| S6 | `channel` 响应侧无 enum | **已做** | 补 enum 与 `wechat=1 / alipay=2 / balance=3` 映射说明 |
| S7 | `CartItem.price_cents` 未说明是实时价 | **已做** | 写明不是快照、不可据此做降价提醒 |
| S8 | 重复加购是累加还是覆盖未定义 | **已做** | 写明累加、上限 999、超限 422（并补上了那个 422 响应） |
| S9 | `/search` 无翻页、无 `total`、无召回来源 | **部分做** | 补 `total` 与 `recall_source`；**翻页判定不做**——检索流水线翻页要么重跑全流程要么缓存中间结果，不是一期成本。已在 description 写明，不让客户端猜 |
| S10 | 无搜索行为回传，`search_logs` 一半列永远 NULL | **已做** | 补 `POST /search/events`，并为此在 `search_logs` 补 `trace_id`（回传的定位键）与 `carted_id`（加购指标） |
| S11 | 分页约定不统一 | **已做** | `GET /orders` 补 `required: [items]`；`/coupons` 改为带 `PageMeta` 的分页 |
| S12 | `/coupons` 过滤缺「锁定」态 | **已做** | 枚举补 `locked`，并说明为何必须可查（下单未支付时券会像凭空消失） |
| S13 | 无领券接口 | **不做（一期）** | 领券中心涉及活动页、分享裂变、限流防刷，是独立产品面，不该夹在核心交易链路里顺手做。`coupon_templates` 的发放控制字段已预留，需要时再开 |
| S14 | `OrderDetail.receiver` 是自由对象 | **已做** | 定义 `ReceiverSnapshot`。**刻意不 `$ref` 复用 `AddressInput`**——快照字段集一旦定下不能再动，跟着地址簿 schema 演进会让历史订单解析不了 |
| S15 | `OrderDetail` 有 `payments` 无 `refunds` | **已做** | 补 `refunds` 数组 |
| S16 | 无批量删除购物车条目 | **已做** | 补 `POST /cart/items/batch-delete`。用 POST 而非 `DELETE ?ids=`：条目多时查询串超长，且 DELETE 带 body 在部分网关行为不一致 |

---

## 复审后追加修正

一轮独立复审又发现若干问题，一并记录处置：

| 问题 | 处置 |
|---|---|
| AI 能力清单中「滞销预警」全仓库无来源；MCP Server / Agent Skills 被拆成两项而来源当一件事；「商品详情自动补全」是 processor 的页面呈现而非独立能力 | **删除 3 项**，清单回到 37 项。架构 §11 的数字随之改回。这是删注水，不是凑数 |
| `POST /search/events` 用 `trace_id` 定位，但 `search_logs` 没有这一列；`add_cart` 事件无处落地 | 数据模型补 `trace_id`（唯一索引）与 `carted_id` |
| `uploads` 把「归属校验」列为建表首要理由，契约却没有任何读取端点可做校验 | 补 `GET /uploads/{upload_id}`，302 到限时地址，按 `purpose` 判权 |
| `GET /orders` 的 `refund_status` 参数复用了带 `default: 0` 的 schema，不传时会只返回无退款订单 | `default` 从共享 schema 挪到 `Order.refund_status` 响应属性上 |
| S13 的「不做」记录写进了 `description:` 块体，会渲染进对外 API 文档 | 改为真正的 YAML 注释 |
| `POST /search/events`、`/cart/items`、`/cart/items/batch-delete` 未接受 `Idempotency-Key` | 三处补齐。校验器新增该规则，豁免项须写明理由。（后记：`/search/events` 实现时改为豁免——它公开、多半没有 `user_id` 可作幂等键作用域，且每列首次为准本身就是天然幂等；理由写进了 `scripts/check_openapi.py` 的豁免表） |
| `sha256 -- 秒传与去重` 与同节的隐私论证对冲（凭 hash 命中会把 A 的退货照片交给 B） | 注释改为「完整性校验与将来去重的预留，一期不做秒传」 |
| `referenced` 是单向布尔，「引用后又解引用」的文件永不回收 | 承认为已知缺口并写明理由，列入待确认事项 |
| 「五项需数据积累」与清单标记对不上（实际 8 行标「是」） | 「需积累」列拆成 否 / 业务 / 行为 三档，架构说的五项对应「行为」档 |
