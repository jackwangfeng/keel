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
            t = self.dart_type(d)
            lines.append('  final %s%s %s;' % (t, '?' if opt and t != 'dynamic' else '', f))
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
