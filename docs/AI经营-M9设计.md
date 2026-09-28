# AI 经营 M9：能接上（设计）

> 2026-09-28。上位文档：[AI经营-规划](./AI经营-规划.md)。迁移号段 `00090`–`00099`。

## 0. 范围与验收

**做**：AI 员工账号与接入密钥 · MCP 服务（读工具、计算工具、提案工具、简报工具）· 提案队列与审批 ·
调用审计 · 两本手册（巡店日报、补货）· Claude Code 参考运行器 · 后台「AI 员工」页 · 演示站模拟经营数据与定时 AI 员工 ·
README 改诚实。

**不做**（M10 / M11）：事件触发、清仓 / 搜索 / 售后三类写工具、自动执行策略、只读 SQL、复盘成绩单（M9 只记执行快照）。

**验收**（演示站上走通，并有自动化测试覆盖服务端部分）：

1. 管理员在后台新建一名 AI 员工（角色：操作员），发出一把接入密钥；
2. Claude Code 用这把密钥连上 `https://eshop.zzss.fun/api/v1/mcp`，列得出工具；
3. 每天 08:00 运行器唤醒 agent：它读经营数据，发一份简报，并对「14 天内会卖断」的 SKU 各提一条补货提案；
4. 后台「AI 员工 → 提案」里能看到每条提案的证据、动作、预计影响；点「批准」后库存加上，提案显示执行结果；
5. 吊销密钥后 agent 立刻连不上；AI 员工越权（比如角色是某门店的门店管理员却给别的门店提补货）被拒，与人一样。

## 1. 总体结构

```
 harness（Claude Code / Codex / …）
   │  读 agent/AGENTS.md 与 agent/skills/*.md
   │  MCP（streamable HTTP，Bearer 接入密钥）
   ▼
 /api/v1/mcp ──► 接入密钥 → AI 员工的 StaffIdentity（与人同一个结构）
   │
   ├─ 读工具 ────────► 现有 service（ReportService、库存、搜索日志…），走同样的判权
   ├─ 计算工具 ──────► 新增的确定性计算（restock_plan）
   ├─ 提案工具 ──────► agent_proposals（待批准）
   └─ 简报工具 ──────► agent_briefs + 站内通知
                            │
 后台「AI 员工」页 ◄────────┘  批准 → 以 AI 员工身份执行（幂等） → 记结果
 每一次工具调用 ──► agent_tool_calls（审计）
```

MCP 服务跑在 API 进程里（与后台接口同一套 service、同一个库），不单独起进程：
工具就是现有能力的另一种入口，分开部署只会多一份判权代码。拆分部署下它跑在 core 进程里。

## 2. AI 员工与接入密钥

### 2.1 AI 员工就是一行 staff

`staff` 加一列 `kind SMALLINT NOT NULL DEFAULT 1`（1 人 / 2 AI，**00090**）。

- 角色、管辖范围（`staff_scopes`）完全复用：AI 员工可以是操作员、大区管理员或门店管理员，
  **不许是管理员**（服务端拒绝；管理员能改店铺设置、设默认门店，不该交给 agent）；
- AI 员工**不能登录后台**：一次性登录令牌、会话那一套对 kind = 2 一律拒绝；它只能用接入密钥；
- 停用 AI 员工（`status = 0`）即刻让它的所有密钥失效（中间件每次请求重读员工状态，与现有会话相同）。

### 2.2 接入密钥 `agent_keys`（00090）

```sql
CREATE TABLE agent_keys (
    id           BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    merchant_id  BIGINT      NOT NULL DEFAULT current_merchant() REFERENCES merchants(id),
    staff_id     BIGINT      NOT NULL,
    name         TEXT        NOT NULL,              -- 「演示站 Claude Code」
    prefix       TEXT        NOT NULL,              -- 明文前 8 位，列表里给人认
    secret_hash  TEXT        NOT NULL,              -- sha256(全文)
    expires_at   TIMESTAMPTZ,                       -- NULL = 不过期
    revoked_at   TIMESTAMPTZ,
    last_used_at TIMESTAMPTZ,
    created_by   BIGINT      NOT NULL REFERENCES staff(id),
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    FOREIGN KEY (staff_id, merchant_id) REFERENCES staff(id, merchant_id)
);
CREATE UNIQUE INDEX uk_agent_keys_hash ON agent_keys(merchant_id, secret_hash);
```

- 明文形如 `kagt_<32 字节 base64url>`，**只在创建响应里出现一次**；库里只存 sha256；
- 查密钥与现有员工会话令牌同一个做法（`TouchLiveStaffSession`）：租户先由 Host 解析出来，
  再在这家店的租户事务里按 sha256 查 —— 不需要任何跨租户的查找路径，唯一索引也收在租户内；
- `last_used_at` 节流更新（同一把密钥一分钟最多写一次）。

### 2.3 后台接口（管理员）

| 接口 | 作用 |
|---|---|
| `POST /admin/agents` | 新建 AI 员工（名称、角色、管辖范围）；角色不许是管理员 |
| `GET /admin/agents` | 列表（含密钥数、最近调用时间） |
| `PATCH /admin/agents/{staff_id}` | 改名、改角色 / 范围、停用 |
| `POST /admin/agents/{staff_id}/keys` | 发一把密钥（明文只回这一次） |
| `DELETE /admin/agents/{staff_id}/keys/{key_id}` | 吊销 |

## 3. MCP 服务

- **传输**：streamable HTTP，挂 `POST/GET /api/v1/mcp`；SDK 用官方 `github.com/modelcontextprotocol/go-sdk`（v1.8）；
- **鉴权**：`Authorization: Bearer kagt_…` → 中间件解析出租户与 AI 员工的 `StaffIdentity`，
  放进 ctx，之后工具调用的判权与后台接口**逐字相同**（`authorizeStore` 等）；
- **租户看 Host**：与后台接口相同，MCP 地址就是这家店的域名（如 `https://eshop.zzss.fun/api/v1/mcp`），
  密钥在这家店的租户事务里查；拿 A 店的密钥打 B 店的域名查不到，回 401；
- **限流**：每把密钥每分钟 120 次请求（进程内固定窗口），超了回 HTTP 429 + `Retry-After`（各 harness 的 HTTP 层都会退避）；
- **无状态**：SDK 的 Stateless 模式，每个请求都重新过密钥鉴权，吊销 / 停用 / 收窄范围在下一次调用立即生效；
- **本地 stdio**：`cmd/keel-mcp` 是一个几十行的桥（stdio ↔ HTTP），给只支持 stdio 的 harness 用；它不含任何业务逻辑。

### 3.1 工具清单（M9）

命名按「动词_对象」，参数与返回都是 JSON，金额一律「分」+ 一个人读的元字符串。

**读工具**（直接执行）

| 工具 | 参数 | 返回 | 复用 |
|---|---|---|---|
| `shop_overview` | `days`（1–90，默认 7）、`store_id?` | 销售额、单量、客单、退款、与上一周期的环比 | `ReportService.Overview` |
| `sales_trend` | `days`、`store_id?` | 按天序列 | `ReportService.Trend` |
| `product_ranking` | `days`、`store_id?`、`limit` | 按销售额 / 销量的商品排行 | `ReportService.Products` |
| `store_comparison` | `days` | 门店对比 | `ReportService.Stores` |
| `inventory_alerts` | `store_id?` | 水位不高于预警线的 (门店, SKU) | `ReportService.InventoryAlerts` |
| `search_insights` | `days`、`limit` | 高频词、无结果词、低点击词 | `ReportService.Search` |
| `list_products` / `get_product` | 分页 / `product_id` | 商品、SKU、各门店价与库存 | 后台商品查询 |
| `list_refunds` | `status?`、`days` | 售后单摘要 | 后台售后查询 |
| `list_stores` | — | 门店（AI 员工管辖范围内的） | 后台门店查询 |
| `list_my_proposals` | `status?` | 自己提过的提案与结果（用来避免重复提） | §4 |

**计算工具**（直接执行，只读）

| 工具 | 说明 |
|---|---|
| `restock_plan` | §5 的算法。参数 `store_id?`、`cover_days`（默认 14）、`lookback_days`（默认 14）。返回每个 (门店, SKU) 的日均销量、可售、预计卖断日、建议补货量、置信（样本天数） |

**提案工具**（生成提案，不执行）

| 工具 | 参数 | 执行时调用 |
|---|---|---|
| `propose_inventory_adjust` | `store_id`、`sku_id`、`delta`（正数，≤ 1000）、`reason`、`evidence`（markdown）、`expected_impact` | 相对调整库存（现有 `POST …/inventory/adjustments`），reason = 补货 |

**简报工具**（直接执行，低风险）

| 工具 | 参数 | 效果 |
|---|---|---|
| `post_brief` | `title`、`body`（markdown，≤ 8 KB）、`period_start`、`period_end` | 写一份简报；给本店管理员发站内通知 |

M9 不开放任何「直接写业务数据」的工具。每个工具的判权与后台对应接口相同：比如门店管理员身份的 AI 员工，
`restock_plan` 只返回它那家店，`propose_inventory_adjust` 给别的店提会被拒（403 语义的 MCP 错误）。

### 3.2 错误

工具错误回 MCP 的 `isError: true` + 文本，文本里带契约里的 problem type（如 `store-unavailable`、`out-of-scope`），
让 agent 能按类型处理；服务端内部错误不透传细节。

## 4. 提案

### 4.1 数据模型 `agent_proposals`（00091）

```sql
CREATE TABLE agent_proposals (
    id              BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    merchant_id     BIGINT      NOT NULL DEFAULT current_merchant() REFERENCES merchants(id),
    agent_staff_id  BIGINT      NOT NULL,              -- 提案的 AI 员工
    kind            TEXT        NOT NULL,              -- inventory_adjust（M10 起更多）
    store_id        BIGINT,                            -- 便于按门店筛与判权
    payload         JSONB       NOT NULL,              -- 执行参数，按 kind 校验
    title           TEXT        NOT NULL,              -- 一句话：「北京门店 连衣裙 M 补 40 件」
    evidence        TEXT        NOT NULL,              -- markdown：看到了什么
    expected_impact TEXT        NOT NULL DEFAULT '',
    status          SMALLINT    NOT NULL DEFAULT 10,   -- 10 待处理 20 已执行 30 已驳回 40 执行失败 50 已过期
    decided_by      BIGINT,                            -- 批准 / 驳回的人
    decided_at      TIMESTAMPTZ,
    reject_reason   TEXT,
    result          JSONB,                             -- 执行结果快照（执行前后的库存等）
    expires_at      TIMESTAMPTZ NOT NULL,              -- 默认创建后 48 小时
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    FOREIGN KEY (agent_staff_id, merchant_id) REFERENCES staff(id, merchant_id),
    FOREIGN KEY (decided_by, merchant_id)     REFERENCES staff(id, merchant_id),
    FOREIGN KEY (store_id, merchant_id)       REFERENCES stores(id, merchant_id)
);
CREATE INDEX idx_agent_proposals_pending ON agent_proposals(merchant_id, status, created_at);
```

### 4.2 状态机

```
10 待处理 ──批准──► 执行 ──成功──► 20 已执行
    │                 └──失败──► 40 执行失败（result 里是 problem type 与说明）
    ├──驳回──► 30 已驳回（reject_reason 回给 agent：list_my_proposals 看得到）
    └──超过 expires_at──► 50 已过期（定时任务扫；过期的不能再批）
```

- 状态迁移都是条件 UPDATE（`WHERE status = 10`），两个人同时点批准只有一个生效；
- **同一 (kind, store_id, sku) 同时只能有一条待处理**（部分唯一索引）：agent 重复提会被拒，并告诉它已有的那条。

### 4.3 批准即执行

- 后台 `POST /admin/agent-proposals/{id}/approve`：**判权两道** —— 批准的人对这件事要有权（按人判），
  执行时再按 AI 员工的身份判一次（它提案之后范围可能被收窄了）；
- 执行调用的是现有 service 函数，幂等键 = `agent-proposal:<id>`（相对调整库存本来就按 biz_id 幂等），
  所以「执行成功但写状态前进程挂了」重试也只加一次；
- 执行结果写进 `result`：执行前可售、执行后可售、库存流水 id；
- 驳回：`POST …/{id}/reject`，理由必填。

### 4.4 后台接口

| 接口 | 作用 |
|---|---|
| `GET /admin/agent-proposals` | 按状态、门店、时间筛；待处理的在前 |
| `GET /admin/agent-proposals/{id}` | 详情（含 evidence 渲染、payload、result） |
| `POST /admin/agent-proposals/{id}/approve` | 批准并执行（Idempotency-Key） |
| `POST /admin/agent-proposals/{id}/reject` | 驳回 |

## 5. 计算工具：`restock_plan`

对每个在售 (门店, SKU)：

1. **日均销量** = 过去 `lookback_days` 天该门店该 SKU 已支付订单（状态 20 / 30 / 40，不含已退款）的件数 ÷ 有货天数。
   「有货天数」从 `inventory_logs` 推：某天结束时可售为 0 的那天不计入分母 —— 否则断过货的 SKU 日均被低估，越断越少补；
2. **预计卖断日** = 今天 + 可售 ÷ 日均（日均为 0 → 不会卖断，不出建议）；
3. **建议补货量** = ⌈日均 × `cover_days` − 可售⌉，下限 0，向上取到 5 的倍数（整箱习惯），上限 1000；
4. **置信**：有效样本天数 < 5 时标 `low`，手册要求 agent 对 low 的只在简报里提、不提案。

纯 SQL + Go，单元测试覆盖边界（从没卖过、断货多天、刚上架）。它不做季节性与促销修正 —— 那是 M10 以后
「销量预测」的事，这里宁可简单可解释。

## 6. 简报 `agent_briefs`（00092）

```sql
CREATE TABLE agent_briefs (
    id             BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    merchant_id    BIGINT      NOT NULL DEFAULT current_merchant() REFERENCES merchants(id),
    agent_staff_id BIGINT      NOT NULL,
    title          TEXT        NOT NULL,
    body           TEXT        NOT NULL,     -- markdown，≤ 8 KB
    period_start   DATE        NOT NULL,
    period_end     DATE        NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    FOREIGN KEY (agent_staff_id, merchant_id) REFERENCES staff(id, merchant_id)
);
```

**调整（实现时）**：M9 不发站内通知。通知的种类与跳转目标在契约和三端客户端里都是枚举，加一种要动的面大，
而 M9 的验收不依赖它；后台「AI 员工」页首页有待处理提案横幅、简报列表，已经够用。通知放到 M10 与事件触发一起加。
简报只给全店范围的人（管理员 / 操作员）看（全店口径的经营信息）。后台渲染 markdown 时**不渲染 HTML**（agent 写的内容当不可信输入）。

## 7. 审计 `agent_tool_calls`（00093）

每次工具调用一行：`(merchant_id, agent_staff_id, key_id, tool, args JSONB, ok BOOLEAN, error_type, duration_ms, proposal_id?, created_at)`。
args 里不会有密钥（密钥在请求头）。保留 90 天（复用保留期清理任务的写法）。后台「AI 员工 → 调用记录」可按工具、时间筛。

## 8. 手册与运行器（仓库里的 `agent/`）

```
agent/
  AGENTS.md              # 入口：你是谁、Keel 是什么、工作纪律、工具总览、何时提案
  CLAUDE.md -> AGENTS.md # Claude Code 读这个名字
  skills/
    巡店日报.md          # 每天做什么、看哪些工具、简报结构、异常的判据
    补货.md              # 何时提、怎么用 restock_plan、证据怎么写、low 置信怎么办
  mcp.json.example       # MCP 连接配置模板（URL + 从环境变量读密钥）
  runner/
    claude-daily.sh      # claude -p "按 skills/巡店日报.md 与 skills/补货.md 做今天的巡店" --mcp-config …
```

**工作纪律**（写进 AGENTS.md）：数字只引用工具返回的；每条提案的 evidence 必须能从工具结果复现；
先 `list_my_proposals` 再提，不重复；一次巡店最多 10 条提案；不确定就写进简报，不提案。

运行器只依赖 harness 的无头模式；换 harness 只换这一个脚本。演示站在本机用 cron 每天 08:00 跑一次，
密钥放 `~/.config/keel/demo-agent-key`（不进仓库）。

## 9. 后台「AI 员工」页

- **AI 员工**：列表、新建、改角色 / 范围、停用；密钥的发放（明文只显示一次，给复制按钮）与吊销；
- **提案**：待处理的卡片流（标题、门店、证据摘要、预计影响，批准 / 驳回），历史列表可筛；
  详情里 evidence 按 markdown 渲染（不渲染 HTML），执行结果显示执行前后可售；
- **简报**：按日期倒序，markdown 渲染；
- **调用记录**：审计表。
- 首页顶部：「AI 员工有 N 条提案待处理」的横幅。

## 10. 演示站

- **模拟经营**（`tools/simulate`，部署层跑）：一组模拟买家（`138000001xx`）按作息曲线每天下 20–60 单，
  付款、发货、确认收货走真实接口；每天若干搜索（含无结果词）；偶发售后。第一次运行时补 14 天历史：
  订单照样走真实接口创建，之后把这批模拟单的时间戳整体前移（只动 created_at / paid_at 等时间列，
  金额与状态不动；只作用于模拟买家的订单），让补货与日报一上线就有东西可看；
- **AI 员工**：演示站上建一名「AI 店长（演示）」，操作员角色；本机 Claude Code 每天 08:00 巡店；
- 演示站后台本来就对访客开放，访客能直接看到提案、简报与执行结果。为避免访客乱批，演示站的批准按钮
  对访客会话只读（演示会话脚本里的那名员工改成只读角色 —— M9 需要给员工加「只读」的判定，或者单独给访客一个只读账号）。

## 11. 契约、文档与闸门

- OpenAPI：§2.3、§4.4 的后台接口进契约；`/api/v1/mcp` 不进（它的形状由 MCP 规范定义），在 contract_test 的
  nonContractRoutes 里挂账并写明理由；
- 数据模型文档：四张新表与 staff.kind 的 DDL（check_tenancy 以文档为真相源）；四张表都是常规 tenant 类，不需要豁免；
- notification_policy：新 kind `agent_brief`；提案的状态迁移登记为 Silent（后台页面就是它的通知面）或发给提案人以外的管理员（定稿时二选一）；
- tenant_context 闸门：MCP 走现有的 Host → 租户解析，不手搓租户上下文，闸门不需要新登记；
- 权限矩阵测试（permission_test.go）：新增的后台接口全部进矩阵；另加 AI 员工的矩阵：同一组 MCP 工具在四种角色下的放行 / 拒绝；
- README（中英）：去掉未实现的销量预测、对话导购等，换上「AI 员工」定位，附演示站链接。

## 12. 任务拆分（按依赖排序）

| # | 任务 | 迁移 |
|---|---|---|
| 0 | README 改诚实 | — |
| 1 | staff.kind、agent_keys、密钥解析函数、MCP 鉴权中间件、AI 员工后台接口 | 00090 |
| 2 | MCP 服务骨架（SDK 接入、审计、限流）+ 读工具 | 00093 |
| 3 | restock_plan（含 inventory_logs 推有货天数）+ 单元测试 | — |
| 4 | 提案：表、状态机、propose 工具、批准即执行、过期任务 | 00091 |
| 5 | 简报：表、post_brief、通知 | 00092 |
| 6 | 后台「AI 员工」页（员工与密钥、提案、简报、调用记录、首页横幅） | — |
| 7 | agent/：AGENTS.md、两本手册、运行器、mcp.json 模板、cmd/keel-mcp 桥 | — |
| 8 | 演示站：模拟经营工具与 14 天回填、只读访客、AI 员工与 cron | — |
| 9 | 端到端验收：在演示站用 Claude Code 实跑一遍 §0 的 5 条 | — |

1 → 2 → (3, 4, 5 可并行) → 6 → 7 → 8 → 9。并行开发时 3/4/5 各自只动自己的迁移号（00091 / 00092 已分配，3 不需要迁移）。

## 13. 未决与风险

- **访客只读**：演示站后台对所有人开放，批准按钮必须对访客失效。倾向加一个「只读员工」角色（role = 5，所有写接口拒绝），
  演示会话改用它；真正批准由本机上的管理员会话或脚本完成。
- **harness 的无头费用**：演示站每天一次巡店，成本可控；M10 的事件触发会让调用次数上去，要设每日上限。
- **提案被人批准但数据已变**：M9 的补货是相对加量，数据变了也不会出错，只是可能多补；详情页显示「提案时可售 → 现在可售」
  让人判断。M10 的活动类提案要在批准时重新试算。
- **模拟数据与真实数据混在演示库**：模拟买家单独一个号段，报表里不区分（演示站本来就是演示）；真实部署不带这个工具。
