# 代码生成器选型 · 用真契约试出来的（2026-09-25）

## 结论

| 侧 | 选定 | 版本 | 理由一句话 |
|---|---|---|---|
| Go | `oapi-codegen` | v2.8.0 | 真正读懂 3.1 的 `type: [T,'null']`，且原生出 Gin server |
| TS | `openapi-typescript` | v7.13.0 | `merchant_id: number \| null` 一次到位 |

**契约保持 `openapi: 3.1.0`，未降级。** 唯一的契约改动是把 `Staff.merchant_id`
放进 `required`（见 §5），与生成器能力无关。

---

## 1. 为什么先做这件事

契约是 3.1，而多数生成器只完整支持 3.0。后面 8 个任务全都建在生成器的产物上——
服务端骨架怎么配、客户端怎么生成，都由这个选择决定。**在骨架搭起来之前把它定死，
比搭到一半发现生成器吃不下要便宜得多。**

更重要的是：这件事的风险**不是「生成失败」，而是「生成成功但生成错了」**。
生成失败会报错，人一定看得见；生成成功却把可空字段生成成非指针类型，
不报任何错，直到线上客户端拿到 `null` 解析失败才暴露。
所以本次验证**不看退出码，只看产物**。

---

## 2. 契约里的 3.1-only 写法清单

### 怎么数的

- `type:` 数组与 `examples:`：`grep -n "type: \[" / "examples:"`
- `$ref` 同级关键字：文本 grep 数不出来，用 YAML 解析器走了一遍 AST，
  找所有「既有 `$ref` 又有其它键」的映射节点（脚本见 §7）

### 清单（16 处，分三类）

| 类别 | 处数 | 位置（行号） |
|---|---|---|
| A. `type: [T, 'null']` | 1 | 2650 |
| B. `examples:` 数组 | 6 | 2027, 2028, 2029, 2111, 2164, 2747 |
| C. `$ref` 带同级关键字 | 9 | 101, 677, 1705, 2293, 2316, 2496, 2545, 2548, 2553 |

**关于这个数字的诚实说明：** 实施计划写的是「4 处」，第一版本文档写的是「7 处」并
断言「无遗漏」——**那个断言当时不成立**，C 类 9 处是复审时才发现的。
现在这张表是**按已知的三类 3.1-only 写法枚举的结果**，不等于契约里不存在第四类写法。
本文档不再声称穷尽；要复核请重跑 §7 的脚本。

C 类为什么是 3.1-only：在 3.0 里 `$ref` 的同级关键字**必须被忽略**，
到了 3.1（JSON Schema 2020-12）才有意义。其中 2 处（101 / 677 的 `default: 1`）
**有行为含义**，另外 7 处是 `description`。

---

## 3. 逐类验证结果

### A. `type: [integer, 'null']`（`Staff.merchant_id`）—— 两侧都正确

Go：

```go
// MerchantId 所属商家。**为 null 表示平台级操作员**——他不属于任何一家店，
// 能建商家、做跨租户运维。
MerchantId *int64  `json:"merchant_id"`
```

TS：

```ts
merchant_id: number | null;
```

**但 `*int64` 这个证据本身会骗人。** 改 `required` 之前，`merchant_id` 是可选字段，
而 oapi-codegen 对**任何**可选字段都生成指针——指针可能纯粹来自可选性，
跟它读没读懂 `'null'` 一点关系都没有。

所以另做了一个隔离实验，把可选性与可空性拆成四象限：

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
	ReqNullable *int64  `json:"req_nullable"`      // 必填+可空 → 指针，且无 omitempty
	ReqPlain    int64   `json:"req_plain"`         // 必填+非空 → 值类型
}
```

`req_nullable` 必填却仍是指针，而 `req_plain` 是值类型——**这才证明 v2.8.0 确实
解析了 `type` 数组里的 `'null'`，不是撞上的。**

### B. 六处 `examples:` —— 降级为注释，无类型失真

`examples` 不参与类型推导，两侧都只落成文档：

```go
// Status Examples: 409
Status int `json:"status"`
```

```ts
/** @example 138****8000 */
phone?: string;
```

无报错、无类型失真。这 6 处零风险。

### C. 九处 `$ref` 同级关键字 —— 7 处两侧都对，2 处只有 TS 落实

| 行 | 字段 | 同级键 | Go | TS |
|---|---|---|---|---|
| 101 | `provider` | `default: 1` | ✗ 不落实 | ✓ `/** @default 1 */` |
| 677 | `provider` | `default: 1` | ✗ 不落实 | ✓ `/** @default 1 */` |
| 1705 | 审核 `freight_cents` | `description` | ✓ | ✓ |
| 2293 | `expected_payable_cents` | `description` | ✓ | ✓ |
| 2316 | `discount_cents` | `description` | ✓ | ✓ |
| 2496 | `RefundItem.amount_cents` | `description` | ✓ | ✓ |
| 2545 | `goods_amount_cents` | `description` | ✓ | ✓ |
| 2548 | `Refund.freight_cents` | `description` | ✓ | ✓ |
| 2553 | `Refund.amount_cents` | `description` | ✓ | ✓ |

7 处 `description` 两侧都正确挂到了引用处，而不是被 `$ref` 目标自己的 description
顶掉。例如 2293 的 Go 产物拿到的是同级描述，不是 `Money` 的描述：

```go
// ExpectedPayableCents 前端展示的应付金额。服务端试算不一致时返回 409，
// 防止价格变动导致用户以旧价成交。
ExpectedPayableCents *Money `json:"expected_payable_cents,omitempty"`
```

**两处 `default` 需要说清楚，因为它有行为含义：**
Go 侧生成的是 `Provider *IdentityProvider`，不带任何默认值。
但这**不是 3.1 或 `$ref` 同级键的问题**——单独试了一把就知道：

```go
// 输入：inline_default 是普通的 {type: integer, default: 1}
type T struct {
	InlineDefault     *int `json:"inline_default,omitempty"`   // 同样不落实
	RefSiblingDefault *E   `json:"ref_sibling_default,omitempty"`
}
```

**oapi-codegen 在 `types` 模式下根本不把 `default` 物化进 Go 类型**，跟写法无关。
所以这是生成器的固有行为，不是契约踩了 3.1 的坑。

**后果要记住：实现登录接口的那个任务，得自己把 `provider == nil` 当作 `1` 处理，
别指望生成的类型替你兜底。**

---

## 4. 不止看产物，还跑了一遍

「生成出来」和「能用」是两件事，所以又验了三层：

| 验证 | 方法 | 结果 |
|---|---|---|
| Go 产物能编译 | 全量 `-generate gin,types,spec`（5112 行）丢进临时模块 `go build ./...` | 通过 |
| TS 产物能过严格检查 | `tsc --noEmit --strict`（TypeScript 7.0.2） | 通过 |
| 可空字段真收得下 `null` | `go test` 实际喂 JSON | 通过 |

---

## 5. 契约改动：`Staff.merchant_id` 进 `required`

```diff
-      required: [id, email, role, status, created_at]
+      required: [id, email, role, status, merchant_id, created_at]
```

**改之前**（`merchant_id` 可选）：

```go
MerchantId *int64 `json:"merchant_id,omitempty"`
```
```ts
merchant_id?: number | null;
```

平台级操作员（`MerchantId = nil`）序列化出去，字段**整个消失**：

```
encode(nil) -> {"created_at":...,"email":"a@b.c","id":1,"role":1,"status":1}
```

而契约描述写的是「**为 null 表示平台级操作员**」。措辞与实现对不上。

**改之后**：

```go
MerchantId *int64 `json:"merchant_id"`     // omitempty 没了
```
```ts
merchant_id: number | null;                 // 必填
```
```
encode(nil) -> {...,"merchant_id":null,...}   // 真的发 null 了
decode(null) -> nil                            // 仍然收得下
```

**为什么现在改**：TS 侧从 `merchant_id?:` 变成必填 `merchant_id:`，
对已经写好的客户端是**破坏性变更**。此刻一个客户端都还没有，代价恰好为零；
再往后三个任务就不是了。**这个窗口只在项目第一天存在。**

---

## 6. 工具版本：独立的 `tools/` 子模块

三个构建期工具钉在 **`tools/go.mod`**（不是主模块）：

```
github.com/oapi-codegen/oapi-codegen/v2 v2.8.0
github.com/pressly/goose/v3             v3.28.0
github.com/sqlc-dev/sqlc                v1.31.1
```

**为什么单独一个模块。** 最初按计划把 `tools.go` 放在主模块里，结果是——
一行业务代码都还没有，主模块已被撑到 `go.sum` 506 行、依赖图 **277 个模块**
（ClickHouse 驱动、Azure SDK 全是 sqlc 拖进来的）。它们不参与编译，
但这是个开源项目，供应链扫描与依赖审计从第一天起就要在这堆噪音里捞信号。

拆成子模块之后，主模块回到**零外部依赖**：

| | 拆之前 | 拆之后 |
|---|---|---|
| 主模块 `go.mod` | 117 行 | 3 行 |
| 主模块 `go.sum` | 506 行 | 不存在 |
| 主模块依赖图 | 277 个模块 | 1 个（它自己） |

**版本仍然是钉死的、可复现的**，没有退回 `@latest`——工具经子模块调用：

```bash
cd tools && go run github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen ...
```

这层调用已经由根目录 `Makefile` 包掉，后续任务与 CI 用 `make generate` 即可，
不要再各自拼命令行。输出位置用 `GO_OUT` / `TS_OUT` 覆盖，**相对路径一律相对仓库根解析**
（`make generate GO_OUT=internal/api/openapi.gen.go`）——工具虽然跑在 `tools/` 里，
但 Makefile 用 `go -C` 而非 `cd`，shell 始终留在仓库根，两个变量行为一致。

**Go 版本下限 1.26.0**：`sqlc v1.31.1` 与 `goose v3.28.0` 都声明要求 Go 1.26。
虽然现在只有 `tools/` 模块被传染，但装不了 1.26 就跑不了 `make generate`，
所以这是**全项目的实际下限**，已同步改进 `CONTRIBUTING.md` 与两个 README 徽章
（原先写的 1.23+ 已是假话）。

另外提醒：`tools.go` **永远编译不过**（`import ... is a program, not an
importable package`），这是该模式的正常行为——它只为让 `go mod tidy` 留住依赖
而存在。**别往 CI 里加 `go build -tags tools`。**

---

## 7. 复现

选型结论已固化成一道回归闸，改契约或换生成器版本后跑：

```bash
make contract-check
```

它重新生成两侧产物，并断言 `Staff.merchant_id` 在 Go 侧是无 `omitempty` 的
`*int64`、在 TS 侧是必填的 `number | null`。**这道闸是验证过会失败的**——
把 `merchant_id` 移出 `required` 再跑，它确实报 FAIL 并以非零码退出，
不是一道永远绿的假检查。

重数 3.1-only 写法（C 类靠 grep 数不出来）：

```python
import yaml
class L(yaml.SafeLoader): pass
def mk(loader, node):
    m = loader.construct_mapping(node, deep=True)
    m['__line__'] = node.start_mark.line + 1
    return m
L.add_constructor(yaml.resolver.BaseResolver.DEFAULT_MAPPING_TAG, mk)
doc = yaml.load(open('docs/电商系统-OpenAPI.yaml', encoding='utf-8'), Loader=L)
hits = []
def walk(n):
    if isinstance(n, dict):
        if '$ref' in n and [k for k in n if k not in ('$ref', '__line__')]:
            hits.append(n['__line__'])
        for k, v in n.items():
            if k != '__line__': walk(v)
    elif isinstance(n, list):
        for v in n: walk(v)
walk(doc)
print(sorted(hits), len(hits))
```

---

## 8. 两条退路都没用上

计划给了两条退路，记下**为什么都没走**，省得下次重新论证：

| 退路 | 代价 | 判定 |
|---|---|---|
| 降级契约到 3.0（`nullable: true` + `openapi: 3.0.3`） | 失去 3.1 的 JSON Schema 对齐 | **不需要**——生成器本来就吃得下，为不存在的问题付代价 |
| 换 `ogen` 等其他生成器 | Gin 集成可能要自己写适配 | **不需要**——oapi-codegen 原生出 Gin，`-generate gin,types,spec` 已验证可编译 |
