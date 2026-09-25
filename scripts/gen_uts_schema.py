#!/usr/bin/env python3
"""从 OpenAPI 契约生成 UTS 侧的契约类型（app/src/api/schema.uts）。

## 为什么不能直接用 web/src/api/schema.d.ts

`web/src/api/schema.d.ts` 是 openapi-typescript 的产物，`web/src/api/client.mts`
在它上面又架了一层条件类型（`Slot<K, Raw>`、`PathsWith<M>`、`MethodsOf<P>`、
`ResponseBodyOf<P, M>`）。**UTS 不是 TypeScript** —— 它的类型系统要能落到
Kotlin / Swift 上，映射类型、条件类型、`keyof`、模板字面量类型、索引签名
都没有对应物。实测结论记在 app/README.md 里。

所以 UTS 侧不能共用那份产物。但「不共用」不等于「手抄一份」——
这个仓库已经有三处两份真相源分叉的教训。做法与 Go / TS / sqlc 三侧完全一致：
**同一份契约，另生成一份产物，产物入库，闸门比对。**
契约改了而这份没重生成，scripts/check_uts_contract.py 会红。

## 映射规则（以及为什么是这些）

  integer / number      -> number
  string                -> string
  boolean               -> boolean
  array                 -> T[]
  $ref                  -> 被引用的类型名
  allOf                 -> **展平成一个对象类型**。UTS 没有交叉类型（`A & B`），
                           Kotlin 那边也没有；契约里 9 处 allOf 全是
                           「基类型 + 附加字段」这一种用法，展平后语义不变。
  additionalProperties  -> UTSJSONObject（UTS 的动态 JSON 对象；TS 侧由
                           app/typecheck/uni-shim.d.ts 声明同名类型）
  type: [X, 'null']     -> X | null（OpenAPI 3.1 的可空写法，契约里 1 处）
  非 required 的字段     -> `name?: T`

内联对象（响应体里现写的 `type: object`）会被提升成具名类型，
名字由它出现的位置拼出来，例如 Problem.errors 的元素 -> ProblemErrorsItem。

## 覆盖到哪些接口

组件 schema 全量生成；路径侧只生成 OPERATIONS 里列的那些（买家端闭环用到的）。
表里的某条路径/方法在契约里消失时，这个脚本**当场失败**而不是少生成一个类型 ——
少生成的话，客户端那条调用会因为「类型不存在」红在一个和真因无关的地方。
"""
import argparse
import os
import re
import sys

try:
    import yaml
except ImportError:  # pragma: no cover
    sys.exit('需要 PyYAML：pip install pyyaml')

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
CONTRACT = os.path.join(ROOT, 'docs', '电商系统-OpenAPI.yaml')

# 买家端闭环用到的接口。(方法, 路径, 名字前缀)
#
# 名字前缀只在「契约那里是内联 schema」时才会被用到 —— 响应体直接 $ref 的那些
# 不会多生成一个别名，客户端直接用组件类型名。多一层别名等于多一个要对齐的名字。
OPERATIONS = [
    ('get', '/products', 'ListProducts'),
    ('get', '/products/{product_id}', 'GetProduct'),
    ('post', '/auth/login', 'Login'),
    ('post', '/auth/refresh', 'RefreshToken'),
    ('post', '/auth/logout', 'Logout'),
    ('post', '/orders/preview', 'PreviewOrder'),
    ('post', '/orders', 'CreateOrder'),
    ('get', '/orders', 'ListOrders'),
    ('get', '/orders/{order_no}', 'GetOrder'),
    ('post', '/orders/{order_no}/payments', 'CreatePayment'),
]

HEADER = '''// 由 scripts/gen_uts_schema.py 从 docs/电商系统-OpenAPI.yaml 生成。**请勿手改。**
//
// 手改会被 scripts/check_uts_contract.py 抓住：那个闸门把契约重新生成到临时目录
// 再和这个文件比对，对不上就红。改契约 -> `make generate-uts` -> 一起提交。
//
// 这里只有类型，没有一行运行时代码 —— 运行时在 app/src/api/client.uts。
'''


class Gen:
    def __init__(self, spec):
        self.spec = spec
        self.schemas = spec['components']['schemas']
        self.out = []          # (名字, 定义文本)
        self.emitted = set()

    # -- 工具 ---------------------------------------------------------------

    def resolve(self, node):
        """把 $ref 解开一层。只支持 #/components/schemas/X —— 契约里没有别的。"""
        while isinstance(node, dict) and '$ref' in node:
            ref = node['$ref']
            m = re.fullmatch(r'#/components/schemas/(\w+)', ref)
            if not m:
                raise SystemExit('不支持的 $ref: %s' % ref)
            node = self.schemas[m.group(1)]
        return node

    @staticmethod
    def ref_name(node):
        if isinstance(node, dict) and '$ref' in node:
            m = re.fullmatch(r'#/components/schemas/(\w+)', node['$ref'])
            if m:
                return m.group(1)
        return None

    # -- 类型表达式 ----------------------------------------------------------

    def type_expr(self, node, name_hint):
        """把一个 schema 节点翻成 UTS 类型表达式，必要时提升出具名类型。"""
        if node is None:
            return 'any'

        ref = self.ref_name(node)
        if ref is not None:
            # 纯 $ref（没有兄弟关键字）就直接用被引用的名字。
            siblings = set(node.keys()) - {'$ref', 'description'}
            if not siblings:
                self.emit_schema(ref)
                return ref
            # 有兄弟关键字（契约里是 $ref + description/default）。语义仍是那个类型。
            self.emit_schema(ref)
            return ref

        if 'allOf' in node:
            # 契约里有一种写法是 `allOf: [$ref X]` + description/default —— 那不是
            # 组合，只是为了给一个 $ref 挂上说明（JSON Schema 里 $ref 的兄弟关键字
            # 在 3.0 时代会被忽略，于是大家用 allOf 包一层）。这种要原样透出被引用
            # 的类型；当成对象展平的话，Money 这类标量会变成一个空对象类型。
            members = node['allOf']
            extra = {k: v for k, v in node.items() if k != 'allOf'}
            if len(members) == 1 and not extra.get('properties') and not extra.get('required'):
                return self.type_expr(members[0], name_hint)
            return self.object_expr(self.flatten_all_of(node), name_hint)

        t = node.get('type')
        nullable = False
        if isinstance(t, list):
            non_null = [x for x in t if x != 'null']
            nullable = len(non_null) != len(t)
            if len(non_null) != 1:
                raise SystemExit('不支持的 type 联合: %r（%s）' % (t, name_hint))
            t = non_null[0]

        if t == 'array':
            inner = self.type_expr(node.get('items'), name_hint + 'Item')
            expr = '%s[]' % inner
        elif t == 'object' or ('properties' in node and t is None):
            if 'properties' not in node and 'additionalProperties' in node:
                # 自由形式的 JSON 对象（spec_values、payload 这类）。
                expr = 'UTSJSONObject'
            else:
                expr = self.object_expr(node, name_hint)
        elif t == 'integer' or t == 'number':
            expr = 'number'
        elif t == 'string':
            expr = 'string'
        elif t == 'boolean':
            expr = 'boolean'
        elif t is None:
            expr = 'any'
        else:
            raise SystemExit('不支持的 type: %r（%s）' % (t, name_hint))

        return expr + ' | null' if nullable else expr

    def flatten_all_of(self, node):
        """展平 allOf。UTS 没有交叉类型，Kotlin 那边也没有。"""
        merged = {'type': 'object', 'properties': {}, 'required': []}
        parts = list(node['allOf'])
        # allOf 之外的兄弟关键字（契约里是 default / description）也并进来。
        extra = {k: v for k, v in node.items() if k not in ('allOf',)}
        if extra.get('properties') or extra.get('required'):
            parts.append(extra)
        for part in parts:
            part = self.resolve(part)
            if 'allOf' in part:
                part = self.flatten_all_of(part)
            if 'properties' not in part and part.get('type') not in (None, 'object'):
                raise SystemExit('allOf 里混进了非对象成员（%r），展平会丢语义' % part.get('type'))
            merged['properties'].update(part.get('properties') or {})
            merged['required'].extend(part.get('required') or [])
        return merged

    def object_expr(self, node, name_hint):
        """对象 -> 具名类型，返回类型名。"""
        self.emit_object(name_hint, node)
        return name_hint

    # -- 产出 ----------------------------------------------------------------

    def emit_object(self, name, node, doc=None):
        if name in self.emitted:
            return
        self.emitted.add(name)          # 先占位，挡住自引用造成的无限递归
        required = set(node.get('required') or [])
        lines = []
        for prop, sub in (node.get('properties') or {}).items():
            hint = name + ''.join(w.capitalize() for w in re.split(r'[^0-9A-Za-z]+', prop) if w)
            expr = self.type_expr(sub, hint)
            desc = (self.resolve(sub).get('description') or '').strip().splitlines()
            if desc and desc[0]:
                lines.append('  /** %s */' % desc[0].strip())
            lines.append('  %s%s : %s' % (prop, '' if prop in required else '?', expr))
        body = '{\n%s\n}' % '\n'.join(lines) if lines else '{}'
        text = ''
        if doc:
            text += '/** %s */\n' % doc
        text += 'export type %s = %s\n' % (name, body)
        self.out.append((name, text))

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
            if 'properties' not in node and 'additionalProperties' in node:
                self.emitted.add(name)
                self.out.append(('%s' % name, 'export type %s = UTSJSONObject\n' % name))
                return
            self.emit_object(name, node, doc)
            return
        # 标量别名（Money、OrderStatus 这类）。
        self.emitted.add(name)
        expr = self.type_expr({k: v for k, v in node.items() if k != 'description'}, name)
        text = ''
        if doc:
            text += '/** %s */\n' % doc
        text += 'export type %s = %s\n' % (name, expr)
        self.out.append((name, text))

    # -- 路径侧 ---------------------------------------------------------------

    def emit_operations(self):
        paths = self.spec['paths']
        params = self.spec['components'].get('parameters') or {}
        pieces = []
        for method, path, prefix in OPERATIONS:
            if path not in paths or method not in paths[path]:
                raise SystemExit(
                    '契约里没有 %s %s —— OPERATIONS 与契约对不上了。\n'
                    '要么契约改了路径（那就改 OPERATIONS 与客户端），'
                    '要么这个表抄错了。' % (method.upper(), path))
            op = paths[path][method]

            # query 参数：拼成一个对象类型。
            query_props, query_required = {}, []
            for p in op.get('parameters') or []:
                if '$ref' in p:
                    ref = re.fullmatch(r'#/components/parameters/(\w+)', p['$ref'])
                    if not ref:
                        raise SystemExit('不支持的 parameter $ref: %s' % p['$ref'])
                    p = params[ref.group(1)]
                if p.get('in') != 'query':
                    continue
                query_props[p['name']] = p['schema']
                if p.get('required'):
                    query_required.append(p['name'])
            if query_props:
                self.emit_object('%sQuery' % prefix,
                                 {'type': 'object', 'properties': query_props,
                                  'required': query_required},
                                 '%s %s 的 query 参数' % (method.upper(), path))

            # 请求体。
            body = (op.get('requestBody') or {}).get('content', {}).get('application/json')
            if body:
                schema = body['schema']
                if self.ref_name(schema) is None:
                    self.emit_object('%sRequest' % prefix, self.expand(schema),
                                     '%s %s 的请求体' % (method.upper(), path))
                else:
                    self.emit_schema(self.ref_name(schema))

            # 成功响应体。
            for code in ('200', '201', '202'):
                resp = (op.get('responses') or {}).get(code)
                if not resp:
                    continue
                content = (resp.get('content') or {}).get('application/json')
                if not content:
                    continue
                schema = content['schema']
                if self.ref_name(schema) is None:
                    self.emit_object('%sResponse' % prefix, self.expand(schema),
                                     '%s %s 的 %s 响应体' % (method.upper(), path, code))
                else:
                    self.emit_schema(self.ref_name(schema))
                break
        return pieces

    def expand(self, schema):
        return self.flatten_all_of(schema) if 'allOf' in schema else self.resolve(schema)

    def render(self):
        for name in sorted(self.schemas):
            self.emit_schema(name)
        self.emit_operations()
        body = '\n'.join(text for _, text in self.out)
        return HEADER + '\n' + body


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument('-o', '--out', required=True)
    ap.add_argument('-c', '--contract', default=CONTRACT)
    args = ap.parse_args()

    with open(args.contract, encoding='utf-8') as f:
        spec = yaml.safe_load(f)

    text = Gen(spec).render()
    os.makedirs(os.path.dirname(os.path.abspath(args.out)), exist_ok=True)
    # 先写临时文件再 rename：生成到一半失败时不留下半个产物。
    tmp = args.out + '.tmp'
    with open(tmp, 'w', encoding='utf-8') as f:
        f.write(text)
    os.replace(tmp, args.out)
    print('generated %s' % os.path.abspath(args.out))


if __name__ == '__main__':
    main()
