# 贡献指南

欢迎贡献。在动手之前，请先读完这一页——尤其是「两条硬规矩」。

## 两条硬规矩

### 一、handler 里不准出现 SQL 和事务

业务逻辑在 service，数据访问在 repository。handler 只做三件事：
参数校验、鉴权、响应封装。

这不是风格偏好。「单机形态平滑演进到微服务」这个架构主张能否成立，
完全取决于这条纪律——handler 里一旦出现 SQL，将来拆服务就得重写业务代码，
而整个项目的卖点就是「业务代码零改动」。

**handler 里出现 SQL 或事务的 PR 会被直接退回。**

### 二、OpenAPI 是唯一真相源

顺序永远是：先改契约 → 生成代码 → 实现。

反过来做（先写 handler 再补契约）会让前后端不一致悄悄溜进主干，
而契约驱动的全部价值就在于接口变更会让构建失败，而不是让线上出错。

同理，**数据库表结构的唯一真相源是 [`docs/电商系统-数据模型设计.md`](./docs/电商系统-数据模型设计.md)**。
其余文档不得出现 `CREATE TABLE`；需要讲结构时引用该文档的章节。
唯一例外是 dtmrs 的 `barrier` 表，它是第三方组件的产物。

## 开发环境

| 依赖 | 版本 | 说明 |
|---|---|---|
| Go | 1.23+ | 后端 |
| Rust | 1.82+ | 编译 dtmrs 的 C ABI 动态库，经 cgo 嵌入 |
| PostgreSQL | 16+ | 需要 pgvector 扩展 |
| Docker | 任意近期版本 | `docker compose up` 起全栈 |

> **为什么需要 Rust 工具链**：事务协调器 [dtmrs](https://github.com/jackwangfeng/dtmrs)
> 是 Rust 实现，通过 cgo 以嵌入式模式运行。这意味着 `CGO_ENABLED=0` 静态编译不可用。
> 如果你不想装 Rust，可以用独立 TC 进程的部署形态开发，
> 见[总体架构](./docs/电商系统-总体架构.md)的「部署演进」一节。

## 提交前自查

```bash
./scripts/check-all.sh
```

这条命令会校验：文档链接是否有效（含中文文件名的百分号编码与锚点）、
README 承诺的文件是否真实存在、OpenAPI 契约结构是否有效且无悬空引用、
架构文档声称的 AI 能力条目数是否与清单一致、
以及**每张业务表是否都带 `merchant_id`**（多租户隔离，漏一次就是跨租户泄露）。

改动文档或契约的 PR 必须先让它通过。

## 提交信息

用英文，遵循 Conventional Commits：

```
feat: add coupon allocation to order items
fix: reject refund when quantity exceeds remaining
docs: clarify saga compensation ordering
```

正文可以用中文解释「为什么」，但标题行用英文。

## 代码之外

- 设计文档用中文，代码与 Issue 用英文（见[总体架构](./docs/电商系统-总体架构.md)的 ADR #9）
- 数据库变更走迁移工具，禁止手改
- 核心逻辑必须有测试：库存扣减、优惠计算、订单状态机
