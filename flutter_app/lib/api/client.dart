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
