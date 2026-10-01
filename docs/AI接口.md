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
（就是 `tools/list` 的返回，测试逐字守着它，共 24 个）。每个工具都声明了 `outputSchema`，服务端返回前按它校验；
结构化结果在 `structuredContent`，同时附一份 JSON 文本给只读文本的客户端。字段与 [OpenAPI 契约](./电商系统-OpenAPI.yaml)
里同名的类型一致（`AgentProposal`、`AdminStore`、`ReportOverview` …），字段含义看那里。

| 类别 | 工具 | 做什么 |
|---|---|---|
| 报表 | `shop_overview` `sales_trend` `product_ranking` `store_comparison` | 经营概览（与上一同长周期对比）、按天趋势、商品排行、门店对比。`period`：today / yesterday / last_7_days / last_30_days / custom |
| 库存 | `inventory_alerts` | 可售不高于预警线的（门店，SKU） |
| 搜索 | `search_insights` | 高频词、无结果词、低点击词 |
| 查询 | `list_stores` `list_products` `get_product` `list_refunds` | 门店、商品（含 SKU 与各店库存）、售后单 |
| 计算 | `restock_plan` | 补货计算：日均（分母去掉断货天）、可售天数、预计卖断日、建议量、置信。**数字由 Keel 算，agent 不要自己估** |
| 查数 | `query_sql` | 只读 SQL 兜底（M11）：现有工具答不了的问题自己写一条 `SELECT` / `WITH`，只能读 `agent_ro` schema 里的脱敏视图（无手机号、地址、买家原话），至多 500 行、3 秒超时；需要全店范围 |
| 计算 | `slow_movers` | 滞销清仓：每个（门店，SKU）的库存周转天数（可售 ÷ 日均，口径同 `restock_plan`）。周转天数为 null 的是回看期内一件没卖出去的，排最前 |
| 计算 | `promotion_review` | 活动 / 券复盘：一个活动窗口内与前一个等长窗口的销售额、单量、客单价、参与 SKU 销量对比；一张券的发出数、核销数、核销率、带来的销售额与优惠 |
| 提案 | `propose_inventory_adjust` | 提一条加库存提案（给某门店某 SKU 加库存） |
| 提案 | `propose_flash_price` | 提一条限时折扣提案：若干 SKU 在一段时间内打折，批准后 Keel 建活动并上线。需要全店范围 |
| 提案 | `propose_coupon` | 提一条发券提案：满减 / 折扣 / 立减，可选放进领券中心。需要全店范围 |
| 提案 | `propose_product_copy` | 提一条改商品标题 / 副标题的提案，批准后 Keel 改，并过广告法违禁词检查。需要全店范围 |
| 提案 | `propose_refund_decision` | 对一张待审核售后单提审核意见（同意 / 驳回，驳回要写给买家看的理由）。按订单履约门店判权；**不允许自动执行** |
| 提案 | `list_my_proposals` | 看自己提过的与结果（执行前后的可售、驳回理由、执行后复盘的 `outcome`） |
| 成绩单 | `my_scorecard` | 自己近 30 天：按提案种类的提 / 批 / 驳回 / 过期数、执行后 `outcome.verdict`（positive / neutral / negative）分布 |
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

**五种提案种类**（`kind`）：`inventory_adjust`（加库存，门店操作范围）、`flash_price`（限时折扣，全店范围）、
`coupon`（发券，全店范围）、`product_copy`（改标题 / 副标题，全店范围）、`refund_decision`（售后审核，
按订单履约门店判权）。营销与商品是全店维度的东西，所以 `flash_price` / `coupon` / `product_copy` 只有全店范围
（角色 2 操作员）的 AI 员工能提；门店 / 大区范围的 AI 员工提这三种会拿到 `role-forbidden`，不是故障。

- 去重按 `target_key`（同一门店同一 SKU、同一商品、同一售后单号、同一组限时折扣 SKU、同一券名同时只能有一条
  待处理提案）；重复提回 409。
- 批准后 Keel **以 AI 员工的身份**、带幂等键执行：审计上看得出是谁提的、谁批的。
- 驳回理由会回到 `list_my_proposals`，agent 下次能读到（「这周有活动，别补这么多」）。
- 人在后台「AI 员工 → 提案」审批。自己做审批界面（比如飞书卡片上的按钮）的话，按钮背后要用**真人员工**的后台会话调
  `POST /api/v1/admin/agent-proposals/{id}/approve` / `reject`（契约里有），不能用 AI 员工的密钥。

### 自动执行（M11）

店长可以在后台按「AI 员工 × 提案种类」配自动执行策略：`enabled`、单笔上限（按种类含义不同：加库存件数 /
限时折扣最低折扣率 / 券面额）、每日条数上限。提案写入时若命中策略且在上限内，**Keel 当场以 AI 员工身份执行**
（与人批准走同一段执行代码），返回的提案 `status` 直接是 20（已执行）或执行失败对应的 40，`decided_by` 为空、
`auto_approved = true`；超出上限的照常进待处理队列等人批准。`refund_decision` 是资金动作，**不允许配自动执行**
（库里 CHECK 挡住）。agent 手册（`agent/AGENTS.md`）要求收到 `auto_approved = true` 的返回照样写进简报，
不能因为「不用等审批」就不报备。

### 成绩单

`GET /admin/agents/{staff_id}/scorecard`（后台 AI 员工页一个标签页）与 MCP 工具 `my_scorecard` 是同一份数据：
按提案种类的提 / 批 / 驳回 / 过期数，以及执行后 7 天（`flash_price` / `coupon` 是活动 / 券结束后 3 天）算出的
`outcome.verdict`（positive / neutral / negative，规则写死、可解释）分布。agent 手册要求 agent 自己每周看一次，
驳回多、negative 多的种类要收着提。

## 事件

Keel 在这些时刻写一条事件，agent 可以被它叫醒，而不必每天定时全量巡一遍：

| type | 什么时候 | store_id | payload |
|---|---|---|---|
| `stock_low` | 一个（门店，SKU）可售不高于预警线（与 `inventory_alerts` 同一口径）；每对每 24 小时至多一条 | 该门店 | `store_id` `sku_id` `available` `threshold` |
| `refund_created` | 买家提交售后申请 | 履约门店 | `refund_no` `order_no` `store_id` `amount_cents` `refund_type` |
| `search_zero_spike` | 一个无结果词近 1 小时出现 ≥ 5 次；每词每天（UTC）至多一条。「无结果」含只回了低于相关度下限的「猜你想要」（`POST /search` 的 `fallback`） | 无（全店） | `query` `count` |
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

`busy`（`https://keel.dev/problems/busy`，status 503）：数据库此刻繁忙（等行锁超时或语句超时），
这次调用被整体撤销、**确定没有生效**。过几秒原样再调一次即可，不必先查「是不是已经成了」，
也不要把它当成业务拒绝写进简报或提案的失败原因。与后台接口同一个判据（契约 `info.description`「数据库繁忙时」）。

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
| 2026-09-28 | AI 经营 M10 / M11：加 `propose_flash_price` `propose_coupon` `propose_product_copy` `propose_refund_decision` 四种提案与 `my_scorecard` 成绩单工具；提案支持按 AI 员工 × 种类的自动执行策略（`auto_approved` 字段），`refund_decision` 不允许自动执行 |
| 2026-09-28 | AI 经营 M11：加 `query_sql`（只读 SQL 兜底，只读 `agent_ro` 脱敏视图，需全店范围） |
| 2026-09-28 | 破坏性测试修：`query_sql` 结果加字节上限（1MB，单个文本值 4KB，超了截断）；提案批准时目标已变记为执行失败（40）而不是 500 卡在 15；`propose_coupon` 与后台建券同一套校验；自动执行的 `daily_limit` 并发下不再被突破 |
| 2026-09-28 | `post_brief` 加 `corrects_brief_id`：发一份更正旧简报（只能更正自己写的；旧的保留原样、标已更正，`AgentBrief.corrected_by_brief_id`） |
| 2026-09-28 | `search_insights` 加 `low_click_queries`（搜过 ≥3 次、都有结果、点击率最低）；每个词带 `click_count` `order_count`。之前工具描述写着「低点击词」但没有这份数据 |
| 2026-09-28 | `propose_coupon` 支持固定可用时段：`valid_start_at` + `valid_end_at`（与 `valid_days` 二选一，时段 ≤ 90 天）；固定时段的券在结束后一天复盘 |
| 2026-09-28 | `promotion_review` 的券分支加 `refunded_order_count`（整单退款的单数；这些单的券已退回，所以 `used_count` 会比 `order_count` 少） |
| 2026-09-28 | 检索加相关度下限：`search_insights` 的无结果词与 `search_zero_spike` 把「只回了低于下限的猜你想要」（fallback）也算作无结果；之前向量召回永远凑满，这两样恒为 0 |
| 2026-09-28 | 演示站实跑验收修两处：全部工具输出里的时刻改为**店铺时区**（带偏移的 RFC 3339，如 `+08:00`；之前是 UTC，AI 店长把它当北京时间写进简报）；`list_refunds` 加 `order_shipped_at`（没发过货时缺席），判发没发货看它，不看 `order_status`（50 / 60 是未发货的整单退款） |
| 2026-10-01 | 工具错误新增 `busy`（503）：数据库等锁 / 语句超时，这次调用确定没有生效，稍后原样重试。之前同样的情况报 `internal` |

## 接入方式举例

**Claude Code 定时跑（演示站就是这么跑的）**：[`agent/runner/claude-daily.sh`](../agent/runner/claude-daily.sh)。
要点：在 `agent/` 目录下跑（读到的 `CLAUDE.md` 是 AI 员工手册）、`--strict-mcp-config` 只挂 keel、
`--allowedTools "mcp__keel__*" "Read"` 不给 shell 与写文件。cron 包一层即可。

**Claude Code 被事件唤醒**：[`agent/runner/claude-events.sh`](../agent/runner/claude-events.sh)。
每次先裸调一次 `tools/call`（不经 claude）看看有没有新事件——MCP 无状态，不需要先 `initialize` 握手，
一条 JSON-RPC 请求就够；没有新事件就安静退出，省 token。有事件才拉起 Claude Code 按
`agent/skills/事件处理.md` 分派。cron 每 10 分钟跑一次即可；配了 webhook 的话收到推送直接跑同一条
prompt，不需要轮询。

**只支持 stdio 的客户端**：

```bash
go build -o keel-mcp ./cmd/keel-mcp
KEEL_MCP_URL=https://<店铺域名>/api/v1/mcp KEEL_AGENT_KEY=kagt_… ./keel-mcp   # 配成客户端的本地命令
```

它启动时列出远端工具、原样注册、每次调用原样转发，不含业务逻辑。

**自己写程序**（Python 官方 SDK）：

```python
# 官方 Python SDK（pip install mcp；2026-09-28 用 2.2.0 实测）。1.x 里函数叫 streamablehttp_client、
# 请求头直接传 headers=，结构化结果是 r.structuredContent —— 按你装的版本对照。
import asyncio, os
import httpx2
from mcp import ClientSession
from mcp.client.streamable_http import streamable_http_client

async def main():
    url = "https://<店铺域名>/api/v1/mcp"
    headers = {"Authorization": "Bearer " + os.environ["KEEL_AGENT_KEY"]}
    async with httpx2.AsyncClient(headers=headers, timeout=30) as http:
        async with streamable_http_client(url, http_client=http) as (read, write):
            async with ClientSession(read, write) as s:
                await s.initialize()
                r = await s.call_tool("shop_overview", {"period": "yesterday"})
                print(r.structured_content)

asyncio.run(main())
```

**做成聊天机器人（飞书 / 钉钉 / 企业微信）**：机器人收到消息 → 用你的 agent 框架带上面的 MCP 配置跑一轮 → 回复。
日报可以定时跑完后把 `post_brief` 的内容也推到群里；提案审批按上文「提案」一节，用真人员工的身份调后台接口。
Keel 这边不需要改任何东西。

## 实测记录（哪些客户端接过）

| 客户端 | 版本 | 结果（2026-09-28） |
|---|---|---|
| Claude Code | 本机 | ✅ 全流程：演示站每天 08:00 巡店（`agent/runner/claude-daily.sh`），简报、补货提案、批准执行、复盘都跑通 |
| 官方 Python SDK | `mcp` 2.2.0 | ✅ 连上、列出全部工具、调 `shop_overview`（结构化结果）与 `query_sql`；上面的示例就是实测过的那一份 |
| Gemini CLI | 0.33.1 | ⚠️ MCP 握手与工具列表成功（`.gemini/settings.json` 里 `mcpServers.keel.httpUrl` + `headers`，可用 `includeTools` 只放行只读工具）；模型调用被 Google 拒绝（个人免费档不再支持这个客户端），所以没跑完一轮对话 |
| opencode | 1.15.13 | ⚠️ 配置见下；这台机器上的模型供应商不允许无头调用，没跑完一轮对话 |
| Codex CLI | — | 未实测（这台机器上没装）。任何支持 streamable HTTP + 自定义请求头的客户端都按「连接参数」一节配；只支持 stdio 的用 `cmd/keel-mcp` |

Gemini CLI（项目目录下 `.gemini/settings.json`；`$KEEL_AGENT_KEY` 由环境变量展开）：

```json
{ "mcpServers": { "keel": {
    "httpUrl": "https://<店铺域名>/api/v1/mcp",
    "headers": { "Authorization": "Bearer $KEEL_AGENT_KEY" },
    "includeTools": ["shop_overview", "sales_trend", "restock_plan", "slow_movers"] } } }
```

opencode（项目目录下 `opencode.json`；工具名是 `keel_<工具名>`）：

```json
{ "mcp": { "keel": { "type": "remote", "url": "https://<店铺域名>/api/v1/mcp",
    "headers": { "Authorization": "Bearer {env:KEEL_AGENT_KEY}" } } },
  "permission": { "edit": "deny", "bash": "deny", "webfetch": "deny" } }
```

给 agent 配权限的原则不分客户端：**只放行 keel 的工具**，关掉 shell、写文件、上网 —— AI 员工的一切动作都应该经过 Keel
（判权、审计、提案），而不是绕到别处。
