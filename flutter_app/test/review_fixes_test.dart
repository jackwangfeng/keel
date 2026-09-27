import 'dart:async';
import 'dart:convert';

import 'package:flutter_test/flutter_test.dart';
import 'package:http/http.dart' as http;
import 'package:http/testing.dart';
import 'package:keel_buyer/api/client.dart';
import 'package:keel_buyer/api/evidence.dart';
import 'package:keel_buyer/api/session.dart';
import 'package:mp_flutter_wechat/mp_flutter_wechat.dart';
import 'package:shared_preferences/shared_preferences.dart';

Future<Session> session() async {
  SharedPreferences.setMockInitialValues({'keel.access': 'old', 'keel.refresh': 'r1'});
  final s = Session();
  await s.load();
  return s;
}

class FakeMp implements MpWechatChannel {
  final uploads = <Map>[];
  @override
  bool get isAvailable => true;
  @override
  void setShareInfo(String json) {}
  @override
  Future<String?> menuButtonRect() async => null;
  @override
  Future<String> call(String api, String paramsJson) async {
    final p = jsonDecode(paramsJson) as Map;
    uploads.add(p);
    final auth = (p['header'] as Map)['Authorization'];
    if (auth == 'Bearer old') return jsonEncode({'statusCode': 401, 'data': jsonEncode({'type': 'x/token-expired', 'title': '过期', 'status': 401})});
    return jsonEncode({'statusCode': 201, 'data': jsonEncode({'id': 5, 'url': '/api/v1/uploads/5', 'content_type': 'image/png', 'size_bytes': 1, 'created_at': 'x'})});
  }
}

void main() {
  test('#1 网络断了（连接异常）变成 status 0 的 ApiFailure，不是往外抛原始异常', () async {
    final c = ApiClient(base: 'http://h/api/v1', session: await session(),
        http: MockClient((r) async => throw http.ClientException('Connection refused')));
    await expectLater(c.send('GET', '/products', decode: (j) => j),
        throwsA(isA<ApiFailure>().having((f) => f.status, 'status', 0)));
  });

  test('#1 响应体解不开也是 ApiFailure', () async {
    final c = ApiClient(base: 'http://h/api/v1', session: await session(),
        http: MockClient((r) async => http.Response('<html>oops', 200)));
    await expectLater(c.send('GET', '/products', decode: (j) => j),
        throwsA(isA<ApiFailure>().having((f) => f.status, 'status', 0)));
  });

  test('#1 请求挂着不回：超时变成 ApiFailure', () async {
    final c = ApiClient(base: 'http://h/api/v1', session: await session(), timeout: const Duration(milliseconds: 50),
        http: MockClient((r) => Completer<http.Response>().future));
    await expectLater(c.send('GET', '/products', decode: (j) => j),
        throwsA(isA<ApiFailure>().having((f) => f.status, 'status', 0)));
  });

  test('#2 小程序里传凭证遇到 401：单飞续期后用同一个幂等键重传', () async {
    final mp = FakeMp();
    MpWechat.debugSetChannel(mp);
    addTearDown(() => MpWechat.debugSetChannel(null));
    final s = await session();
    final c = ApiClient(base: 'http://h/api/v1', session: s, http: MockClient((r) async {
      expect(r.url.path, '/api/v1/auth/refresh');
      return http.Response(jsonEncode({'access_token': 'new', 'refresh_token': 'r2', 'token_type': 'Bearer', 'expires_in': 7200,
        'user': {'id': 2, 'nickname': 'x'}}), 200, headers: {'content-type': 'application/json'});
    }));
    final url = await uploadEvidence(c, const PickedImage(mpPath: 'wxfile://tmp/a.png', name: 'a.png', contentType: 'image/png'), 'k1');
    expect(url, '/api/v1/uploads/5');
    expect(mp.uploads.length, 2);
    expect(mp.uploads.map((u) => (u['header'] as Map)['Idempotency-Key']).toSet(), {'k1'});
    expect((mp.uploads.last['header'] as Map)['Authorization'], 'Bearer new');
  });
}
