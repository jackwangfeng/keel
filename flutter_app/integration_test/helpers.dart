import 'dart:convert';

import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:http/http.dart' as http;
import 'package:http_parser/http_parser.dart';
import 'package:keel_buyer/main.dart' as app;
import 'package:keel_buyer/main.dart' show appRouter;

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

/// 等一个 finder 消失：比如登录页退场动画没走完时，底下的页已经能找到，但点击会落在登录页上。
Future<void> waitGone(WidgetTester t, Finder f, {Duration timeout = const Duration(seconds: 20)}) async {
  final end = DateTime.now().add(timeout);
  while (DateTime.now().isBefore(end)) {
    await t.pump(const Duration(milliseconds: 200));
    if (f.evaluate().isEmpty) return;
  }
  throw TestFailure('等 $f 消失超时');
}

/// 等一个条件成立（比如服务端购物车里出现某个 sku）。
Future<T> waitUntil<T>(WidgetTester t, Future<T?> Function() probe, String what,
    {Duration timeout = const Duration(seconds: 20)}) async {
  final end = DateTime.now().add(timeout);
  while (DateTime.now().isBefore(end)) {
    final v = await probe();
    if (v != null) return v;
    await t.pump(const Duration(milliseconds: 300));
  }
  throw TestFailure('等 $what 超时');
}

/// Web 上 flutter drive 只报「有 N 个异常」，不打印内容：startApp 把 FlutterError 记在这里，
/// 用例失败时连同第一个异常一起写进失败信息。
final seen = <String>[];

/// 用它代替 testWidgets。
void e2e(String name, Future<void> Function(WidgetTester t) body) {
  testWidgets(name, (t) async {
    seen.clear();
    try {
      await body(t);
    } catch (e) {
      fail('${'$e'.split('\n').take(8).join(' / ')}${seen.isEmpty ? '' : ' || FlutterError: ${seen.join(' || ')}'}');
    }
  });
}

Finder byKey(String k) => find.byKey(Key(k));

/// 列表是懒构建的：远处的元素要滚过去才会出现在树里。
Future<void> scrollTo(WidgetTester t, String k) async {
  await waitFor(t, find.byType(Scrollable));
  await t.scrollUntilVisible(byKey(k), 300, scrollable: find.byType(Scrollable).first, maxScrolls: 60);
  await t.pump();
}

/// 在「我的」页退出登录（退出按钮在列表下面，要滚过去）。
Future<void> logoutInApp(WidgetTester t) async {
  await scrollTo(t, 'me.logout');
  await t.tap(byKey('me.logout'));
  await waitFor(t, byKey('me.login'));
}

Future<void> tapKey(WidgetTester t, String k) async {
  await waitFor(t, byKey(k));
  await t.ensureVisible(byKey(k));
  await t.pump();
  await t.tap(byKey(k));
  await t.pump();
}

/// 带这个 Key 的 Text，且内容是 text（find.descendant 不含自身，所以另写一个）。
Finder keyedText(String key, String text) =>
    find.byWidgetPredicate((w) => w is Text && w.key == Key(key) && w.data == text, description: '$key = 「$text」');

/// 往输入框里打字：先滚到它、点一下拿焦点（它在首屏以外时 enterText 的那一下点击会落空，字打不进去）。
Future<void> typeInto(WidgetTester t, String key, String text) async {
  await waitFor(t, byKey(key));
  await t.ensureVisible(byKey(key));
  // Web 上：点过按钮之后框架还当这一格有焦点，但浏览器那边的输入连接已经断了 —— enterText 就不再点它、
  // 字发到一个过期的连接上被丢掉（实测第二次改运单号发出去的还是旧值）。先收掉焦点再来。
  FocusManager.instance.primaryFocus?.unfocus();
  await t.pumpAndSettle();
  await t.enterText(byKey(key), text);
  await t.pump();
}

/// 带这个 Key 的 Text，内容包含 part。
Finder keyedTextContaining(String key, String part) =>
    find.byWidgetPredicate((w) => w is Text && w.key == Key(key) && (w.data ?? '').contains(part), description: '$key ∋ 「$part」');

String textOf(String key) => (byKey(key).evaluate().first.widget as Text).data ?? '';

/// 起 App 并等首页的门店行出来。
Future<void> startApp(WidgetTester t) async {
  // 测试框架只报「有 N 个异常」，不打印内容：先把每个异常打出来，再交给框架。
  final prev = FlutterError.onError;
  FlutterError.onError = (d) {
    if (seen.length < 4) seen.add('onError: ${d.exceptionAsString().split('\n').take(4).join(' / ')} | ${d.context} | ${d.library}');
    prev?.call(d);
  };
  await app.main();
  await waitFor(t, byKey('home.store'));
}

void requireAccount() {
  if (e2ePhone.isEmpty || e2ePassword.isEmpty) {
    fail('没有 e2e 账号：用 tool/e2e_web.sh 跑（读 ~/.config/keel/e2e.env），或传 --dart-define=KEEL_E2E_PHONE/KEEL_E2E_PASSWORD');
  }
}

/// 直接打开某一页（深链），压在当前页上。
Future<void> open(WidgetTester t, String location) async {
  appRouter.push(location);
  await t.pump();
  await t.pump(const Duration(milliseconds: 400));
}

/// 「¥12.30」/「-¥8.00」-> 分。
int centsOf(String t) => (double.parse(t.replaceAll(RegExp('[-¥]'), '')) * 100).round() * (t.startsWith('-') ? -1 : 1);

/// 在 App 里登录 e2e 专用买家（已登录就不动）。结束时停在「我的」。
Future<void> loginInApp(WidgetTester t) async {
  requireAccount();
  await tapKey(t, 'tab.me');
  await t.pumpAndSettle();
  if (byKey('me.nickname').evaluate().isNotEmpty) return;
  await tapKey(t, 'me.login');
  await waitFor(t, byKey('login.phone'));
  await t.enterText(byKey('login.phone'), e2ePhone);
  await t.enterText(byKey('login.password'), e2ePassword);
  await t.tap(byKey('login.submit'));
  await waitFor(t, byKey('me.nickname'));
  await waitGone(t, byKey('login.submit'));
}

/// 测试进程自己的 HTTP：对着同一个服务端查 / 清前置状态（Web 同源，走 /api/v1 反代）。
class Api {
  static final _base = Uri.base.resolve('/api/v1').toString();
  static String? _token;

  static Future<String> token() async {
    if (_token != null) return _token!;
    final r = await http.post(Uri.parse('$_base/auth/login'),
        headers: {'Content-Type': 'application/json'}, body: jsonEncode({'phone': e2ePhone, 'password': e2ePassword}));
    if (r.statusCode != 200) fail('测试进程登录失败：${r.statusCode}（口令不对就别再跑，免得锁号）');
    return _token = (jsonDecode(utf8.decode(r.bodyBytes)) as Map)['access_token'] as String;
  }

  static Future<dynamic> call(String method, String path, {Object? body, bool auth = true}) async {
    final req = http.Request(method, Uri.parse('$_base$path'));
    if (auth) req.headers['Authorization'] = 'Bearer ${await token()}';
    if (body != null) {
      req.headers['Content-Type'] = 'application/json';
      req.body = jsonEncode(body);
    }
    final res = await http.Response.fromStream(await req.send());
    final text = utf8.decode(res.bodyBytes);
    if (res.statusCode >= 300) return {'_status': res.statusCode, if (text.isNotEmpty) '_body': jsonDecode(text)};
    return text.isEmpty ? null : jsonDecode(text);
  }

  static Future<dynamic> get(String path, {bool auth = true}) => call('GET', path, auth: auth);

  static Future<void> clearCart() => call('DELETE', '/cart');

  /// 挑一个库存够的规格；跳过限时特价 / 秒杀的（有每人限购，反复买会把额度用光）。
  static Future<({int productId, int skuId, int qty})> pickSku([int minQty = 5]) async {
    ({int productId, int skuId, int qty})? best;
    for (final p in ((await get('/products?page_size=50', auth: false)) as Map)['items'] as List) {
      if (p['status'] != 1) continue;
      final d = await get('/products/${p['id']}', auth: false) as Map;
      final first = (d['skus'] as List).cast<Map>().where((s) => (s['available_qty'] as int) > 0 && s['promo_price_cents'] == null).firstOrNull;
      if (first == null) continue;
      final q = first['available_qty'] as int;
      if (best == null || q > best.qty) best = (productId: p['id'] as int, skuId: first['id'] as int, qty: q);
    }
    if (best == null || best.qty < minQty) fail('演示库里没有库存 ≥ $minQty 的规格了，请重置演示库：$best');
    return best;
  }

  static Future<int> storeId() async =>
      ((((await get('/products?page_size=1', auth: false)) as Map)['store']) as Map)['store_id'] as int;

  static Future<List<Map>> addresses() async => ((await get('/addresses')) as List).cast<Map>();

  /// 服务端试算（拿这一单能用的券等）。
  static Future<Map> preview(int skuId, int qty, int addressId, {int? couponId}) async => (await call('POST', '/orders/preview', body: {
        'items': [{'sku_id': skuId, 'quantity': qty}], 'store_id': await storeId(), 'address_id': addressId,
        'user_coupon_id': ?couponId,
      })) as Map;

  static Future<int> makeAddress(Map<String, dynamic> fields) async {
    final r = await idem('POST', '/addresses', {'phone': '13900000000', 'detail': '1 号', ...fields});
    if (r is! Map || r['id'] == null) fail('建地址失败：$r');
    return r['id'] as int;
  }

  static Future<dynamic> idem(String method, String path, Object? body) async {
    final req = http.Request(method, Uri.parse('$_base$path'))
      ..headers['Authorization'] = 'Bearer ${await token()}'
      ..headers['Idempotency-Key'] = _uuid();
    if (body != null) {
      req.headers['Content-Type'] = 'application/json';
      req.body = jsonEncode(body);
    }
    final res = await http.Response.fromStream(await req.send());
    final text = utf8.decode(res.bodyBytes);
    if (res.statusCode >= 300) return {'_status': res.statusCode, if (text.isNotEmpty) '_body': jsonDecode(text)};
    return text.isEmpty ? null : jsonDecode(text);
  }

  static int _n = 0;
  static String _uuid() {
    final h = (DateTime.now().microsecondsSinceEpoch * 1000 + (_n++ % 1000)).toRadixString(16).padLeft(12, '0');
    return '00000000-0000-4000-8000-${h.substring(h.length - 12)}';
  }

  /// 从测试进程下一单（默认地址、默认门店），pay 时顺手付掉：发起支付 → 把服务端签好的沙箱回调原样投回去 → 等已支付。
  static Future<String> placeOrder({bool pay = false, int quantity = 1}) async {
    final sku = await pickSku(quantity + 2);
    final addrs = await addresses();
    final addr = addrs.where((a) => a['is_default'] == true).firstOrNull ?? addrs.firstOrNull;
    if (addr == null) fail('e2e 买家名下没有地址');
    final o = await idem('POST', '/orders', {
      'items': [{'sku_id': sku.skuId, 'quantity': quantity}], 'store_id': await storeId(), 'address_id': addr['id'],
    });
    if (o is! Map || o['order_no'] == null) fail('下单失败：$o');
    final no = o['order_no'] as String;
    if (!pay) return no;
    final p = await idem('POST', '/orders/$no/payments', {'channel': 'wechat'});
    final payload = p is Map ? p['payload'] as Map? : null;
    final settle = payload?['settle'] as Map?;
    if (settle == null) fail('支付响应里没有沙箱回调信封：$p');
    final w = await http.post(Uri.base.resolve(settle['url'] as String),
        headers: {for (final e in ((settle['headers'] as Map?) ?? const {}).entries) '${e.key}': '${e.value}'}, body: settle['body'] as String);
    if (w.statusCode >= 300) fail('沙箱回调失败：${w.statusCode} ${w.body}');
    for (var i = 0; i < 30; i++) {
      if (((await get('/orders/$no')) as Map)['status'] == 20) return no;
      await Future<void>.delayed(const Duration(milliseconds: 300));
    }
    fail('付款后订单没有变成已支付：$no');
  }

  /// 从测试进程传一张 1×1 的 PNG 当凭证（purpose=3），返回 /api/v1/uploads/{id}。
  static Future<String> uploadPng() async {
    const png = [137, 80, 78, 71, 13, 10, 26, 10, 0, 0, 0, 13, 73, 72, 68, 82, 0, 0, 0, 1, 0, 0, 0, 1, 8, 6, 0, 0, 0, 31, 21, 196,
      137, 0, 0, 0, 13, 73, 68, 65, 84, 120, 156, 99, 248, 207, 192, 240, 31, 0, 5, 0, 1, 255, 137, 153, 61, 29, 0, 0, 0, 0, 73, 69, 78,
      68, 174, 66, 96, 130];
    final req = http.MultipartRequest('POST', Uri.parse('$_base/uploads'))
      ..headers['Authorization'] = 'Bearer ${await token()}'
      ..headers['Idempotency-Key'] = _uuid()
      ..fields['purpose'] = '3'
      ..files.add(http.MultipartFile.fromBytes('file', png, filename: 'e2e.png', contentType: MediaType('image', 'png')));
    final res = await http.Response.fromStream(await req.send());
    if (res.statusCode != 201) fail('传凭证失败：${res.statusCode} ${res.body}');
    return (jsonDecode(res.body) as Map)['url'] as String;
  }

  static Future<List<dynamic>> cartItems() async => ((await get('/cart')) as Map)['items'] as List;
}
