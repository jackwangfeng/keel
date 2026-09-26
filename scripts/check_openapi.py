#!/usr/bin/env python3
"""校验 OpenAPI 契约：结构有效、零悬空 $ref、无不可达 schema、必备路径与约定齐全。

设计要点：引用与孤儿检测一律走 **解析后的对象树**，不扫原文——
扫原文会让 YAML 注释里的 $ref「养活」一个孤儿，也会把 description 正文里的
"$ref:" 误报成悬空引用。
"""
import io
import re
import os
import sys

import yaml

SPEC = 'docs/电商系统-OpenAPI.yaml'

# 契约必须提供的路径与方法。每个任务往这里追加，就是在写失败的测试。
REQUIRED_PATHS = [
    ('/uploads', 'post'),
    ('/uploads/{upload_id}', 'get'),
    ('/search/events', 'post'),
    ('/cart/items/batch-delete', 'post'),
    ('/admin/auth/bootstrap', 'post'),
    ('/admin/auth/email-link', 'post'),
    ('/admin/auth/session', 'post'),
    ('/admin/staff', 'get'),
    ('/admin/staff', 'post'),
    ('/admin/staff/{staff_id}', 'patch'),
    ('/admin/me', 'get'),
    ('/admin/merchants', 'post'),
    ('/admin/orders/{order_no}/shipments', 'post'),
    ('/admin/refunds/{refund_no}/audit', 'post'),
    # M4 任务 1：商家自助发布的写接口面。此前 /admin/ 下一条商品写接口都没有
    # ——商家能处理订单，却没法上架商品。登记在这里，是为了让「哪天有人把这一段
    # 删了或改了名」当场红，而不是又一次在开工时当成惊喜发现。
    ('/admin/uploads', 'post'),
    ('/admin/products', 'get'),
    ('/admin/products', 'post'),
    ('/admin/products/{product_id}', 'get'),
    ('/admin/products/{product_id}', 'patch'),
    ('/admin/products/{product_id}', 'delete'),
    ('/admin/products/{product_id}/publication', 'post'),
    ('/admin/products/{product_id}/images', 'put'),
    ('/admin/products/{product_id}/skus', 'post'),
    ('/admin/skus/{sku_id}', 'patch'),
    ('/admin/skus/{sku_id}', 'delete'),
    ('/admin/skus/{sku_id}/inventory', 'put'),
    ('/admin/categories', 'get'),
    ('/admin/categories', 'post'),
    ('/admin/categories/{category_id}', 'patch'),
    ('/admin/categories/{category_id}', 'delete'),
]

REQUIRED_SCHEMAS = ['UploadTarget', 'Upload',
                    'Staff', 'StaffRole', 'StaffSession', 'StaffCreateRequest',
                    'Merchant', 'MerchantCreateRequest',
                    'Shipment', 'ShipmentCreateRequest',
                    'AdminProduct', 'AdminProductDetail',
                    'ProductCreateRequest', 'ProductUpdateRequest',
                    'ProductPublicationRequest',
                    'ProductImage', 'ProductImageInput', 'ProductImagesReplaceRequest',
                    'AdminSku', 'SkuCreateRequest', 'SkuUpdateRequest',
                    'AdminInventory', 'InventorySetRequest', 'InventoryConflict',
                    'AdminCategory', 'CategoryCreateRequest', 'CategoryUpdateRequest']

# (schema 名, 必须存在的属性名)
REQUIRED_FIELDS = [
    ('Cart', 'selected_total_cents'),
    ('OrderItem', 'refunding_qty'),
    ('OrderDetail', 'refunds'),
    # 库存写接口的乐观并发全靠这一个字段。它一旦被「简化」掉，接口就退回成
    # 无条件覆盖：商家把 10 改成 20 的那三分钟里卖掉的 4 件会被静默抹掉，
    # 而没有任何东西会响。所以它进这张表，不只是写在描述里。
    ('InventorySetRequest', 'expected_available_qty'),
    # 商品图这一路的落点。M2 验收记过 image_url / images「声明了但从不填」，
    # 原因是没有任何东西把 uploads 和商品连起来。
    ('ProductImageInput', 'upload_id'),
]

# 有副作用的 POST 必须接受 Idempotency-Key（文件头约定 5）。
# 豁免必须在此显式列出并写明理由，不允许静默遗漏。
IDEMPOTENCY_EXEMPT = {
    '/search':            '无副作用；用 POST 只因请求体结构复杂',
    '/admin/auth/bootstrap':  '一次性 token 换会话，重放由 used_at 拦截',
    '/admin/auth/email-link': '重复请求只是多发一封信，且有频控',
    '/admin/auth/session':    '一次性 token 换会话，重放由 used_at 拦截',
    '/admin/merchants':       'code 全局唯一，重复建店必然撞唯一索引',
    '/assistant/chat':    '无副作用；会话状态由 session_id 承载',
    '/orders/preview':    '无副作用；纯试算',
    '/coupons/applicable': '无副作用；纯查询',
    '/auth/sms/code':     '重复请求由 429 频控拦截，不是幂等键的战场',
    '/auth/login':        '同一凭据重复登录天然幂等（换发 token 是预期行为）',
    '/auth/wechat/login': '同上',
    '/auth/refresh':      'refresh token 一次性，重复使用必然失败，无需幂等键',
    '/auth/logout':       '重复登出是幂等的',
    '/webhooks/payments/{channel}': '渠道不会带我们的幂等头；幂等由 '
                                    '(channel, channel_txn_id) 唯一索引兜底',
    '/webhooks/refunds/{channel}':  '同上，靠 (channel, channel_refund_id)',
}

# 这些查询参数带 default 是正常的：它们不改变「返回哪些行」，只改变排序/分页/详略。
SAFE_DEFAULT_PARAMS = {'page', 'page_size', 'size', 'sort', 'explain', 'strategy'}


class DupKeyLoader(yaml.SafeLoader):
    """拒绝重复键。默认 loader 会静默让后者覆盖前者——
    一个被覆盖的 schema 在结构上完全合法，却不是作者写的那个。"""


# 契约里出现的 problem type URI。判据是路径里带 /problems/ 或域名以 errors. 开头——
# 后者是为了让「退回旧占位域名」这个动作也被抓住，而不是只认新域名然后对旧的失明。
PROBLEM_TYPE_RE = re.compile(r'https?://(?:errors\.[^/\s"\']+|[^/\s"\']+)/problems?/[a-z0-9-]+'
                             r'|https?://errors\.[^/\s"\']+/[a-z0-9-]+')


def _no_dup(loader, node, deep=False):
    mapping = {}
    for key_node, value_node in node.value:
        key = loader.construct_object(key_node, deep=deep)
        if key in mapping:
            raise yaml.YAMLError('重复的键: %r（第 %d 行）'
                                 % (key, key_node.start_mark.line + 1))
        mapping[key] = loader.construct_object(value_node, deep=deep)
    return mapping


DupKeyLoader.add_constructor(
    yaml.resolver.BaseResolver.DEFAULT_MAPPING_TAG, _no_dup)


def walk(node, fn):
    """遍历解析后的对象树。"""
    fn(node)
    if isinstance(node, dict):
        for v in node.values():
            walk(v, fn)
    elif isinstance(node, list):
        for v in node:
            walk(v, fn)


def refs_in(node):
    found = []

    def visit(n):
        if isinstance(n, dict) and isinstance(n.get('$ref'), str):
            found.append(n['$ref'])
    walk(node, visit)
    return found


def resolve(doc, ref):
    node = doc
    for part in ref.lstrip('#/').split('/'):
        part = part.replace('~1', '/').replace('~0', '~')
        if isinstance(node, dict) and part in node:
            node = node[part]
        else:
            return None
    return node


def reachable_schemas(doc):
    """从 paths 出发做可达性闭包。只看「谁引用了谁」会漏掉
    「孤儿 A 引用孤儿 B」——B 被 A 养活，两个都逃掉。"""
    schemas = doc.get('components', {}).get('schemas', {})
    seen = set()
    frontier = [r for r in refs_in(doc.get('paths', {}))]
    # 被 components 下的 responses / parameters / headers 引用的也算可达，
    # 前提是那些自身被 paths 引用到——这里简化为：从 paths 出发的引用即可达。
    while frontier:
        ref = frontier.pop()
        name = ref.rsplit('/', 1)[-1]
        target = resolve(doc, ref)
        if target is None:
            continue
        if '/schemas/' in ref:
            if name in seen:
                continue
            seen.add(name)
        frontier.extend(refs_in(target))
    return seen, schemas


def main():
    root = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
    path = os.path.join(root, SPEC)
    text = io.open(path, encoding='utf-8').read()
    problems = []

    try:
        doc = yaml.load(text, Loader=DupKeyLoader)
    except yaml.YAMLError as exc:
        print('YAML 无法解析或有重复键: %s' % exc)
        return 1

    if str(doc.get('openapi', '')).split('.')[0:2] != ['3', '1']:
        problems.append('openapi 版本不是 3.1，实际为 %r' % doc.get('openapi'))

    # 悬空 $ref（走对象树，注释与正文中的 "$ref:" 不参与）
    for ref in sorted(set(refs_in(doc))):
        if resolve(doc, ref) is None:
            problems.append('悬空 $ref: %s' % ref)

    # 不可达 schema
    seen, schemas = reachable_schemas(doc)
    for name in sorted(set(schemas) - seen):
        problems.append('不可达 schema（从 paths 出发引用不到）: %s' % name)

    # RFC 9457 的 problem type 必须全部同源。
    #
    # 这里抓过一次真的：契约里 13 个 type 全在 https://errors.example.com/，
    # 而 internal/problem 产出的 13 个全在 https://keel.dev/problems/，
    # 只有 4 个名字重合。两份「唯一真相源」在这件事上整体分叉了一个里程碑，
    # 而没有任何东西会响 —— type 是客户端**用来分支**的字段，不是展示文案，
    # 分叉的代价是客户端照契约写的 switch 一条都命不中。
    #
    # 判据只管「同源」，不管域名叫什么：域名将来会变，而「契约里说的 type
    # 和服务端发的 type 是同一个」这条不会变。
    hosts = {}
    for m in PROBLEM_TYPE_RE.finditer(text):
        url = m.group(0)
        host = url.split('/')[2]
        hosts.setdefault(host, []).append(url)
    if len(hosts) > 1:
        problems.append(
            'problem type 用了不止一个域名：%s —— type 是客户端用来分支的字段，'
            '分叉之后照契约写的 switch 一条都命不中'
            % '、'.join('%s（%d 处）' % (h, len(v)) for h, v in sorted(hosts.items())))
    elif not hosts:
        problems.append('契约里一个 problem type 都没找到——这条检查本身失效了')

    paths = doc.get('paths', {})
    for p, method in REQUIRED_PATHS:
        if p not in paths:
            problems.append('缺少路径: %s' % p)
        elif method not in paths[p]:
            problems.append('路径 %s 缺少方法: %s' % (p, method))

    for name in REQUIRED_SCHEMAS:
        if name not in schemas:
            problems.append('缺少 schema: %s' % name)

    def props_of(schema_name, _seen=None):
        _seen = _seen or set()
        if schema_name in _seen:
            return set()
        _seen.add(schema_name)
        node = schemas.get(schema_name, {})
        names = set(node.get('properties', {}))
        for branch in node.get('allOf', []):
            if '$ref' in branch:
                names |= props_of(branch['$ref'].rsplit('/', 1)[-1], _seen)
            else:
                names |= set(branch.get('properties', {}))
        return names

    for schema_name, field in REQUIRED_FIELDS:
        if field not in props_of(schema_name):
            problems.append('schema %s 缺少字段: %s' % (schema_name, field))

    METHODS = ('get', 'put', 'post', 'delete', 'patch', 'head', 'options')
    for p, item in paths.items():
        # 路径模板参数必须被声明
        tmpl = {seg[1:-1] for seg in p.split('/')
                if seg.startswith('{') and seg.endswith('}')}
        for method, op in item.items():
            if method not in METHODS:
                continue
            declared = set()
            for param in list(item.get('parameters', [])) + list(op.get('parameters', [])):
                if '$ref' in param:
                    param = resolve(doc, param['$ref']) or {}
                if param.get('in') == 'path':
                    declared.add(param.get('name'))
            for missing in sorted(tmpl - declared):
                problems.append('%s %s 路径参数 {%s} 未声明'
                                % (method.upper(), p, missing))

            # 每个响应必须有 description（OAS 必填）
            for code, resp in (op.get('responses') or {}).items():
                if '$ref' in resp:
                    resp = resolve(doc, resp['$ref']) or {}
                if not resp.get('description'):
                    problems.append('%s %s 的 %s 响应缺 description'
                                    % (method.upper(), p, code))

            # 查询参数不得带 default —— 缺省会被代入，静默改变筛选语义
            for param in list(item.get('parameters', [])) + list(op.get('parameters', [])):
                raw = param
                if '$ref' in param:
                    param = resolve(doc, param['$ref']) or {}
                if param.get('in') != 'query':
                    continue
                sch = param.get('schema') or {}
                if '$ref' in sch:
                    target = resolve(doc, sch['$ref']) or {}
                    if 'default' in target:
                        problems.append(
                            '%s %s 的查询参数 %s 引用了带 default 的 schema %s'
                            ' —— 不传该参数时会被静默代入默认值'
                            % (method.upper(), p, param.get('name'),
                               sch['$ref'].rsplit('/', 1)[-1]))
                elif 'default' in sch and param.get('name') not in SAFE_DEFAULT_PARAMS:
                    problems.append('%s %s 的查询参数 %s 带 default'
                                    % (method.upper(), p, param.get('name')))

            # 约定 6：后台接口一律在 /admin/ 前缀下
            if 'Admin' in (op.get('tags') or []) and not p.startswith('/admin/'):
                problems.append('%s %s 带 Admin tag 却不在 /admin/ 前缀下'
                                '（文件头约定 6）' % (method.upper(), p))

            # 有副作用的 POST 必须接受 Idempotency-Key
            if method == 'post' and p not in IDEMPOTENCY_EXEMPT:
                names = set()
                for param in list(item.get('parameters', [])) + list(op.get('parameters', [])):
                    if '$ref' in param:
                        param = resolve(doc, param['$ref']) or {}
                    names.add(param.get('name'))
                if 'Idempotency-Key' not in names:
                    problems.append('POST %s 未接受 Idempotency-Key（文件头约定 5）' % p)

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
