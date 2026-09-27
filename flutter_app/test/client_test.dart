import 'dart:math';
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

  test('没有安全随机数源（小程序实测）：退回普通随机数，照样是 UUID v4、每次不同', () {
    Random broken() => throw UnsupportedError('No source of cryptographically secure random numbers available.');
    final keys = {for (var i = 0; i < 50; i++) newIdempotencyKey(secure: broken)};
    expect(keys.length, 50);
    for (final k in keys) {
      expect(k, matches(RegExp(r'^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$')));
    }
  });
}
