# M1 工程骨架 实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 把 `GET /products` 一条路从 OpenAPI 契约一直打通到 PostgreSQL，并证明契约在 Go 与 TypeScript 两侧都能生成可用代码。

**Architecture:** 契约是唯一真相源，两侧代码都由它生成。请求经 Gin handler → service → repository，repository 用 `WithTenant` 开事务并 `SET LOCAL app.merchant_id`，RLS 在数据库层兜底。sqlc 产物放在 `internal/repository/internal/db`，让 handler 与 service 在编译期就拿不到它。

**Tech Stack:** Go 1.25、Gin、pgx/v5、sqlc、goose、PostgreSQL 16、TypeScript

**Spec:** `docs/superpowers/specs/2026-09-25-m1-engineering-skeleton-design.md`

## Global Constraints

- 模块路径 `github.com/keel/keel`；Go 1.23+（本机 1.25.6）
- **handler 里不准出现 SQL 和事务**（CONTRIBUTING 硬规矩一）
- **OpenAPI 是唯一真相源**：先改契约 → 生成代码 → 实现（CONTRIBUTING 硬规矩二）
- 数据模型 `docs/电商系统-数据模型设计.md` 是全部业务表 DDL 的唯一真相源
- 金额 `BIGINT` 分、字段后缀 `_cents`；时间 `TIMESTAMPTZ`；枚举 `SMALLINT`
- **每张业务表都带 `merchant_id`**；父子关系用复合外键；RLS 必须 `ENABLE` **且** `FORCE`
- RLS 策略函数必须是 `STABLE`，**不能是 `IMMUTABLE`**（会被折进缓存计划，静默串租户）
- 租户上下文用 `SET LOCAL`，**不能用 `SET`**（会泄漏给同一连接的下一个事务）
- 生成的代码不手改：`internal/api/`、`internal/repository/internal/db/`
- 提交信息用英文，遵循 Conventional Commits
- M1 不接 dtmrs、不建 UI 工程、不写业务逻辑

## Review Focus

以下五条是 spec 隐含、但不刻意设计就会漏掉的失败模式，已各自挂到对应任务：

1. **未知子域名**——`Host` 解析不到任何商家时必须 404，**不能回落到默认商家**（那等于随便谁都能看到默认店的数据）。（Task 4）
2. **商家已停用**——`merchants.status = 2` 时店铺前台应不可访问，而不是照常返回商品。（Task 4）
3. **分页参数越界**——`page=0`、`page_size=10000` 之类，必须有钳制而不是直接塞进 SQL。（Task 5）
4. **`type: [integer,'null']` 在 TS 侧的实际形状**——契约里 `Staff.merchant_id` 用了 3.1 的可空写法，生成器若不支持会静默产出错误类型。（Task 1、Task 7）
5. **迁移重复执行**——goose 在已有数据的库上再跑一次必须是幂等的，尤其 RLS 策略与函数的 `CREATE`。（Task 2）

---

### Task 1: 验证生成器能吃下这份契约

**这是前置风险，必须最先做。** 其余任务都依赖它的结论。

**Files:**
- Create: `docs/decisions/2026-09-25-codegen-choice.md`
- Create: `tools/tools.go`
- Modify: `docs/电商系统-OpenAPI.yaml`（仅在生成器确实吃不下时）

**Interfaces:**
- Consumes: 无（首个任务）
- Produces: 选定的 Go 生成器与 TS 生成器；`tools/tools.go` 钉住 Go 工具版本

- [ ] **Step 1: 装 Go 侧候选生成器**

```bash
go install github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@latest
export PATH="$(go env GOPATH)/bin:$PATH"
oapi-codegen --version
```

- [ ] **Step 2: 拿真契约试生成，观察它对 3.1 的反应**

```bash
mkdir -p /tmp/codegen-probe
oapi-codegen -package probe -generate types \
    docs/电商系统-OpenAPI.yaml > /tmp/codegen-probe/types.gen.go
echo "exit=$?"
```

Expected: 要么成功，要么报错。**两种结果都要记录**，不要只看成功与否——
成功也必须检查下一步那 4 处 3.1-only 写法生成成了什么。

- [ ] **Step 3: 检查那 4 处 3.1-only 写法的产物**

契约里的 4 处分别是：`Staff.merchant_id` 的 `type: [integer, 'null']`，
以及 3 处 `examples:` 数组。

```bash
grep -n "MerchantId" /tmp/codegen-probe/types.gen.go | head -5
```

Expected: `MerchantId` 应当是指针类型（`*int64`）或等价的可空表示。
**若生成成非指针的 `int64`，就是静默错误**——契约说它可以是 null，
而生成的类型表达不了，客户端拿到 null 会解析失败。

- [ ] **Step 4: 试 TS 侧生成器**

```bash
cd /tmp/codegen-probe
npx --yes openapi-typescript@latest \
    /home/jeffwang/work/keel/docs/电商系统-OpenAPI.yaml -o schema.d.ts
grep -n "merchant_id" schema.d.ts | head -3
```

Expected: `merchant_id?: number | null`。若只有 `number`，同样是静默错误。

- [ ] **Step 5: 根据结果决定，并写下决策**

创建 `docs/decisions/2026-09-25-codegen-choice.md`，记录：

- 两个生成器的版本与实际输出
- 那 4 处 3.1-only 写法各自生成成了什么
- 最终选择，以及**为什么**
- 若选择降级契约到 3.0 写法，逐条列出改了哪里、为什么可以接受

若 Go 生成器吃不下 3.1，两条退路（择一并写明理由）：

1. 把 `type: [integer,'null']` 改成 3.0 的 `type: integer` + `nullable: true`，
   并把 `openapi: 3.1.0` 降到 `3.0.3`。代价：失去 3.1 的 JSON Schema 对齐。
2. 换 `ogen` 等其他生成器。代价：Gin 集成可能要自己写适配。

- [ ] **Step 6: 钉住 Go 工具版本**

创建 `tools/tools.go`：

```go
//go:build tools

// Package tools 把构建期用到的 Go 工具钉进 go.mod，
// 这样 CI 与本地装的是同一个版本，不靠 @latest 碰运气。
package tools

import (
	_ "github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen"
	_ "github.com/pressly/goose/v3/cmd/goose"
	_ "github.com/sqlc-dev/sqlc/cmd/sqlc"
)
```

- [ ] **Step 7: 提交**

```bash
git add docs/decisions/2026-09-25-codegen-choice.md tools/ docs/电商系统-OpenAPI.yaml
git commit -m "chore: pick the code generators by testing them against the real contract

The contract is OpenAPI 3.1 and uses four constructs 3.0 does not have.
Feeding it to both generators before building anything on them was the
cheapest moment to find out whether that holds."
```

---

### Task 2: Go module、goose 迁移、五张表与 RLS

**Files:**
- Create: `go.mod`、`db/migrations/00001_init.sql`、`db/migrations/00002_rls.sql`
- Create: `internal/db/dsn.go`
- Test: `internal/db/migrate_test.go`

**Interfaces:**
- Consumes: Task 1 选定的工具版本
- Produces: `db.DSN(env string) string`；迁移后的 schema，含 `current_merchant()` 函数

- [ ] **Step 1: 建 module**

```bash
go mod init github.com/keel/keel
go get github.com/jackc/pgx/v5/pgxpool github.com/pressly/goose/v3
```

- [ ] **Step 2: 写第一份迁移（五张表）**

`db/migrations/00001_init.sql`。DDL **逐字取自** `docs/电商系统-数据模型设计.md`，
不要在这里重新设计。本任务只迁移这条链路用得到的：

```sql
-- +goose Up
CREATE TABLE merchants (
    id           BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    code         TEXT        NOT NULL UNIQUE,
    name         TEXT        NOT NULL,
    status       SMALLINT    NOT NULL DEFAULT 1,
    deleted_at   TIMESTAMPTZ,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE shop_settings (
    merchant_id       BIGINT   PRIMARY KEY REFERENCES merchants(id),
    domain            TEXT     UNIQUE,
    logo_url          TEXT,
    currency          TEXT     NOT NULL DEFAULT 'CNY',
    timezone          TEXT     NOT NULL DEFAULT 'Asia/Shanghai',
    auto_confirm_days SMALLINT NOT NULL DEFAULT 7,
    extra             JSONB    NOT NULL DEFAULT '{}',
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE categories (
    id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    merchant_id BIGINT      NOT NULL REFERENCES merchants(id),
    parent_id   BIGINT      REFERENCES categories(id),
    name        TEXT        NOT NULL,
    path        TEXT        NOT NULL,
    level       SMALLINT    NOT NULL DEFAULT 1,
    sort_order  INT         NOT NULL DEFAULT 0,
    status      SMALLINT    NOT NULL DEFAULT 1,
    deleted_at  TIMESTAMPTZ,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (id, merchant_id)
);

CREATE TABLE products (
    id              BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    merchant_id     BIGINT      NOT NULL REFERENCES merchants(id),
    category_id     BIGINT      NOT NULL,
    brand_id        BIGINT,
    title           TEXT        NOT NULL,
    subtitle        TEXT,
    description     TEXT,
    min_price_cents BIGINT      NOT NULL DEFAULT 0,
    max_price_cents BIGINT      NOT NULL DEFAULT 0,
    total_stock     INT         NOT NULL DEFAULT 0,
    sales_count     INT         NOT NULL DEFAULT 0,
    status          SMALLINT    NOT NULL DEFAULT 0,
    published_at    TIMESTAMPTZ,
    deleted_at      TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (id, merchant_id),
    FOREIGN KEY (category_id, merchant_id) REFERENCES categories(id, merchant_id)
);
CREATE INDEX idx_products_listing
    ON products(merchant_id, category_id, status, published_at DESC)
    WHERE deleted_at IS NULL;

CREATE TABLE skus (
    id           BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    merchant_id  BIGINT      NOT NULL REFERENCES merchants(id),
    product_id   BIGINT      NOT NULL,
    sku_code     TEXT        NOT NULL,
    spec_values  JSONB       NOT NULL DEFAULT '{}',
    price_cents  BIGINT      NOT NULL,
    cost_cents   BIGINT      NOT NULL DEFAULT 0,
    weight_gram  INT         NOT NULL DEFAULT 0,
    image_url    TEXT,
    status       SMALLINT    NOT NULL DEFAULT 1,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT chk_price_nonneg CHECK (price_cents >= 0),
    UNIQUE (id, merchant_id),
    FOREIGN KEY (product_id, merchant_id) REFERENCES products(id, merchant_id)
);
CREATE UNIQUE INDEX uk_skus_code ON skus(merchant_id, sku_code);

-- +goose Down
DROP TABLE skus;
DROP TABLE products;
DROP TABLE categories;
DROP TABLE shop_settings;
DROP TABLE merchants;
```

- [ ] **Step 2b: 写第二份迁移（RLS）**

`db/migrations/00002_rls.sql`。注意 plpgsql 函数体里有分号，
**必须用 goose 的语句块标记**，否则 goose 会在分号处切断：

```sql
-- +goose Up
-- +goose StatementBegin
CREATE FUNCTION current_merchant() RETURNS BIGINT
LANGUAGE plpgsql STABLE AS $$
DECLARE v TEXT := nullif(current_setting('app.merchant_id', true), '');
BEGIN
    IF v IS NULL THEN
        RAISE EXCEPTION '租户上下文未设置：事务开始时必须 SET LOCAL app.merchant_id'
            USING ERRCODE = 'insufficient_privilege';
    END IF;
    RETURN v::BIGINT;
END $$;
-- +goose StatementEnd

ALTER TABLE categories ENABLE ROW LEVEL SECURITY;
ALTER TABLE categories FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant ON categories USING (merchant_id = current_merchant());

ALTER TABLE products ENABLE ROW LEVEL SECURITY;
ALTER TABLE products FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant ON products USING (merchant_id = current_merchant());

ALTER TABLE skus ENABLE ROW LEVEL SECURITY;
ALTER TABLE skus FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant ON skus USING (merchant_id = current_merchant());

-- +goose Down
DROP POLICY tenant ON skus;
DROP POLICY tenant ON products;
DROP POLICY tenant ON categories;
DROP FUNCTION current_merchant();
```

> `merchants` 与 `shop_settings` **不加 RLS**：租户解析中间件要在知道租户之前
> 先查它们，加了 RLS 就成了鸡生蛋。它们由应用层保证只按 code / domain 精确查询。

- [ ] **Step 3: 写 DSN 助手**

`internal/db/dsn.go`：

```go
// Package db 提供数据库连接相关的公共设施。
package db

import (
	"fmt"
	"os"
)

// DSN 从环境变量拼出连接串。CI 与本地 docker compose 用同一套变量名。
func DSN() string {
	get := func(k, def string) string {
		if v := os.Getenv(k); v != "" {
			return v
		}
		return def
	}
	return fmt.Sprintf("postgres://%s:%s@%s:%s/%s?sslmode=disable",
		get("PGUSER", "keel"), get("PGPASSWORD", "keel"),
		get("PGHOST", "127.0.0.1"), get("PGPORT", "5432"),
		get("PGDATABASE", "keel"))
}
```

- [ ] **Step 4: 写失败的测试（Review Focus 第 5 条：迁移幂等）**

`internal/db/migrate_test.go`：

```go
package db_test

import (
	"context"
	"os/exec"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/keel/keel/internal/db"
)

// 迁移必须能在已经迁过的库上再跑一次而不报错。
// goose 靠版本表保证这一点，但 RLS 策略与函数的 CREATE 是最容易踩的地方——
// 如果有人把它们写进了会重复执行的位置，这里会红。
func TestMigrateIsIdempotent(t *testing.T) {
	for i := 0; i < 2; i++ {
		out, err := exec.Command("goose", "-dir", "../../db/migrations",
			"postgres", db.DSN(), "up").CombinedOutput()
		if err != nil {
			t.Fatalf("第 %d 次迁移失败: %v\n%s", i+1, err, out)
		}
	}

	conn, err := pgx.Connect(context.Background(), db.DSN())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())

	// current_merchant() 必须是 STABLE —— IMMUTABLE 会被折进缓存计划，
	// 导致同一条预编译语句对所有租户返回第一个租户的数据。
	var volatility string
	err = conn.QueryRow(context.Background(),
		`SELECT provolatile FROM pg_proc WHERE proname = 'current_merchant'`).
		Scan(&volatility)
	if err != nil {
		t.Fatal(err)
	}
	if volatility != "s" {
		t.Fatalf("current_merchant() 的 volatility 是 %q，必须是 \"s\"(STABLE)", volatility)
	}

	// 三张业务表必须同时 ENABLE 且 FORCE —— 只 ENABLE 的话表属主绕过 RLS，
	// 而迁移工具跑出来的属主通常就是应用自己。
	for _, tbl := range []string{"categories", "products", "skus"} {
		var enabled, forced bool
		err := conn.QueryRow(context.Background(),
			`SELECT relrowsecurity, relforcerowsecurity FROM pg_class WHERE relname = $1`,
			tbl).Scan(&enabled, &forced)
		if err != nil {
			t.Fatal(err)
		}
		if !enabled || !forced {
			t.Fatalf("%s: ENABLE=%v FORCE=%v，两者都必须为 true", tbl, enabled, forced)
		}
	}
}
```

- [ ] **Step 5: 运行，确认它失败**

```bash
docker run -d --name keel-pg -e POSTGRES_PASSWORD=keel -e POSTGRES_USER=keel \
    -e POSTGRES_DB=keel -p 5432:5432 postgres:16
go test ./internal/db/ -run TestMigrateIsIdempotent -v
```

Expected: FAIL —— 迁移文件此时还没被 goose 认到，或 `current_merchant` 不存在。

- [ ] **Step 6: 跑迁移让它通过**

```bash
export PATH="$(go env GOPATH)/bin:$PATH"
go test ./internal/db/ -run TestMigrateIsIdempotent -v
```

Expected: PASS。若 `FORCE` 那条断言红了，说明 `00002_rls.sql` 漏了 `FORCE ROW LEVEL SECURITY`——
**这正是它存在的理由**，不要改断言去迁就迁移。

- [ ] **Step 7: 提交**

```bash
git add go.mod go.sum db/ internal/db/
git commit -m "feat: module, goose migrations for the five tables, and RLS

The test asserts FORCE alongside ENABLE and that current_merchant() is
STABLE. Both were mistakes found by verification earlier: ENABLE alone
leaves the table owner reading every tenant, and IMMUTABLE lets the first
tenant's value get folded into a cached plan."
```

---

### Task 3: sqlc 与 WithTenant，并用编译器堵住那个缺口

**Files:**
- Create: `sqlc.yaml`、`db/queries/products.sql`
- Create: `internal/tenant/tenant.go`
- Create: `internal/repository/tenant.go`
- Test: `internal/repository/tenant_test.go`、`internal/handler/isolation_test.go`

**Interfaces:**
- Consumes: `db.DSN()`（Task 2）
- Produces:
  - `tenant.FromContext(ctx) (int64, error)`、`tenant.NewContext(ctx, id) context.Context`
  - `repository.New(pool *pgxpool.Pool) *repository.Repo`
  - `(*Repo).WithTenant(ctx context.Context, fn func(*db.Queries) error) error`
  - sqlc 产物包路径 `github.com/keel/keel/internal/repository/internal/db`

- [ ] **Step 1: 写 sqlc 配置与查询**

`sqlc.yaml`：

```yaml
version: "2"
sql:
  - engine: postgresql
    schema: db/migrations
    queries: db/queries
    gen:
      go:
        package: db
        # 关键：放在 repository/internal 下，让 handler 与 service 在编译期拿不到
        out: internal/repository/internal/db
        sql_package: pgx/v5
        emit_pointers_for_null_types: true
```

`db/queries/products.sql`：

```sql
-- name: ListProducts :many
-- 不带 WHERE merchant_id —— 租户由 RLS 在数据库层过滤。
-- 这不是偷懒：应用层再加一遍条件会让「RLS 是否真的生效」变得测不出来。
SELECT id, title, subtitle, min_price_cents, max_price_cents,
       total_stock, sales_count, status
  FROM products
 WHERE deleted_at IS NULL
   AND status = 1
 ORDER BY published_at DESC NULLS LAST, id DESC
 LIMIT $1 OFFSET $2;
```

- [ ] **Step 2: 生成**

```bash
export PATH="$(go env GOPATH)/bin:$PATH"
sqlc generate
ls internal/repository/internal/db/
```

Expected: 出现 `db.go` / `models.go` / `products.sql.go`。

- [ ] **Step 3: 写租户上下文包**

`internal/tenant/tenant.go`：

```go
// Package tenant 承载「当前请求属于哪个商家」。
//
// 它只导出读取入口。写入由中间件完成，业务代码拿不到构造方式——
// 这样「传错租户」这个动作在类型层面就不存在。
package tenant

import (
	"context"
	"errors"
)

type ctxKey struct{}

// ErrNoTenant 表示请求上下文里没有租户。这总是一个 bug，不是可恢复的业务错误。
var ErrNoTenant = errors.New("请求上下文中没有租户；中间件是否未挂载？")

// NewContext 由租户解析中间件调用。
func NewContext(ctx context.Context, merchantID int64) context.Context {
	return context.WithValue(ctx, ctxKey{}, merchantID)
}

// FromContext 取当前租户。取不到返回 ErrNoTenant，绝不返回零值。
func FromContext(ctx context.Context) (int64, error) {
	v, ok := ctx.Value(ctxKey{}).(int64)
	if !ok || v <= 0 {
		return 0, ErrNoTenant
	}
	return v, nil
}
```

- [ ] **Step 4: 写 WithTenant**

`internal/repository/tenant.go`：

```go
// Package repository 是数据访问层。
//
// sqlc 产物在 internal/db 之下，Go 的路径规则让这个包之外无法 import 它。
// 于是业务代码只能经由 WithTenant 拿到 Queries，而 WithTenant 保证
// 每次访问都在一个设过 SET LOCAL app.merchant_id 的事务里。
package repository

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/keel/keel/internal/repository/internal/db"
	"github.com/keel/keel/internal/tenant"
)

type Repo struct{ pool *pgxpool.Pool }

func New(pool *pgxpool.Pool) *Repo { return &Repo{pool: pool} }

// Queries 是给业务层看的类型别名，这样 service 的签名里不必出现 internal 包。
type Queries = db.Queries

// WithTenant 在一个设好租户上下文的事务里执行 fn。
//
// 租户从 ctx 取，不从参数传 —— 调用方没有那个参数可以传错。
func (r *Repo) WithTenant(ctx context.Context, fn func(*Queries) error) error {
	merchantID, err := tenant.FromContext(ctx)
	if err != nil {
		return err
	}

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	// SET LOCAL 不接受参数占位符，只能拼接。merchantID 是 int64 且来自
	// 中间件解析结果，不是用户输入，没有注入面。
	//
	// 必须是 SET LOCAL 而不是 SET：后者会留在连接上，
	// 被连接池交给下一个请求时就是一次跨租户泄露。
	if _, err := tx.Exec(ctx,
		fmt.Sprintf("SET LOCAL app.merchant_id = '%d'", merchantID)); err != nil {
		return err
	}

	if err := fn(db.New(tx)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
```

- [ ] **Step 5: 写编译期隔离的测试**

`internal/handler/isolation_test.go`：

```go
package handler_test

import (
	"os/exec"
	"strings"
	"testing"
)

// handler 与 service 必须无法 import sqlc 产物。
//
// 这条不是风格偏好：sqlc 的 DBTX 接口同时被 *pgxpool.Pool 满足，
// 所以 db.New(pool) 是能编译过的——而那条路径没有事务，也就没有
// SET LOCAL，租户过滤只剩 RLS 一层。把产物放进 repository/internal 之后，
// 这个错误的写法在编译期就不存在了。这个测试守住那个结构。
func TestGeneratedDBIsUnreachableFromHandler(t *testing.T) {
	const pkg = "github.com/keel/keel/internal/repository/internal/db"
	tmp := t.TempDir()
	src := `package probe

import _ "` + pkg + `"
`
	if err := writeFile(tmp+"/probe.go", src); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("go", "build", "./...")
	cmd.Dir = tmp
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("从 %s 之外竟然能 import 生成的 db 包 —— internal 隔离没生效", tmp)
	}
	if !strings.Contains(string(out), "internal") {
		t.Logf("构建失败信息（应提到 internal）：%s", out)
	}
}

func writeFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o644)
}
```

import 块是 `"os"` / `"os/exec"` / `"strings"` / `"testing"`。

> 临时目录里没有 go.mod，`go build` 会先报模块错误而不是 internal 错误。
> 所以在写 `probe.go` 之前还要写一个最小的 go.mod：
>
> ```go
> if err := writeFile(tmp+"/go.mod", "module probe\n\ngo 1.23\n"); err != nil {
>     t.Fatal(err)
> }
> ```

- [ ] **Step 6: 写 WithTenant 的行为测试**

`internal/repository/tenant_test.go`：

```go
package repository_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/keel/keel/internal/db"
	"github.com/keel/keel/internal/repository"
	"github.com/keel/keel/internal/tenant"
)

func pool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	p, err := pgxpool.New(context.Background(), db.DSN())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	return p
}

// 没有租户上下文时必须直接失败，不能退化成「查全部」。
func TestWithTenantRefusesMissingTenant(t *testing.T) {
	r := repository.New(pool(t))
	err := r.WithTenant(context.Background(), func(q *repository.Queries) error {
		t.Fatal("不应该执行到这里")
		return nil
	})
	if !errors.Is(err, tenant.ErrNoTenant) {
		t.Fatalf("期望 ErrNoTenant，实得 %v", err)
	}
}

// 租户上下文必须随事务结束而复位：同一个池上的下一次无租户调用仍然失败。
func TestTenantDoesNotLeakToNextCall(t *testing.T) {
	p := pool(t)
	r := repository.New(p)

	ctx := tenant.NewContext(context.Background(), 1)
	if err := r.WithTenant(ctx, func(q *repository.Queries) error { return nil }); err != nil {
		t.Fatal(err)
	}

	err := r.WithTenant(context.Background(), func(q *repository.Queries) error {
		t.Fatal("不应该执行到这里")
		return nil
	})
	if !errors.Is(err, tenant.ErrNoTenant) {
		t.Fatalf("上一次调用的租户可能泄漏了：期望 ErrNoTenant，实得 %v", err)
	}
}
```

- [ ] **Step 7: 运行两组测试**

```bash
go test ./internal/repository/ ./internal/handler/ -v
```

Expected: 全部 PASS。

- [ ] **Step 8: 提交**

```bash
git add sqlc.yaml db/queries internal/
git commit -m "feat: sqlc, tenant context, and WithTenant as the only way in

The generated package sits under repository/internal so handler and
service cannot import it at all. sqlc's DBTX is satisfied by a pool as
well as a transaction, so db.New(pool) compiles and quietly skips
SET LOCAL; making that import impossible turns a runtime refusal into a
compile error."
```

---

### Task 4: 租户解析中间件

**Files:**
- Create: `internal/tenant/resolver.go`
- Test: `internal/tenant/resolver_test.go`

**Interfaces:**
- Consumes: `tenant.NewContext`（Task 3）
- Produces: `tenant.Resolver` 与 `func (*Resolver) Middleware() gin.HandlerFunc`

- [ ] **Step 1: 写失败的测试（含 Review Focus 第 1、2 条）**

`internal/tenant/resolver_test.go`：

```go
package tenant_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/keel/keel/internal/db"
	"github.com/keel/keel/internal/tenant"
)

func newRouter(t *testing.T, defaultCode string) *gin.Engine {
	t.Helper()
	p, err := pgxpool.New(context.Background(), db.DSN())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)

	gin.SetMode(gin.TestMode)
	r := gin.New()
	res := tenant.NewResolver(p, defaultCode)
	r.Use(res.Middleware())
	r.GET("/probe", func(c *gin.Context) {
		id, err := tenant.FromContext(c.Request.Context())
		if err != nil {
			c.Status(http.StatusInternalServerError)
			return
		}
		c.JSON(http.StatusOK, gin.H{"merchant_id": id})
	})
	return r
}

func do(r *gin.Engine, host string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/probe", nil)
	req.Host = host
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// 单商家部署：配了默认商家，任何 Host 都解析到它。
func TestDefaultMerchantResolves(t *testing.T) {
	if got := do(newRouter(t, "shop-a"), "localhost:8080").Code; got != 200 {
		t.Fatalf("期望 200，实得 %d", got)
	}
}

// 多商家部署：子域名匹配 merchants.code。
func TestSubdomainResolves(t *testing.T) {
	if got := do(newRouter(t, ""), "shop-b.example.com").Code; got != 200 {
		t.Fatalf("期望 200，实得 %d", got)
	}
}

// Review Focus 第 1 条：未知子域名必须 404，**不能回落到默认商家**。
// 回落等于任何人拼一个不存在的子域名就能看到默认店的数据。
func TestUnknownHostIs404NotFallback(t *testing.T) {
	if got := do(newRouter(t, "shop-a"), "nope.example.com").Code; got != 404 {
		t.Fatalf("未知 Host 期望 404，实得 %d —— 是否错误地回落到了默认商家？", got)
	}
}

// Review Focus 第 2 条：停用的商家不可访问。
func TestDisabledMerchantIs404(t *testing.T) {
	if got := do(newRouter(t, ""), "shop-closed.example.com").Code; got != 404 {
		t.Fatalf("停用商家期望 404，实得 %d", got)
	}
}
```

- [ ] **Step 2: 运行，确认它失败**

```bash
go test ./internal/tenant/ -v
```

Expected: FAIL，`tenant.NewResolver` 未定义。

- [ ] **Step 3: 实现**

`internal/tenant/resolver.go`：

```go
package tenant

import (
	"context"
	"net/http"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Resolver 把请求映射到商家。
//
// 租户来源只有两种，都不可由客户端伪造：
//   - 配置的默认商家（单商家部署，docker compose 的常规形态）
//   - Host 头（多商家部署）
//
// 刻意不支持用请求头指定租户。公开接口没有鉴权，
// 那等于让调用方自己声明它是哪家店。
type Resolver struct {
	pool        *pgxpool.Pool
	defaultCode string

	mu    sync.RWMutex
	cache map[string]int64
}

func NewResolver(pool *pgxpool.Pool, defaultCode string) *Resolver {
	return &Resolver{pool: pool, defaultCode: defaultCode, cache: map[string]int64{}}
}

func (r *Resolver) Middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		key := r.defaultCode
		if key == "" {
			key = hostKey(c.Request.Host)
		}
		id, err := r.lookup(c.Request.Context(), key, r.defaultCode == "")
		if err != nil {
			// 解析不到就是 404：这个店不存在，而不是「服务器出错」。
			c.AbortWithStatus(http.StatusNotFound)
			return
		}
		c.Request = c.Request.WithContext(NewContext(c.Request.Context(), id))
		c.Next()
	}
}

// hostKey 从 Host 里取出用于匹配的键：去掉端口，取第一段作为子域名。
func hostKey(host string) string {
	if i := strings.IndexByte(host, ':'); i >= 0 {
		host = host[:i]
	}
	if i := strings.IndexByte(host, '.'); i >= 0 {
		return host[:i]
	}
	return host
}

func (r *Resolver) lookup(ctx context.Context, key string, byHost bool) (int64, error) {
	r.mu.RLock()
	id, ok := r.cache[key]
	r.mu.RUnlock()
	if ok {
		return id, nil
	}

	// status = 1 才算可访问：停用的商家不该还能被逛。
	// 先按 code 匹配，再按自定义域名匹配。
	const q = `
	SELECT m.id FROM merchants m
	 WHERE m.deleted_at IS NULL AND m.status = 1
	   AND (m.code = $1 OR EXISTS (
	         SELECT 1 FROM shop_settings s
	          WHERE s.merchant_id = m.id AND s.domain = $2))
	 LIMIT 1`
	err := r.pool.QueryRow(ctx, q, key, key).Scan(&id)
	if err != nil {
		if err == pgx.ErrNoRows {
			return 0, err
		}
		return 0, err
	}
	r.mu.Lock()
	r.cache[key] = id
	r.mu.Unlock()
	return id, nil
}
```

> `merchants` 与 `shop_settings` 没有 RLS，正是为了这一步——
> 租户解析必须在知道租户之前完成，否则是鸡生蛋。见 Task 2 的说明。

- [ ] **Step 4: 加种子数据供测试使用**

测试需要 `shop-a`（正常）、`shop-b`（正常，带自定义域名）、
`shop-closed`（`status = 2`）。在 `db/migrations` 之外单独放，
避免把测试数据混进迁移：`db/seed/dev.sql`

```sql
INSERT INTO merchants (code, name, status) VALUES
    ('shop-a', '示例店 A', 1),
    ('shop-b', '示例店 B', 1),
    ('shop-closed', '已停用的店', 2)
ON CONFLICT (code) DO NOTHING;

INSERT INTO shop_settings (merchant_id, domain)
SELECT id, code || '.example.com' FROM merchants
ON CONFLICT (merchant_id) DO NOTHING;
```

在 `internal/tenant/resolver_test.go` 里加载它（额外 import `"os"` 与 `"os/exec"`）：

```go
func TestMain(m *testing.M) {
	out, err := exec.Command("psql", db.DSN(), "-f", "../../db/seed/dev.sql").CombinedOutput()
	if err != nil {
		panic(string(out))
	}
	os.Exit(m.Run())
}
```

- [ ] **Step 5: 运行，确认通过**

```bash
go test ./internal/tenant/ -v
```

Expected: 四个测试全 PASS。

- [ ] **Step 6: 把多商家形态写进契约的 servers 块**

租户定位是部署配置，**不改任何路径**；但多商家形态要在契约里可见，
否则读契约的人不知道这套 API 怎么定位店铺。修改 `docs/电商系统-OpenAPI.yaml`：

```yaml
servers:
  - url: http://localhost:8080/api/v1
    description: 本地（单商家；租户由 KEEL_DEFAULT_MERCHANT 指定，请求不必携带任何东西）
  - url: https://{shop}.example.com/api/v1
    description: 多商家；租户由子域名确定，也可为商家绑定自定义域名
    variables:
      shop:
        default: demo
        description: 商家的 code
```

改完跑 `python3 scripts/check_openapi.py`，确认结构仍然有效。

- [ ] **Step 7: 提交**

```bash
git add internal/tenant/ db/seed/ docs/电商系统-OpenAPI.yaml
git commit -m "feat: resolve the tenant from configuration or Host

Two sources only, neither forgeable by the caller: a configured default
merchant for single-tenant deployments, and the Host header otherwise. An
unknown host is a 404 rather than a fall back to the default, since
falling back would hand the default shop's data to anyone who invents a
subdomain. A disabled merchant is a 404 as well."
```

---

### Task 5: handler、service 与 GET /products 打通

**Files:**
- Create: `internal/api/generate.go`、`internal/service/product.go`、`internal/handler/product.go`
- Create: `cmd/keel/main.go`
- Test: `internal/handler/product_test.go`

**Interfaces:**
- Consumes: `repository.New`、`(*Repo).WithTenant`、`tenant.NewResolver`
- Produces: 可运行的 HTTP 服务；`GET /healthz` 与 `GET /api/v1/products`

- [ ] **Step 1: 配置契约生成**

`internal/api/generate.go`：

```go
// Package api 存放由 OpenAPI 契约生成的类型与服务端接口。
//
// 不要手改本包内 *.gen.go —— 改契约，然后 go generate ./...
package api

//go:generate oapi-codegen -config ../../oapi-codegen.yaml ../../docs/电商系统-OpenAPI.yaml
```

`oapi-codegen.yaml`（放仓库根）：

```yaml
package: api
output: internal/api/openapi.gen.go
generate:
  models: true
  gin-server: true
  strict-server: true
output-options:
  skip-prune: true
```

> 若 Task 1 的结论是换了别的生成器，这一步按那份决策记录调整，
> 并在提交信息里说明。

- [ ] **Step 2: 写失败的集成测试（含 Review Focus 第 3 条）**

`internal/handler/product_test.go`：

```go
package handler_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

type listResp struct {
	Items []struct {
		ID    int64  `json:"id"`
		Title string `json:"title"`
	} `json:"items"`
}

func get(t *testing.T, host, path string) (*httptest.ResponseRecorder, listResp) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Host = host
	w := httptest.NewRecorder()
	testRouter(t).ServeHTTP(w, req)
	var body listResp
	if w.Code == http.StatusOK {
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("响应不是预期结构: %v\n%s", err, w.Body.String())
		}
	}
	return w, body
}

// 两个租户各自只看得到自己的商品，且 id 集合无交集。
// 这一条是整套多租户设计唯一真正要命的地方，而它在单租户测试数据下
// 完全看不出来——加不加 WHERE merchant_id 结果一模一样。
func TestTenantsSeeOnlyTheirOwnProducts(t *testing.T) {
	wa, a := get(t, "shop-a.example.com", "/api/v1/products")
	wb, b := get(t, "shop-b.example.com", "/api/v1/products")
	if wa.Code != 200 || wb.Code != 200 {
		t.Fatalf("状态码 a=%d b=%d", wa.Code, wb.Code)
	}
	if len(a.Items) == 0 || len(b.Items) == 0 {
		t.Fatalf("种子数据不足：a=%d b=%d 件；跨租户测试需要两边都有商品",
			len(a.Items), len(b.Items))
	}
	seen := map[int64]bool{}
	for _, it := range a.Items {
		seen[it.ID] = true
	}
	for _, it := range b.Items {
		if seen[it.ID] {
			t.Fatalf("商品 %d 同时出现在两个租户的结果里 —— RLS 没生效", it.ID)
		}
	}
}

// Review Focus 第 3 条：分页参数越界必须被钳制，而不是直接进 SQL。
func TestPaginationIsClamped(t *testing.T) {
	for _, q := range []string{"?page=0", "?page=-1", "?page_size=100000", "?page_size=0"} {
		w, _ := get(t, "shop-a.example.com", "/api/v1/products"+q)
		if w.Code != http.StatusOK && w.Code != http.StatusBadRequest {
			t.Fatalf("%s 期望 200 或 400，实得 %d", q, w.Code)
		}
	}
}
```

`internal/handler/main_test.go` —— 建池、加载种子、装中间件与路由：

```go
package handler_test

import (
	"context"
	"os"
	"os/exec"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/keel/keel/internal/db"
	"github.com/keel/keel/internal/handler"
	"github.com/keel/keel/internal/repository"
	"github.com/keel/keel/internal/service"
	"github.com/keel/keel/internal/tenant"
)

var testPool *pgxpool.Pool

func TestMain(m *testing.M) {
	p, err := pgxpool.New(context.Background(), db.DSN())
	if err != nil {
		panic(err)
	}
	testPool = p

	// 种子用 psql 加载：它是纯 SQL，用 psql 跑比在 Go 里切语句可靠。
	out, err := exec.Command("psql", db.DSN(), "-f", "../../db/seed/dev.sql").CombinedOutput()
	if err != nil {
		panic(string(out))
	}

	code := m.Run()
	p.Close()
	os.Exit(code)
}

// testRouter 装配的链路与 cmd/keel/main.go 一致。
// 刻意不配默认商家 —— 跨租户测试要走 Host 解析这条真实路径。
func testRouter(t *testing.T) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	res := tenant.NewResolver(testPool, "")
	ph := handler.NewProductHandler(service.NewProductService(repository.New(testPool)))
	v1 := r.Group("/api/v1", res.Middleware())
	v1.GET("/products", ph.List)
	return r
}
```

- [ ] **Step 3: 运行，确认它失败**

```bash
go test ./internal/handler/ -run TestTenantsSeeOnly -v
```

Expected: FAIL，路由不存在。

- [ ] **Step 4: 写 service**

`internal/service/product.go`：

```go
// Package service 承载业务规则与事务编排入口。这里不拼 SQL。
package service

import (
	"context"

	"github.com/keel/keel/internal/repository"
)

const (
	defaultPageSize = 20
	maxPageSize     = 100
)

type ProductSummary struct {
	ID            int64
	Title         string
	Subtitle      *string
	MinPriceCents int64
	MaxPriceCents int64
	SalesCount    int32
	Status        int16
}

type ProductService struct{ repo *repository.Repo }

func NewProductService(r *repository.Repo) *ProductService { return &ProductService{repo: r} }

// List 返回当前租户的在架商品。
//
// page / pageSize 在这里钳制，不在 handler：分页规则是业务规则，
// 换一个 handler（比如将来的 gRPC）不该重写一遍。
func (s *ProductService) List(ctx context.Context, page, pageSize int) ([]ProductSummary, error) {
	if page < 1 {
		page = 1
	}
	if pageSize < 1 {
		pageSize = defaultPageSize
	}
	if pageSize > maxPageSize {
		pageSize = maxPageSize
	}

	var out []ProductSummary
	err := s.repo.WithTenant(ctx, func(q *repository.Queries) error {
		rows, err := q.ListProducts(ctx, int32(pageSize), int32((page-1)*pageSize))
		if err != nil {
			return err
		}
		out = make([]ProductSummary, 0, len(rows))
		for _, r := range rows {
			out = append(out, ProductSummary{
				ID: r.ID, Title: r.Title, Subtitle: r.Subtitle,
				MinPriceCents: r.MinPriceCents, MaxPriceCents: r.MaxPriceCents,
				SalesCount: r.SalesCount, Status: r.Status,
			})
		}
		return nil
	})
	return out, err
}
```

> sqlc 生成的 `ListProducts` 参数名与类型以实际产物为准；
> 若签名不同（例如打包成一个 params 结构体），按产物调整此处调用，
> **不要改 SQL 去迁就这段代码**。

- [ ] **Step 5: 写 handler**

`internal/handler/product.go`：handler 只做参数解析与响应封装，
不出现任何 SQL 或事务（CONTRIBUTING 硬规矩一）。

```go
// Package handler 实现 OpenAPI 生成的服务端接口。
//
// 这里不准出现 SQL 和事务。业务规则在 service，数据访问在 repository。
package handler

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/keel/keel/internal/service"
)

type ProductHandler struct{ svc *service.ProductService }

func NewProductHandler(s *service.ProductService) *ProductHandler {
	return &ProductHandler{svc: s}
}

func (h *ProductHandler) List(c *gin.Context) {
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	pageSize, _ := strconv.Atoi(c.DefaultQuery("page_size", "20"))

	items, err := h.svc.List(c.Request.Context(), page, pageSize)
	if err != nil {
		// 错误体遵循 RFC 9457，不使用 {code,message,data} 信封
		c.Header("Content-Type", "application/problem+json")
		c.JSON(http.StatusInternalServerError, gin.H{
			"type":   "https://keel.dev/problems/internal",
			"title":  "服务内部错误",
			"status": http.StatusInternalServerError,
		})
		return
	}
	c.JSON(http.StatusOK, gin.H{"items": items})
}
```

- [ ] **Step 6: 写入口**

`cmd/keel/main.go`：装配池、Resolver、Repo、Service、Handler 与路由，
并提供 `GET /healthz`（compose 与 smoke 脚本要用）。

```go
package main

import (
	"context"
	"log"
	"net/http"
	"os"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/keel/keel/internal/db"
	"github.com/keel/keel/internal/handler"
	"github.com/keel/keel/internal/repository"
	"github.com/keel/keel/internal/service"
	"github.com/keel/keel/internal/tenant"
)

func main() {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, db.DSN())
	if err != nil {
		log.Fatal(err)
	}
	defer pool.Close()

	r := gin.New()
	r.Use(gin.Recovery())
	r.GET("/healthz", func(c *gin.Context) { c.String(http.StatusOK, "ok") })

	res := tenant.NewResolver(pool, os.Getenv("KEEL_DEFAULT_MERCHANT"))
	ph := handler.NewProductHandler(service.NewProductService(repository.New(pool)))

	v1 := r.Group("/api/v1", res.Middleware())
	v1.GET("/products", ph.List)

	addr := ":8080"
	if v := os.Getenv("KEEL_ADDR"); v != "" {
		addr = v
	}
	log.Printf("listening on %s", addr)
	log.Fatal(r.Run(addr))
}
```

- [ ] **Step 7: 补种子商品（两个租户各若干件）**

追加到 `db/seed/dev.sql`。**必须两个租户都有商品**，
否则跨租户测试形同虚设（spec 风险表最后一条）。

```sql
INSERT INTO categories (merchant_id, name, path, status)
SELECT id, '默认分类', '/', 1 FROM merchants WHERE code IN ('shop-a','shop-b')
ON CONFLICT DO NOTHING;

INSERT INTO products (merchant_id, category_id, title, min_price_cents,
                      max_price_cents, status, published_at)
SELECT c.merchant_id, c.id,
       m.code || ' 的商品 ' || g, 1990, 4990, 1, now()
  FROM categories c
  JOIN merchants m ON m.id = c.merchant_id
  CROSS JOIN generate_series(1, 3) g
 WHERE m.code IN ('shop-a','shop-b')
ON CONFLICT DO NOTHING;
```

- [ ] **Step 8: 运行，确认通过**

```bash
go test ./internal/... -v
```

Expected: 全部 PASS，含跨租户与分页钳制两条。

- [ ] **Step 9: 提交**

```bash
git add internal/ cmd/ oapi-codegen.yaml db/seed/
git commit -m "feat: GET /products end to end

Handler parses and shapes, service holds the paging rules, repository
owns the transaction and the tenant. The cross-tenant test asserts the
two shops' id sets do not intersect, which is the one thing about this
design that single-tenant test data cannot reveal."
```

---

### Task 6: docker compose、种子与 smoke

**Files:**
- Create: `docker/Dockerfile`、`compose.yaml`、`scripts/smoke.sh`

**Interfaces:**
- Consumes: `cmd/keel`、`db/migrations`、`db/seed/dev.sql`
- Produces: `docker compose up` 起全栈；`scripts/smoke.sh` 退出码 0 表示链路通

- [ ] **Step 1: 写 smoke 脚本（此时它应当失败）**

`scripts/smoke.sh`：

```bash
#!/usr/bin/env bash
# 验证 docker compose up 之后整条链路是通的。
set -euo pipefail

BASE="${KEEL_BASE:-http://localhost:8080}"

echo "==> 等待 /healthz"
for i in $(seq 1 60); do
    if curl -sf "$BASE/healthz" >/dev/null; then break; fi
    sleep 1
    if [ "$i" = 60 ]; then echo "健康检查超时" >&2; exit 1; fi
done

echo "==> GET /api/v1/products 应返回非空"
body=$(curl -sf "$BASE/api/v1/products")
count=$(echo "$body" | python3 -c 'import json,sys; print(len(json.load(sys.stdin)["items"]))')
if [ "$count" -lt 1 ]; then
    echo "商品列表为空，种子数据没起作用" >&2
    echo "$body" >&2
    exit 1
fi
echo "    返回 $count 件商品"

echo "全部通过。"
```

`chmod +x scripts/smoke.sh`

- [ ] **Step 2: 运行，确认失败**

```bash
./scripts/smoke.sh
```

Expected: 健康检查超时（服务还没起）。

- [ ] **Step 3: 写 Dockerfile**

`docker/Dockerfile`：

```dockerfile
FROM golang:1.25 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/keel ./cmd/keel

FROM gcr.io/distroless/static-debian12
COPY --from=build /out/keel /keel
EXPOSE 8080
ENTRYPOINT ["/keel"]
```

> M1 不接 dtmrs，所以这里 `CGO_ENABLED=0` 仍然可用。
> M2 接入嵌入式协调器后这一行要改，届时镜像也不能再用 distroless/static。

- [ ] **Step 4: 写 compose**

`compose.yaml`：

```yaml
services:
  postgres:
    image: postgres:16
    environment:
      POSTGRES_USER: keel
      POSTGRES_PASSWORD: keel
      POSTGRES_DB: keel
    ports: ["5432:5432"]
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U keel"]
      interval: 2s
      timeout: 3s
      retries: 30

  migrate:
    image: golang:1.25
    working_dir: /src
    volumes: ["./:/src"]
    environment:
      PGHOST: postgres
    command: >
      bash -c "go run github.com/pressly/goose/v3/cmd/goose@v3 -dir db/migrations
               postgres \"postgres://keel:keel@postgres:5432/keel?sslmode=disable\" up &&
               psql \"postgres://keel:keel@postgres:5432/keel\" -f db/seed/dev.sql"
    depends_on:
      postgres: {condition: service_healthy}

  app:
    build: {context: ., dockerfile: docker/Dockerfile}
    environment:
      PGHOST: postgres
      KEEL_DEFAULT_MERCHANT: shop-a
    ports: ["8080:8080"]
    depends_on:
      migrate: {condition: service_completed_successfully}
```

> `KEEL_DEFAULT_MERCHANT: shop-a` 让默认形态就是单商家——
> 这正是 README 承诺的「小商家一条命令起来，感知不到多租户」。

- [ ] **Step 5: 起栈并跑 smoke**

```bash
docker compose up -d --build
./scripts/smoke.sh
```

Expected: 退出码 0，打印返回的商品件数。

- [ ] **Step 6: 提交**

```bash
git add docker/ compose.yaml scripts/smoke.sh
git commit -m "feat: one-command stack with migrations, seed and a smoke check

KEEL_DEFAULT_MERCHANT makes the default shape single-tenant, which is
what the README promises a small shop gets."
```

---

### Task 7: TypeScript SDK 与一次真调用

**Files:**
- Create: `clients/ts/package.json`、`clients/ts/tsconfig.json`、`clients/ts/src/call.ts`
- Create: `clients/ts/.gitignore`

**Interfaces:**
- Consumes: 运行中的服务（Task 6）、Task 1 选定的 TS 生成器
- Produces: `npm run check` 通过；`npm run call` 打印种子商品标题

- [ ] **Step 1: 建工程**

```bash
mkdir -p clients/ts/src && cd clients/ts
npm init -y
npm i -D typescript openapi-typescript
npx tsc --init --strict --target es2022 --module node16 --moduleResolution node16
printf 'node_modules/\nsrc/schema.d.ts\n' > .gitignore
```

- [ ] **Step 2: 从契约生成类型**

`package.json` 的 scripts：

```json
{
  "scripts": {
    "gen": "openapi-typescript ../../docs/电商系统-OpenAPI.yaml -o src/schema.d.ts",
    "check": "npm run gen && tsc --noEmit",
    "call": "npm run gen && tsc && node dist/call.js"
  }
}
```

- [ ] **Step 3: 写调用脚本（Review Focus 第 4 条在此落地）**

`clients/ts/src/call.ts`：

```ts
import type { components } from "./schema";

// 这个别名的存在本身就是一次检查：若生成器没能正确处理契约里的
// 3.1 写法，下面 tsc --noEmit 会红。
type ProductSummary = components["schemas"]["ProductSummary"];
type Staff = components["schemas"]["Staff"];

// Staff.merchant_id 在契约里是 3.1 的 type: [integer, 'null']。
// 生成器若不支持，这里会被推成 number 而不是 number | null，
// 下面这行赋值就不会报错 —— 那说明类型是错的。
const nullableCheck: Staff["merchant_id"] = null;
void nullableCheck;

const BASE = process.env.KEEL_BASE ?? "http://localhost:8080";

async function main(): Promise<void> {
  const res = await fetch(`${BASE}/api/v1/products`);
  if (!res.ok) {
    throw new Error(`GET /products 返回 ${res.status}`);
  }
  const body = (await res.json()) as { items: ProductSummary[] };
  if (body.items.length === 0) {
    throw new Error("商品列表为空；种子数据没起作用");
  }
  for (const p of body.items) {
    console.log(`${p.id}\t${p.title}\t${p.min_price_cents} 分`);
  }
  console.log(`共 ${body.items.length} 件`);
}

main().catch((e) => {
  console.error(e);
  process.exit(1);
});
```

- [ ] **Step 4: 类型检查**

```bash
cd clients/ts && npm run check
```

Expected: PASS。若 `const nullableCheck: Staff["merchant_id"] = null;` 报错，
说明生成器把可空字段生成成了非空——**那是 Task 1 该拦下的问题**，
回到那份决策记录处理，不要在这里把 `null` 改掉绕过去。

- [ ] **Step 5: 真调用**

```bash
cd clients/ts && npm run call
```

Expected: 打印出三条 `shop-a 的商品 N`。

- [ ] **Step 6: 提交**

```bash
git add clients/ts
git commit -m "feat: generate the TypeScript client and make one real call

The contract is OpenAPI 3.1; the call script asserts a nullable field really
is nullable in the generated types, so a generator that silently drops
3.1 nullability fails the type check instead of shipping."
```

---

### Task 8: CI、codegen 漂移检查，把徽章挂回去

**Files:**
- Create: `.github/workflows/ci.yml`、`.github/workflows/example-dtmrs.yml`
- Modify: `README.md`、`README.zh-CN.md`

**Interfaces:**
- Consumes: 前七个任务的全部产物
- Produces: 绿色 CI；`check_promises.py` 因 `.github/workflows/` 存在而认可徽章

- [ ] **Step 1: 写主 workflow**

`.github/workflows/ci.yml`：

```yaml
name: CI
on:
  push: {branches: [main]}
  pull_request:

jobs:
  build:
    runs-on: ubuntu-latest
    services:
      postgres:
        image: postgres:16
        env:
          POSTGRES_USER: keel
          POSTGRES_PASSWORD: keel
          POSTGRES_DB: keel
        ports: ["5432:5432"]
        options: >-
          --health-cmd "pg_isready -U keel"
          --health-interval 2s --health-timeout 3s --health-retries 30
    env:
      PGHOST: 127.0.0.1
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with: {go-version: "1.25"}
      - uses: actions/setup-node@v4
        with: {node-version: "24"}

      - name: 文档闸门
        run: ./scripts/check-all.sh

      - name: 装工具
        run: |
          go install github.com/pressly/goose/v3/cmd/goose@latest
          go install github.com/sqlc-dev/sqlc/cmd/sqlc@latest
          go install github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@latest
          echo "$(go env GOPATH)/bin" >> "$GITHUB_PATH"

      - name: codegen 漂移检查
        run: |
          sqlc generate
          go generate ./...
          if ! git diff --quiet; then
            echo "生成的代码与契约/查询不一致。改了契约就要重新生成。" >&2
            git diff --stat >&2
            exit 1
          fi

      - name: 迁移与种子
        run: |
          goose -dir db/migrations postgres \
            "postgres://keel:keel@127.0.0.1:5432/keel?sslmode=disable" up
          psql "postgres://keel:keel@127.0.0.1:5432/keel" -f db/seed/dev.sql

      - run: go vet ./...
      - run: go test ./... -race
      - run: go build ./...

      - name: TS 客户端类型检查
        run: cd clients/ts && npm ci && npm run check
```

- [ ] **Step 2: 写例子的独立 workflow**

`.github/workflows/example-dtmrs.yml`：

```yaml
name: example-dtmrs
on:
  push:
    paths: ["examples/dtmrs-embedded/**", ".github/workflows/example-dtmrs.yml"]
  pull_request:
    paths: ["examples/dtmrs-embedded/**"]

jobs:
  verify:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with: {go-version: "1.25"}
      - uses: dtolnay/rust-toolchain@stable
      - name: 构建 libdtmrs 并跑验证
        working-directory: examples/dtmrs-embedded
        run: make deps && make verify
```

> 单独一个 workflow 是因为它要 Rust 工具链与网络，不该拖慢主 CI；
> 但也不能不跑——它是「一套代码从单机长到分布式」唯一的可执行证据。

- [ ] **Step 3: 验证漂移检查真的会红（故意制造失败）**

```bash
# 改契约但不重新生成
sed -i 's/summary: 商品列表/summary: 商品列表（改过）/' docs/电商系统-OpenAPI.yaml
go generate ./...
git diff --quiet && echo "!! 漂移检查形同虚设" || echo "OK: 检测到漂移"
git checkout docs/电商系统-OpenAPI.yaml && go generate ./...
```

Expected: 打印 `OK: 检测到漂移`。**没见它红过的闸门不算数。**

- [ ] **Step 4: 把徽章挂回去**

`.github/workflows/` 现在存在了，`check_promises.py` 的虚标规则因此放行。
在两份 README 的徽章区加回：

```markdown
[![CI](https://github.com/<org>/keel/actions/workflows/ci.yml/badge.svg)](https://github.com/<org>/keel/actions/workflows/ci.yml)
```

- [ ] **Step 5: 全量校验**

```bash
./scripts/check-all.sh && go test ./... && echo "全绿"
```

Expected: 五个文档闸门 + Go 测试全绿。

- [ ] **Step 6: 提交**

```bash
git add .github README.md README.zh-CN.md
git commit -m "ci: gates for the contract, the tenant boundary and codegen drift

The drift check was verified by editing the contract without regenerating
and watching it fail. The CI badge goes back because a workflow now
exists, which is the condition check_promises.py was already testing."
```

---

### Task 9: 收口

**Files:**
- Modify: `CONTRIBUTING.md`、`docs/电商系统-总体架构.md`

- [ ] **Step 1: CONTRIBUTING 补本地开发步骤**

在「提交前自查」之前插入：

```markdown
## 跑起来

```bash
docker compose up -d --build
./scripts/smoke.sh
```

只需要 Docker。M1 的服务端不依赖 Rust——`examples/dtmrs-embedded` 才需要，
它有自己的构建说明。
```

- [ ] **Step 2: 架构文档路线图勾掉 M1**

把 §13 路线图里 M1 那一行标记为已完成，并注明产出：
契约在 Go 与 TS 两侧均可生成、`GET /products` 端到端打通、跨租户隔离有测试守住。

- [ ] **Step 3: 全量校验并提交**

```bash
./scripts/check-all.sh && go test ./...
git add CONTRIBUTING.md docs/
git commit -m "docs: mark M1 done and document how to run it locally"
```

- [ ] **Step 4: 逐条核对 spec 的验收标准**

对照 `docs/superpowers/specs/2026-09-25-m1-engineering-skeleton-design.md` §七：

1. `docker compose up` 起全栈含种子 —— `./scripts/smoke.sh` 退出码 0
2. TS 客户端调通 —— `cd clients/ts && npm run call` 打印商品
3. 租户 A 读不到租户 B —— `go test ./internal/handler/ -run TestTenantsSeeOnly`
4. 漏设租户则失败 —— `go test ./internal/repository/ -run TestWithTenantRefuses`
5. 契约漂移 → CI 失败 —— Task 8 Step 3 已故意验证
6. 五闸门 + `go test ./...` 全绿 —— CI

任一条未达成，回到对应任务修复，不要在收口任务里打补丁。
