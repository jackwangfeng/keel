# AI 接口（给接入方）

Keel 不内置大模型。店里的「AI 员工」是**你自己选的 agent**——Claude Code、Codex、Cursor、自己写的脚本、
飞书 / 钉钉 / 企业微信机器人背后的程序都行——凭一把接入密钥，通过 [MCP](https://modelcontextprotocol.io)
调用 Keel 的工具：读经营数据、做确定性计算、写简报、提提案。**写操作一律是提案，人在后台批准后才执行。**

这份文档写给「要把某个 agent 接进 Keel」的人。给 agent 自己读的工作手册在 [`agent/AGENTS.md`](../agent/AGENTS.md)，
设计背景在 [AI 经营规划](./AI经营-规划.md) 与 [M9 设计](./AI经营-M9设计.md)。

## 三步接上

1. **在后台建一名 AI 员工**：「AI 员工 → AI 员工与密钥 → 新建」，选角色和管辖范围（与给真人员工分配权限完全一样）。
2. **发一把接入密钥**：同一页「发密钥」。密钥（`kagt_…`）**只显示这一次**，页面同时给出可直接粘贴的 MCP 配置。
3. **把配置放进你的 agent**，例如 Claude Code / Claude Desktop / Cursor 的 `mcp.json`：

   ```json
   {
     "mcpServers": {
       "keel": {
         "type": "http",
         "url": "https://<店铺域名>/api/v1/mcp",
         "headers": { "Authorization": "Bearer ${KEEL_AGENT_KEY}" }
       }
     }
   }
   ```

   连上以后先调一次 `shop_overview` 看看。只支持 stdio 的客户端用本地桥 `cmd/keel-mcp`（见下文）。

自检（不经 MCP，给运行器与排障用）：

```bash
curl -H "Authorization: Bearer $KEEL_AGENT_KEY" https://<店铺域名>/api/v1/agent/whoami
# {"staff_id":5,"name":"AI 店长","role":2,"region_ids":[],"store_ids":[],"key_id":3}
```

## 连接参数

| 项 | 值 |
|---|---|
| 端点 | `https://<店铺域名>/api/v1/mcp`（POST / GET） |
| 传输 | MCP streamable HTTP，**无状态**（不需要也不保留会话），响应是 JSON（不走 SSE） |
| 认证 | `Authorization: Bearer kagt_…`，**每个请求都要带** |
| 租户 | 按 **Host** 解析：必须用这家店的域名访问。A 店的密钥打 B 店的域名回 401 |
| 限流 | 每把密钥每分钟 120 个 HTTP 请求，超了回 **429 + `Retry-After`**（秒） |

无状态意味着：吊销密钥、停用 AI 员工、收窄管辖范围，**下一次调用就生效**，不存在「一个长连接一直带着旧身份」。

## 身份与权限

- AI 员工就是一名员工（`staff.kind = 2`），角色取 **2 操作员**（全店）、**3 大区管理员**、**4 门店管理员**（只管指定的大区 / 门店）。
  不能是管理员。
- **判权与后台接口逐字相同**：同一个工具，门店管理员只看得到、只提得了自己那几家店的。越权回错误，不要换参数重试。
- 密钥只能用在 `/api/v1/mcp` 与 `/api/v1/agent/whoami`。**它登录不了后台、调不了后台接口**——
  所以 AI 员工批准不了自己的提案。
- 每次工具调用（成功或失败）都记审计：谁、哪把密钥、哪个工具、参数、结果、耗时。

## 工具

完整的名字、说明、**输入与输出的 JSON Schema** 在 [`AI接口-工具清单.json`](./AI接口-工具清单.json)
（就是 `tools/list` 的返回，测试逐字守着它）。每个工具都声明了 `outputSchema`，服务端返回前按它校验；
结构化结果在 `structuredContent`，同时附一份 JSON 文本给只读文本的客户端。字段与 [OpenAPI 契约](./电商系统-OpenAPI.yaml)
里同名的类型一致（`AgentProposal`、`AdminStore`、`ReportOverview` …），字段含义看那里。

| 类别 | 工具 | 做什么 |
|---|---|---|
| 报表 | `shop_overview` `sales_trend` `product_ranking` `store_comparison` | 经营概览（与上一同长周期对比）、按天趋势、商品排行、门店对比。`period`：today / yesterday / last_7_days / last_30_days / custom |
| 库存 | `inventory_alerts` | 可售不高于预警线的（门店，SKU） |
| 搜索 | `search_insights` | 高频词、无结果词、低点击词 |
| 查询 | `list_stores` `list_products` `get_product` `list_refunds` | 门店、商品（含 SKU 与各店库存）、售后单 |
| 计算 | `restock_plan` | 补货计算：日均（分母去掉断货天）、可售天数、预计卖断日、建议量、置信。**数字由 Keel 算，agent 不要自己估** |
| 计算 | `slow_movers` | 滞销清仓：每个（门店，SKU）的库存周转天数（可售 ÷ 日均，口径同 `restock_plan`）。周转天数为 null 的是回看期内一件没卖出去的，排最前 |
| 计算 | `promotion_review` | 活动 / 券复盘：一个活动窗口内与前一个等长窗口的销售额、单量、客单价、参与 SKU 销量对比；一张券的发出数、核销数、核销率、带来的销售额与优惠 |
| 提案 | `propose_inventory_adjust` `list_my_proposals` | 提一条加库存提案；看自己提过的与结果（执行前后的可售、驳回理由） |
| 简报 | `post_brief` | 写一份 markdown 简报（巡店日报等），直接生效 |
| 事件 | `list_events` `ack_events` | 拉你管辖范围内的新事件（库存预警、售后申请、无结果词突增、提案结果）；确认处理到哪一条。见下文「事件」 |

金额一律是**分**（整数），同时附人读的元。时间是 RFC 3339（UTC），日期按店铺时区。

## 提案：写操作怎么落地

```
agent: propose_inventory_adjust(store, sku, +70, 理由, 证据, 预计影响)
          │
          ▼
  待处理（10）──人批准──▶ 执行中（15）──▶ 已执行（20，记执行前后的可售）
          │           └──执行失败──▶ 执行失败（40，带原因）
          ├──人驳回（必须写理由）──▶ 已驳回（30）
          └──48 小时没人处理──▶ 已过期（50）
```

- 同一门店同一 SKU 同时只能有一条待处理提案；重复提回 409。
- 批准后 Keel **以 AI 员工的身份**、带幂等键执行：审计上看得出是谁提的、谁批的。
- 驳回理由会回到 `list_my_proposals`，agent 下次能读到（「这周有活动，别补这么多」）。
- 人在后台「AI 员工 → 提案」审批。自己做审批界面（比如飞书卡片上的按钮）的话，按钮背后要用**真人员工**的后台会话调
  `POST /api/v1/admin/agent-proposals/{id}/approve` / `reject`（契约里有），不能用 AI 员工的密钥。

## 事件

Keel 在这些时刻写一条事件，agent 可以被它叫醒，而不必每天定时全量巡一遍：

| type | 什么时候 | store_id | payload |
|---|---|---|---|
| `stock_low` | 一个（门店，SKU）可售不高于预警线（与 `inventory_alerts` 同一口径）；每对每 24 小时至多一条 | 该门店 | `store_id` `sku_id` `available` `threshold` |
| `refund_created` | 买家提交售后申请 | 履约门店 | `refund_no` `order_no` `store_id` `amount_cents` `refund_type` |
| `search_zero_spike` | 一个无结果词近 1 小时出现 ≥ 5 次；每词每天（UTC）至多一条 | 无（全店） | `query` `count` |
| `proposal_decided` | 提案被批准执行 / 执行失败 / 驳回 / 过期 | 提案的门店 | `proposal_id` `kind` `agent_staff_id` `status`（`executed` / `failed` / `rejected` / `expired`） |

库存预警与无结果词每 5 分钟扫一轮，所以最多晚 5 分钟；售后申请与提案结果与业务同一个事务写入，即时。
**范围**：门店级事件只给能操作那家店的 AI 员工；全店事件（没有 `store_id`）只给全店范围（操作员）的。

### 拉

```
list_events()                 → {items:[{id,type,store_id,payload,created_at}…], cursor, next_after_id, has_more}
  …处理 items…
ack_events(up_to_id=next_after_id) → {cursor}
```

- 游标由 Keel 记（每名 AI 员工一个）。`list_events` 不带 `after_id` 就从上次 `ack_events` 的位置之后读；
  带 `after_id` 可以回看。`limit` 默认 50、最多 100，`has_more` 为真就接着 `list_events(after_id=next_after_id)`。
- 游标只进不退；`up_to_id` 必须是一条存在的事件。没 ack 的下次还会读到 —— **至少一次**，按 `id` 去重。

### 推（webhook）

管理员在后台给 AI 员工配一个 URL（契约 `PUT /api/v1/admin/agents/{staff_id}/webhook`，只接受 https）。
**签名密钥只在新建、或 `rotate_secret: true` 时回一次**，存好；丢了就轮换。事件写入后 Keel 投递：

```
POST <你的 URL>
Content-Type: application/json
X-Keel-Event: refund_created
X-Keel-Event-Id: 1234
X-Keel-Signature: sha256=<hex(HMAC-SHA256(secret, 原始请求体))>

{"id":1234,"type":"refund_created","store_id":3,"payload":{…},"created_at":"2026-09-28T08:00:00Z"}
```

- 5 秒超时、不跟随重定向；回 2xx 算成功，否则指数退避重试（2、4、8、16、32 秒），至多 6 次。至少一次，按 `X-Keel-Event-Id` 去重。
- 只推这名 AI 员工**投递那一刻**管辖范围内的事件；AI 员工停用、webhook 关掉或删掉之后不再推。
- 后台 `GET` 同一个地址能看到最近 20 次投递（第几次、状态码、错误）。

验签（**对原始请求体字节**算，不要先解析再序列化）：

```python
import hmac, hashlib

def verify(secret: str, body: bytes, header: str) -> bool:
    want = "sha256=" + hmac.new(secret.encode(), body, hashlib.sha256).hexdigest()
    return hmac.compare_digest(want, header)
```

推送里只有事件本身；收到后照常用接入密钥连 MCP、`list_events` 拉取处理 —— webhook 只是「叫醒」，
伪造的推送拿不到任何数据。

## 错误

两层，别混：

| 层 | 什么时候 | 形状 |
|---|---|---|
| HTTP | 没带 / 无效 / 吊销的密钥（401）、AI 员工停用（403 `account-disabled`）、限流（429 + `Retry-After`） | RFC 9457 problem JSON，连 MCP 都没进 |
| 工具 | 越权、参数不对、提案重复、门店不存在…… | 工具结果 `isError: true`；`content` 是给模型读的一句话；**`_meta["keel/problem"]`** 是给程序读的 `{type, title, status, detail}` |

`type` 与后台接口的 problem type 逐字相同（如 `https://keel.dev/problems/out-of-scope`），判断用它，不要解析文字。
内部错误（`internal`）不带 detail。

## 兼容承诺

接入方照着工具清单写解析，所以清单的变化要可预期：

- **只加不删**：可以加新工具、加**可选**入参、加出参字段。解析时忽略不认识的字段。
- **破坏性改动起新名字**：删字段、改类型、改语义、把可选改成必填，都要新起一个工具（如 `restock_plan_v2`），
  旧的至少保留一个版本并在说明里标「已废弃」。
- 守门：`internal/handler/mcp_schema_test.go` 的快照测试逐字比对 `AI接口-工具清单.json`，任何改动都要显式重生成快照、
  在下面的变更记录里写一行。

### 变更记录

| 日期 | 变更 |
|---|---|
| 2026-09-28 | 首版：14 个工具；全部工具声明 `outputSchema`；工具错误带 `_meta["keel/problem"]` |
| 2026-09-28 | 加 `list_events` `ack_events`（M10 事件）；AI 员工可配事件 webhook（`X-Keel-Signature` HMAC 签名） |
| 2026-09-28 | AI 经营 M10：加 `slow_movers`（滞销清仓）、`promotion_review`（活动 / 券复盘）两个只读计算工具 |

## 接入方式举例

**Claude Code 定时跑（演示站就是这么跑的）**：[`agent/runner/claude-daily.sh`](../agent/runner/claude-daily.sh)。
要点：在 `agent/` 目录下跑（读到的 `CLAUDE.md` 是 AI 员工手册）、`--strict-mcp-config` 只挂 keel、
`--allowedTools "mcp__keel__*" "Read"` 不给 shell 与写文件。cron 包一层即可。

**只支持 stdio 的客户端**：

```bash
go build -o keel-mcp ./cmd/keel-mcp
KEEL_MCP_URL=https://<店铺域名>/api/v1/mcp KEEL_AGENT_KEY=kagt_… ./keel-mcp   # 配成客户端的本地命令
```

它启动时列出远端工具、原样注册、每次调用原样转发，不含业务逻辑。

**自己写程序**（Python 官方 SDK）：

```python
import asyncio, os
from mcp import ClientSession
from mcp.client.streamable_http import streamablehttp_client

async def main():
    url = "https://<店铺域名>/api/v1/mcp"
    headers = {"Authorization": "Bearer " + os.environ["KEEL_AGENT_KEY"]}
    async with streamablehttp_client(url, headers=headers) as (read, write, _):
        async with ClientSession(read, write) as s:
            await s.initialize()
            r = await s.call_tool("shop_overview", {"period": "yesterday"})
            print(r.structuredContent)

asyncio.run(main())
```

**做成聊天机器人（飞书 / 钉钉 / 企业微信）**：机器人收到消息 → 用你的 agent 框架带上面的 MCP 配置跑一轮 → 回复。
日报可以定时跑完后把 `post_brief` 的内容也推到群里；提案审批按上文「提案」一节，用真人员工的身份调后台接口。
Keel 这边不需要改任何东西。

## 还没有的

- **更多写操作**：清仓活动、发券、改标题、售后审核的提案，排在 M10（[规划](./AI经营-规划.md)）。
