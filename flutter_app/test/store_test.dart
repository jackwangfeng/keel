import 'dart:convert';

import 'package:flutter_test/flutter_test.dart';
import 'package:http/http.dart' as http;
import 'package:http/testing.dart';
import 'package:keel_buyer/api/client.dart';
import 'package:keel_buyer/api/session.dart';
import 'package:keel_buyer/api/store.dart';

void main() {
  test('只解析一次；并发调用共用一次请求；none 时 storeId 为 null', () async {
    var calls = 0;
    final c = ApiClient(base: 'http://h/api/v1', session: Session(), http: MockClient((r) async {
      calls++;
      await Future<void>.delayed(const Duration(milliseconds: 10));
      return http.Response(jsonEncode({'match_type': 'default', 'stores': [
        {'id': 1, 'name': '示例小店（默认门店）', 'is_default': true, 'distance_m': null}]}), 200,
          headers: {'content-type': 'application/json; charset=utf-8'});
    }));
    final s = StoreService(c);
    final r = await Future.wait([s.ensure(), s.ensure()]);
    expect(calls, 1);
    expect(r[0].storeId, 1);
    expect(r[0].name, '示例小店（默认门店）');
    await s.ensure();
    expect(calls, 1);

    final none = StoreService(ApiClient(base: 'http://h/api/v1', session: Session(),
        http: MockClient((r) async => http.Response(jsonEncode({'match_type': 'none', 'stores': []}), 200))));
    expect((await none.ensure()).storeId, isNull);
  });
}
