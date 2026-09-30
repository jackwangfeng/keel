#!/usr/bin/env python3
"""校验新迁移给大表建索引的写法（CONTRIBUTING.md「迁移怎么写」）。

规矩两条，只对编号 > 00151 的迁移（db/migrations 与 db/migrations-inventory 两个目录）生效：

1. **大表上的 CREATE INDEX 必须 CONCURRENTLY。** 普通 CREATE INDEX 持 SHARE 锁，
   建多久就挡多久的写 —— orders 上建一条索引几分钟，这几分钟里一单都下不了。
2. **CONCURRENTLY 必须配 `-- +goose NO TRANSACTION`。** goose 默认把每份迁移包在事务里，
   而 CREATE INDEX CONCURRENTLY 不能在事务块里跑：不写 NO TRANSACTION，迁移在上线那一刻报
   `cannot run inside a transaction block` —— 本地库小、CI 也会报，但它值得在提交前就报。

### 为什么从 00152 起，而不是全部

00057（报表索引）和 00064 以「goose 把迁移包在事务里」为由在大表上不用 CONCURRENTLY，
00085 在同一个事务里 ADD COLUMN 之后全表 UPDATE。那个理由不成立（goose 支持
NO TRANSACTION），但它们已经在线上跑过了：改一份已应用的迁移不会重跑，只会让库与文件对不上
（internal/testdb 的文件头写过这个坑）。所以旧的不动，新的守住。

### 这个检查挡不住的

它只认 SQL 文本：DO 块里 EXECUTE 拼出来的 CREATE INDEX、表名经 search_path 解析到别处、
「大表」清单外新长大的表，都看不见。清单是一个需要维护的东西 —— 一张表开始按商家数 × 时间
线性增长，就该加进 BIG_TABLES。
"""
import re
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
DIRS = [ROOT / "db" / "migrations", ROOT / "db" / "migrations-inventory"]

# 编号大于它的迁移才检查（main 在立这条规矩时的最高编号）。
BASELINE = 151

# 按行数随业务线性增长、在线上建索引会挡住写的表。
BIG_TABLES = {
    "orders", "order_items", "payments", "refunds", "refund_items", "shipments",
    "products", "skus", "product_images", "product_text_vectors", "product_image_vectors",
    "inventories", "inventory_logs", "product_store_stock",
    "search_logs", "idempotency_keys", "agent_tool_calls", "agent_events",
    "users", "user_coupons", "carts", "cart_items",
    "notifications", "notification_deliveries", "jobs",
}

NO_TX = re.compile(r"^\s*--\s*\+goose\s+NO\s+TRANSACTION\s*$", re.I | re.M)
CREATE_INDEX = re.compile(
    r"\bCREATE\s+(?:UNIQUE\s+)?INDEX\s+(CONCURRENTLY\s+)?(?:IF\s+NOT\s+EXISTS\s+)?"
    r"(?:[\w\"]+\s+)?ON\s+(?:ONLY\s+)?(?:[\w\"]+\.)?\"?(\w+)\"?",
    re.I,
)


def strip_comments(sql: str) -> str:
    """去掉 -- 行注释与 /* */ 块注释（goose 的指令也是注释，调用方先单独取）。"""
    sql = re.sub(r"/\*.*?\*/", " ", sql, flags=re.S)
    return re.sub(r"--[^\n]*", "", sql)


def up_section(sql: str) -> str:
    """只看 Up 那一段：Down 里的 CREATE INDEX 是回滚用的，照样该 CONCURRENTLY，
    但回滚在事故里跑，规矩在那里由写的人自己权衡 —— 这里不替他挡。"""
    m = re.search(r"^\s*--\s*\+goose\s+Up\b", sql, re.I | re.M)
    start = m.end() if m else 0
    d = re.search(r"^\s*--\s*\+goose\s+Down\b", sql[start:], re.I | re.M)
    return sql[start: start + d.start()] if d else sql[start:]


def check_text(name: str, sql: str) -> list[str]:
    errors = []
    no_tx = bool(NO_TX.search(sql))
    body = strip_comments(up_section(sql))
    for m in CREATE_INDEX.finditer(body):
        concurrently, table = bool(m.group(1)), m.group(2).lower()
        if table in BIG_TABLES and not concurrently:
            errors.append(f"{name}: 在大表 {table} 上 CREATE INDEX 没有 CONCURRENTLY —— "
                          f"建索引期间会挡住这张表的全部写入")
        if concurrently and not no_tx:
            errors.append(f"{name}: CREATE INDEX CONCURRENTLY（{table}）却没有 "
                          f"`-- +goose NO TRANSACTION` —— goose 默认包事务，上线时会报 "
                          f"cannot run inside a transaction block")
    return errors


def self_test() -> None:
    """几条正反例，钉住正则：它悄悄失配时整个检查会变成永远绿。"""
    cases = [
        ("-- +goose Up\nCREATE INDEX idx_a ON orders (x);", 1),
        ("-- +goose Up\nCREATE INDEX idx_a ON public.orders USING btree (x);", 1),
        ("-- +goose Up\nCREATE UNIQUE INDEX IF NOT EXISTS idx_a ON ONLY orders (x);", 1),
        ("-- +goose NO TRANSACTION\n-- +goose Up\nCREATE INDEX CONCURRENTLY idx_a ON orders (x);", 0),
        ("-- +goose Up\nCREATE INDEX CONCURRENTLY IF NOT EXISTS idx_a ON orders (x);", 1),
        ("-- +goose Up\nCREATE INDEX idx_a ON categories (x);", 0),
        ("-- +goose Up\n-- CREATE INDEX idx_a ON orders (x);\nSELECT 1;", 0),
        ("-- +goose Up\nSELECT 1;\n-- +goose Down\nCREATE INDEX idx_a ON orders (x);", 0),
    ]
    for i, (sql, want) in enumerate(cases):
        got = len(check_text(f"case{i}", sql))
        if got != want:
            sys.exit(f"check_migrations 自检失败：第 {i} 条期望 {want} 个错误，实得 {got}\n{sql}")


def main() -> int:
    self_test()
    errors, checked = [], 0
    for d in DIRS:
        for f in sorted(d.glob("*.sql")):
            m = re.match(r"(\d+)_", f.name)
            if not m or int(m.group(1)) <= BASELINE:
                continue
            checked += 1
            errors += check_text(f"{d.name}/{f.name}", f.read_text(encoding="utf-8"))
    if errors:
        print("check_migrations FAIL（写法见 CONTRIBUTING.md「迁移怎么写」）：")
        for e in errors:
            print("  - " + e)
        return 1
    print(f"check_migrations OK: {checked} 份新迁移（编号 > {BASELINE:05d}）里大表索引都是 "
          f"CONCURRENTLY + NO TRANSACTION")
    return 0


if __name__ == "__main__":
    sys.exit(main())
