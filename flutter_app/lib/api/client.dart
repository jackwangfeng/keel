import 'dart:async';
import 'dart:convert';
import 'dart:math';

import 'package:http/http.dart' as http;
import 'package:http_parser/http_parser.dart';

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

  /// 问题类型的最后一段，例如 'insufficient-stock'。
  bool isType(String slug) => (problem?.type ?? '').endsWith('/$slug');

  /// 同一个失败换一句给人看的话（问题类型翻成页面上的说法）。
  ApiFailure withMessage(String m) => ApiFailure(status, problem, m, retryAfter: retryAfter, fieldErrors: fieldErrors);

  @override
  String toString() => 'ApiFailure($status, $message)';
}

class ApiResult<T> {
  final T data;
  final bool replayed;
  const ApiResult(this.data, this.replayed);
}

/// 随机 UUID v4。幂等键由页面生成并持有：同一次操作重试复用，内容变了换键。
///
/// 优先用安全随机数；拿不到（mp-flutter 编的小程序里实测没有 crypto 源：`Random.secure()` 直接抛
/// UnsupportedError，结算 / 订单详情一打开就崩）就退回普通随机数 —— 幂等键要的是不撞，不是防猜：
/// 用微秒时间 + 自增序号做种子，两次调用不会拿到同一个序列。
String newIdempotencyKey({Random Function() secure = Random.secure}) {
  Random r;
  try {
    r = secure();
  } on UnsupportedError {
    r = Random(DateTime.now().microsecondsSinceEpoch ^ (++_keySeq * 0x9E3779B1));
  }
  final b = List<int>.generate(16, (_) => r.nextInt(256));
  b[6] = (b[6] & 0x0f) | 0x40;
  b[8] = (b[8] & 0x3f) | 0x80;
  String h(int i) => b[i].toRadixString(16).padLeft(2, '0');
  final s = List.generate(16, h).join();
  return '${s.substring(0, 8)}-${s.substring(8, 12)}-${s.substring(12, 16)}-${s.substring(16, 20)}-${s.substring(20)}';
}

int _keySeq = 0;

/// 唯一的网络出口。
class ApiClient {
  ApiClient({required this.base, required this.session, http.Client? http, this.onSessionExpired,
      this.timeout = const Duration(seconds: 30)})
      : _http = http ?? _defaultClient();

  /// 单个请求最多等多久：连接挂着不回时，页面要能停下「提交中…」、让人用同一个幂等键重试。
  final Duration timeout;

  /// 服务地址。「服务地址」页可以换（换了就是换一家店）。
  String base;
  final Session session;
  final http.Client _http;

  /// 刷新也救不回来时调一次（跳登录页）。同一轮并发失败只调一次。
  final void Function()? onSessionExpired;

  Completer<bool>? _refreshing;

  static http.Client _defaultClient() => http.Client();

  Future<ApiResult<T>> send<T>(String method, String path,
      {Map<String, String>? query, Object? body, String? idempotencyKey, required T Function(dynamic json) decode}) async =>
      _finish(await _withRefresh(path, () => _raw(method, path, query, body, idempotencyKey)), decode);

  /// 网络层的失败（断网、DNS、连接被拒、超时、小程序 wx.request fail）一律变成 status 0 的 ApiFailure：
  /// 页面只接 ApiFailure，漏出去的原始异常会让页面停在「提交中…」「正在加载…」。
  Future<http.Response> _net(Future<http.Response> Function() attempt) async {
    try {
      return await attempt().timeout(timeout);
    } on TimeoutException {
      throw const ApiFailure(0, null, '网络超时，请检查网络后重试');
    } on ApiFailure {
      rethrow;
    } catch (e) {
      throw ApiFailure(0, null, '网络异常，请检查网络后重试（$e）');
    }
  }

  /// 给自己发请求的调用方（小程序的 wx.uploadFile）用：同一套 401 单飞续期 + 同键重放。
  Future<http.Response> withRefresh(String path, Future<http.Response> Function() attempt) => _withRefresh(path, attempt);

  /// 401 且登着录：单飞刷新一次，再用同一个请求（同一个幂等键 —— 第一次既然是 401，服务端什么都没做）重放。
  Future<http.Response> _withRefresh(String path, Future<http.Response> Function() raw) async {
    Future<http.Response> attempt() => _net(raw);
    final res = await attempt();
    if (res.statusCode != 401 || !session.loggedIn || path.startsWith('/auth/')) return res;
    if (!await _refresh()) throw _expired();
    final again = await attempt();
    if (again.statusCode == 401) {
      await _giveUp();
      throw _expired();
    }
    return again;
  }

  /// multipart 上传（售后凭证：POST /uploads，purpose=3）。要令牌和幂等键；过期同样单飞刷新后重传。
  Future<ApiResult<T>> upload<T>(String path, {required List<int> bytes, required String filename, required String contentType,
      required Map<String, String> fields, required String idempotencyKey, required T Function(dynamic json) decode}) async {
    Future<http.Response> attempt() async {
      final req = http.MultipartRequest('POST', Uri.parse('$base$path'))
        ..headers['Accept'] = 'application/json, application/problem+json'
        ..headers['Idempotency-Key'] = idempotencyKey
        ..fields.addAll(fields)
        ..files.add(http.MultipartFile.fromBytes('file', bytes, filename: filename, contentType: _mediaType(contentType)));
      final t = session.accessToken;
      if (t != null && t.isNotEmpty) req.headers['Authorization'] = 'Bearer $t';
      return http.Response.fromStream(await _http.send(req));
    }

    return _finish(await _withRefresh(path, attempt), decode);
  }

  /// 带令牌读一个资源的字节（本人的凭证图：GET /uploads/{id} 本人 302 到限时地址，匿名 403）。url 是服务端给的路径。
  Future<List<int>> bytesOf(String url) async {
    Future<http.Response> attempt() {
      final req = http.Request('GET', Uri.parse(assetUrl(url)));
      final t = session.accessToken;
      if (t != null && t.isNotEmpty) req.headers['Authorization'] = 'Bearer $t';
      return _http.send(req).then(http.Response.fromStream);
    }

    final res = await _withRefresh(url, attempt);
    if (res.statusCode >= 200 && res.statusCode < 300) return res.bodyBytes;
    throw _failure(res.statusCode, utf8.decode(res.bodyBytes, allowMalformed: true), null);
  }

  /// 当前的访问令牌（小程序里用 wx.uploadFile 上传时要自己带）。
  String? get accessToken => session.accessToken;

  /// 公开的「发了就不管」接口（搜索回传）：不带令牌，错误一律吞掉。
  Future<void> fireAndForget(String path, Object body) async {
    try {
      await _http.post(Uri.parse('$base$path'),
          headers: {'Content-Type': 'application/json'}, body: jsonEncode(body));
    } catch (_) {}
  }

  /// 原样 POST（沙箱回调）：头与报文体由调用方给，不加令牌、不加 Accept。
  Future<({int status, String text})> postRaw(String url, Map<String, String> headers, String body) async {
    final res = await _http.post(Uri.parse(url), headers: headers, body: body);
    return (status: res.statusCode, text: utf8.decode(res.bodyBytes));
  }

  ApiFailure failureOf(int status, String text) => _failure(status, text, null);

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
      try {
        return ApiResult(decode(text.isEmpty ? null : jsonDecode(text)), replayed);
      } catch (e) {
        // 2xx 但响应体不是契约里的形状（网关错页、半截响应）：照实说，不让解析异常漏出去。
        throw ApiFailure(0, null, '服务器返回的内容读不懂（HTTP ${res.statusCode}）');
      }
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

  static MediaType? _mediaType(String v) {
    try {
      return MediaType.parse(v);
    } catch (_) {
      return null;
    }
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
