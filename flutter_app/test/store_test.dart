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

  group('送货位置（POI）', () {
    (StoreService, List<Uri>) svc({Future<({double lat, double lng})?> Function()? locate, Future<String> Function(double, double)? reverse}) {
      final seen = <Uri>[];
      final c = ApiClient(base: 'http://h/api/v1', session: Session(), http: MockClient((r) async {
        seen.add(r.url);
        return http.Response(jsonEncode({'match_type': 'fence', 'stores': [{'id': 2, 'name': '西湖店', 'is_default': false, 'distance_m': 800}]}), 200,
            headers: {'content-type': 'application/json; charset=utf-8'});
      }));
      return (StoreService(c, locate: locate ?? () async => (lat: 30.25, lng: 120.13), reverse: reverse), seen);
    }

    test('定位到了：门店照旧按坐标解析，地址名随后补上（逆地理编码）', () async {
      final asked = <(double, double)>[];
      final (s, _) = svc(reverse: (lat, lng) async {
        asked.add((lat, lng));
        return '黄龙时代广场';
      });
      var notified = 0;
      s.addListener(() => notified++);
      await s.ensure();
      await Future<void>.delayed(Duration.zero);
      expect(asked, [(30.25, 120.13)]);
      expect(s.placeLabel, '黄龙时代广场');
      expect(notified, greaterThanOrEqualTo(2), reason: '门店一次、地址名一次');
    });

    test('逆地理编码失败（501 没配服务商 / 503）：地址名空着，门店不受影响', () async {
      final (s, _) = svc(reverse: (_, _) async => throw const ApiFailure(501, null, 'not-implemented'));
      expect((await s.ensure()).storeId, 2);
      await Future<void>.delayed(Duration.zero);
      expect(s.placeLabel, isEmpty);
    });

    test('没坐标（拒绝定位）：不去问地址名', () async {
      var asked = 0;
      final (s, _) = svc(locate: () async => null, reverse: (_, _) async {
        asked++;
        return 'x';
      });
      await s.ensure();
      await Future<void>.delayed(Duration.zero);
      expect(asked, 0);
      expect(s.placeLabel, isEmpty);
    });

    test('换地址：用它的坐标重新解析门店，地址名就用选中的那个，不再定位', () async {
      var located = 0;
      final (s, seen) = svc(locate: () async {
        located++;
        return (lat: 30.25, lng: 120.13);
      }, reverse: (_, _) async => '定位处');
      await s.ensure();
      s.deliverTo(at: (lat: 31.23, lng: 121.47), label: '人民广场');
      expect(s.current, isNull, reason: '作废当前门店，首页听到后重拉');
      expect(s.placeLabel, '人民广场');
      await s.ensure();
      expect(located, 1);
      expect(seen.last.queryParameters, {'lat': '31.23', 'lng': '121.47'});
      expect(s.placeLabel, '人民广场');
    });

    test('换成一条没有坐标的地址：不带坐标解析（默认店），不回头去定位', () async {
      var located = 0;
      final (s, seen) = svc(locate: () async {
        located++;
        return (lat: 30.25, lng: 120.13);
      });
      s.deliverTo(at: null, label: '文三路 1 号');
      await s.ensure();
      expect(located, 0);
      expect(seen.single.queryParameters, isEmpty);
    });

    test('改回「用当前定位」：重新定位', () async {
      var located = 0;
      final (s, seen) = svc(locate: () async {
        located++;
        return (lat: 30.25, lng: 120.13);
      }, reverse: (_, _) async => '定位处');
      s.deliverTo(at: (lat: 31.23, lng: 121.47), label: '人民广场');
      await s.ensure();
      s.useDeviceLocation();
      expect(s.placeLabel, isEmpty);
      await s.ensure();
      expect(located, 1);
      expect(seen.last.queryParameters, {'lat': '30.25', 'lng': '120.13'});
    });
  });
}
