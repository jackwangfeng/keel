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
