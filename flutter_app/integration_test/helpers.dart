import 'dart:convert';

import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:http/http.dart' as http;
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

Finder byKey(String k) => find.byKey(Key(k));

/// 列表是懒构建的：远处的元素要滚过去才会出现在树里。
Future<void> scrollTo(WidgetTester t, String k) async {
  await waitFor(t, find.byType(Scrollable));
  await t.scrollUntilVisible(byKey(k), 300, scrollable: find.byType(Scrollable).first, maxScrolls: 60);
  await t.pump();
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

String textOf(String key) => (byKey(key).evaluate().first.widget as Text).data ?? '';

/// 起 App 并等首页的门店行出来。
Future<void> startApp(WidgetTester t) async {
  await app.main();
  await waitFor(t, byKey('home.store'));
}

void requireAccount() {
  if (e2ePhone.isEmpty || e2ePassword.isEmpty) {
    fail('没有 e2e 账号：用 tool/e2e_web.sh 跑（读 ~/.config/keel/e2e.env），或传 --dart-define=KEEL_E2E_PHONE/KEEL_E2E_PASSWORD');
  }
}

/// 在 App 里登录 e2e 专用买家（已登录就不动）。结束时停在「我的」。
Future<void> loginInApp(WidgetTester t) async {
  requireAccount();
  await tapKey(t, 'tab.me');
  await t.pumpAndSettle();
  if (byKey('me.logout').evaluate().isNotEmpty) return;
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

  static Future<List<dynamic>> cartItems() async => ((await get('/cart')) as Map)['items'] as List;
}
