import 'dart:async';
import 'dart:convert';

import 'package:fake_async/fake_async.dart';

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

  group('定位', () {
    Future<(StoreService, List<Uri>)> svc(Future<({double lat, double lng})?> Function() locate) async {
      final seen = <Uri>[];
      final c = ApiClient(base: 'http://h/api/v1', session: Session(), http: MockClient((r) async {
        seen.add(r.url);
        return http.Response(jsonEncode({'match_type': 'fence', 'stores': [{'id': 2, 'name': '西湖店', 'is_default': false, 'distance_m': 800}]}), 200,
            headers: {'content-type': 'application/json; charset=utf-8'});
      }));
      return (StoreService(c, locate: locate), seen);
    }

    test('拿到坐标：带 lat / lng 去解析', () async {
      final (s, seen) = await svc(() async => (lat: 30.25, lng: 120.13));
      expect((await s.ensure()).storeId, 2);
      expect(seen.single.queryParameters, {'lat': '30.25', 'lng': '120.13'});
    });

    test('拿不到（拒绝授权 / 出错）：不带坐标解析，不是错误', () async {
      final (s, seen) = await svc(() async => throw StateError('denied'));
      expect((await s.ensure()).storeId, 2);
      expect(seen.single.queryParameters, isEmpty);
    });

    test('定位挂着不回：最多等 5 秒就按拿不到处理', () {
      fakeAsync((fa) {
        late (StoreService, List<Uri>) pair;
        svc(() => Completer<({double lat, double lng})?>().future).then((p) => pair = p);
        fa.flushMicrotasks();
        CurrentStore? got;
        pair.$1.ensure().then((v) => got = v);
        fa.elapse(const Duration(seconds: 4));
        expect(got, isNull);
        fa.elapse(const Duration(seconds: 2));
        fa.flushMicrotasks();
        expect(got?.storeId, 2);
        expect(pair.$2.single.queryParameters, isEmpty);
      });
    });
  });
}
