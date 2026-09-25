# 设计遗留闭环 实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 把 Keel 从「对外承诺与实际内容对不上」推进到「文档自洽、承诺兑现、可以开始写代码」。

**Architecture:** 本轮不写业务代码，产出全部是文档与契约。验收不靠人眼，靠三个校验脚本：坏链接、OpenAPI 结构、README 承诺兑现。每个任务先写校验器让它失败，再改文档让它通过——文档工作同样可以有红绿灯。

**Tech Stack:** Python 3（校验脚本，仅用标准库 + PyYAML）、Git、Markdown、OpenAPI 3.1

**Spec:** `docs/superpowers/specs/2026-09-25-design-debt-closure-design.md`

## Global Constraints

- 设计文档用中文；代码、Issue、提交信息用英文（ADR #9 改写后的口径）
- `docs/电商系统-数据模型设计.md` 是全部业务表 DDL 的唯一真相源；其余文档不得出现 `CREATE TABLE` / `ALTER TABLE ... ADD COLUMN`。例外：dtmrs 的 `barrier` 表
- 金额 `BIGINT` 单位分，字段后缀 `_cents`；时间 `TIMESTAMPTZ`；主键 `BIGINT GENERATED ALWAYS AS IDENTITY`；枚举用 `SMALLINT` 不用 PG ENUM
- 对外编号：**仅**订单 / 支付 / 退款用不可枚举字符串（`order_no` / `payment_no` / `refund_no`），其余对象对外用自增 `id`
- OpenAPI 3.1；错误体用 RFC 9457 Problem Details；不使用 `{code,message,data}` 信封
- **不写业务代码。** 本轮产出全部是文档与契约
- 一个任务一个 commit，可单独回滚
- 所有校验脚本放在 `scripts/`，用 `python3` 直接可跑，不引入构建工具

## Review Focus

以下五条是 spec 隐含要求、但若不刻意设计测试就会漏掉的失败模式，已各自挂到对应任务：

1. **中文文件名的 URL 编码链接**——文档全是中文文件名，Markdown 链接可能写成 `%E7%94%B5%E5%95%86...` 编码形式。校验器只比字面量会漏判为坏链接。（Task 1）
2. **带锚点的链接**——`./foo.md#section` 若只截取文件名验存在，锚点失效不会被发现。（Task 1）
3. **上传接口未表达大小与类型限制**——客户端无从得知能传多大、什么格式，只能试到 413 才知道。（Task 3）
4. **孤儿回收误删仍被引用的文件**——退款凭证已提交但清理任务判定为孤儿，证据消失。（Task 3）
5. **能力清单条目数与架构 §11 不一致时的处理方向**——必须改架构里的数字，不能为了凑 37 而注水。（Task 5b）

---

### Task 1: 文档归位与链接修复

**Files:**
- Create: `scripts/check_links.py`
- Move: 四份设计文档 → `docs/`
- Modify: `README.md`、`README.zh-CN.md`、`docs/电商系统-总体架构.md`

**Interfaces:**
- Consumes: 无（首个任务）
- Produces: `scripts/check_links.py` —— 命令行退出码 0 表示无坏链接，非 0 表示有；后续任务与 CI 复用

- [ ] **Step 1: 写校验器（此时它应当失败）**

创建 `scripts/check_links.py`：

```python
#!/usr/bin/env python3
"""检查仓库内全部 Markdown 的相对链接是否指向真实存在的文件与锚点。"""
import io
import os
import re
import sys
import urllib.parse

LINK_RE = re.compile(r'\[[^\]]*\]\(([^)]+)\)')
HEADING_RE = re.compile(r'^#{1,6}\s+(.*?)\s*$', re.MULTILINE)
SKIP_DIRS = {'.git', 'node_modules', 'target', 'vendor'}


def slugify(heading):
    """GitHub 风格锚点：小写、去标点、空格转连字符。中文原样保留。"""
    s = heading.strip().lower()
    s = re.sub(r'[^\w一-鿿\s-]', '', s)
    return re.sub(r'\s+', '-', s)


def anchors_of(path):
    try:
        text = io.open(path, encoding='utf-8').read()
    except (OSError, UnicodeDecodeError):
        return set()
    return {slugify(h) for h in HEADING_RE.findall(text)}


def md_files(root):
    for dirpath, dirnames, filenames in os.walk(root):
        dirnames[:] = [d for d in dirnames if d not in SKIP_DIRS]
        for name in filenames:
            if name.endswith('.md'):
                yield os.path.join(dirpath, name)


def main():
    root = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
    problems = []
    for path in sorted(md_files(root)):
        lines = io.open(path, encoding='utf-8').read().split('\n')
        for lineno, line in enumerate(lines, 1):
            for target in LINK_RE.findall(line):
                target = target.strip()
                # 跳过外链、锚点内跳转、邮件、图片数据
                if target.startswith(('http://', 'https://', 'mailto:', '#', 'data:')):
                    continue
                filepart, _, anchor = target.partition('#')
                # 关键：中文文件名常被写成百分号编码，必须先解码再比对
                filepart = urllib.parse.unquote(filepart)
                anchor = urllib.parse.unquote(anchor)
                if not filepart:
                    continue
                resolved = os.path.normpath(
                    os.path.join(os.path.dirname(path), filepart))
                rel = os.path.relpath(path, root)
                if not os.path.exists(resolved):
                    problems.append('%s:%d 指向不存在的路径 -> %s' % (rel, lineno, target))
                elif anchor and resolved.endswith('.md'):
                    if slugify(anchor) not in anchors_of(resolved):
                        problems.append(
                            '%s:%d 文件存在但锚点缺失 -> %s' % (rel, lineno, target))

    if problems:
        print('发现 %d 处坏链接：' % len(problems))
        for p in problems:
            print('  ' + p)
        return 1
    print('全部 Markdown 链接有效')
    return 0


if __name__ == '__main__':
    sys.exit(main())
```

> **关于锚点校验的一个提醒**：`slugify()` 模拟的是 GitHub 的锚点生成规则，
> 对中文标题属于尽力而为。若遇到明显的假阳性（链接手动点开是好的，脚本却报错），
> 修 `slugify()` 而不是改文档去迁就脚本——**校验器为文档服务，不是反过来**。

- [ ] **Step 2: 运行，确认它失败**

Run: `python3 scripts/check_links.py`

Expected: 退出码 1，列出坏链接。当前已知至少包含：
`README.md` / `README.zh-CN.md` 指向 `./docs/architecture.md`、`./docs/schema-design.md`、
`./docs/search-design.md`、`./docs/product-understanding.md`、`./CONTRIBUTING.md`；
`电商系统-总体架构.md` 开头指向 `./schema-design.md`、`./openapi.yaml`、
`./search-design.md`、`./product-understanding.md`、`./ai-capabilities.md`。

- [ ] **Step 3: 文档移入 docs/**

```bash
git mv "电商系统-总体架构.md"         docs/电商系统-总体架构.md
git mv "电商系统-数据模型设计.md"     docs/电商系统-数据模型设计.md
git mv "电商系统-语义检索层设计.md"   docs/电商系统-语义检索层设计.md
git mv "电商系统-商品理解服务设计.md" docs/电商系统-商品理解服务设计.md
git mv "电商系统-OpenAPI.yaml"        docs/电商系统-OpenAPI.yaml
```

- [ ] **Step 4: 修 README 双语的链接**

`README.md` 与 `README.zh-CN.md` 中的四处子文档链接改为实际路径：

```
./docs/architecture.md        -> ./docs/电商系统-总体架构.md
./docs/schema-design.md       -> ./docs/电商系统-数据模型设计.md
./docs/search-design.md       -> ./docs/电商系统-语义检索层设计.md
./docs/product-understanding.md -> ./docs/电商系统-商品理解服务设计.md
```

`[文档](./docs)` / `[Docs](./docs)` 保持不变（目录存在即有效）。
`./CONTRIBUTING.md` 暂时留着——Task 2 会创建该文件，届时自动变为有效链接。
**本任务的校验器允许 `CONTRIBUTING.md` 仍然报错**，Step 6 只要求其余链接归零。

- [ ] **Step 5: 修架构文档开头的链接，并改写 ADR #9**

`docs/电商系统-总体架构.md` 开头的引用块改为：

```markdown
> 本文是架构总纲。详细设计见四份子文档：
> [数据模型](./电商系统-数据模型设计.md) · [OpenAPI 契约](./电商系统-OpenAPI.yaml) ·
> [语义检索层](./电商系统-语义检索层设计.md) · [商品理解服务](./电商系统-商品理解服务设计.md)
```

§11 中 `[AI 能力全景](./ai-capabilities.md)` 暂时保留（Task 5 创建），
与 CONTRIBUTING.md 同样处理。

ADR 表格第 9 行改写：

```markdown
| 9 | 代码英文、设计文档中文 | 代码/Issue/提交信息全英文，便于国际协作；设计文档用中文，因为作者与早期贡献者都在中文环境 | 国际贡献者暂时只能读 README |
```

并在 ADR 表格后补一段说明：

```markdown
> **ADR #9 为什么从「英文优先」改成现在这样**
>
> 原先写的是「README/代码/Issue 全英文，中文作为翻译版」。方向没错——
> 开源项目要国际贡献者就得降低语言门槛。但四份长设计文档翻译加上后续双语维护
> 是持续成本，而项目现在连一行代码都没有。**为一个还不存在的贡献者群体先付这笔钱，
> 顺序反了。**
>
> 等真的出现国际贡献者，那时也更清楚该优先翻哪几份。代价是短期内他们只能读 README。
```

- [ ] **Step 6: 运行校验器，确认只剩两处待创建文件的链接**

Run: `python3 scripts/check_links.py`

Expected: 仅剩 `CONTRIBUTING.md`（Task 2 创建）与 `ai-capabilities.md`（Task 5 创建）报错，
其余全部归零。四处子文档链接与架构文档开头四处必须消失。

- [ ] **Step 7: 提交**

```bash
git add -A
git commit -m "docs: move design docs into docs/ and fix broken links

Rewrite ADR #9: code and issues in English, design docs stay Chinese.
Add scripts/check_links.py as the gate (handles percent-encoded Chinese
filenames and anchor targets).

CONTRIBUTING.md and ai-capabilities.md are still missing by design;
they land in later tasks.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>"
```

---

### Task 2: 补齐 README 承诺的文件

**Files:**
- Create: `LICENSE`、`CONTRIBUTING.md`、`scripts/check_promises.py`
- Modify: `README.md`、`README.zh-CN.md`

**Interfaces:**
- Consumes: `scripts/check_links.py`（Task 1）
- Produces: `scripts/check_promises.py` —— 退出码 0 表示 README 承诺全部兑现

- [ ] **Step 1: 写校验器（此时它应当失败）**

创建 `scripts/check_promises.py`：

```python
#!/usr/bin/env python3
"""检查 README 宣称存在的东西是否真的存在，以及有没有占位/虚标。"""
import io
import os
import re
import sys

# README 承诺存在的文件，相对仓库根目录
PROMISED_FILES = [
    'LICENSE',
    'CONTRIBUTING.md',
    'docs/电商系统-总体架构.md',
    'docs/电商系统-数据模型设计.md',
    'docs/电商系统-语义检索层设计.md',
    'docs/电商系统-商品理解服务设计.md',
    'docs/电商系统-OpenAPI.yaml',
]

READMES = ['README.md', 'README.zh-CN.md']
PLACEHOLDER_LINK_RE = re.compile(r'\[[^\]]*\]\(#\)')


def main():
    root = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
    problems = []

    for rel in PROMISED_FILES:
        if not os.path.exists(os.path.join(root, rel)):
            problems.append('README 承诺的文件不存在: %s' % rel)

    for rel in READMES:
        path = os.path.join(root, rel)
        if not os.path.exists(path):
            problems.append('README 本身缺失: %s' % rel)
            continue
        lines = io.open(path, encoding='utf-8').read().split('\n')
        for lineno, line in enumerate(lines, 1):
            for hit in PLACEHOLDER_LINK_RE.findall(line):
                problems.append(
                    '%s:%d 占位链接（指向 "#"）: %s' % (rel, lineno, hit.strip()))
            if 'img.shields.io' in line and 'CI' in line:
                problems.append(
                    '%s:%d 挂着 CI 徽章但仓库没有 CI，属于虚标' % (rel, lineno))

    if problems:
        print('发现 %d 处问题：' % len(problems))
        for p in problems:
            print('  ' + p)
        return 1
    print('README 承诺全部兑现')
    return 0


if __name__ == '__main__':
    sys.exit(main())
```

- [ ] **Step 2: 运行，确认它失败**

Run: `python3 scripts/check_promises.py`

Expected: 退出码 1。至少报出 `LICENSE` 与 `CONTRIBUTING.md` 不存在、
双语 README 各有 CI 徽章虚标与 `(#)` 占位链接（CI 徽章、在线演示）。

- [ ] **Step 3: 创建 LICENSE**

写入 Apache License 2.0 全文（标准文本，版权行如下）：

```
Copyright 2026 The Keel Authors

Licensed under the Apache License, Version 2.0 (the "License");
...
```

获取标准全文：`curl -sL https://www.apache.org/licenses/LICENSE-2.0.txt -o LICENSE`
然后在文件末尾补上 `Copyright 2026 The Keel Authors` 归属行。
若无网络，从 `https://spdx.org/licenses/Apache-2.0.html` 或本地任一 Apache-2.0 项目复制。

- [ ] **Step 4: 创建 CONTRIBUTING.md**

内容以 README 已写明的两条硬规矩为核心，用中文撰写：

```markdown
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

同理，**数据库表结构的唯一真相源是 `docs/电商系统-数据模型设计.md`**。
其余文档不得出现 `CREATE TABLE`；需要讲结构时引用该文档的章节。

## 开发环境

| 依赖 | 版本 | 说明 |
|---|---|---|
| Go | 1.23+ | 后端 |
| Rust | 1.82+ | 编译 dtmrs 的 C ABI 动态库，经 cgo 嵌入 |
| PostgreSQL | 16+ | 需要 pgvector 扩展 |
| Docker | 任意近期版本 | `docker compose up` 起全栈 |

> **为什么需要 Rust 工具链**：事务协调器 dtmrs 是 Rust 实现，
> 通过 cgo 以嵌入式模式运行。这意味着 `CGO_ENABLED=0` 静态编译不可用。
> 如果你不想装 Rust，可以用独立 TC 进程的部署形态开发，
> 见架构文档「部署演进」一节。

## 提交信息

用英文，遵循 Conventional Commits：

```
feat: add coupon allocation to order items
fix: reject refund when quantity exceeds remaining
docs: clarify saga compensation ordering
```

正文可以用中文解释「为什么」，但标题行用英文。

## 代码之外

- 设计文档用中文，代码与 Issue 用英文（见架构文档 ADR #9）
- 数据库变更走迁移工具，禁止手改
- 核心逻辑必须有测试：库存扣减、优惠计算、订单状态机
```

- [ ] **Step 5: 摘掉虚标徽章与占位链接**

`README.md` 与 `README.zh-CN.md` 顶部：

- 删除 `[![CI](https://img.shields.io/badge/CI-passing-brightgreen)](#)` 整行——
  CI 不存在，绿标是虚标
- `[在线演示](#)` / `[Live Demo](#)` 删除该链接项（Demo 尚未上线）
- 保留 License / Go / PostgreSQL 三个徽章，它们陈述的是事实
- License 徽章链接由 `(./LICENSE)` 保持，此时该文件已存在

- [ ] **Step 6: 运行两个校验器，确认全绿**

Run: `python3 scripts/check_promises.py && python3 scripts/check_links.py`

Expected: `check_promises.py` 退出码 0；`check_links.py` 此时仅剩
`ai-capabilities.md` 一处报错（Task 5 创建）。

- [ ] **Step 7: 提交**

```bash
git add -A
git commit -m "docs: add LICENSE and CONTRIBUTING, drop the fake CI badge

The README linked to both files and neither existed. The CI badge showed
green while no CI exists at all.

Add scripts/check_promises.py so this cannot silently regress.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>"
```

---

### Task 3: 文件存储设计

**Files:**
- Create: `scripts/check_openapi.py`
- Modify: `docs/电商系统-数据模型设计.md`（新增 `uploads` 表与 Storage 抽象说明）
- Modify: `docs/电商系统-OpenAPI.yaml`（新增上传接口）
- Modify: `README.md`、`README.zh-CN.md`（「只有一个数据库」加脚注）

**Interfaces:**
- Consumes: `scripts/check_links.py`、`scripts/check_promises.py`
- Produces: `scripts/check_openapi.py` —— 校验 OpenAPI 结构有效、零悬空 `$ref`、
  无孤儿 schema，并断言 `REQUIRED_PATHS` 中列出的路径全部存在。后续任务通过
  往 `REQUIRED_PATHS` / `REQUIRED_SCHEMAS` 追加条目来「写失败的测试」

- [ ] **Step 1: 写校验器，并把尚不存在的上传接口列进去（此时它应当失败）**

创建 `scripts/check_openapi.py`：

```python
#!/usr/bin/env python3
"""校验 OpenAPI 契约：可解析、零悬空 $ref、无孤儿 schema、必备路径齐全。"""
import io
import os
import re
import sys

import yaml

SPEC = 'docs/电商系统-OpenAPI.yaml'

# 契约必须提供的路径与方法。每个任务往这里追加，就是在写失败的测试。
REQUIRED_PATHS = [
    ('/uploads', 'post'),
]

# 契约必须定义的 schema。
REQUIRED_SCHEMAS = [
    'UploadTarget',
    'Upload',
]


def walk_refs(text):
    return set(re.findall(r"\$ref:\s*['\"]?(#[^'\"\s]+)", text))


def resolve(doc, ref):
    node = doc
    for part in ref.lstrip('#/').split('/'):
        part = part.replace('~1', '/').replace('~0', '~')
        if isinstance(node, dict) and part in node:
            node = node[part]
        else:
            return False
    return True


def main():
    root = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
    path = os.path.join(root, SPEC)
    text = io.open(path, encoding='utf-8').read()
    problems = []

    try:
        doc = yaml.safe_load(text)
    except yaml.YAMLError as exc:
        print('YAML 无法解析: %s' % exc)
        return 1

    if doc.get('openapi', '').split('.')[0:2] != ['3', '1']:
        problems.append('openapi 版本不是 3.1，实际为 %r' % doc.get('openapi'))

    for ref in sorted(walk_refs(text)):
        if not resolve(doc, ref):
            problems.append('悬空 $ref: %s' % ref)

    schemas = doc.get('components', {}).get('schemas', {})
    referenced = {r.rsplit('/', 1)[-1] for r in walk_refs(text)
                  if '/schemas/' in r}
    for name in sorted(set(schemas) - referenced):
        problems.append('孤儿 schema（无人引用）: %s' % name)

    paths = doc.get('paths', {})
    for p, method in REQUIRED_PATHS:
        if p not in paths:
            problems.append('缺少路径: %s' % p)
        elif method not in paths[p]:
            problems.append('路径 %s 缺少方法: %s' % (p, method))

    for name in REQUIRED_SCHEMAS:
        if name not in schemas:
            problems.append('缺少 schema: %s' % name)

    if problems:
        print('发现 %d 处问题：' % len(problems))
        for p in problems:
            print('  ' + p)
        return 1
    print('OpenAPI 契约校验通过（%d 个路径，%d 个 schema）'
          % (len(paths), len(schemas)))
    return 0


if __name__ == '__main__':
    sys.exit(main())
```

- [ ] **Step 2: 运行，确认它失败**

Run: `python3 scripts/check_openapi.py`

Expected: 退出码 1，报出 `缺少路径: /uploads`、`缺少 schema: UploadTarget`、
`缺少 schema: Upload`。结构性检查（悬空 `$ref`、孤儿 schema）应当已经通过。

- [ ] **Step 3: 数据模型新增 `uploads` 表**

在 `docs/电商系统-数据模型设计.md` 的语义检索与 AI 层之后、用户域之前新增一节
「文件存储」，包含：

```sql
CREATE TABLE uploads (
    id            BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    user_id       BIGINT      NOT NULL REFERENCES users(id),
    purpose       SMALLINT    NOT NULL,   -- 1商品图 2头像 3退款凭证
    driver        SMALLINT    NOT NULL DEFAULT 1,  -- 1本地磁盘 2S3兼容
    storage_key   TEXT        NOT NULL,   -- driver 内部路径，不对外暴露
    content_type  TEXT        NOT NULL,
    size_bytes    BIGINT      NOT NULL,
    sha256        TEXT        NOT NULL,   -- 秒传与去重
    referenced    BOOLEAN     NOT NULL DEFAULT FALSE,  -- 是否已被业务对象引用
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT chk_upload_size CHECK (size_bytes > 0)
);
CREATE UNIQUE INDEX uk_uploads_key ON uploads(driver, storage_key);
-- 孤儿回收任务扫描：创建超过 24 小时仍未被引用的文件
CREATE INDEX idx_uploads_orphan ON uploads(created_at) WHERE NOT referenced;
```

并写入以下说明（Review Focus 第 4 条在此落地）：

```markdown
> **为什么建表而不是只写磁盘返回 URL**
>
> 三个理由，每个单独都不够，合起来是充分的：
>
> 1. **归属校验**。退款凭证是隐私内容。只写磁盘意味着任何人拿到 URL
>    就能看别人的退货照片——而 URL 是会被转发、被日志记录、被截图的。
>    必须能查「这个文件属于谁」。
> 2. **孤儿回收**。用户上传了凭证但没提交退款申请，文件就永远躺在磁盘上。
> 3. **迁移能力**。将来换 S3 driver 要批量搬运存量文件，没有清单就搬不了。
>
> **孤儿回收的安全条件**：`referenced` 必须在**引用它的业务对象落库的同一个事务内**
> 置为 TRUE，而不是靠事后扫描反查。否则存在这样的窗口——退款申请刚提交、
> 清理任务恰好扫到、凭证被删，而用户界面上那条申请还带着一个 404 的图片链接。
> 清理任务只删 `NOT referenced AND created_at < now() - interval '24 hours'`，
> 24 小时是给「上传了但还没点提交」的用户留的余量。
>
> 清理任务挂在 §11 已有的 `jobs` 表上，不引入新机制。
```

以及 Storage 抽象的取舍说明：

```markdown
### Storage 接口抽象

定义三个方法即可覆盖全部需求：写入（返回 `storage_key`）、删除、生成访问 URL。
一期只实现本地磁盘 driver，S3 driver 留位置不实现。

> **为什么不直接上 MinIO**：README 的核心卖点之一是「只有一个数据库，
> 不需要 Redis / RabbitMQ / ES」。加对象存储要么破坏这句话，要么逼着改写卖点。
> 本地磁盘 driver 不是新增组件——它就是容器的一个卷。
>
> 更重要的是这与 dtmrs 那套「部署形态是配置，不是重写」**同构**：
> 形态 A 用本地磁盘，形态 B/C 换 S3，业务代码不改。
> 架构主张应当在每一处都成立，而不只在事务层成立。
>
> 代价诚实说两条：形态 A 多实例部署时要共享卷；换 S3 时确实会多一个组件。
```

- [ ] **Step 4: OpenAPI 新增上传接口**

新增 `POST /uploads`（`multipart/form-data`），以及 `UploadTarget`、`Upload` 两个 schema。

要点（Review Focus 第 3 条在此落地）——**契约必须表达大小与类型限制**，
否则客户端只能试到 413 才知道：

- `UploadTarget` 为整数枚举 `[1, 2, 3]`（1 商品图 / 2 头像 / 3 退款凭证），
  与 `uploads.purpose` 逐值一致
- `Upload` 返回 `id` / `url` / `content_type` / `size_bytes` / `created_at`
- 在 `POST /uploads` 的 `description` 中明确写出：
  单文件上限 10 MB；允许的 `content_type` 为 `image/jpeg`、`image/png`、`image/webp`
- 定义错误响应：413（超过大小上限）、415（类型不被允许）、401
- 遵循 RFC 9457：错误体用 Problem Details，复用 `components/responses` 中已有定义
- 带 `Idempotency-Key`（写接口，遵循全局约定第 5 条）

- [ ] **Step 5: README 双语的「只有一个数据库」加脚注**

在两份 README 中该句之后补一行：

- 中文：`> 文件（商品图、头像、退款凭证）默认写本地磁盘卷，不是额外的服务。换 S3 形态时才会多一个组件。`
- 英文：`> Files (product images, avatars, refund evidence) go to a local disk volume by default — not another service. Switching to the S3 driver is what adds a component.`

- [ ] **Step 6: 运行三个校验器，确认全绿**

Run: `python3 scripts/check_openapi.py && python3 scripts/check_promises.py && python3 scripts/check_links.py`

Expected: `check_openapi.py` 退出码 0；`check_promises.py` 退出码 0；
`check_links.py` 仅剩 `ai-capabilities.md` 一处（Task 5 创建）。

- [ ] **Step 7: 提交**

```bash
git add -A
git commit -m "docs: design file storage with a driver abstraction

Adds the uploads table, the Storage interface rationale, and POST /uploads.
The contract asked clients for evidence_urls and avatar_url with no way to
obtain either.

Local disk is the only driver in scope; S3 keeps its slot. Orphan cleanup
runs on the existing jobs table.

Add scripts/check_openapi.py as the structural gate.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>"
```

---

### Task 4: OpenAPI S1–S16

**Files:**
- Modify: `scripts/check_openapi.py`（追加断言）
- Modify: `docs/电商系统-OpenAPI.yaml`

**Interfaces:**
- Consumes: `scripts/check_openapi.py`（Task 3）的 `REQUIRED_PATHS` / `REQUIRED_SCHEMAS` 机制
- Produces: 无新接口供后续任务消费

- [ ] **Step 1: 读清单**

完整读 `/tmp/claude-1000/-home-jeffwang-work-keel/e5743e59-8a7d-4a1b-98fa-19f3d94f30ad/scratchpad/openapi-gap.md`
的「建议改」一档（S1–S16）。若该临时文件已不存在，则以本计划下方列出的四条优先项为准，
其余按契约与 `docs/电商系统-数据模型设计.md` 自行核对补齐。

- [ ] **Step 2: 把四条优先项写成失败的断言**

在 `scripts/check_openapi.py` 中，于 `REQUIRED_SCHEMAS` 之后追加字段级断言表并在
`main()` 的 schema 检查之后执行：

```python
# (schema 名, 必须存在的属性名)
REQUIRED_FIELDS = [
    ('Cart', 'selected_total_cents'),   # S1
    ('OrderItem', 'refunding_qty'),     # S2
    ('OrderDetail', 'refunds'),         # S15
]
```

在 `main()` 中 `REQUIRED_SCHEMAS` 循环之后插入：

```python
    def props_of(schema_name):
        """取 schema 的属性名集合。OrderDetail 用 allOf 继承 Order，
        只查顶层 properties 会漏掉继承来的字段，必须展开 allOf。"""
        node = schemas.get(schema_name, {})
        names = set(node.get('properties', {}))
        for branch in node.get('allOf', []):
            if '$ref' in branch:
                names |= props_of(branch['$ref'].rsplit('/', 1)[-1])
            else:
                names |= set(branch.get('properties', {}))
        return names

    for schema_name, field in REQUIRED_FIELDS:
        if field not in props_of(schema_name):
            problems.append('schema %s 缺少字段: %s' % (schema_name, field))

    # S3: GET /orders 必须支持按售后状态筛选
    get_orders = paths.get('/orders', {}).get('get', {})
    names = {p.get('name') for p in get_orders.get('parameters', [])
             if isinstance(p, dict)}
    if 'refund_status' not in names:
        problems.append('GET /orders 缺少 refund_status 查询参数')
```

- [ ] **Step 3: 运行，确认它失败**

Run: `python3 scripts/check_openapi.py`

Expected: 退出码 1，报出四条：`Cart` 缺 `selected_total_cents`、
`OrderItem` 缺 `refunding_qty`、`OrderDetail` 缺 `refunds`、
`GET /orders` 缺 `refund_status` 查询参数。

- [ ] **Step 4: 实现四条优先项**

- **S1** `Cart` 增加 `selected_total_cents`（int64，单位分）。
  有了 `selected` 之后 `total_cents` 语义歧义——是全车总额还是勾选总额？
  保留 `total_cents` 为全车总额，新增字段表示勾选部分，两者都写进 `description` 说明。
- **S2** `OrderItem` 增加 `refunding_qty`（integer，退款流程中占用的件数）。
  没有它前端算不出「本行还可退几件」，只能提交后被 409 打回。
  可退件数 = `quantity - refunded_qty - refunding_qty`，把这个算式写进 `description`。
- **S15** `OrderDetail` 增加 `refunds` 数组（`$ref: Refund`）。
  已有 `payments` 数组却没有 `refunds`，不对称。
- **S3** `GET /orders` 增加 `refund_status` 查询参数（`$ref: OrderRefundStatus`，可选）。

- [ ] **Step 5: 逐条处理其余 S 项，并记录判定**

对 S4–S14、S16 逐条评估。**判定为「不做」是允许的，但必须写明理由**——
在契约对应位置以 YAML 注释记录，格式：

```yaml
# S7 不做：<一句话理由>
```

不要静默跳过。将来有人重新发现同一个问题时，要能看到上一次是怎么想的。

- [ ] **Step 6: 运行校验器，确认全绿**

Run: `python3 scripts/check_openapi.py`

Expected: 退出码 0，且路径数与 schema 数不少于 Task 3 结束时的数量。

- [ ] **Step 7: 提交**

```bash
git add -A
git commit -m "docs: apply the S-tier contract findings

Cart gains selected_total_cents, OrderItem gains refunding_qty,
OrderDetail gains refunds, GET /orders can filter by refund_status.

Items judged not-worth-doing carry an inline comment saying why, so the
next person to notice them can see the previous reasoning.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>"
```

---

### Task 5a: AI 能力全景起草

**Files:**
- Create: `docs/ai-capabilities.md`

**Interfaces:**
- Consumes: 无（新建文件，不依赖其他任务）
- Produces: `docs/ai-capabilities.md` —— Task 5b 读取其条目数

> **可并行**：本任务只新建文件，不修改任何既有文件，可与 Task 1–2 同时进行。

- [ ] **Step 1: 从四处来源抽取能力项**

逐一读取并抽取：

1. `README.zh-CN.md` 的能力表格（进货 / 卖货 / 经营 / 反馈四栏）
2. `docs/电商系统-总体架构.md` §11「AI 能力全景」正文，含三个底座
   （推理服务 embed/rerank/generate/forecast、商品理解服务、数据智能层）
   与五项需数据积累的功能（个性化推荐、尺码推荐、个性化排序、促销预测、刷单识别）
3. `docs/电商系统-商品理解服务设计.md` §1 的 8 个 processor 清单
4. `docs/电商系统-语义检索层设计.md` 的查询理解、Query Rewrite、业务重排等能力

去重后成表。

- [ ] **Step 2: 写文档**

`docs/ai-capabilities.md` 结构：

```markdown
# AI 能力全景

（一段导语：说明本文是清单，判断与优先级见架构文档 §11）

## 一、三个底座

（推理服务 / 商品理解服务 / 数据智能层，各自说明为什么是底座——
建成后清单里约三分之二的功能退化为「调两个接口 + 一段 prompt + 一个页面」）

## 二、能力清单

| # | 能力 | 环节 | 价值 | 难度 | 依赖 | 需数据积累 |
|---|---|---|---|---|---|---|
（逐项填写。环节取值：进货 / 卖货 / 经营 / 反馈 / 底座。
价值与难度用 高/中/低。依赖填底座名或其他能力编号。）

## 三、早期做不出效果的五项

（个性化推荐、尺码推荐、个性化排序、促销预测、刷单识别。
说明为什么——依赖用户行为数据积累，预留接口，不前期投入。）

## 四、三条纪律

- 每个 AI 功能必须有评测方式
- 写操作永不由模型执行
- AI 故障不得阻塞交易主链路
```

**严禁为了凑到 37 项而注水。** 实事求是抽出多少写多少，
数字对不上由 Task 5b 改架构文档，不改这里。

- [ ] **Step 3: 自查**

通读一遍，确认：每一项都能追溯到上述四处来源之一；没有「智能化升级」
这类无法验收的条目；每项的「依赖」列填的是真实存在的底座或能力编号。

- [ ] **Step 4: 提交**

```bash
git add docs/ai-capabilities.md
git commit -m "docs: write the AI capability inventory

The architecture doc promised this file and a count; neither existed.
Derived from the README matrix, the processor list, and the search design.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>"
```

---

### Task 5b: 架构 §11 对齐

**Files:**
- Create: `scripts/check_capabilities.py`
- Modify: `docs/电商系统-总体架构.md` §11

**Interfaces:**
- Consumes: `docs/ai-capabilities.md`（Task 5a）；Task 1 已修好的文档路径
- Produces: `scripts/check_capabilities.py` —— 校验架构 §11 声称的条目数与实际一致

> **必须排在 Task 1 之后**：本任务与 Task 1 都写 `docs/电商系统-总体架构.md`。

- [ ] **Step 1: 写校验器（此时它应当失败）**

创建 `scripts/check_capabilities.py`：

```python
#!/usr/bin/env python3
"""校验架构文档 §11 声称的 AI 能力条目数与 ai-capabilities.md 实际条目数一致。"""
import io
import os
import re
import sys

ARCH = 'docs/电商系统-总体架构.md'
CAPS = 'docs/ai-capabilities.md'
# 匹配「完整清单（37 项，...」这类声明
CLAIM_RE = re.compile(r'完整清单[（(]\s*(\d+)\s*项')
# 能力清单表格的数据行：| 1 | 名称 | ...
ROW_RE = re.compile(r'^\|\s*(\d+)\s*\|')


def main():
    root = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
    arch_path = os.path.join(root, ARCH)
    caps_path = os.path.join(root, CAPS)

    if not os.path.exists(caps_path):
        print('能力清单文档不存在: %s' % CAPS)
        return 1

    arch = io.open(arch_path, encoding='utf-8').read()
    caps = io.open(caps_path, encoding='utf-8').read()

    claims = CLAIM_RE.findall(arch)
    if not claims:
        print('架构文档中找不到「完整清单（N 项」的声明，无法校验')
        return 1

    actual = len([ln for ln in caps.split('\n') if ROW_RE.match(ln)])
    problems = []
    for claimed in claims:
        if int(claimed) != actual:
            problems.append(
                '架构 §11 声称 %s 项，%s 实际 %d 项' % (claimed, CAPS, actual))

    if problems:
        print('发现 %d 处不一致：' % len(problems))
        for p in problems:
            print('  ' + p)
        print('\n处理方向：改架构文档里的数字，不要为了凑数往清单里注水。')
        return 1
    print('架构 §11 声称的条目数与实际一致（%d 项）' % actual)
    return 0


if __name__ == '__main__':
    sys.exit(main())
```

- [ ] **Step 2: 运行，确认它失败**

Run: `python3 scripts/check_capabilities.py`

Expected: 退出码 1，报出架构声称 37 项而实际为 N 项（N 为 Task 5a 的实际产出）。
若恰好相等则此步直接通过，进入 Step 3 只需修链接。

- [ ] **Step 3: 改架构 §11 的数字与链接**

把 §11 中的 `完整清单（37 项，含价值 / 难度 / 依赖 / 是否需数据积累）见
[AI 能力全景](./ai-capabilities.md)` 改为实际条目数。

链接路径此时已由 Task 1 处理为 `./ai-capabilities.md`（同在 `docs/` 下，相对路径正确）。

**Review Focus 第 5 条**：方向是**改架构里的数字**。如果你发现自己正在往
`ai-capabilities.md` 里添加条目以凑够 37，停下——那是在为了一个随口写下的数字
制造内容。那个数字本来就没有依据。

- [ ] **Step 4: 运行全部校验器，确认全绿**

Run:

```bash
python3 scripts/check_links.py && \
python3 scripts/check_promises.py && \
python3 scripts/check_openapi.py && \
python3 scripts/check_capabilities.py
```

Expected: 四个全部退出码 0。`check_links.py` 此时应当**完全无报错**——
`ai-capabilities.md` 与 `CONTRIBUTING.md` 都已创建。

- [ ] **Step 5: 提交**

```bash
git add -A
git commit -m "docs: align the architecture doc with the real capability count

Section 11 claimed 37 items against a file that did not exist. The count
now matches what the inventory actually contains.

Add scripts/check_capabilities.py so the claim and the file cannot drift.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>"
```

---

### Task 6: 收口

**Files:**
- Create: `scripts/check-all.sh`
- Modify: `CONTRIBUTING.md`（补一节说明校验脚本）

**Interfaces:**
- Consumes: 全部四个校验脚本
- Produces: `scripts/check-all.sh` —— 一条命令跑全部校验，供将来接入 CI

- [ ] **Step 1: 写汇总脚本**

创建 `scripts/check-all.sh`：

```bash
#!/usr/bin/env bash
# 跑全部文档校验。任一失败即整体失败。
set -uo pipefail

cd "$(dirname "$0")/.."

fail=0
for script in check_links check_promises check_openapi check_capabilities; do
    printf '\n=== %s ===\n' "$script"
    if ! python3 "scripts/${script}.py"; then
        fail=1
    fi
done

if [ "$fail" -ne 0 ]; then
    echo ''
    echo '校验未通过。'
    exit 1
fi
echo ''
echo '全部校验通过。'
```

然后 `chmod +x scripts/check-all.sh`。

- [ ] **Step 2: 运行，确认全绿**

Run: `./scripts/check-all.sh`

Expected: 退出码 0，四段输出全部通过。

- [ ] **Step 3: 在 CONTRIBUTING.md 补一节**

```markdown
## 提交前自查

```bash
./scripts/check-all.sh
```

这条命令会校验：文档链接是否有效（含中文文件名的百分号编码与锚点）、
README 承诺的文件是否真实存在、OpenAPI 契约结构是否有效且无悬空引用、
架构文档声称的 AI 能力条目数是否与清单一致。

改动文档或契约的 PR 必须先让它通过。
```

- [ ] **Step 4: 提交**

```bash
git add -A
git commit -m "chore: add scripts/check-all.sh and document it

One command runs every documentation gate. Ready to wire into CI when
the engineering skeleton lands in M1.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>"
```

- [ ] **Step 5: 确认 spec 的全局验收标准逐条达成**

对照 `docs/superpowers/specs/2026-09-25-design-debt-closure-design.md` 第六节：

1. 全项目零坏链接 —— `check_links.py` 退出码 0
2. OpenAPI 3.1 VALID + 零悬空 `$ref` + 无孤儿 schema —— `check_openapi.py` 退出码 0
3. README 中每个宣称存在的文件都真实存在 —— `check_promises.py` 退出码 0
4. 架构 §11 承诺的数字与实际一致 —— `check_capabilities.py` 退出码 0
5. 每组独立成 commit —— `git log --oneline` 应有 6 个以上本轮提交

任一条未达成，回到对应任务修复，不要在收口任务里打补丁。
