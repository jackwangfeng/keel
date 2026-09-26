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
| 429 | — | 触发限流（目前只有 `/search` 按 IP 限流） |
| 501 | — | 这条路依赖的外部服务没接（短信、微信、邮件），明确告诉你没开 |

---

## 下单与支付

一次完整的购买是这几步：

1. `GET /stores/resolve?lat=&lng=` —— 按买家位置解析服务门店。不带坐标时回落到默认门店。
   门店决定了价格和库存。
2. `POST /orders/preview` —— 试算：价格、可用的券、优惠分摊。不落库，可以反复调。
3. `POST /orders` —— 下单，**必须带 `Idempotency-Key`**。扣库存、锁券、建订单三步由分布式事务
   协调器（dtmrs 的 SAGA）编排，任何一步失败，已经执行的步骤都会被补偿回去。
4. `POST /orders/{order_no}/payments` —— 发起支付，返回渠道需要的支付参数。
5. 渠道回调 `POST /webhooks/payments/{channel}` 到账后，订单变成已支付，券被核销。
   超时未支付的订单会被自动关闭，库存和券退回。

`scripts/smoke.sh` 把这几步完整跑了一遍（用沙箱支付），可以直接照着读。

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
