# 代码生成器选型 · 用真契约试出来的（2026-09-25）

## 结论

| 侧 | 选定 | 版本 | 理由一句话 |
|---|---|---|---|
| Go | `oapi-codegen` | v2.8.0 | 真正读懂 3.1 的 `type: [T,'null']`，且原生出 Gin server |
| TS | `openapi-typescript` | v7.13.0 | `merchant_id?: number \| null` 一次到位 |

**契约不降级。** `openapi: 3.1.0` 原样保留，`docs/电商系统-OpenAPI.yaml` 一个字没改。

---

## 为什么先做这件事

契约是 3.1，而多数生成器只完整支持 3.0。后面 8 个任务全都建在生成器的产物上——
服务端骨架怎么配、客户端怎么生成，都由这个选择决定。**在骨架搭起来之前把它定死，
比搭到一半发现生成器吃不下要便宜得多。**

更重要的是：这件事的风险**不是"生成失败"，而是"生成成功但生成错了"**。
生成失败会报错，人一定看得见；生成成功却把可空字段生成成非指针类型，
不报任何错，直到线上客户端拿到 `null` 解析失败才暴露。
所以本次验证**不看退出码，只看产物**。

---

## 契约里的 3.1-only 写法：实际有 7 处，不是 4 处

实施计划写的是「4 处」。实际 grep 下来是 **7 处**，分布在 5 个 schema：

| # | 位置 | 行 | 写法 |
|---|---|---|---|
| 1 | `Staff.merchant_id` | 2650 | `type: [integer, 'null']` |
| 2 | `Problem.type` | 2027 | `examples: [...]` |
| 3 | `Problem.title` | 2028 | `examples: [...]` |
| 4 | `Problem.status` | 2029 | `examples: [409]` |
| 5 | `Sku.spec_values` | 2111 | `examples: [{...}]` |
| 6 | `ChatReply.intent` | 2164 | `examples: [{...}]` |
| 7 | `UserProfile.phone` | 2747 | `examples: ['138****8000']` |

计划把 `Problem` 的 3 处算成了 1 处。**7 处全部逐一验证过**，无遗漏。

---

## 逐处验证结果

### 1. `type: [integer, 'null']` —— 两侧都正确

Go（`oapi-codegen v2.8.0`）：

```go
type Staff struct {
	// MerchantId 所属商家。**为 null 表示平台级操作员**——他不属于任何一家店，
	// 能建商家、做跨租户运维。
	MerchantId *int64  `json:"merchant_id,omitempty"`
```

TS（`openapi-typescript v7.13.0`）：

```ts
Staff: {
    /**
     * Format: int64
     * @description 所属商家。**为 null 表示平台级操作员**...
     */
    merchant_id?: number | null;
```

**但 `*int64` 这个结果本身不足以证明生成器读懂了 `'null'`。**
`merchant_id` 不在 `required` 里，光是"可选"就足以让 oapi-codegen 生成指针——
指针可能是可选性带来的，跟可空性无关。这是个会骗人的证据。

所以另做了一个隔离实验，把可选性和可空性拆开：

```yaml
T:
  required: [req_nullable, req_plain]
  properties:
    req_nullable:  { type: [integer, 'null'], format: int64 }
    req_plain:     { type: integer, format: int64 }
    opt_nullable:  { type: [integer, 'null'], format: int64 }
    opt_plain:     { type: integer, format: int64 }
```

生成：

```go
type T struct {
	OptNullable *int64  `json:"opt_nullable,omitempty"`
	OptPlain    *int64  `json:"opt_plain,omitempty"`
	ReqNullable *int64  `json:"req_nullable"`       // ← 必填但可空：仍是指针，且无 omitempty
	ReqPlain    int64   `json:"req_plain"`          // ← 必填非空：值类型
}
```

`req_nullable` 必填却仍生成指针，而 `req_plain` 是值类型——**证明 v2.8.0 确实解析了
`type` 数组里的 `'null'`，不是碰巧**。

### 2. 六处 `examples:` 数组 —— 降级为注释，无副作用

`examples` 不参与类型推导，两侧都只把它落成文档：

```go
// Status Examples: 409
Status int `json:"status"`
// SpecValues Examples: {"尺码":"XL","颜色":"黑"}
SpecValues *map[string]string `json:"spec_values,omitempty"`
```

```ts
/**
 * @description 脱敏返回，如 138****8000。完整号码只在绑定流程中由用户自己输入
 * @example 138****8000
 */
phone?: string;
```

无报错、无类型失真。这 6 处零风险。

---

## 不止看产物，还跑了一遍

「生成出来」和「能用」是两件事，所以又验了三层：

| 验证 | 命令 | 结果 |
|---|---|---|
| Go 产物能编译 | `go build ./...`（含 `-generate gin,types,spec` 全量 5112 行） | 通过 |
| TS 产物能通过严格类型检查 | `tsc --noEmit --strict`（TypeScript 7.0.2） | 通过 |
| 可空字段真能收下 `null` | `go test`，见下 | 通过 |

最后一项是关键——**指针类型只是"看起来对"，要真喂一个 `null` 进去才算数**：

```
decode({"merchant_id":null,...})  → MerchantId = nil    ✅ 不报错
decode(字段缺席)                    → MerchantId = nil    ✅ 不报错
encode(MerchantId = nil)          → {"created_at":...,"email":...,"id":1,"role":1,"status":1}
```

前两行是本次要排除的那个静默错误，已排除。

---

## 一个残留的小口子（不阻塞，记下来）

看上面第三行：平台级操作员（`MerchantId = nil`）序列化出去时，
`merchant_id` 字段是**整个消失**，而不是 `"merchant_id": null`。
因为 `merchant_id` 不在 `required` 里，生成的 tag 带了 `omitempty`。

契约的描述写的是「**为 null 表示平台级操作员**」，但服务端实际发出去的是「字段不存在」。

**不影响互通**：TS 侧类型是 `merchant_id?: number | null`，两种形态都收得下；
Go 客户端也是两种都解析成 `nil`。所以这不是 bug，是措辞与实现的偏差。

**想让线上真发 `null`**，只需把 `merchant_id` 放进 `Staff.required`——
上面的隔离实验已经证明，必填+可空会生成 `json:"req_nullable"`（无 `omitempty`），
`nil` 就会老老实实编码成 `null`。

**本次不改**：这是契约语义调整，不属于"生成器吃不下才动契约"的授权范围，
留给需要它的人显式决定。

---

## 两条退路都没用上

计划里给了两条退路，这里记下**为什么都没走**，省得下次重新论证：

| 退路 | 代价 | 判定 |
|---|---|---|
| 降级契约到 3.0（`nullable: true` + `openapi: 3.0.3`） | 失去 3.1 的 JSON Schema 对齐 | **不需要**——生成器本来就吃得下，为不存在的问题付代价 |
| 换 `ogen` 等其他生成器 | Gin 集成可能要自己写适配 | **不需要**——oapi-codegen 原生出 Gin，`-generate gin,types,spec` 已验证可编译 |

---

## `tools/tools.go`：钉住了版本，但有两个代价要知道

按计划建了 `tools/tools.go`，三个工具的版本已进 `go.mod`：

```
github.com/oapi-codegen/oapi-codegen/v2 v2.8.0
github.com/pressly/goose/v3             v3.28.0
github.com/sqlc-dev/sqlc                v1.31.1
```

`go run github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen --version` → `v2.8.0`，
钉版本的目的达到了，CI 与本地不会再靠 `@latest` 碰运气。

**两个副作用，后续任务会撞上：**

**① Go 版本下限被抬到 1.26.0。** `go mod init` 时是 `go 1.25.6`，
`go mod tidy` 后变成 `go 1.26.0`——因为 `sqlc v1.31.1` 和 `goose v3.28.0` 都要求 1.26。
本机装的是 go1.25.6，执行时会自动下载 go1.26.0 工具链。
**配 CI 的那个任务必须用 Go 1.26+，不能照抄 1.25。**

**② 工具的依赖全都灌进了主模块。** 一行业务代码都还没有，`go.mod` 已经 117 行、
`go.sum` 506 行、依赖图 277 个模块（ClickHouse、Azure SDK 这些全是 sqlc 拖进来的）。
它们不参与编译，但会进供应链扫描和依赖审计的噪音里。

这是 tools.go 这个模式的固有代价，不是做错了。真嫌吵的话有两条路——
把工具拆成独立的 `tools/go.mod` 子模块，或者改用 Go 1.24+ 的 `tool` 指令。
**本次按计划照做，不擅自改设计**，把选项记在这里供后续决定。

另外提醒一句：`tools.go` **永远编译不过**（`import ... is a program, not an importable
package`），这是该模式的正常行为——它只为让 `go mod tidy` 留住依赖而存在。
**别往 CI 里加 `go build -tags tools`。**

---

## 复现方式

```bash
go install github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@v2.8.0
oapi-codegen -package probe -generate types docs/电商系统-OpenAPI.yaml > /tmp/types.gen.go
grep -n "MerchantId" /tmp/types.gen.go        # 期望 *int64

npx openapi-typescript@7.13.0 docs/电商系统-OpenAPI.yaml -o /tmp/schema.d.ts
grep -n "merchant_id" /tmp/schema.d.ts        # 期望 number | null
```
