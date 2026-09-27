# Flutter 买家端 · 第 1 步（骨架）实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 在 `flutter_app/` 立起 Flutter 买家端的骨架：契约生成的 Dart 类型与闸门、请求层（错误映射 / 幂等键 / 401 单飞续期）、会话、当前门店、首页商品网格、登录与「我的」，并打通「契约 → 页面 → 单测 → Web 无头 e2e → mp-flutter 小程序编译」整条管线。

**Architecture:** 契约字段只在 `lib/api/` 里读写：`schema.g.dart` 由 `scripts/gen_dart_schema.py` 从 OpenAPI 生成并由闸门比对；`client.dart` 是唯一的网络出口；`view.dart` 把契约类型翻成页面行模型；页面只碰行模型。全局状态只有 `ChangeNotifier`（会话、当前门店）。

**Tech Stack:** Flutter 3.41.9 stable（`~/development/flutter`）、Dart 3.11、`http`、`shared_preferences`、`go_router`；Python 3 + PyYAML（生成器）；integration_test + chromedriver（Web 无头 e2e）；mp-flutter（`~/work/mp-flutter/packages/mp_flutter`，dev 依赖）。

**Spec:** `docs/superpowers/specs/2026-09-27-flutter-buyer-app-design.md`（本计划实现其中「交付顺序」第 1 步；第 2～5 步在本步落地后各写一份计划）

## Global Constraints

- Flutter SDK 用官方 stable：`FLUTTER ?= $(HOME)/development/flutter/bin/flutter`（PATH 上默认的是 flutter_ohos，不用它）。
- 依赖只用 mp-flutter 能透明接管的：`http`、`shared_preferences`；路由 `go_router`（纯 Dart）。不引入状态管理框架、不依赖 Cookie、不用原生插件（定位等第一阶段不接，门店走「回落默认店」）。
- 契约字段只在 `lib/api/` 里出现；`lib/pages/`、`lib/widgets/` 不许 import `api/schema.g.dart`（闸门强制）。
- `schema.g.dart` 请勿手改；契约一改须 `make flutter-generate` 并一起提交，闸门比对。
- 金额一律 `int`（分）；页面只显示服务端算好的数，客户端不复算。
- 时间是 UTC 的 RFC3339，显示按设备本地时区（`DateTime.parse(...).toLocal()`）。
- 服务端给的资源地址是相对路径（`/api/v1/uploads/..`），非 Web 同源时补服务地址的 origin。
- 视觉照 `app/src/App.uvue`：底 `#F6F1EA`、卡片 `#FFFDF9`、分割线 `#ECE4DA`、主色 `#3B2A20`、强调 `#B5733A`、文字 `#2A211B / #7A6E64 / #A89C90`、状态 ok `#5E7D5A` / warn `#B7791F` / err `#B4462E`。
- e2e 用专用买家（口令读 `~/.config/keel/e2e.env`，与 uni-app x 的 e2e 相同），按 widget `Key` 找元素。
- mp-flutter 的问题直接发消息给 mp-flutter 会话，不在 App 里绕开、不改 mp-flutter 仓库。
- 提交信息结尾带仓库要求的 Co-Authored-By / Claude-Session 两行。

## Review Focus

1. **契约字段缺失或多出**：服务端响应里少了非 required 字段、或多了新字段 —— `fromJson` 不崩，多出的忽略、缺的为 null。（Task 3 生成器单测 + Task 4 往返测试）
2. **整数字段来了浮点 / 数字类型不稳**：JSON 里 `12` 与 `12.0` 都要能进 `int` / `double` 字段。（Task 3 单测）
3. **令牌过期时多个请求同时 401**：只刷新一次，其余排队，刷完用原幂等键重放；刷新失败只跳一次登录页。（Task 5 单测）
4. **错误响应体不是 Problem**（网关 502 的 HTML、空体）：给出可读的 message，不抛解析异常。（Task 5 单测）
5. **Web 与小程序的服务地址**：Web 同源用相对路径；小程序（`Uri.base` 为 `https://mp.local/`）没注入绝对地址时启动即报清楚的错，而不是请求打到 mp.local。（Task 5 单测）

---

## File Structure

```
scripts/contract_operations.py      新：OPERATIONS 清单（UTS 与 Dart 两个生成器共用）
scripts/gen_uts_schema.py           改：OPERATIONS 改为 import（产物不变）
scripts/gen_dart_schema.py          新：OpenAPI → lib/api/schema.g.dart
scripts/test_gen_dart_schema.py     新：生成器单测（小型 spec 夹具）
scripts/check_dart_contract.py      新：契约闸门
scripts/check_flutter_pages.py      新：页面闸门
scripts/check-all.sh                改：接入两道新闸门
Makefile                            改：flutter-* 目标
flutter_app/
  pubspec.yaml, analysis_options.yaml, .gitignore, README.md, mp_flutter.yaml
  web_dev_config.yaml               （e2e 时由脚本按 KEEL_API_BASE 生成，gitignore）
  lib/main.dart                     启动
  lib/theme.dart                    色板 / 字号 / ThemeData
  lib/router.dart                   go_router + 底部 tab 外壳
  lib/config.dart                   服务地址解析
  lib/api/schema.g.dart             生成物
  lib/api/client.dart               请求层
  lib/api/session.dart              会话
  lib/api/store.dart                当前门店
  lib/api/view.dart                 行模型
  lib/api/services.dart             把上面几样装在一起给页面用（InheritedWidget）
  lib/widgets/product_card.dart     商品卡
  lib/widgets/states.dart           加载 / 空态 / 错误卡
  lib/pages/home_page.dart, login_page.dart, me_page.dart
  test/…                            单测（见各 Task）
  test/fixtures/*.json              真实响应样本
  integration_test/smoke_test.dart  e2e
  integration_test/e2e_env.dart     e2e 账号 / 地址读取
  test_driver/integration_test.dart flutter drive 入口
  tool/e2e_web.sh                   生成 web_dev_config.yaml、取 chromedriver、跑 flutter drive
```

---

### Task 1: 工程骨架与主题

**Files:**
- Create: `flutter_app/`（`flutter create`）、`flutter_app/lib/theme.dart`、`flutter_app/test/theme_test.dart`
- Modify: `flutter_app/pubspec.yaml`、`flutter_app/analysis_options.yaml`、`flutter_app/.gitignore`、`Makefile`

**Interfaces:**
- Produces: `KeelColors`（静态常量 `bg`, `card`, `line`, `primary`, `accent`, `text`, `textSub`, `textHint`, `ok`, `warn`, `err`）、`ThemeData keelTheme()`、Makefile 变量 `FLUTTER`、`FLUTTER_APP`

- [ ] **Step 1: 建工程**

```bash
cd /Users/jeff/work/keel
~/development/flutter/bin/flutter create --org dev.keel --project-name keel_buyer --platforms=android,ios,web flutter_app
```

- [ ] **Step 2: 依赖与 lint**

`flutter_app/pubspec.yaml` 的 `dependencies` / `dev_dependencies` 改为：

```yaml
environment:
  sdk: ^3.11.0

dependencies:
  flutter:
    sdk: flutter
  http: ^1.2.2
  shared_preferences: ^2.3.2
  go_router: ^14.6.0

dev_dependencies:
  flutter_test:
    sdk: flutter
  integration_test:
    sdk: flutter
  flutter_driver:
    sdk: flutter
  flutter_lints: ^5.0.0
```

`flutter_app/analysis_options.yaml`：

```yaml
include: package:flutter_lints/flutter.yaml
analyzer:
  exclude:
    - lib/api/schema.g.dart
linter:
  rules:
    prefer_const_constructors: true
```

`flutter_app/.gitignore` 末尾追加：

```
web_dev_config.yaml
.tools/
build/
```

运行：`cd flutter_app && ~/development/flutter/bin/flutter pub get` —— Expected：`Got dependencies!`

- [ ] **Step 3: 写主题的失败测试** `flutter_app/test/theme_test.dart`

```dart
import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:keel_buyer/theme.dart';

void main() {
  test('色板与 uni-app x 的 App.uvue 一致', () {
    expect(KeelColors.bg, const Color(0xFFF6F1EA));
    expect(KeelColors.card, const Color(0xFFFFFDF9));
    expect(KeelColors.primary, const Color(0xFF3B2A20));
    expect(KeelColors.err, const Color(0xFFB4462E));
  });

  test('主题：底色与主色', () {
    final t = keelTheme();
    expect(t.scaffoldBackgroundColor, KeelColors.bg);
    expect(t.colorScheme.primary, KeelColors.primary);
  });
}
```

运行：`~/development/flutter/bin/flutter test test/theme_test.dart` —— Expected：FAIL（`theme.dart` 不存在）

- [ ] **Step 4: 实现** `flutter_app/lib/theme.dart`

```dart
import 'package:flutter/material.dart';

/// 设计系统：照 app/src/App.uvue（uni-app x 买家端）搬过来，两端看起来是同一个产品。
/// 改色板只改这一个文件。
class KeelColors {
  static const bg = Color(0xFFF6F1EA);
  static const card = Color(0xFFFFFDF9);
  static const line = Color(0xFFECE4DA);
  static const primary = Color(0xFF3B2A20);
  static const accent = Color(0xFFB5733A);
  static const text = Color(0xFF2A211B);
  static const textSub = Color(0xFF7A6E64);
  static const textHint = Color(0xFFA89C90);
  static const ok = Color(0xFF5E7D5A);
  static const warn = Color(0xFFB7791F);
  static const err = Color(0xFFB4462E);
  static const chipBorder = Color(0xFFE3D9CC);
  static const searchBox = Color(0xFFEFE7DC);
}

class KeelText {
  static const display = TextStyle(fontSize: 28, fontWeight: FontWeight.w700, color: KeelColors.text);
  static const title = TextStyle(fontSize: 18, fontWeight: FontWeight.w700, color: KeelColors.text);
  static const section = TextStyle(fontSize: 16, fontWeight: FontWeight.w700, color: KeelColors.text);
  static const body = TextStyle(fontSize: 14, color: KeelColors.text);
  static const sub = TextStyle(fontSize: 13, color: KeelColors.textSub);
  static const hint = TextStyle(fontSize: 12, color: KeelColors.textHint);
  static const overline = TextStyle(fontSize: 12, fontWeight: FontWeight.w700, letterSpacing: 3, color: KeelColors.accent);
  static const price = TextStyle(fontSize: 16, fontWeight: FontWeight.w700, color: KeelColors.primary);
  static const err = TextStyle(fontSize: 13, color: KeelColors.err, height: 1.5);
  static const ok = TextStyle(fontSize: 13, color: KeelColors.ok, height: 1.5);
}

ThemeData keelTheme() {
  return ThemeData(
    useMaterial3: true,
    scaffoldBackgroundColor: KeelColors.bg,
    colorScheme: ColorScheme.fromSeed(
      seedColor: KeelColors.primary,
      primary: KeelColors.primary,
      surface: KeelColors.card,
    ),
    appBarTheme: const AppBarTheme(
      backgroundColor: KeelColors.bg,
      foregroundColor: KeelColors.text,
      elevation: 0,
      centerTitle: true,
    ),
    cardTheme: CardThemeData(
      color: KeelColors.card,
      elevation: 0,
      shape: RoundedRectangleBorder(borderRadius: BorderRadius.circular(16)),
    ),
    filledButtonTheme: FilledButtonThemeData(
      style: FilledButton.styleFrom(
        backgroundColor: KeelColors.primary,
        foregroundColor: KeelColors.card,
        minimumSize: const Size.fromHeight(48),
        shape: RoundedRectangleBorder(borderRadius: BorderRadius.circular(24)),
      ),
    ),
    dividerColor: KeelColors.line,
  );
}
```

运行：`~/development/flutter/bin/flutter test test/theme_test.dart` —— Expected：PASS

- [ ] **Step 5: Makefile 目标**

在 `Makefile` 的 `.PHONY` 行追加 `flutter-get flutter-analyze flutter-test`，并加：

```makefile
# Flutter 买家端（flutter_app/）。用官方 stable：PATH 上默认的是 flutter_ohos。
FLUTTER ?= $(HOME)/development/flutter/bin/flutter
FLUTTER_APP := $(ROOT)/flutter_app

flutter-get:
	cd $(FLUTTER_APP) && $(FLUTTER) pub get

flutter-analyze:
	cd $(FLUTTER_APP) && $(FLUTTER) analyze

flutter-test:
	cd $(FLUTTER_APP) && $(FLUTTER) test
```

删掉 `flutter create` 生成的 `test/widget_test.dart`（它测的是计数器模板，Task 7 会重写 `main.dart`）。

运行：`make flutter-analyze && make flutter-test` —— Expected：`No issues found!`、`All tests passed!`

- [ ] **Step 6: 提交**

```bash
git add flutter_app Makefile
git commit -m "Flutter 买家端：工程骨架与主题（色板照 uni-app x 的 App.uvue）"
```

---

### Task 2: 接口清单抽成共用模块

**Files:**
- Create: `scripts/contract_operations.py`
- Modify: `scripts/gen_uts_schema.py:58-93`（删掉 `OPERATIONS = [...]` 定义，改为 import）

**Interfaces:**
- Produces: `contract_operations.OPERATIONS : list[tuple[str, str, str]]`（方法、路径、名字前缀），内容与原 `gen_uts_schema.py` 里的逐条相同

- [ ] **Step 1: 记下现状**

运行：`V=<PyYAML venv>; $V/bin/python scripts/check_uts_contract.py` —— Expected：`check_uts_contract OK`

- [ ] **Step 2: 搬清单**

把 `scripts/gen_uts_schema.py` 里 `OPERATIONS = [` 到对应 `]` 的整段（含注释）原样剪切到新文件 `scripts/contract_operations.py`，文件头加：

```python
"""买家端用到的接口清单（方法, 路径, 名字前缀）。

UTS（gen_uts_schema.py）与 Dart（gen_dart_schema.py）两个生成器共用这一份：
两端接口面一致，新接口加在这里，两个生成物一起变、各自的闸门一起比对。
"""
```

`scripts/gen_uts_schema.py` 原位置改为：

```python
from contract_operations import OPERATIONS  # noqa: E402  共用清单，见该文件说明
```

（放在 `import yaml` 之后；脚本以 `python scripts/gen_uts_schema.py` 运行，`scripts/` 在 `sys.path[0]`。）

- [ ] **Step 3: 验证产物不变**

运行：`$V/bin/python scripts/check_uts_contract.py` —— Expected：`check_uts_contract OK`（逐字相同）

- [ ] **Step 4: 提交**

```bash
git add scripts/contract_operations.py scripts/gen_uts_schema.py
git commit -m "契约生成：接口清单抽成 contract_operations.py，UTS 与 Dart 生成器共用"
```

---

### Task 3: Dart 类型生成器与契约闸门

**Files:**
- Create: `scripts/gen_dart_schema.py`、`scripts/test_gen_dart_schema.py`、`scripts/check_dart_contract.py`
- Create（生成）: `flutter_app/lib/api/schema.g.dart`
- Modify: `scripts/check-all.sh`、`Makefile`

**Interfaces:**
- Consumes: `contract_operations.OPERATIONS`；`gen_uts_schema.Gen` 的遍历规则（`allOf` 展平、单成员 `allOf` 透出、内联对象提升命名 `父名 + 属性 PascalCase`）
- Produces:
  - 每个对象 schema 一个 Dart 类：字段 lowerCamelCase（JSON 键保持原样），`const` 构造，`factory X.fromJson(Map<String, dynamic> j)`，`Map<String, dynamic> toJson()`
  - 标量 schema 一个 `typedef`（如 `typedef Money = int;`）
  - 自由对象（只有 `additionalProperties`）：`Map<String, String>`（`additionalProperties: {type: string}`）或 `Map<String, dynamic>`
  - `make flutter-generate`；`scripts/check_dart_contract.py` 退出码 0 = 同步

- [ ] **Step 1: 写生成器的失败测试** `scripts/test_gen_dart_schema.py`

```python
"""gen_dart_schema 的单测：用一份小 spec 夹具覆盖映射规则。运行：python -m unittest scripts/test_gen_dart_schema.py"""
import os
import sys
import unittest

sys.path.insert(0, os.path.dirname(__file__))
import gen_dart_schema as g  # noqa: E402

SPEC = {
    'paths': {},
    'components': {
        'parameters': {},
        'schemas': {
            'Money': {'type': 'integer', 'description': '分'},
            'Item': {
                'type': 'object',
                'required': ['id', 'price_cents', 'tags', 'note'],
                'properties': {
                    'id': {'type': 'integer'},
                    'price_cents': {'$ref': '#/components/schemas/Money'},
                    'weight': {'type': 'number'},
                    'tags': {'type': 'array', 'items': {'type': 'string'}},
                    'note': {'type': ['string', 'null']},
                    'spec_values': {'type': 'object', 'additionalProperties': {'type': 'string'}},
                    'owner': {'type': 'object', 'properties': {'name': {'type': 'string'}}},
                    'default': {'type': 'boolean'},
                },
            },
            'Page': {'allOf': [
                {'type': 'object', 'required': ['total'], 'properties': {'total': {'type': 'integer'}}},
                {'type': 'object', 'required': ['items'], 'properties': {
                    'items': {'type': 'array', 'items': {'$ref': '#/components/schemas/Item'}}}},
            ]},
        },
    },
}


class GenDartTest(unittest.TestCase):
    def setUp(self):
        self.out = g.render(SPEC, operations=[])

    def test_scalar_alias(self):
        self.assertIn('typedef Money = int;', self.out)

    def test_required_and_optional_fields(self):
        self.assertIn('final int id;', self.out)
        self.assertIn('final Money priceCents;', self.out)
        self.assertIn('final double? weight;', self.out)
        self.assertIn('final List<String> tags;', self.out)
        self.assertIn('final String? note;', self.out)
        self.assertIn('final Map<String, String>? specValues;', self.out)

    def test_inline_object_promoted(self):
        self.assertIn('class ItemOwner {', self.out)
        self.assertIn('final ItemOwner? owner;', self.out)

    def test_reserved_word_escaped(self):
        self.assertIn('final bool? default_;', self.out)
        self.assertIn("'default': default_", self.out)

    def test_number_tolerance(self):
        # 整数字段收到 12.0、浮点字段收到 12 都要能进
        self.assertIn("(j['id'] as num).toInt()", self.out)
        self.assertIn("(j['weight'] as num?)?.toDouble()", self.out)

    def test_nullable_required_written_even_if_null(self):
        # type [X, null] 且 required：toJson 恒写这个键；非 required 为 null 时不写
        self.assertIn("'note': note,", self.out)
        self.assertIn("if (weight != null) 'weight': weight,", self.out)

    def test_all_of_flattened(self):
        self.assertIn('class Page {', self.out)
        self.assertIn('final int total;', self.out)
        self.assertIn('final List<Item> items;', self.out)
        self.assertIn("(j['items'] as List).map((e) => Item.fromJson(e as Map<String, dynamic>)).toList()", self.out)

    def test_unknown_operation_fails(self):
        with self.assertRaises(SystemExit):
            g.render(SPEC, operations=[('get', '/nope', 'Nope')])


if __name__ == '__main__':
    unittest.main()
```

运行：`$V/bin/python -m unittest scripts/test_gen_dart_schema.py` —— Expected：FAIL（`No module named gen_dart_schema`）

- [ ] **Step 2: 实现生成器** `scripts/gen_dart_schema.py`

```python
#!/usr/bin/env python3
"""从 OpenAPI 契约生成 Dart 侧的契约类型（flutter_app/lib/api/schema.g.dart）。

与 gen_uts_schema.py 同一份契约、同一份接口清单（contract_operations.py）、同一套遍历规则
（allOf 展平、单成员 allOf 透出、内联对象提升成「父名 + 属性名」的具名类型），只换输出语言。
产物入库，scripts/check_dart_contract.py 比对。

映射：integer→int，number→double，string→String，boolean→bool，array→List<T>，
$ref→被引用的类型，additionalProperties:{type:string}→Map<String,String>（其他自由对象→Map<String,dynamic>），
type:[X,null]→X?；非 required→X?（toJson 为 null 时不写键）；枚举不生成 enum（服务端加值时旧客户端不崩）。
"""
import argparse
import os
import re
import sys

try:
    import yaml
except ImportError:  # pragma: no cover
    sys.exit('需要 PyYAML：pip install pyyaml')

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from contract_operations import OPERATIONS  # noqa: E402

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
CONTRACT = os.path.join(ROOT, 'docs', '电商系统-OpenAPI.yaml')
OUT = os.path.join(ROOT, 'flutter_app', 'lib', 'api', 'schema.g.dart')

HEADER = '''// 由 scripts/gen_dart_schema.py 从 docs/电商系统-OpenAPI.yaml 生成。**请勿手改。**
//
// 手改会被 scripts/check_dart_contract.py 抓住：改契约 -> `make flutter-generate` -> 一起提交。
// 只有类型与 JSON 编解码，没有网络代码 —— 网络在 lib/api/client.dart。
// ignore_for_file: non_constant_identifier_names, unnecessary_cast, prefer_null_aware_operators
'''

DART_RESERVED = {
    'abstract', 'as', 'assert', 'async', 'await', 'break', 'case', 'catch', 'class', 'const',
    'continue', 'default', 'deferred', 'do', 'dynamic', 'else', 'enum', 'export', 'extends',
    'extension', 'external', 'factory', 'false', 'final', 'finally', 'for', 'get', 'if',
    'implements', 'import', 'in', 'interface', 'is', 'late', 'library', 'mixin', 'new', 'null',
    'on', 'operator', 'part', 'required', 'rethrow', 'return', 'set', 'show', 'static', 'super',
    'switch', 'sync', 'this', 'throw', 'true', 'try', 'typedef', 'var', 'void', 'while', 'with', 'yield',
}

# 类型描述：('prim', 'int'|'double'|'String'|'bool'|'dynamic') | ('list', T) | ('ref', Name) | ('map', 'String'|'dynamic')


def camel(name):
    parts = [p for p in re.split(r'[^0-9A-Za-z]+', name) if p]
    s = parts[0] + ''.join(p[:1].upper() + p[1:] for p in parts[1:]) if parts else name
    return s + '_' if s in DART_RESERVED else s


def pascal(name):
    return ''.join(w.capitalize() for w in re.split(r'[^0-9A-Za-z]+', name) if w)


class Gen:
    def __init__(self, spec):
        self.spec = spec
        self.schemas = spec['components']['schemas']
        self.out = []            # (名字, 文本)
        self.emitted = set()
        self.aliases = {}        # 标量别名名 -> 底层 prim 描述

    def resolve(self, node):
        while isinstance(node, dict) and '$ref' in node:
            m = re.fullmatch(r'#/components/schemas/(\w+)', node['$ref'])
            if not m:
                raise SystemExit('不支持的 $ref: %s' % node['$ref'])
            node = self.schemas[m.group(1)]
        return node

    @staticmethod
    def ref_name(node):
        if isinstance(node, dict) and '$ref' in node:
            m = re.fullmatch(r'#/components/schemas/(\w+)', node['$ref'])
            if m:
                return m.group(1)
        return None

    def flatten_all_of(self, node):
        merged = {'type': 'object', 'properties': {}, 'required': []}
        parts = list(node['allOf'])
        extra = {k: v for k, v in node.items() if k != 'allOf'}
        if extra.get('properties') or extra.get('required'):
            parts.append(extra)
        for part in parts:
            part = self.resolve(part)
            if 'allOf' in part:
                part = self.flatten_all_of(part)
            if 'properties' not in part and part.get('type') not in (None, 'object'):
                raise SystemExit('allOf 里混进了非对象成员（%r）' % part.get('type'))
            merged['properties'].update(part.get('properties') or {})
            merged['required'].extend(part.get('required') or [])
        return merged

    # -- 类型描述 -------------------------------------------------------------

    def type_of(self, node, hint):
        """返回 (描述, 可空)。"""
        if node is None:
            return ('prim', 'dynamic'), True
        ref = self.ref_name(node)
        if ref is not None:
            self.emit_schema(ref)
            if ref in self.aliases:
                return ('alias', ref), False
            return ('ref', ref), False
        if 'allOf' in node:
            members = node['allOf']
            extra = {k: v for k, v in node.items() if k != 'allOf'}
            if len(members) == 1 and not extra.get('properties') and not extra.get('required'):
                return self.type_of(members[0], hint)
            self.emit_object(hint, self.flatten_all_of(node))
            return ('ref', hint), False
        t = node.get('type')
        nullable = False
        if isinstance(t, list):
            non_null = [x for x in t if x != 'null']
            nullable = len(non_null) != len(t)
            if len(non_null) != 1:
                raise SystemExit('不支持的 type 联合: %r（%s）' % (t, hint))
            t = non_null[0]
        if t == 'array':
            inner, _ = self.type_of(node.get('items'), hint + 'Item')
            return ('list', inner), nullable
        if t == 'object' or ('properties' in node and t is None):
            if 'properties' not in node:
                ap = node.get('additionalProperties')
                if isinstance(ap, dict) and ap.get('type') == 'string':
                    return ('map', 'String'), nullable
                return ('map', 'dynamic'), nullable
            self.emit_object(hint, node)
            return ('ref', hint), nullable
        prim = {'integer': 'int', 'number': 'double', 'string': 'String', 'boolean': 'bool', None: 'dynamic'}
        if t not in prim:
            raise SystemExit('不支持的 type: %r（%s）' % (t, hint))
        return ('prim', prim[t]), nullable

    def dart_type(self, d):
        k = d[0]
        if k in ('prim',):
            return d[1]
        if k in ('ref', 'alias'):
            return d[1]
        if k == 'list':
            return 'List<%s>' % self.dart_type(d[1])
        if k == 'map':
            return 'Map<String, %s>' % d[1]
        raise AssertionError(d)

    def base_prim(self, d):
        return self.aliases[d[1]] if d[0] == 'alias' else (d[1] if d[0] == 'prim' else None)

    def decode(self, d, v, nullable):
        """把 JSON 值表达式 v 解成描述 d 的 Dart 表达式。"""
        q = '?' if nullable else ''
        prim = self.base_prim(d)
        if prim == 'int':
            return '(%s as num%s)%s.toInt()' % (v, q, q)
        if prim == 'double':
            return '(%s as num%s)%s.toDouble()' % (v, q, q)
        if prim in ('String', 'bool'):
            return '%s as %s%s' % (v, prim, q)
        if prim == 'dynamic':
            return v
        if d[0] == 'ref':
            if nullable:
                return '%s == null ? null : %s.fromJson(%s as Map<String, dynamic>)' % (v, d[1], v)
            return '%s.fromJson(%s as Map<String, dynamic>)' % (d[1], v)
        if d[0] == 'list':
            inner = self.decode(d[1], 'e', False)
            expr = '(%s as List%s)%s.map((e) => %s).toList()' % (v, q, q, inner)
            return expr
        if d[0] == 'map':
            if d[1] == 'String':
                return '(%s as Map%s)%s.map((k, e) => MapEntry(k as String, e as String))' % (v, q, q)
            return '%s as Map<String, dynamic>%s' % (v, q)
        raise AssertionError(d)

    def encode(self, d, v):
        if d[0] == 'ref':
            return '%s.toJson()' % v
        if d[0] == 'list':
            inner = self.encode(d[1], 'e')
            return v if inner == 'e' else '%s.map((e) => %s).toList()' % (v, inner)
        return v

    # -- 产出 -----------------------------------------------------------------

    def emit_object(self, name, node, doc=None):
        if name in self.emitted:
            return
        self.emitted.add(name)
        required = set(node.get('required') or [])
        fields = []
        for prop, sub in (node.get('properties') or {}).items():
            d, nullable = self.type_of(sub, name + pascal(prop))
            is_req = prop in required
            fields.append((prop, camel(prop), d, nullable or not is_req, is_req and nullable))
        lines = []
        if doc:
            lines.append('/// %s' % doc)
        lines.append('class %s {' % name)
        for prop, f, d, opt, _ in fields:
            lines.append('  final %s%s %s;' % (self.dart_type(d), '?' if opt else '', f))
        if fields:
            args = ', '.join(('%sthis.%s' % ('' if opt else 'required ', f)) for _, f, _, opt, _ in fields)
            lines.append('  const %s({%s});' % (name, args))
        else:
            lines.append('  const %s();' % name)
        lines.append('  factory %s.fromJson(Map<String, dynamic> j) => %s(' % (name, name))
        for prop, f, d, opt, _ in fields:
            lines.append("        %s: %s," % (f, self.decode(d, "j['%s']" % prop, opt)))
        lines.append('      );')
        lines.append('  Map<String, dynamic> toJson() => {')
        for prop, f, d, opt, always in fields:
            if opt and not always:
                enc = self.encode(d, '%s!' % f)
                enc = enc.replace('%s!.' % f, '%s!.' % f)
                lines.append("        if (%s != null) '%s': %s," % (f, prop, f if enc == '%s!' % f else enc))
            elif opt and always:
                enc = self.encode(d, '%s!' % f)
                lines.append("        '%s': %s," % (prop, f if enc == '%s!' % f else '%s == null ? null : %s' % (f, enc)))
            else:
                lines.append("        '%s': %s," % (prop, self.encode(d, f)))
        lines.append('      };')
        lines.append('}')
        self.out.append((name, '\n'.join(lines) + '\n'))

    @staticmethod
    def find_prop(node, prop):
        return (node.get('properties') or {}).get(prop) or {}

    def emit_schema(self, name):
        if name in self.emitted:
            return
        node = self.schemas[name]
        doc = (node.get('description') or '').strip().splitlines()
        doc = doc[0].strip() if doc else None
        if 'allOf' in node:
            self.emit_object(name, self.flatten_all_of(node), doc)
            return
        t = node.get('type')
        if t == 'object' or 'properties' in node:
            if 'properties' not in node:
                self.emitted.add(name)
                ap = node.get('additionalProperties')
                inner = 'String' if isinstance(ap, dict) and ap.get('type') == 'string' else 'dynamic'
                self.out.append((name, 'typedef %s = Map<String, %s>;\n' % (name, inner)))
                return
            self.emit_object(name, node, doc)
            return
        self.emitted.add(name)
        d, _ = self.type_of({k: v for k, v in node.items() if k != 'description'}, name)
        if d[0] != 'prim':
            raise SystemExit('标量别名 %s 不是基础类型（%r）' % (name, d))
        self.aliases[name] = d[1]
        text = ('/// %s\n' % doc if doc else '') + 'typedef %s = %s;\n' % (name, d[1])
        self.out.append((name, text))

    def emit_operations(self, operations):
        paths = self.spec['paths']
        params = self.spec['components'].get('parameters') or {}
        for method, path, prefix in operations:
            if path not in paths or method not in paths[path]:
                raise SystemExit('契约里没有 %s %s —— contract_operations.py 与契约对不上了' % (method.upper(), path))
            op = paths[path][method]
            qprops, qreq = {}, []
            for p in op.get('parameters') or []:
                if '$ref' in p:
                    m = re.fullmatch(r'#/components/parameters/(\w+)', p['$ref'])
                    if not m:
                        raise SystemExit('不支持的 parameter $ref: %s' % p['$ref'])
                    p = params[m.group(1)]
                if p.get('in') != 'query':
                    continue
                qprops[p['name']] = p['schema']
                if p.get('required'):
                    qreq.append(p['name'])
            if qprops:
                self.emit_object('%sQuery' % prefix, {'type': 'object', 'properties': qprops, 'required': qreq},
                                 '%s %s 的 query 参数' % (method.upper(), path))
            body = (op.get('requestBody') or {}).get('content', {}).get('application/json')
            if body:
                s = body['schema']
                if self.ref_name(s) is None:
                    self.emit_object('%sRequest' % prefix, self.expand(s), '%s %s 的请求体' % (method.upper(), path))
                else:
                    self.emit_schema(self.ref_name(s))
            for code in ('200', '201', '202'):
                resp = (op.get('responses') or {}).get(code)
                content = ((resp or {}).get('content') or {}).get('application/json')
                if not content:
                    continue
                s = content['schema']
                if self.ref_name(s) is None and self.expand(s).get('type', 'object') == 'object' and 'properties' in self.expand(s):
                    self.emit_object('%sResponse' % prefix, self.expand(s), '%s %s 的 %s 响应体' % (method.upper(), path, code))
                elif self.ref_name(s) is not None:
                    self.emit_schema(self.ref_name(s))
                break

    def expand(self, s):
        return self.flatten_all_of(s) if 'allOf' in s else self.resolve(s)


def render(spec, operations=OPERATIONS):
    gen = Gen(spec)
    for name in sorted(spec['components']['schemas']):
        gen.emit_schema(name)
    gen.emit_operations(operations)
    return HEADER + '\n' + '\n'.join(text for _, text in gen.out)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument('-o', '--output', default=OUT)
    args = ap.parse_args()
    with open(CONTRACT, encoding='utf-8') as f:
        spec = yaml.safe_load(f)
    text = render(spec)
    os.makedirs(os.path.dirname(args.output), exist_ok=True)
    with open(args.output, 'w', encoding='utf-8') as f:
        f.write(text)
    print('generated', args.output)


if __name__ == '__main__':
    main()
```

数组响应（如 `GET /categories` 返回裸数组）不生成 Response 类，客户端直接 `List<Category>`，与 UTS 一致。

运行：`$V/bin/python -m unittest scripts/test_gen_dart_schema.py -v` —— Expected：8 个测试 PASS

- [ ] **Step 3: 生成真契约并确认能编译**

```bash
$V/bin/python scripts/gen_dart_schema.py
cd flutter_app && ~/development/flutter/bin/dart analyze lib/api/schema.g.dart
```

Expected：`generated .../schema.g.dart`；`dart analyze` 无 error（生成物在 `analysis_options.yaml` 里 exclude 了 lint，但编译错误仍会报）。有编译错误就回到生成器修，不许手改产物。

- [ ] **Step 4: 契约闸门** `scripts/check_dart_contract.py`

```python
#!/usr/bin/env python3
"""契约闸门：重新生成到临时文件，与入库的 flutter_app/lib/api/schema.g.dart 逐字比对。"""
import os
import subprocess
import sys
import tempfile

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
CHECKED_IN = os.path.join(ROOT, 'flutter_app', 'lib', 'api', 'schema.g.dart')


def main():
    with tempfile.TemporaryDirectory() as d:
        out = os.path.join(d, 'schema.g.dart')
        subprocess.run([sys.executable, os.path.join(ROOT, 'scripts', 'gen_dart_schema.py'), '-o', out],
                       check=True, stdout=subprocess.DEVNULL)
        want = open(out, encoding='utf-8').read()
    have = open(CHECKED_IN, encoding='utf-8').read() if os.path.exists(CHECKED_IN) else ''
    if want != have:
        sys.exit('check_dart_contract FAIL：flutter_app/lib/api/schema.g.dart 与契约不同步，跑 make flutter-generate 后一起提交')
    print('check_dart_contract OK: flutter_app/lib/api/schema.g.dart 与契约同步')


if __name__ == '__main__':
    main()
```

运行：`$V/bin/python scripts/check_dart_contract.py` —— Expected：`OK`；手改 `schema.g.dart` 一个字符再跑 —— Expected：FAIL；恢复。

- [ ] **Step 5: 接进 check-all 与 Makefile**

`Makefile` 加：

```makefile
flutter-generate:
	python3 $(ROOT)/scripts/gen_dart_schema.py
```

`scripts/check-all.sh`：在跑 `check_uts_contract.py` 的那一行后面照样加一行跑 `check_dart_contract.py`，再加一行 `python3 -m unittest scripts/test_gen_dart_schema.py`（与现有写法一致，用同一个 python）。

运行：`PATH=$V/bin:$PATH bash scripts/check-all.sh` —— Expected：`全部校验通过。`

- [ ] **Step 6: 提交**

```bash
git add scripts/gen_dart_schema.py scripts/test_gen_dart_schema.py scripts/check_dart_contract.py scripts/check-all.sh Makefile flutter_app/lib/api/schema.g.dart
git commit -m "契约生成 Dart 类型：gen_dart_schema.py + 闸门，产物 flutter_app/lib/api/schema.g.dart"
```

---

### Task 4: 生成类型的往返测试（真实响应样本）

**Files:**
- Create: `flutter_app/test/fixtures/products_page.json`、`flutter_app/test/fixtures/login.json`、`flutter_app/test/fixtures/store_resolve.json`、`flutter_app/test/schema_roundtrip_test.dart`

**Interfaces:**
- Consumes: `ListProductsResponse`、`LoginResponse`、`StoreResolveResult`（Task 3 生成）

- [ ] **Step 1: 取样本**

```bash
B=http://192.168.0.110:18099/api/v1
curl -s "$B/products?page_size=3" > flutter_app/test/fixtures/products_page.json
curl -s "$B/stores/resolve" > flutter_app/test/fixtures/store_resolve.json
```

`login.json` 手写（不入库真令牌）：

```json
{"access_token":"t.a","refresh_token":"t.r","token_type":"Bearer","expires_in":7200,
 "user":{"id":2,"nickname":"e2e 买家","phone":"138****0001"}}
```

- [ ] **Step 2: 写测试** `flutter_app/test/schema_roundtrip_test.dart`

```dart
import 'dart:convert';
import 'dart:io';

import 'package:flutter_test/flutter_test.dart';
import 'package:keel_buyer/api/schema.g.dart';

Map<String, dynamic> load(String name) =>
    jsonDecode(File('test/fixtures/$name').readAsStringSync()) as Map<String, dynamic>;

void main() {
  test('商品列表：解析真实响应，往返后字段不丢', () {
    final raw = load('products_page.json');
    final page = ListProductsResponse.fromJson(raw);
    expect(page.items, isNotEmpty);
    expect(page.items.first.minPriceCents, isA<int>());
    final again = ListProductsResponse.fromJson(jsonDecode(jsonEncode(page.toJson())) as Map<String, dynamic>);
    expect(again.items.first.title, page.items.first.title);
    expect(again.total, page.total);
  });

  test('多出的字段忽略、缺的非必填字段为 null', () {
    final raw = load('products_page.json');
    final item = Map<String, dynamic>.from((raw['items'] as List).first as Map);
    item['a_field_from_the_future'] = 1;
    item.remove('subtitle');
    final p = ProductSummary.fromJson(item);
    expect(p.subtitle, isNull);
  });

  test('整数字段收到 12.0 也能进 int', () {
    final raw = load('products_page.json');
    final item = Map<String, dynamic>.from((raw['items'] as List).first as Map);
    item['min_price_cents'] = 4990.0;
    expect(ProductSummary.fromJson(item).minPriceCents, 4990);
  });

  test('登录响应', () {
    final r = LoginResponse.fromJson(load('login.json'));
    expect(r.user.nickname, 'e2e 买家');
    expect(r.refreshToken, 't.r');
  });

  test('门店解析', () {
    final r = StoreResolveResult.fromJson(load('store_resolve.json'));
    expect(r.matchType, isNotEmpty);
  });
}
```

运行：`make flutter-test` —— Expected：PASS（失败则说明生成器有映射问题：回 Task 3 修生成器、重新生成）

- [ ] **Step 3: 提交**

```bash
git add flutter_app/test
git commit -m "Flutter：生成类型用真实响应样本做往返测试"
```

---

### Task 5: 服务地址、请求层与会话

**Files:**
- Create: `flutter_app/lib/config.dart`、`flutter_app/lib/api/client.dart`、`flutter_app/lib/api/session.dart`
- Test: `flutter_app/test/config_test.dart`、`flutter_app/test/client_test.dart`

**Interfaces:**
- Consumes: `LoginResponse`、`RefreshTokenRequest`、`Problem`（Task 3）
- Produces:
  - `String resolveApiBase({required String defined, required bool isWeb, required Uri base})`、`String apiBase()`（运行时用 `String.fromEnvironment('KEEL_API_BASE')`、`kIsWeb`、`Uri.base`）
  - `class ApiFailure implements Exception { final int status; final Problem? problem; final String message; final int retryAfter; final List<FieldError> fieldErrors; bool get notImplemented; }`
  - `class ApiResult<T> { final T data; final bool replayed; }`
  - `class ApiClient { ApiClient({required String base, required Session session, http.Client? http, void Function()? onSessionExpired}); Future<ApiResult<T>> send<T>(String method, String path, {Map<String, String>? query, Object? body, String? idempotencyKey, required T Function(dynamic json) decode}); Future<void> fireAndForget(String path, Object body); String assetUrl(String path); }`
  - `class Session extends ChangeNotifier { String? accessToken; String? refreshToken; String nickname; bool get loggedIn; Future<void> load(); Future<void> save(LoginResponse r); Future<void> clear(); }`
  - `String newIdempotencyKey()`（在 client.dart，随机 UUID v4）

- [ ] **Step 1: 服务地址的失败测试** `flutter_app/test/config_test.dart`

```dart
import 'package:flutter_test/flutter_test.dart';
import 'package:keel_buyer/config.dart';

void main() {
  test('编译期注入了就用注入的，去掉末尾斜杠', () {
    expect(resolveApiBase(defined: 'http://x:1/api/v1/', isWeb: false, base: Uri.parse('file:///')),
        'http://x:1/api/v1');
  });
  test('Web 没注入：同源相对路径', () {
    expect(resolveApiBase(defined: '', isWeb: true, base: Uri.parse('http://localhost:5000/')), '/api/v1');
  });
  test('小程序（Uri.base 是 https://mp.local/）没注入：报清楚的错，不打到 mp.local', () {
    expect(() => resolveApiBase(defined: '', isWeb: true, base: Uri.parse('https://mp.local/')),
        throwsA(isA<StateError>().having((e) => e.message, 'message', contains('KEEL_API_BASE'))));
  });
  test('原生没注入：报错', () {
    expect(() => resolveApiBase(defined: '', isWeb: false, base: Uri.parse('file:///')), throwsStateError);
  });
}
```

运行：`~/development/flutter/bin/flutter test test/config_test.dart` —— Expected：FAIL

- [ ] **Step 2: 实现** `flutter_app/lib/config.dart`

```dart
import 'package:flutter/foundation.dart';

/// 服务地址（= 租户）。
///
/// - 编译期 `--dart-define=KEEL_API_BASE=...` 注入的优先（原生与小程序必须注入：它们没有页面 origin）。
/// - Web 没注入：用相对路径 `/api/v1` 同源访问（服务端不发 CORS 头，跨源打不通），开发 / e2e 走反代。
/// - 小程序里 `Uri.base` 固定是 `https://mp.local/`（mp-flutter 垫片写死的）：没注入就报错，
///   否则请求会打到不存在的 mp.local。
String resolveApiBase({required String defined, required bool isWeb, required Uri base}) {
  var v = defined.trim();
  while (v.endsWith('/')) {
    v = v.substring(0, v.length - 1);
  }
  if (v.isNotEmpty) return v;
  if (isWeb && base.host != 'mp.local') return '/api/v1';
  throw StateError('没有服务地址：编译时加 --dart-define=KEEL_API_BASE=http(s)://<主机>/api/v1');
}

String apiBase() => resolveApiBase(
      defined: const String.fromEnvironment('KEEL_API_BASE'),
      isWeb: kIsWeb,
      base: Uri.base,
    );
```

运行：`~/development/flutter/bin/flutter test test/config_test.dart` —— Expected：PASS

- [ ] **Step 3: 请求层的失败测试** `flutter_app/test/client_test.dart`

```dart
import 'dart:async';
import 'dart:convert';

import 'package:flutter_test/flutter_test.dart';
import 'package:http/http.dart' as http;
import 'package:http/testing.dart';
import 'package:keel_buyer/api/client.dart';
import 'package:keel_buyer/api/session.dart';
import 'package:shared_preferences/shared_preferences.dart';

http.Response j(int status, Object body, {Map<String, String> headers = const {}}) =>
    http.Response(jsonEncode(body), status, headers: {'content-type': 'application/json', ...headers});

Future<Session> loggedIn() async {
  SharedPreferences.setMockInitialValues({});
  final s = Session();
  await s.load();
  await s.saveTokens(access: 'old', refresh: 'r1', nickname: '买家');
  return s;
}

void main() {
  test('成功：解码并带上 Authorization 与幂等键；重放头透出', () async {
    final s = await loggedIn();
    late http.Request seen;
    final c = ApiClient(base: 'http://h/api/v1', session: s, http: MockClient((r) async {
      seen = r;
      return j(201, {'ok': true}, headers: {'idempotency-replayed': 'true'});
    }));
    final res = await c.send('POST', '/orders', body: {'a': 1}, idempotencyKey: 'k1', decode: (x) => x);
    expect(seen.headers['authorization'], 'Bearer old');
    expect(seen.headers['idempotency-key'], 'k1');
    expect(seen.url.toString(), 'http://h/api/v1/orders');
    expect(res.replayed, isTrue);
  });

  test('Problem：message 优先 detail，带字段级错误与 Retry-After', () async {
    final s = await loggedIn();
    final c = ApiClient(base: 'http://h/api/v1', session: s, http: MockClient((r) async => j(409, {
          'type': 'https://keel.dev/problems/idempotency-key-in-flight', 'title': '处理中', 'status': 409,
          'detail': '稍后重试', 'errors': [{'field': 'phone', 'message': '不对'}],
        }, headers: {'retry-after': '2'})));
    try {
      await c.send('POST', '/x', decode: (x) => x);
      fail('应当抛 ApiFailure');
    } on ApiFailure catch (f) {
      expect(f.status, 409);
      expect(f.message, '稍后重试');
      expect(f.retryAfter, 2);
      expect(f.fieldErrors.single.field, 'phone');
      expect(f.problem!.type, endsWith('/idempotency-key-in-flight'));
    }
  });

  test('响应体不是 Problem（网关 HTML）：给可读 message，不抛解析异常', () async {
    final s = await loggedIn();
    final c = ApiClient(base: 'http://h/api/v1', session: s,
        http: MockClient((r) async => http.Response('<html>bad gateway</html>', 502)));
    await expectLater(c.send('GET', '/x', decode: (x) => x),
        throwsA(isA<ApiFailure>().having((f) => f.message, 'message', contains('502'))));
  });

  test('401：只刷新一次，并发请求排队，刷完用原幂等键重放', () async {
    final s = await loggedIn();
    var refreshCalls = 0;
    final keys = <String?>[];
    final c = ApiClient(base: 'http://h/api/v1', session: s, http: MockClient((r) async {
      if (r.url.path.endsWith('/auth/refresh')) {
        refreshCalls++;
        await Future<void>.delayed(const Duration(milliseconds: 20));
        return j(200, {'access_token': 'new', 'refresh_token': 'r2', 'token_type': 'Bearer', 'expires_in': 7200,
          'user': {'id': 2, 'nickname': '买家'}});
      }
      if (r.headers['authorization'] == 'Bearer old') return j(401, {'type': 'x', 'title': '过期', 'status': 401});
      keys.add(r.headers['idempotency-key']);
      return j(200, {'n': r.url.path});
    }));
    final results = await Future.wait([
      c.send('POST', '/a', idempotencyKey: 'ka', decode: (x) => x),
      c.send('GET', '/b', decode: (x) => x),
      c.send('GET', '/c', decode: (x) => x),
    ]);
    expect(refreshCalls, 1);
    expect(results.map((r) => (r.data as Map)['n']), ['/api/v1/a', '/api/v1/b', '/api/v1/c']);
    expect(keys, contains('ka'));
    expect(s.accessToken, 'new');
    expect(s.refreshToken, 'r2');
  });

  test('刷新失败：清会话、只通知一次登录过期、原请求报 401', () async {
    final s = await loggedIn();
    var expired = 0;
    final c = ApiClient(base: 'http://h/api/v1', session: s, onSessionExpired: () => expired++,
        http: MockClient((r) async => j(401, {'type': 'x', 'title': '过期', 'status': 401})));
    final all = await Future.wait([
      c.send('GET', '/a', decode: (x) => x).then((_) => 0, onError: (Object e) => (e as ApiFailure).status),
      c.send('GET', '/b', decode: (x) => x).then((_) => 0, onError: (Object e) => (e as ApiFailure).status),
    ]);
    expect(all, [401, 401]);
    expect(expired, 1);
    expect(s.loggedIn, isFalse);
  });

  test('/auth/ 自己的 401 不去刷新', () async {
    final s = await loggedIn();
    var calls = 0;
    final c = ApiClient(base: 'http://h/api/v1', session: s, http: MockClient((r) async {
      calls++;
      return j(401, {'type': 'x', 'title': '密码错误', 'status': 401});
    }));
    await expectLater(c.send('POST', '/auth/login', body: {}, decode: (x) => x), throwsA(isA<ApiFailure>()));
    expect(calls, 1);
  });

  test('assetUrl：相对路径补 origin；base 是相对的（Web 同源）原样', () {
    final s = Session();
    expect(ApiClient(base: 'http://h:1/api/v1', session: s).assetUrl('/api/v1/uploads/3'), 'http://h:1/api/v1/uploads/3');
    expect(ApiClient(base: '/api/v1', session: s).assetUrl('/api/v1/uploads/3'), '/api/v1/uploads/3');
    expect(ApiClient(base: 'http://h/api/v1', session: s).assetUrl('https://cdn/x.png'), 'https://cdn/x.png');
    expect(ApiClient(base: 'http://h/api/v1', session: s).assetUrl(''), '');
  });

  test('newIdempotencyKey 是 UUID v4 形状且每次不同', () {
    final a = newIdempotencyKey(), b = newIdempotencyKey();
    expect(a, matches(RegExp(r'^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$')));
    expect(a, isNot(b));
  });
}
```

运行：`~/development/flutter/bin/flutter test test/client_test.dart` —— Expected：FAIL（文件不存在）

- [ ] **Step 4: 实现会话** `flutter_app/lib/api/session.dart`

```dart
import 'package:flutter/foundation.dart';
import 'package:shared_preferences/shared_preferences.dart';

import 'schema.g.dart';

/// 本机会话。放在 shared_preferences（小程序里由 mp-flutter 换成 wx.*StorageSync）。
class Session extends ChangeNotifier {
  static const _kAccess = 'keel.access';
  static const _kRefresh = 'keel.refresh';
  static const _kNick = 'keel.nickname';

  String? accessToken;
  String? refreshToken;
  String nickname = '';

  bool get loggedIn => accessToken != null && accessToken!.isNotEmpty;

  Future<void> load() async {
    final p = await SharedPreferences.getInstance();
    accessToken = p.getString(_kAccess);
    refreshToken = p.getString(_kRefresh);
    nickname = p.getString(_kNick) ?? '';
    notifyListeners();
  }

  Future<void> save(LoginResponse r) =>
      saveTokens(access: r.accessToken, refresh: r.refreshToken ?? refreshToken, nickname: r.user.nickname);

  Future<void> saveTokens({required String access, String? refresh, required String nickname}) async {
    accessToken = access;
    refreshToken = refresh;
    this.nickname = nickname;
    final p = await SharedPreferences.getInstance();
    await p.setString(_kAccess, access);
    if (refresh != null) await p.setString(_kRefresh, refresh);
    await p.setString(_kNick, nickname);
    notifyListeners();
  }

  Future<void> clear() async {
    accessToken = null;
    refreshToken = null;
    nickname = '';
    final p = await SharedPreferences.getInstance();
    await p.remove(_kAccess);
    await p.remove(_kRefresh);
    await p.remove(_kNick);
    notifyListeners();
  }
}
```

- [ ] **Step 5: 实现请求层** `flutter_app/lib/api/client.dart`

```dart
import 'dart:async';
import 'dart:convert';
import 'dart:math';

import 'package:http/http.dart' as http;

import 'schema.g.dart';
import 'session.dart';

/// 一次失败的调用。message 是给人看的一句话（Problem 的 detail 优先，没有就 title）。
class ApiFailure implements Exception {
  final int status;
  final Problem? problem;
  final String message;
  final int retryAfter;
  final List<FieldError> fieldErrors;
  const ApiFailure(this.status, this.problem, this.message, {this.retryAfter = 0, this.fieldErrors = const []});

  /// 路由层 404 / 405 或显式的 not-implemented：「这条接口后端还没接上」，与「东西不存在」分开。
  bool get notImplemented {
    final t = problem?.type ?? '';
    return t.endsWith('/not-implemented') ||
        ((status == 404 || status == 405) && (t.endsWith('/not-found') || t.endsWith('/method-not-allowed')));
  }

  @override
  String toString() => 'ApiFailure($status, $message)';
}

class ApiResult<T> {
  final T data;
  final bool replayed;
  const ApiResult(this.data, this.replayed);
}

/// 随机 UUID v4。幂等键由页面生成并持有：同一次操作重试复用，内容变了换键。
String newIdempotencyKey() {
  final r = Random.secure();
  final b = List<int>.generate(16, (_) => r.nextInt(256));
  b[6] = (b[6] & 0x0f) | 0x40;
  b[8] = (b[8] & 0x3f) | 0x80;
  String h(int i) => b[i].toRadixString(16).padLeft(2, '0');
  final s = List.generate(16, h).join();
  return '${s.substring(0, 8)}-${s.substring(8, 12)}-${s.substring(12, 16)}-${s.substring(16, 20)}-${s.substring(20)}';
}

/// 唯一的网络出口。
class ApiClient {
  ApiClient({required this.base, required this.session, http.Client? http, this.onSessionExpired})
      : _http = http ?? _defaultClient();

  final String base;
  final Session session;
  final http.Client _http;

  /// 刷新也救不回来时调一次（跳登录页）。同一轮并发失败只调一次。
  final void Function()? onSessionExpired;

  Completer<bool>? _refreshing;

  static http.Client _defaultClient() => http.Client();

  Future<ApiResult<T>> send<T>(String method, String path,
      {Map<String, String>? query, Object? body, String? idempotencyKey, required T Function(dynamic json) decode}) async {
    final res = await _raw(method, path, query, body, idempotencyKey);
    if (res.statusCode == 401 && session.loggedIn && !path.startsWith('/auth/')) {
      final ok = await _refresh();
      if (!ok) throw _expired();
      // 重放用同一个幂等键：第一次既然是 401，服务端什么都没做。
      final again = await _raw(method, path, query, body, idempotencyKey);
      if (again.statusCode == 401) {
        await _giveUp();
        throw _expired();
      }
      return _finish(again, decode);
    }
    return _finish(res, decode);
  }

  /// 公开的「发了就不管」接口（搜索回传）：不带令牌，错误一律吞掉。
  Future<void> fireAndForget(String path, Object body) async {
    try {
      await _http.post(Uri.parse('$base$path'),
          headers: {'Content-Type': 'application/json'}, body: jsonEncode(body));
    } catch (_) {}
  }

  /// 服务端给的资源地址是相对路径（/api/v1/uploads/..）：补上服务地址的 origin。
  /// base 本身是相对的（Web 同源）或资源已是绝对地址时原样返回。
  String assetUrl(String path) {
    if (path.isEmpty || !path.startsWith('/')) return path;
    final b = Uri.tryParse(base);
    if (b == null || !b.hasScheme) return path;
    return '${b.scheme}://${b.authority}$path';
  }

  Future<http.Response> _raw(String method, String path, Map<String, String>? query, Object? body, String? key) {
    final uri = Uri.parse('$base$path').replace(queryParameters: (query == null || query.isEmpty) ? null : query);
    final req = http.Request(method, uri);
    req.headers['Accept'] = 'application/json, application/problem+json';
    final t = session.accessToken;
    if (t != null && t.isNotEmpty) req.headers['Authorization'] = 'Bearer $t';
    if (key != null) req.headers['Idempotency-Key'] = key;
    if (body != null) {
      req.headers['Content-Type'] = 'application/json';
      req.body = jsonEncode(body);
    }
    return _http.send(req).then(http.Response.fromStream);
  }

  ApiResult<T> _finish<T>(http.Response res, T Function(dynamic json) decode) {
    final text = utf8.decode(res.bodyBytes);
    if (res.statusCode >= 200 && res.statusCode < 300) {
      final replayed = res.headers['idempotency-replayed'] == 'true';
      return ApiResult(decode(text.isEmpty ? null : jsonDecode(text)), replayed);
    }
    throw _failure(res.statusCode, text, res.headers['retry-after']);
  }

  ApiFailure _failure(int status, String text, String? retryAfter) {
    Problem? p;
    try {
      final j = jsonDecode(text);
      if (j is Map<String, dynamic> && j['type'] is String && j['title'] is String) p = Problem.fromJson(j);
    } catch (_) {}
    final msg = p != null ? ((p.detail ?? '').isNotEmpty ? p.detail! : p.title) : 'HTTP $status：响应体不是契约里的 Problem';
    return ApiFailure(status, p, msg,
        retryAfter: int.tryParse(retryAfter ?? '') ?? 0, fieldErrors: p?.errors ?? const []);
  }

  ApiFailure _expired() => const ApiFailure(401, null, '登录已过期，请重新登录');

  /// 单飞：服务端每次刷新都轮换 refresh_token（旧的立即作废），并发的 401 只刷一次，其余等它。
  Future<bool> _refresh() {
    final inflight = _refreshing;
    if (inflight != null) return inflight.future;
    final c = Completer<bool>();
    _refreshing = c;
    () async {
      var ok = false;
      try {
        final rt = session.refreshToken;
        if (rt != null && rt.isNotEmpty) {
          final res = await _raw('POST', '/auth/refresh', null, RefreshTokenRequest(refreshToken: rt).toJson(), null);
          if (res.statusCode == 200) {
            await session.save(LoginResponse.fromJson(jsonDecode(utf8.decode(res.bodyBytes)) as Map<String, dynamic>));
            ok = true;
          }
        }
      } catch (_) {}
      if (!ok) await _giveUp();
      _refreshing = null;
      c.complete(ok);
    }();
    return c.future;
  }

  bool _gaveUp = false;

  Future<void> _giveUp() async {
    await session.clear();
    if (_gaveUp) return;
    _gaveUp = true;
    onSessionExpired?.call();
    Future<void>.delayed(const Duration(seconds: 1), () => _gaveUp = false);
  }
}
```

同时在 `session.dart` 已有 `saveTokens`（测试用它造登录态）。

运行：`~/development/flutter/bin/flutter test test/client_test.dart` —— Expected：8 个测试 PASS

- [ ] **Step 6: 提交**

```bash
git add flutter_app/lib/config.dart flutter_app/lib/api/client.dart flutter_app/lib/api/session.dart flutter_app/test/config_test.dart flutter_app/test/client_test.dart
git commit -m "Flutter：服务地址解析、请求层（Problem 映射、幂等键、401 单飞续期）与会话"
```

---

### Task 6: 当前门店与展示层

**Files:**
- Create: `flutter_app/lib/api/store.dart`、`flutter_app/lib/api/view.dart`、`flutter_app/lib/api/services.dart`
- Test: `flutter_app/test/view_test.dart`、`flutter_app/test/store_test.dart`

**Interfaces:**
- Consumes: `ApiClient.send`、`ApiClient.assetUrl`（Task 5）；`ProductSummary`、`StoreResolveResult`、`StoreMatch`、`ListProductsResponse`、`LoginRequest`、`LoginResponse`（Task 3）
- Produces:
  - `String yuan(int? cents)`（`4990 → '¥49.90'`，负数 `-¥…`，null → `¥0.00`）
  - `String shortTime(String iso)`（本地时区 `MM-dd HH:mm`；解析失败原样）
  - `class Cover { final String imageUrl; final Color color; final String glyph; }`、`Cover coverOf(int key, String title, String imageUrl)`
  - `class ProductRow { final int id; final String title; final String subtitle; final String priceText; final bool hasRange; final bool offShelf; final bool soldOut; final Cover cover; final List<String> promoTags; }`、`ProductRow productRow(ProductSummary p, String Function(String) asset)`
  - `class CurrentStore { final int? storeId; final String name; }`
  - `class StoreService extends ChangeNotifier { StoreService(ApiClient c); CurrentStore? current; Future<CurrentStore> ensure(); }`（只解析一次；并发调用共用一次请求）
  - `class Services extends InheritedWidget { final ApiClient client; final Session session; final StoreService store; static Services of(BuildContext) }`
  - 页面用的数据函数（都在 view.dart，页面不碰契约）：`Future<List<ProductRow>> fetchProducts(ApiClient c, {required int? storeId, int page = 1, int pageSize = 20})`、`Future<void> login(ApiClient c, Session s, String phone, String password)`

- [ ] **Step 1: 展示层的失败测试** `flutter_app/test/view_test.dart`

```dart
import 'package:flutter_test/flutter_test.dart';
import 'package:keel_buyer/api/schema.g.dart';
import 'package:keel_buyer/api/view.dart';

void main() {
  test('yuan', () {
    expect(yuan(4990), '¥49.90');
    expect(yuan(5), '¥0.05');
    expect(yuan(-800), '-¥8.00');
    expect(yuan(null), '¥0.00');
  });

  test('shortTime：UTC 转本地；带微秒；解析不了原样', () {
    final iso = '2026-09-26T22:55:37.188864Z';
    final d = DateTime.parse(iso).toLocal();
    String p(int n) => n.toString().padLeft(2, '0');
    expect(shortTime(iso), '${p(d.month)}-${p(d.day)} ${p(d.hour)}:${p(d.minute)}');
    expect(shortTime('昨天'), '昨天');
  });

  test('productRow：多规格价格区间、特价标签、图片补 origin', () {
    const p = ProductSummary(id: 19, title: '挂耳咖啡 10 包', minPriceCents: 6900, maxPriceCents: 8900, status: 1,
        imageUrl: '/api/v1/uploads/3', promotionTags: [
          PromotionTag(promotionId: 2, promotionType: 3, label: '限时特价 ¥49.9'),
          PromotionTag(promotionId: 1, promotionType: 1, label: '满199减20'),
          PromotionTag(promotionId: 9, promotionType: 1, label: '第三个'),
        ]);
    final r = productRow(p, (u) => 'http://h$u');
    expect(r.priceText, '¥69.00');
    expect(r.hasRange, isTrue);
    expect(r.promoTags, ['限时特价 ¥49.9', '满199减20']);
    expect(r.cover.imageUrl, 'http://h/api/v1/uploads/3');
    expect(r.cover.glyph, '挂');
  });

  test('productRow：in_stock 缺席时不说它没货；下架标出来', () {
    const p = ProductSummary(id: 1, title: 'A', minPriceCents: 100, status: 2);
    final r = productRow(p, (u) => u);
    expect(r.soldOut, isFalse);
    expect(r.offShelf, isTrue);
    expect(r.cover.imageUrl, '');
  });
}
```

运行：`~/development/flutter/bin/flutter test test/view_test.dart` —— Expected：FAIL

> 生成类的字段名以 Task 3 产物为准（`promotionId`、`promotionType`、`minPriceCents`…）；若构造参数名不同，按产物改测试，不改产物。

- [ ] **Step 2: 实现** `flutter_app/lib/api/view.dart`

```dart
import 'package:flutter/painting.dart';

import 'client.dart';
import 'schema.g.dart';
import 'session.dart';

/// 契约类型 -> 页面要显示的东西。页面只碰这里的行模型，契约字段一个都不写（页面闸门强制）。

String yuan(int? cents) {
  final c = cents ?? 0;
  final neg = c < 0;
  final a = c.abs();
  return '${neg ? '-' : ''}¥${a ~/ 100}.${(a % 100).toString().padLeft(2, '0')}';
}

/// 「2026-09-26T22:55:37.188864Z」-> 本地时区「09-27 06:55」。解析不了就原样返回。
String shortTime(String iso) {
  final d = DateTime.tryParse(iso);
  if (d == null) return iso;
  final l = d.toLocal();
  String p(int n) => n.toString().padLeft(2, '0');
  return '${p(l.month)}-${p(l.day)} ${p(l.hour)}:${p(l.minute)}';
}

class Cover {
  final String imageUrl;
  final Color color;
  final String glyph;
  const Cover(this.imageUrl, this.color, this.glyph);
}

const _tones = [Color(0xFFC9A27E), Color(0xFFA7B49A), Color(0xFFD4B896), Color(0xFFB98A6E), Color(0xFF9FA8A3), Color(0xFFC7A9A0)];

/// 有图显示图；没有图用按 id 取色的色块 + 标题首字（同一件商品各处颜色一致）。
Cover coverOf(int key, String title, String imageUrl) =>
    Cover(imageUrl, _tones[key.abs() % _tones.length], title.isEmpty ? '·' : title.substring(0, 1));

class ProductRow {
  final int id;
  final String title;
  final String subtitle;
  final String priceText;
  final bool hasRange;
  final bool offShelf;
  final bool soldOut;
  final Cover cover;
  final List<String> promoTags;
  const ProductRow({required this.id, required this.title, required this.subtitle, required this.priceText,
      required this.hasRange, required this.offShelf, required this.soldOut, required this.cover, required this.promoTags});
}

/// 首页卡片：多规格只显示最低价 + 「起」（hasRange），搜索页另显示完整区间（第 2 步）。
ProductRow productRow(ProductSummary p, String Function(String) asset) {
  final max = p.maxPriceCents;
  return ProductRow(
    id: p.id,
    title: p.title,
    subtitle: p.subtitle ?? '',
    priceText: yuan(p.minPriceCents),
    hasRange: max != null && max > p.minPriceCents,
    offShelf: p.status == 2,
    soldOut: p.inStock == false,
    cover: coverOf(p.id, p.title, p.imageUrl == null ? '' : asset(p.imageUrl!)),
    promoTags: (p.promotionTags ?? const []).take(2).map((t) => t.label).toList(),
  );
}

Future<List<ProductRow>> fetchProducts(ApiClient c, {required int? storeId, int page = 1, int pageSize = 20}) async {
  final q = <String, String>{'page': '$page', 'page_size': '$pageSize'};
  if (storeId != null) q['store_id'] = '$storeId';
  final res = await c.send('GET', '/products', query: q,
      decode: (j) => ListProductsResponse.fromJson(j as Map<String, dynamic>));
  return res.data.items.map((p) => productRow(p, c.assetUrl)).toList();
}

Future<void> login(ApiClient c, Session s, String phone, String password) async {
  final res = await c.send('POST', '/auth/login', body: LoginRequest(phone: phone, password: password).toJson(),
      decode: (j) => LoginResponse.fromJson(j as Map<String, dynamic>));
  await s.save(res.data);
}

Future<void> logout(ApiClient c, Session s) async {
  try {
    await c.send('POST', '/auth/logout', decode: (_) => null);
  } catch (_) {
    // 服务端吊销失败也要把本机清掉。
  }
  await s.clear();
}
```

运行：`~/development/flutter/bin/flutter test test/view_test.dart` —— Expected：PASS

- [ ] **Step 3: 门店的失败测试** `flutter_app/test/store_test.dart`

```dart
import 'dart:convert';

import 'package:flutter_test/flutter_test.dart';
import 'package:http/http.dart' as http;
import 'package:http/testing.dart';
import 'package:keel_buyer/api/client.dart';
import 'package:keel_buyer/api/session.dart';
import 'package:keel_buyer/api/store.dart';

void main() {
  test('只解析一次；并发调用共用一次请求；none 时 storeId 为 null', () async {
    var calls = 0;
    final c = ApiClient(base: 'http://h/api/v1', session: Session(), http: MockClient((r) async {
      calls++;
      await Future<void>.delayed(const Duration(milliseconds: 10));
      return http.Response(jsonEncode({'match_type': 'default', 'stores': [
        {'id': 1, 'name': '示例小店（默认门店）', 'is_default': true, 'distance_m': null}]}), 200);
    }));
    final s = StoreService(c);
    final r = await Future.wait([s.ensure(), s.ensure()]);
    expect(calls, 1);
    expect(r[0].storeId, 1);
    expect(r[0].name, '示例小店（默认门店）');
    await s.ensure();
    expect(calls, 1);

    final none = StoreService(ApiClient(base: 'http://h/api/v1', session: Session(),
        http: MockClient((r) async => http.Response(jsonEncode({'match_type': 'none', 'stores': []}), 200))));
    expect((await none.ensure()).storeId, isNull);
  });
}
```

- [ ] **Step 4: 实现** `flutter_app/lib/api/store.dart`

```dart
import 'dart:async';

import 'package:flutter/foundation.dart';

import 'client.dart';
import 'schema.g.dart';

class CurrentStore {
  final int? storeId;
  final String name;
  const CurrentStore(this.storeId, this.name);
}

/// 「哪家店在服务你」。价格与在售范围都跟着门店走，列表 / 详情 / 购物车 / 结算用同一家。
/// 第一阶段不接定位（原生定位插件 mp-flutter 接管不了）：不带坐标问 /stores/resolve，走「回落默认店」。
class StoreService extends ChangeNotifier {
  StoreService(this._c);
  final ApiClient _c;
  CurrentStore? current;
  Future<CurrentStore>? _inflight;

  Future<CurrentStore> ensure() {
    final cur = current;
    if (cur != null) return Future.value(cur);
    return _inflight ??= () async {
      try {
        final res = await _c.send('GET', '/stores/resolve',
            decode: (j) => StoreResolveResult.fromJson(j as Map<String, dynamic>));
        final stores = res.data.stores;
        final s = res.data.matchType == 'none' || stores.isEmpty
            ? const CurrentStore(null, '')
            : CurrentStore(stores.first.id, stores.first.name);
        current = s;
        notifyListeners();
        return s;
      } finally {
        _inflight = null;
      }
    }();
  }
}
```

运行：`~/development/flutter/bin/flutter test test/store_test.dart` —— Expected：PASS

- [ ] **Step 5: 装配** `flutter_app/lib/api/services.dart`

```dart
import 'package:flutter/widgets.dart';

import 'client.dart';
import 'session.dart';
import 'store.dart';

/// 页面拿依赖的地方：Services.of(context).client / session / store。
class Services extends InheritedWidget {
  const Services({super.key, required this.client, required this.session, required this.store, required super.child});
  final ApiClient client;
  final Session session;
  final StoreService store;

  static Services of(BuildContext context) {
    final s = context.dependOnInheritedWidgetOfExactType<Services>();
    assert(s != null, '上层没有 Services');
    return s!;
  }

  @override
  bool updateShouldNotify(Services old) => client != old.client || session != old.session || store != old.store;
}
```

运行：`make flutter-test && make flutter-analyze` —— Expected：全 PASS、`No issues found!`

- [ ] **Step 6: 提交**

```bash
git add flutter_app/lib/api flutter_app/test
git commit -m "Flutter：当前门店（只解析一次）、展示层（金额 / 本地时间 / 封面 / 商品行）与依赖装配"
```

---

### Task 7: 页面：外壳、首页、登录、我的，与页面闸门

**Files:**
- Create: `flutter_app/lib/router.dart`、`flutter_app/lib/widgets/product_card.dart`、`flutter_app/lib/widgets/states.dart`、`flutter_app/lib/pages/home_page.dart`、`flutter_app/lib/pages/login_page.dart`、`flutter_app/lib/pages/me_page.dart`、`scripts/check_flutter_pages.py`
- Modify: `flutter_app/lib/main.dart`、`scripts/check-all.sh`
- Test: `flutter_app/test/pages_test.dart`

**Interfaces:**
- Consumes: `Services`、`fetchProducts`、`login`、`logout`、`ProductRow`、`KeelColors` / `KeelText`
- Produces（e2e 与后续步骤依赖的 Key）：`Key('home.grid')`、`Key('product.card.<id>')`、`Key('home.store')`、`Key('login.phone')`、`Key('login.password')`、`Key('login.submit')`、`Key('login.message')`、`Key('me.login')`、`Key('me.logout')`、`Key('me.nickname')`、`Key('tab.home')`、`Key('tab.me')`；路由 `/`、`/me`、`/login?from=<path>`

- [ ] **Step 1: 页面闸门** `scripts/check_flutter_pages.py`

```python
#!/usr/bin/env python3
"""页面闸门：flutter_app/lib/pages 与 lib/widgets 下不许 import api/schema.g.dart。

契约字段只在 lib/api/ 里读写（view.dart 翻成行模型）；页面直接碰契约类型，契约改名时
报错不会集中在 api/ 这一层 —— 与 app/（uni-app x）的 check_app_types.py 同一条规矩。
"""
import os
import re
import sys

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
DIRS = [os.path.join(ROOT, 'flutter_app', 'lib', d) for d in ('pages', 'widgets')]
PAT = re.compile(r"""^\s*import\s+['"][^'"]*schema\.g\.dart['"]""", re.M)


def main():
    bad = []
    for d in DIRS:
        for dp, _, files in os.walk(d):
            for f in files:
                if f.endswith('.dart'):
                    p = os.path.join(dp, f)
                    for m in PAT.finditer(open(p, encoding='utf-8').read()):
                        bad.append(os.path.relpath(p, ROOT))
    if bad:
        sys.exit('check_flutter_pages FAIL：这些文件直接 import 了 schema.g.dart（改用 api/view.dart 的行模型）：\n  ' + '\n  '.join(bad))
    print('check_flutter_pages OK')


if __name__ == '__main__':
    main()
```

`scripts/check-all.sh` 在 `check_dart_contract.py` 那行后面加一行跑它。

- [ ] **Step 2: 页面的失败测试** `flutter_app/test/pages_test.dart`

```dart
import 'dart:convert';

import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:http/http.dart' as http;
import 'package:http/testing.dart';
import 'package:keel_buyer/api/client.dart';
import 'package:keel_buyer/api/services.dart';
import 'package:keel_buyer/api/session.dart';
import 'package:keel_buyer/api/store.dart';
import 'package:keel_buyer/router.dart';
import 'package:shared_preferences/shared_preferences.dart';

http.Response j(Object body, [int status = 200]) => http.Response(jsonEncode(body), status,
    headers: {'content-type': 'application/json; charset=utf-8'});

Future<Widget> app(MockClient fake) async {
  SharedPreferences.setMockInitialValues({});
  final session = Session();
  await session.load();
  final client = ApiClient(base: 'http://h/api/v1', session: session, http: fake);
  return Services(client: client, session: session, store: StoreService(client),
      child: MaterialApp.router(routerConfig: buildRouter(session)));
}

MockClient server() => MockClient((r) async {
      switch (r.url.path) {
        case '/api/v1/stores/resolve':
          return j({'match_type': 'default', 'stores': [{'id': 1, 'name': '示例小店', 'is_default': true, 'distance_m': null}]});
        case '/api/v1/products':
          return j({'page': 1, 'page_size': 20, 'total': 1, 'store': {'match_type': 'default', 'store_id': 1},
            'items': [{'id': 23, 'title': '冷萃咖啡液 6 支', 'min_price_cents': 5580, 'status': 1,
              'promotion_tags': [{'promotion_id': 1, 'promotion_type': 1, 'label': '满199减20'}]}]});
        case '/api/v1/auth/login':
          final b = jsonDecode(r.body) as Map;
          if (b['password'] != 'pw') return j({'type': 'x', 'title': '手机号或密码错误', 'status': 401}, 401);
          return j({'access_token': 'a', 'refresh_token': 'r', 'token_type': 'Bearer', 'expires_in': 7200,
            'user': {'id': 2, 'nickname': 'e2e 买家'}});
      }
      return j({'type': 'x', 'title': 'nope', 'status': 404}, 404);
    });

void main() {
  testWidgets('首页：门店行与商品卡（标题、价格、活动标签）', (t) async {
    await t.pumpWidget(await app(server()));
    await t.pumpAndSettle();
    expect(find.byKey(const Key('home.store')), findsOneWidget);
    expect(find.text('由「示例小店」为你配送'), findsOneWidget);
    expect(find.byKey(const Key('product.card.23')), findsOneWidget);
    expect(find.text('¥55.80'), findsOneWidget);
    expect(find.text('满199减20'), findsOneWidget);
  });

  testWidgets('我的 → 登录：密码错显示服务端原因；对了回到我的并显示昵称', (t) async {
    await t.pumpWidget(await app(server()));
    await t.pumpAndSettle();
    await t.tap(find.byKey(const Key('tab.me')));
    await t.pumpAndSettle();
    await t.tap(find.byKey(const Key('me.login')));
    await t.pumpAndSettle();
    await t.enterText(find.byKey(const Key('login.phone')), '13800000001');
    await t.enterText(find.byKey(const Key('login.password')), 'wrong');
    await t.tap(find.byKey(const Key('login.submit')));
    await t.pumpAndSettle();
    expect(find.text('手机号或密码错误'), findsOneWidget);
    await t.enterText(find.byKey(const Key('login.password')), 'pw');
    await t.tap(find.byKey(const Key('login.submit')));
    await t.pumpAndSettle();
    expect(find.byKey(const Key('me.nickname')), findsOneWidget);
    expect(find.text('e2e 买家'), findsOneWidget);
    await t.tap(find.byKey(const Key('me.logout')));
    await t.pumpAndSettle();
    expect(find.byKey(const Key('me.login')), findsOneWidget);
  });
}
```

运行：`~/development/flutter/bin/flutter test test/pages_test.dart` —— Expected：FAIL

- [ ] **Step 3: 共用组件** `flutter_app/lib/widgets/states.dart`

```dart
import 'package:flutter/material.dart';

import '../theme.dart';

class ErrorCard extends StatelessWidget {
  const ErrorCard({super.key, required this.message, required this.onRetry});
  final String message;
  final VoidCallback onRetry;
  @override
  Widget build(BuildContext context) => Card(
        margin: const EdgeInsets.all(16),
        child: Padding(
          padding: const EdgeInsets.all(18),
          child: Column(crossAxisAlignment: CrossAxisAlignment.start, children: [
            Text(message, style: KeelText.err),
            const SizedBox(height: 16),
            OutlinedButton(onPressed: onRetry, child: const Text('重新加载')),
          ]),
        ),
      );
}

class EmptyState extends StatelessWidget {
  const EmptyState({super.key, required this.text});
  final String text;
  @override
  Widget build(BuildContext context) => Padding(
        padding: const EdgeInsets.symmetric(vertical: 48),
        child: Center(child: Text(text, style: KeelText.sub)),
      );
}
```

`flutter_app/lib/widgets/product_card.dart`

```dart
import 'package:flutter/material.dart';

import '../api/view.dart';
import '../theme.dart';

/// 首页网格的商品卡：封面（真图 / 首字色块）、标题两行省略、活动标签、价格（多规格加「起」）。
class ProductCard extends StatelessWidget {
  const ProductCard({super.key, required this.row, required this.onTap});
  final ProductRow row;
  final VoidCallback onTap;

  @override
  Widget build(BuildContext context) {
    final c = row.cover;
    return GestureDetector(
      key: Key('product.card.${row.id}'),
      onTap: onTap,
      child: Container(
        decoration: BoxDecoration(color: KeelColors.card, borderRadius: BorderRadius.circular(16)),
        clipBehavior: Clip.antiAlias,
        child: Column(crossAxisAlignment: CrossAxisAlignment.start, children: [
          AspectRatio(
            aspectRatio: 1,
            child: c.imageUrl.isNotEmpty
                ? Image.network(c.imageUrl, fit: BoxFit.cover,
                    errorBuilder: (_, __, ___) => _Glyph(cover: c))
                : _Glyph(cover: c),
          ),
          Padding(
            padding: const EdgeInsets.fromLTRB(12, 10, 12, 12),
            child: Column(crossAxisAlignment: CrossAxisAlignment.start, children: [
              Text(row.title, maxLines: 2, overflow: TextOverflow.ellipsis,
                  style: KeelText.body.copyWith(fontWeight: FontWeight.w700)),
              if (row.subtitle.isNotEmpty) Text(row.subtitle, maxLines: 1, overflow: TextOverflow.ellipsis, style: KeelText.hint),
              if (row.promoTags.isNotEmpty)
                Padding(
                  padding: const EdgeInsets.only(top: 4),
                  child: Wrap(spacing: 4, runSpacing: 2, children: [
                    for (final tag in row.promoTags)
                      Container(
                        padding: const EdgeInsets.symmetric(horizontal: 5, vertical: 1),
                        decoration: BoxDecoration(border: Border.all(color: const Color(0xFFE8B4A6)), borderRadius: BorderRadius.circular(4)),
                        child: Text(tag, style: const TextStyle(fontSize: 11, color: KeelColors.err)),
                      ),
                  ]),
                ),
              const SizedBox(height: 6),
              Row(crossAxisAlignment: CrossAxisAlignment.end, children: [
                Text(row.priceText, style: KeelText.price),
                if (row.hasRange) const Text(' 起', style: KeelText.hint),
              ]),
            ]),
          ),
        ]),
      ),
    );
  }
}

class _Glyph extends StatelessWidget {
  const _Glyph({required this.cover});
  final Cover cover;
  @override
  Widget build(BuildContext context) => ColoredBox(
        color: cover.color,
        child: Center(child: Text(cover.glyph, style: const TextStyle(fontSize: 44, fontWeight: FontWeight.w700, color: KeelColors.card))),
      );
}
```

- [ ] **Step 4: 页面** `flutter_app/lib/pages/home_page.dart`

```dart
import 'package:flutter/material.dart';

import '../api/client.dart';
import '../api/services.dart';
import '../api/view.dart';
import '../theme.dart';
import '../widgets/product_card.dart';
import '../widgets/states.dart';

class HomePage extends StatefulWidget {
  const HomePage({super.key});
  @override
  State<HomePage> createState() => _HomePageState();
}

class _HomePageState extends State<HomePage> {
  List<ProductRow> _rows = const [];
  String _storeLine = '';
  String _error = '';
  bool _loading = true;
  bool _outOfRange = false;

  @override
  void didChangeDependencies() {
    super.didChangeDependencies();
    if (_loading && _rows.isEmpty && _error.isEmpty) _load();
  }

  Future<void> _load() async {
    final s = Services.of(context);
    setState(() {
      _loading = true;
      _error = '';
    });
    try {
      // 先定门店再拉列表：价格与在售范围按门店算。
      final store = await s.store.ensure();
      if (store.storeId == null) {
        setState(() {
          _outOfRange = true;
          _loading = false;
        });
        return;
      }
      final rows = await fetchProducts(s.client, storeId: store.storeId);
      if (!mounted) return;
      setState(() {
        _storeLine = '由「${store.name}」为你配送';
        _rows = rows;
        _loading = false;
      });
    } on ApiFailure catch (f) {
      if (!mounted) return;
      setState(() {
        _error = f.message;
        _loading = false;
      });
    }
  }

  String get _greeting {
    final h = DateTime.now().hour;
    return h < 11 ? '早上好' : (h < 18 ? '下午好' : '晚上好');
  }

  @override
  Widget build(BuildContext context) {
    return Scaffold(
      body: SafeArea(
        child: RefreshIndicator(
          onRefresh: _load,
          child: CustomScrollView(slivers: [
            SliverToBoxAdapter(
              child: Padding(
                padding: const EdgeInsets.fromLTRB(20, 20, 20, 8),
                child: Column(crossAxisAlignment: CrossAxisAlignment.start, children: [
                  const Text('KEEL · 精选好物', style: KeelText.overline),
                  const SizedBox(height: 8),
                  Text(_greeting, style: KeelText.display),
                  const Text('慢一点，好好喝一杯', style: KeelText.sub),
                  if (_storeLine.isNotEmpty)
                    Padding(
                      padding: const EdgeInsets.only(top: 6),
                      child: Text(_storeLine, key: const Key('home.store'), style: KeelText.hint),
                    ),
                ]),
              ),
            ),
            if (_error.isNotEmpty) SliverToBoxAdapter(child: ErrorCard(message: _error, onRetry: _load)),
            if (_outOfRange) const SliverToBoxAdapter(child: EmptyState(text: '当前位置暂不在配送范围内')),
            if (_loading && _rows.isEmpty) const SliverToBoxAdapter(child: EmptyState(text: '正在加载…')),
            SliverPadding(
              padding: const EdgeInsets.symmetric(horizontal: 10),
              sliver: SliverGrid(
                key: const Key('home.grid'),
                gridDelegate: const SliverGridDelegateWithFixedCrossAxisCount(
                    crossAxisCount: 2, mainAxisSpacing: 12, crossAxisSpacing: 12, childAspectRatio: 0.62),
                delegate: SliverChildBuilderDelegate(
                  (_, i) => ProductCard(row: _rows[i], onTap: () {}),
                  childCount: _rows.length,
                ),
              ),
            ),
            const SliverToBoxAdapter(child: SizedBox(height: 28)),
          ]),
        ),
      ),
    );
  }
}
```

（商品卡的 `onTap` 在第 2 步接详情页。）

`flutter_app/lib/pages/login_page.dart`

```dart
import 'package:flutter/material.dart';
import 'package:go_router/go_router.dart';

import '../api/client.dart';
import '../api/services.dart';
import '../api/view.dart';
import '../theme.dart';

class LoginPage extends StatefulWidget {
  const LoginPage({super.key, required this.from});
  final String from;
  @override
  State<LoginPage> createState() => _LoginPageState();
}

class _LoginPageState extends State<LoginPage> {
  final _phone = TextEditingController();
  final _password = TextEditingController();
  String _message = '';
  bool _busy = false;

  Future<void> _submit() async {
    if (_busy) return;
    final s = Services.of(context);
    setState(() {
      _busy = true;
      _message = '';
    });
    try {
      await login(s.client, s.session, _phone.text.trim(), _password.text);
      if (!mounted) return;
      context.go(widget.from.isEmpty ? '/me' : widget.from);
    } on ApiFailure catch (f) {
      if (!mounted) return;
      setState(() => _message = f.message);
    } finally {
      if (mounted) setState(() => _busy = false);
    }
  }

  @override
  Widget build(BuildContext context) => Scaffold(
        appBar: AppBar(title: const Text('登录')),
        body: ListView(padding: const EdgeInsets.all(20), children: [
          const Text('欢迎回来', style: KeelText.display),
          const SizedBox(height: 24),
          TextField(key: const Key('login.phone'), controller: _phone, keyboardType: TextInputType.phone,
              decoration: const InputDecoration(labelText: '手机号')),
          TextField(key: const Key('login.password'), controller: _password, obscureText: true,
              decoration: const InputDecoration(labelText: '密码')),
          const SizedBox(height: 24),
          FilledButton(key: const Key('login.submit'), onPressed: _busy ? null : _submit,
              child: Text(_busy ? '登录中…' : '登录')),
          if (_message.isNotEmpty)
            Padding(padding: const EdgeInsets.only(top: 12),
                child: Text(_message, key: const Key('login.message'), style: KeelText.err)),
        ]),
      );
}
```

`flutter_app/lib/pages/me_page.dart`

```dart
import 'package:flutter/material.dart';
import 'package:go_router/go_router.dart';

import '../api/services.dart';
import '../api/view.dart';
import '../theme.dart';

class MePage extends StatelessWidget {
  const MePage({super.key});

  @override
  Widget build(BuildContext context) {
    final s = Services.of(context);
    return ListenableBuilder(
      listenable: s.session,
      builder: (context, _) => Scaffold(
        body: SafeArea(
          child: ListView(padding: const EdgeInsets.all(20), children: [
            if (s.session.loggedIn) ...[
              Text(s.session.nickname.isEmpty ? '买家' : s.session.nickname, key: const Key('me.nickname'), style: KeelText.title),
              const Text('欢迎回来', style: KeelText.sub),
              const SizedBox(height: 24),
              OutlinedButton(key: const Key('me.logout'), onPressed: () => logout(s.client, s.session),
                  child: const Text('退出登录')),
            ] else ...[
              const Text('登录 / 注册', style: KeelText.title),
              const Text('登录后可以下单、查看订单', style: KeelText.sub),
              const SizedBox(height: 24),
              FilledButton(key: const Key('me.login'), onPressed: () => context.go('/login?from=/me'),
                  child: const Text('登录')),
            ],
            const SizedBox(height: 40),
            const Center(child: Text('Keel 买家端 · Flutter', style: KeelText.hint)),
          ]),
        ),
      ),
    );
  }
}
```

`flutter_app/lib/router.dart`

```dart
import 'package:flutter/material.dart';
import 'package:go_router/go_router.dart';

import 'api/session.dart';
import 'pages/home_page.dart';
import 'pages/login_page.dart';
import 'pages/me_page.dart';
import 'theme.dart';

/// 底部 tab：第 1 步只有 首页 / 我的；购物车、订单在第 3、4 步加进来。
GoRouter buildRouter(Session session) => GoRouter(
      refreshListenable: session,
      routes: [
        StatefulShellRoute.indexedStack(
          builder: (context, state, shell) => Scaffold(
            body: shell,
            bottomNavigationBar: NavigationBar(
              backgroundColor: KeelColors.card,
              selectedIndex: shell.currentIndex,
              onDestinationSelected: shell.goBranch,
              destinations: const [
                NavigationDestination(key: Key('tab.home'), icon: Icon(Icons.home_outlined), label: '首页'),
                NavigationDestination(key: Key('tab.me'), icon: Icon(Icons.person_outline), label: '我的'),
              ],
            ),
          ),
          branches: [
            StatefulShellBranch(routes: [GoRoute(path: '/', builder: (_, __) => const HomePage())]),
            StatefulShellBranch(routes: [GoRoute(path: '/me', builder: (_, __) => const MePage())]),
          ],
        ),
        GoRoute(path: '/login', builder: (_, st) => LoginPage(from: st.uri.queryParameters['from'] ?? '')),
      ],
    );
```

`flutter_app/lib/main.dart`

```dart
import 'package:flutter/material.dart';

import 'api/client.dart';
import 'api/services.dart';
import 'api/session.dart';
import 'api/store.dart';
import 'config.dart';
import 'router.dart';
import 'theme.dart';

Future<void> main() async {
  WidgetsFlutterBinding.ensureInitialized();
  final session = Session();
  await session.load();
  final router = buildRouter(session);
  final client = ApiClient(base: apiBase(), session: session,
      onSessionExpired: () => router.go('/login?from=${Uri.encodeComponent(router.state.uri.toString())}'));
  runApp(Services(
    client: client,
    session: session,
    store: StoreService(client),
    child: MaterialApp.router(title: 'Keel', theme: keelTheme(), routerConfig: router, debugShowCheckedModeBanner: false),
  ));
}
```

运行：`~/development/flutter/bin/flutter test test/pages_test.dart` —— Expected：PASS；再跑 `make flutter-test && make flutter-analyze` 和 `python3 scripts/check_flutter_pages.py` —— Expected：全过

- [ ] **Step 5: 提交**

```bash
git add flutter_app/lib flutter_app/test scripts/check_flutter_pages.py scripts/check-all.sh
git commit -m "Flutter：外壳（首页 / 我的）、首页商品网格、登录、我的；页面闸门（pages 不许 import schema.g.dart）"
```

---

### Task 8: Web 无头 e2e

**Files:**
- Create: `flutter_app/integration_test/e2e_env.dart`、`flutter_app/integration_test/smoke_test.dart`、`flutter_app/test_driver/integration_test.dart`、`flutter_app/tool/e2e_web.sh`
- Modify: `Makefile`

**Interfaces:**
- Consumes: Task 7 的 Key 与路由
- Produces: `make flutter-e2e-web`（需 `KEEL_API_BASE`；账号读 `~/.config/keel/e2e.env` 或环境变量 `KEEL_E2E_PHONE` / `KEEL_E2E_PASSWORD`）

- [ ] **Step 1: 驱动入口** `flutter_app/test_driver/integration_test.dart`

```dart
import 'package:integration_test/integration_test_driver.dart';

Future<void> main() => integrationDriver();
```

- [ ] **Step 2: e2e 账号** `flutter_app/integration_test/e2e_env.dart`

```dart
/// e2e 账号经 --dart-define 传入（tool/e2e_web.sh 从 ~/.config/keel/e2e.env 读出来再传），
/// 不写进仓库。没传就用演示买家（登录锁定豁免的那个）。
const e2ePhone = String.fromEnvironment('KEEL_E2E_PHONE', defaultValue: '13800000000');
const e2ePassword = String.fromEnvironment('KEEL_E2E_PASSWORD', defaultValue: 'keel-demo-2026');
```

- [ ] **Step 3: 冒烟用例** `flutter_app/integration_test/smoke_test.dart`

```dart
import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:integration_test/integration_test.dart';
import 'package:keel_buyer/main.dart' as app;

import 'e2e_env.dart';

/// 等一个 finder 出现：接口有网络延迟，按条件等，不 sleep 固定秒数。
Future<void> waitFor(WidgetTester t, Finder f, {Duration timeout = const Duration(seconds: 20)}) async {
  final end = DateTime.now().add(timeout);
  while (DateTime.now().isBefore(end)) {
    await t.pump(const Duration(milliseconds: 200));
    if (f.evaluate().isNotEmpty) return;
  }
  throw TestFailure('等 $f 超时');
}

void main() {
  IntegrationTestWidgetsFlutterBinding.ensureInitialized();

  testWidgets('冒烟：首页显示门店与服务端的商品；登录后「我的」显示昵称；退出', (t) async {
    await app.main();
    await waitFor(t, find.byKey(const Key('home.store')));
    await waitFor(t, find.byWidgetPredicate((w) => w.key is ValueKey<String> &&
        (w.key! as ValueKey<String>).value.startsWith('product.card.')));

    await t.tap(find.byKey(const Key('tab.me')));
    await t.pumpAndSettle();
    if (find.byKey(const Key('me.logout')).evaluate().isNotEmpty) {
      await t.tap(find.byKey(const Key('me.logout')));
      await waitFor(t, find.byKey(const Key('me.login')));
    }
    await t.tap(find.byKey(const Key('me.login')));
    await waitFor(t, find.byKey(const Key('login.phone')));
    await t.enterText(find.byKey(const Key('login.phone')), e2ePhone);
    await t.enterText(find.byKey(const Key('login.password')), e2ePassword);
    await t.tap(find.byKey(const Key('login.submit')));
    await waitFor(t, find.byKey(const Key('me.nickname')));
    await t.tap(find.byKey(const Key('me.logout')));
    await waitFor(t, find.byKey(const Key('me.login')));
  });
}
```

- [ ] **Step 4: 跑法** `flutter_app/tool/e2e_web.sh`

```bash
#!/usr/bin/env bash
# Flutter 买家端 Web 无头 e2e：按 KEEL_API_BASE 生成 web_dev_config.yaml（/api 反代到服务端，Web 只能同源），
# 取与本机 Chrome 同版本的 chromedriver，flutter drive -d web-server --headless 跑 integration_test/。
set -euo pipefail
cd "$(dirname "$0")/.."
: "${KEEL_API_BASE:?要设 KEEL_API_BASE（例如 http://192.168.0.110:18099/api/v1）}"
FLUTTER=${FLUTTER:-$HOME/development/flutter/bin/flutter}
ORIGIN=$(python3 -c "import sys,urllib.parse as u;p=u.urlparse(sys.argv[1]);print(f'{p.scheme}://{p.netloc}')" "$KEEL_API_BASE")
cat > web_dev_config.yaml <<EOF
server:
  proxy:
    - target: "$ORIGIN/"
      prefix: "/api/"
EOF

# e2e 账号：环境变量优先，没有就读 ~/.config/keel/e2e.env（不进仓库，与 app/ 的 e2e 同一个文件）。
ENV_FILE=${KEEL_E2E_ENV:-$HOME/.config/keel/e2e.env}
if [ -z "${KEEL_E2E_PHONE:-}" ] && [ -f "$ENV_FILE" ]; then set -a; . "$ENV_FILE"; set +a; fi
DEFINES=()
[ -n "${KEEL_E2E_PHONE:-}" ] && DEFINES+=(--dart-define=KEEL_E2E_PHONE="$KEEL_E2E_PHONE")
[ -n "${KEEL_E2E_PASSWORD:-}" ] && DEFINES+=(--dart-define=KEEL_E2E_PASSWORD="$KEEL_E2E_PASSWORD")

# chromedriver：与本机 Chrome 同一个主版本，缓存在 .tools/（gitignore）。
CHROME="/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"
VER=$("$CHROME" --version | awk '{print $3}')
DRIVER=".tools/chromedriver-$VER/chromedriver"
if [ ! -x "$DRIVER" ]; then
  mkdir -p .tools
  npx -y @puppeteer/browsers install "chromedriver@$VER" --path "$PWD/.tools/pb" >/dev/null
  mkdir -p ".tools/chromedriver-$VER"
  cp "$(find .tools/pb -name chromedriver -type f | head -1)" "$DRIVER"
fi
"$DRIVER" --port=4444 >/dev/null 2>&1 &
DRV=$!
trap 'kill $DRV 2>/dev/null || true' EXIT
sleep 1

for f in integration_test/*_test.dart; do
  "$FLUTTER" drive --driver=test_driver/integration_test.dart --target="$f" \
    -d web-server --headless --browser-name=chrome "${DEFINES[@]}"
done
```

`Makefile` 加：

```makefile
flutter-e2e-web:
	FLUTTER=$(FLUTTER) bash $(FLUTTER_APP)/tool/e2e_web.sh
```

（`chmod +x flutter_app/tool/e2e_web.sh`）

- [ ] **Step 5: 跑**

运行：`KEEL_API_BASE=http://192.168.0.110:18099/api/v1 make flutter-e2e-web` —— Expected：`All tests passed.`

排障：如果 `web_dev_config.yaml` 的反代没生效（请求 404 到 Flutter 自己的 dev server），先确认 3.41 的 flutter_tools 读的是工程根的 `web_dev_config.yaml`（`packages/flutter_tools/lib/src/web/devfs_config.dart` 里 `webDevServerConfigFilePath`），再确认 `prefix` 与 `target` 的写法；不要改成跨源访问（服务端没有 CORS）。

- [ ] **Step 6: 提交**

```bash
git add flutter_app/integration_test flutter_app/test_driver flutter_app/tool Makefile
git commit -m "Flutter：Web 无头 e2e（integration_test + chromedriver，/api 经 web_dev_config.yaml 反代同源）"
```

---

### Task 9: mp-flutter 编小程序与收尾

**Files:**
- Create: `flutter_app/mp_flutter.yaml`、`flutter_app/README.md`
- Modify: `flutter_app/pubspec.yaml`（dev 依赖 mp_flutter）、`Makefile`

**Interfaces:**
- Consumes: mp-flutter 的 `dart run mp_flutter`（`--dart-define` 透传由 mp-flutter 会话正在实现；`mp_flutter.yaml` 的 `dart_define` 键）
- Produces: `make flutter-build-mp`（产物 `flutter_app/build/weapp`）、`make flutter-build`

- [ ] **Step 0: 前置确认**

mp-flutter 已在 phase6 分支（06c00b4）支持 `--dart-define` 透传与 `mp_flutter.yaml` 的 `dart_define`。依赖用 git（私有库，ssh），`ref: phase6`。

- [ ] **Step 1: 依赖与配置**

`flutter_app/pubspec.yaml` 的 `dev_dependencies` 加：

```yaml
  mp_flutter:
    git:
      url: git@github.com:jackwangfeng/mp-flutter.git
      ref: phase6
      path: packages/mp_flutter
```

`flutter_app/mp_flutter.yaml`：

```yaml
# mp-flutter（把本工程编成微信小程序）的配置。服务地址在编译期注入（小程序没有页面 origin）。
appid: wx49d872db96d90546
output: build/weapp
```

运行：`make flutter-get` —— Expected：`Got dependencies!`；再跑 `cd flutter_app && dart run mp_flutter doctor` —— Expected：各项 ✓（有 ✗ 按提示补；是 mp-flutter 自身问题就发给 mp-flutter 会话）

- [ ] **Step 2: Makefile**

```makefile
# 编译检查：Web / Android / iOS（不签名）。服务地址用 KEEL_API_BASE 注入（原生必须注入）。
flutter-build:
	cd $(FLUTTER_APP) && $(FLUTTER) build web
	cd $(FLUTTER_APP) && $(FLUTTER) build apk --dart-define=KEEL_API_BASE=$(KEEL_API_BASE)
	cd $(FLUTTER_APP) && $(FLUTTER) build ios --no-codesign --dart-define=KEEL_API_BASE=$(KEEL_API_BASE)

# 微信小程序（mp-flutter）。产物 flutter_app/build/weapp，用微信开发者工具打开。
flutter-build-mp:
	cd $(FLUTTER_APP) && dart run mp_flutter --flutter $(FLUTTER) --dart-define=KEEL_API_BASE=$(KEEL_API_BASE)
```

运行：`KEEL_API_BASE=http://192.168.0.110:18099/api/v1 make flutter-build` —— Expected：三种产物都构建成功
运行：`KEEL_API_BASE=http://192.168.0.110:18099/api/v1 make flutter-build-mp` —— Expected：`build/weapp/app.json` 存在，体积校验通过

- [ ] **Step 3: 开发者工具里看一眼**

```bash
CLI=/Applications/wechatwebdevtools.app/Contents/MacOS/cli
$CLI open --project "$PWD/flutter_app/build/weapp"
```

在开发者工具里（勾「不校验合法域名」）确认：首页出现「由「…」为你配送」与商品卡、我的 → 登录能登上。只截开发者工具窗口，不截全屏。编译或运行时的 mp-flutter 问题：附报错发给 mp-flutter 会话。

- [ ] **Step 4: README** `flutter_app/README.md`

```markdown
# Keel 买家端（Flutter）

同一套代码：Android / iOS / Web / 微信小程序（经 mp-flutter）。uni-app x 那套（`app/`）已冻结，新功能只接这里。

## 跑起来

​```bash
make flutter-get
make flutter-generate      # 契约改了之后：重新生成 lib/api/schema.g.dart 并一起提交
make flutter-analyze flutter-test
KEEL_API_BASE=http://192.168.0.110:18099/api/v1 make flutter-e2e-web   # Web 无头 e2e
KEEL_API_BASE=http://192.168.0.110:18099/api/v1 make flutter-build     # Web / apk / iOS 编译
KEEL_API_BASE=https://eshop.zzss.fun/api/v1 make flutter-build-mp      # 微信小程序
​```

## 规矩

- 契约字段只在 `lib/api/` 读写；`lib/pages/`、`lib/widgets/` 不许 import `schema.g.dart`（`scripts/check_flutter_pages.py`）。
- `schema.g.dart` 由 `scripts/gen_dart_schema.py` 生成，`scripts/check_dart_contract.py` 比对，请勿手改。
- 服务地址：原生与小程序编译期 `--dart-define=KEEL_API_BASE=...` 注入；Web 用相对路径 `/api/v1` 同源（开发 / e2e 走 `web_dev_config.yaml` 反代）。
- 依赖只用 mp-flutter 能透明接管的（`http`、`shared_preferences`）；不依赖 Cookie；不用原生插件。
- 日常验收：`flutter analyze`、两道闸门、单测、Web 无头 e2e、各平台编译、mp-flutter 编译；真机只在发版前跑。
```

（README 里的代码围栏去掉前面的零宽字符 `​`，那是为了在本计划里嵌套显示。）

- [ ] **Step 5: 全量闸门**

```bash
PATH=$V/bin:$PATH bash scripts/check-all.sh
make flutter-analyze flutter-test
KEEL_API_BASE=http://192.168.0.110:18099/api/v1 make flutter-e2e-web
```

Expected：全部通过。

- [ ] **Step 6: 提交并推送**

```bash
git add flutter_app Makefile
git commit -m "Flutter：mp-flutter 编小程序、各平台编译目标与 README（第 1 步骨架完成）"
git push origin main
```
