<div align="center">

# Keel

**自带分布式事务引擎的 AI 原生电商系统**
*单机可跑，扩展无需重写，AI 全本地推理*

<!-- 徽章沿用快速开始里 clone 地址的 <org> 占位符：这个仓库还没有远端。
     组织名定下来之后，两份 README 各 sed 一次即可。
     scripts/check_promises.py 会在 .github/workflows/ 不存在时把构建徽章判为虚标，
     所以徽章活不过它所宣称的那套 CI。 -->
[![CI](https://github.com/<org>/keel/actions/workflows/ci.yml/badge.svg)](https://github.com/<org>/keel/actions/workflows/ci.yml)
[![License](https://img.shields.io/badge/license-Apache--2.0-blue)](./LICENSE)
![Go](https://img.shields.io/badge/Go-1.26+-00ADD8)
![PostgreSQL](https://img.shields.io/badge/PostgreSQL-16+-336791)

[文档](./docs) · [English](./README.md)

</div>

---

## 为什么还要再做一个电商系统

开源电商已经有不少好项目——Saleor、Medusa、Shopware、mall。
Keel 无意成为第五十个。它存在的理由是三件别人没有的事：

### 一、内置分布式事务引擎

交易一致性由 [dtmrs](https://github.com/jackwangfeng/dtmrs) 保障——
Rust 实现的事务协调器，支持 SAGA、TCC、二阶段消息、XA 与 workflow。

下单要跨三个分支：扣减库存、核销优惠券、创建订单。
任一步失败则已执行的分支全部补偿——幂等、可靠，子事务屏障能扛住进程崩溃。

多数开源电商要么回避这个问题（一个大本地事务），要么外挂一个协调器。
Keel 把它当作一等公民。

### 二、AI 贯穿经营全链路，且全部本地推理

不是加一个 AI 功能，是每个环节都有，而且跑在你自己的机器上：

| 环节 | 能力 |
|---|---|
| **进货** | 从杂乱的供应商表格抽取 SKU 与属性 · 自动归类 · 跨供应商同款识别 · 发布前拦截广告法违禁词 |
| **卖货** | 语义与关键词混合检索 · 对话导购 · 拍照搜同款 · 搜不到时的意图澄清 |
| **经营** | 销量预测 · 补货建议 · 用大白话查自己的数据 |
| **反馈** | 把几千条评价聚成可执行的改进建议 · 退货原因归因 |

**零外部 API 调用，数据不出内网，无按量计费。**

供应商报价、成本结构、客户对话都留在自己机器上。

这件事比听起来重要。其他平台的 AI 要么是第三方接口的封装，要么只在自家托管云上
提供——比如 Medusa 的语义检索跑在 Medusa Cloud 上，你要自托管，还是得自己去接
Algolia。Keel 默认你想完整拥有自己的技术栈。

而且它们的 AI 几乎全堆在卖货这一侧。**没有人把 AI 用在进货和经营上**——
而那恰恰是商家每天花时间最多的地方。

### 三、一套代码，从笔记本到集群

借助 dtmrs 的嵌入式协调器，事务编排代码在「进程内函数调用」与
「远程服务调用」两种形态下**完全一致**。

```diff
  // 单进程
- tc.saga(gid).step("local://deduct_stock", "local://restore_stock").submit()
  // 微服务 —— 只有这一行变了
+ tc.saga(gid).step("http://inventory/deduct", "http://inventory/restore").submit()
```

传统系统逼你二选一：单体好部署但难扩展，微服务能扩展但部署是地狱。
Keel 不需要选。

### 四、一套部署，多个商家

每个商家有自己独立的店铺前台，订单永远不跨商家。

**但这不影响单机叙事。** 小商家 `docker compose up` 起来就是租户数为 1 的部署，
感知不到任何多租户的存在；需要开第二家店时不用换架构。

跨租户隔离不靠「每个查询记得加 WHERE」——那种 bug 在单租户测试数据下完全看不出来。
靠的是每张表都带 `merchant_id`、父子关系用复合外键钉死、以及 PostgreSQL 行级安全兜底。
三条都有机械检查：`merchant_id` 那条由脚本核对**设计文档里的 DDL**，
行级安全与复合外键那两条由测试直接查**真实数据库的系统目录**。

---

## 快速开始

```bash
git clone https://github.com/<org>/keel && cd keel
docker compose up -d --build
./scripts/smoke.sh                        # 退出码 0 表示链路通
curl http://localhost:8080/api/v1/products
```

一条命令起 PostgreSQL、跑迁移、加载一家种子店铺、起 API。
那条 `curl` 直接返回这家店的商品，而请求里没有任何东西说明「哪家店」——
这套部署的租户数就是 1，正是上面承诺给小商家的那个形态。

8080 被占就整组换端口 —— 上面四条命令读的是同一个变量，但那条 `curl` 里的
端口号是写死的，别只改第一条：

```bash
export KEEL_HTTP_PORT=18080
docker compose up -d --build
./scripts/smoke.sh
curl "http://localhost:$KEEL_HTTP_PORT/api/v1/products"
```

> 8080 被别的服务占着时，原样照抄的那条 `curl` 会从**那个服务**拿到 404，
> 看起来像「Keel 起崩了」，其实是打到了别人身上。

要多商家形态（由 `Host` 头决定是哪家店）：
`docker compose -f compose.yaml -f compose.multi.yaml up -d --build`。

不需要 Elasticsearch，不需要 MongoDB，不需要 RabbitMQ，不需要 Redis。
**只有一个数据库。** 向量检索在 `pgvector`，全文检索在 `tsvector`，任务队列是一张表。

> 文件（商品图、头像、退款凭证）默认写本地磁盘卷，不是额外的服务。
> 换 S3 形态时才会多一个组件。

### 还没在盒子里的

上面那三个服务——PostgreSQL、迁移与种子、API——就是今天 `docker compose up`
起来的全部。这份 README 的其余部分描述的是正在建的系统；下面这些还在路线图上，
列在这里是为了让上面那段不会被读成「已经有了」：

- `pgvector` 扩展与语义检索
- 推理引擎，以及没有 GPU 时降级到 CPU 小模型这件事
- 店铺前台与后台界面——3000 端口上目前没有任何页面

---

## 能力

**电商核心**
商品与 SKU · 库存 · 购物车 · 下单 · 支付 · 退款 · 优惠券 · 订单状态机

**AI 原生能力**
- **语义检索** —— 向量与关键词双路召回、RRF 融合、cross-encoder 精排，
  最后做*业务重排*（库存、活动、口碑）。**语义相关不等于应该卖。**
- **对话导购** —— 理解意图，但绝不编造商品。展示的每一件都来自真实检索结果。
- **拍照搜同款** —— 用一张图找到同一件商品。
- **商品理解** —— 从杂乱的供应商表格里抽取结构化属性、自动归类、
  识别多供应商之间的重复商品。
- **合规检查** —— 发布前拦截广告法违禁词。

**对正确性的认真程度**
- 金额是 `BIGINT` 分，绝不用浮点。
- 订单金额恒等式写进数据库 `CHECK` 约束——算错在写入瞬间被拒绝，
  而不是等到对账时才发现。
- 支付回调靠 `(渠道, 渠道流水号)` 唯一索引保证幂等。
- 库存用条件原子更新，并有 `CHECK` 约束兜底。
- 优惠按行分摊，所以部分退款能算对金额。

---

## 客户端

Keel 自带客户端，不只是一套 API。

| 客户端 | 技术栈 | 目标平台 |
|---|---|---|
| **商城前台** | uni-app (Vue 3) | H5 · 微信小程序 · iOS · Android，一套代码 |
| **管理后台** | React + Tailwind + shadcn/ui | 桌面 Web |

所有客户端由同一份 OpenAPI 契约生成代码——接口变更会让构建失败，
而不是悄悄把线上搞坏。

设计 token（颜色、间距、字体、圆角）跨平台统一，组件实现分平台——
因为小程序渲染不了浏览器能渲染的东西，强求像素级一致只会两边都难看。

**支持微信小程序是有意为之。** 中国大部分电商交易发生在微信里，
而没有任何一个主流开源电商平台把它当作目标平台。
如果你在这个市场卖东西，只有 Web 的商城等于没有商城。

> 首个版本交付管理后台与 H5 / 小程序商城前台。
> 原生 App 打包与桌面端优化的 Web 商城随后跟上。

---

## 买家端 —— `app/` 里现在真的有什么

上面那张表是计划。这一节是已经存在的那部分，免得上面读起来像是都已经发布了。

[`app/`](./app) 是买家端店面，用 [uni-app x](https://doc.dcloud.net.cn/uni-app-x/)
写（UTS 编译成原生 Kotlin/Swift，不走 webview）。页面流是：商品列表 → 商品详情 →
登录 → 下单（试算 → 提交）→ 我的订单 → 订单详情 → 发起支付。
搜索框是禁用的占位：`GET /search` 是 M3 的东西。

**它的类型同样从契约生成，但不是 `web/` 那份产物。** UTS 不是 TypeScript ——
它的类型系统要落到 Kotlin 与 Swift 上，`web/src/api/client.mts` 里那套条件类型与
映射类型没有任何东西可以生成成。所以另有一个生成器
（`scripts/gen_uts_schema.py`）从同一份 `docs/电商系统-OpenAPI.yaml` 生成
`app/src/api/schema.uts`，产物入库，两道闸门钉着它：
`scripts/check_uts_contract.py`（重生成到临时目录再 diff）与
`scripts/check_app_types.py`（`tsc --strict` 编译每一个 `.uts`）。
把契约里的字段改个名，两道都会红 —— 变异验证的完整输出在
[`app/README.md`](./app/README.md)。

**命令行能构建到 H5 与 Kotlin，构建不出 apk。**
`uni build --platform h5` 出的是可直接发布的 web 产物；
`uni build --platform app-android` 出的是 Kotlin 源码，到此为止 ——
从那堆 Kotlin 到一个能装的 App 需要 HBuilderX 或 DCloud 云打包，没有对应的
命令行。CI 跑的就是这两步，不多不少。`app/README.md` 记了实测到哪一步，
包括为了让一个纯命令行项目能编起来绕开的五个坑。

怎么跑起来见 [`app/README.md`](./app/README.md)。H5 形态必须与 API 同源
（服务端不发 CORS 头），`app/vite.config.js` 里那段开发服务器代理干的就是这件事。

---

## 架构

```
客户端（Web · 小程序 · App · 后台）
             │  OpenAPI 3.1 —— 前后端由同一份契约生成
      ┌──────▼───────┐
      │   Go / Gin   │  handler 只做校验，无 SQL、无事务
      └──────┬───────┘
      ┌──────▼───────────────────────────────┐
      │  Service：商品 · 库存 · 订单 ·         │
      │  支付 · 营销 · 检索                    │
      └───┬───────────────┬──────────────┬───┘
          │               │              │
    ┌─────▼────┐   ┌──────▼──────┐  ┌────▼─────┐
    │  dtmrs   │   │ 商品理解服务 │  │  sqlc    │
    │  (Rust)  │   │              │  │          │
    └─────┬────┘   └──────┬──────┘  └────┬─────┘
          │        ┌──────▼──────┐       │
          │        │  推理引擎    │       │
          │        └─────────────┘       │
          └───────────────┬──────────────┘
                   ┌──────▼──────┐
                   │ PostgreSQL  │  业务表 + barrier + pgvector
                   └─────────────┘
```

详见：[总体架构](./docs/电商系统-总体架构.md) ·
[数据模型](./docs/电商系统-数据模型设计.md) ·
[语义检索层](./docs/电商系统-语义检索层设计.md) ·
[商品理解服务](./docs/电商系统-商品理解服务设计.md)

---

## Keel 适合你吗

**适合，如果你想要**
- 真正能推理清楚的交易正确性
- 不把商品库交给第三方的 AI 能力
- 今天单机跑、将来拆服务的系统
- 现代的 Go / PostgreSQL 技术栈

**不适合，如果你想要**
- 今天就要开箱即用的完整商城 ——
  [mall](https://github.com/macrozheng/mall) 功能覆盖广得多，
  教程生态也极其完善
- 托管的 SaaS —— 用 Shopify
- 成熟的插件市场 —— 用 Magento 或 WooCommerce

Keel 还年轻。功能比上面这些少，也没有经过大规模真实流量的考验。
它有的是一个更硬的内核。

---

## 路线图

- [x] 数据模型、OpenAPI 契约、架构设计
- [ ] **M2** —— 商品 → 购物车 → 下单 → 支付，由 dtmrs 编排
- [ ] **M3** —— 文本向量 + 混合检索 → *首个公开版本*
- [ ] **M4** —— 合规检查 + 商品理解服务
- [ ] **M5** —— 精排 + 业务重排 + 搜索分析
- [ ] **M6** —— 图像向量 → 拍照搜同款
- [ ] **M7** —— 对话导购
- [ ] **M8** —— 跨供应商同款识别

---

## 参与贡献

欢迎贡献，请先阅读 [CONTRIBUTING.md](./CONTRIBUTING.md)。

最重要的两条规矩：

1. **handler 里不准出现 SQL 和事务。** 业务逻辑在 service，数据访问在 repository。
   这是「单体平滑演进到微服务」能成立的前提。
2. **OpenAPI 是唯一真相源。** 先改契约，再生成代码，最后实现。

---

## 许可证

Apache-2.0

---

> 本文档为 [README.md](./README.md) 的中文翻译。若有出入，以英文版为准。
