import 'dart:convert';

import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:http/http.dart' as http;
import 'package:http/testing.dart';
import 'package:keel_buyer/api/cart_count.dart';
import 'package:keel_buyer/api/catalog.dart';
import 'package:keel_buyer/api/client.dart';
import 'package:keel_buyer/api/notification.dart';
import 'package:keel_buyer/api/services.dart';
import 'package:keel_buyer/api/session.dart';
import 'package:keel_buyer/api/store.dart';
import 'package:keel_buyer/router.dart';
import 'package:keel_buyer/theme.dart';
import 'package:shared_preferences/shared_preferences.dart';

http.Response j(Object body, [int status = 200, Map<String, String> h = const {}]) => http.Response(jsonEncode(body), status,
    headers: {'content-type': 'application/json; charset=utf-8', ...h});

Map<String, dynamic> coupon(int id, int save) => {
      'id': id, 'coupon_code': 'C$id', 'template_id': 1, 'name': '券$id', 'coupon_type': 1, 'threshold_cents': 0,
      'discount_cents': save, 'discount_rate': 0, 'max_discount_cents': 0, 'status': 1, 'source': 1,
      'valid_start_at': '2026-09-01T00:00:00Z', 'valid_end_at': '2026-12-01T00:00:00Z', 'scopes': [], 'applicable_discount_cents': save,
    };

class Fake {
  final previews = <Map>[];
  final orders = <http.Request>[];
  bool replay = false;
  bool slowAddr1 = false;
  bool noCoupons = false;
  String status = '10';
  /// 地址 1 带坐标、落在默认店（1）的围栏外：按门店 1 试算 / 下单回 422 address-out-of-range；按它的坐标解析得到门店 5。
  bool fence = false;
  bool fenceOnSubmit = false;
  final resolves = <Map<String, String>>[];
  late final client = MockClient((r) async {
    final p = r.url.path.replaceFirst('/api/v1', '');
    if (p == '/stores/resolve') {
      resolves.add(r.url.queryParameters);
      if (r.url.queryParameters['lat'] == '31.23') {
        return j({'match_type': 'fence', 'stores': [{'id': 5, 'name': '人民广场店', 'is_default': false, 'distance_m': 300}]});
      }
      return j({'match_type': 'default', 'stores': [{'id': 1, 'name': '示例小店', 'is_default': true}]});
    }
    if (p == '/products' ) return j({'page': 1, 'page_size': 20, 'total': 0, 'store': {'match_type': 'default', 'store_id': 1}, 'items': []});
    if (p == '/categories') return j([]);
    if (p == '/cart') return j({'items': [], 'store': {'match_type': 'default', 'store_id': 1}, 'total_cents': 0, 'selected_total_cents': 0, 'promotion_discount_cents': 0, 'promotions': []});
    if (p == '/products/9') {
      return j({'id': 9, 'title': '挂耳', 'min_price_cents': 5000, 'status': 1,
        'skus': [{'id': 91, 'sku_code': 'S', 'price_cents': 5000, 'available_qty': 9, 'spec_values': {'规格': '10 包'}}]});
    }
    if (p == '/addresses') {
      return j([
        {'id': 1, 'receiver_name': '张三', 'phone': '139', 'province': '浙江省', 'city': '杭州市', 'district': '西湖区', 'detail': '1 号', 'is_default': true,
          if (fence) 'lat': 31.23, if (fence) 'lng': 121.47},
        {'id': 2, 'receiver_name': '李四', 'phone': '138', 'province': '新疆维吾尔自治区', 'city': '乌鲁木齐市', 'district': '天山区', 'detail': '2 号', 'is_default': false},
      ]);
    }
    if (p == '/orders/preview') {
      final b = jsonDecode(r.body) as Map;
      previews.add(b);
      if (fence && !fenceOnSubmit && b['store_id'] == 1) return outOfRange();
      if (slowAddr1 && b['address_id'] == 1) await Future<void>.delayed(const Duration(seconds: 2));
      if (((b['items'] as List).first as Map)['quantity'] == 2 && b['user_coupon_id'] == 3) {
        return j({'type': 'https://keel.dev/problems/coupon-not-applicable', 'title': '这张优惠券本单不可用', 'status': 409,
          'detail': '这张优惠券本单不可用: 包邮券抵不了钱'}, 409);
      }
      final freight = b['address_id'] == 2 ? 1500 : 0;
      final c = b['user_coupon_id'] as int?;
      final off = c == 3 ? 1000 : (c == 4 ? 500 : 0);
      return j({'store_id': 1, 'goods_amount_cents': 5000, 'freight_cents': freight, 'freight_discount_cents': 0,
        'freight': {'freight_cents': freight, 'freight_discount_cents': 0, 'groups': []}, 'payable_cents': 5000 - off,
        'promotion_discount_cents': 0, 'coupon_discount_cents': off, 'items': [], 'promotions': [],
        'user_coupon_id': ?c, 'applicable_coupons': noCoupons ? [] : [coupon(3, 1000), coupon(4, 500)]});
    }
    if (p == '/orders') {
      orders.add(r);
      if (fenceOnSubmit && (jsonDecode(r.body) as Map)['store_id'] == 1) return outOfRange();
      return j({'order_no': 'N100', 'store_id': 1, 'status': 10, 'refund_status': 0, 'payable_cents': 4000, 'created_at': '2026-09-26T00:05:12Z'},
          201, replay ? {'idempotency-replayed': 'true'} : {});
    }
    if (p == '/orders/N100') {
      return j({'order_no': 'N100', 'store_id': 1, 'status': int.parse(status), 'refund_status': 0, 'payable_cents': 4000,
        'created_at': '2026-09-26T00:05:12Z', 'items': []});
    }
    if (p == '/orders/N100/payments') {
      return j({'payment_no': 'P1', 'channel': 'wechat', 'amount_cents': 4000, 'payload': {'sandbox': true, 'sandbox_notice': '演示环境',
        'settle': {'method': 'POST', 'url': '/api/v1/webhooks/payments/wechat', 'headers': {'X-Sign': 's'}, 'body': '{}'}}}, 201);
    }
    if (p == '/webhooks/payments/wechat') {
      status = '20';
      return http.Response('', 204);
    }
    return j({'type': 'x', 'title': 'nope $p', 'status': 404}, 404);
  });
}

http.Response outOfRange() => j({'type': 'https://keel.dev/problems/address-out-of-range', 'title': '收货地址不在门店配送范围',
      'status': 422, 'detail': '收货地址不在「示例小店」的配送范围内'}, 422);

Future<Widget> app(Fake f) async {
  SharedPreferences.setMockInitialValues({'keel.access': 'a', 'keel.refresh': 'r', 'keel.nickname': 'e2e'});
  final session = Session();
  await session.load();
  final client = ApiClient(base: 'http://h/api/v1', session: session, http: f.client);
  final router = buildRouter(session);
  router.go('/checkout?sku_id=91&product_id=9');
  return Services(client: client, session: session, store: StoreService(client, locate: () async => null), cart: CartCount(client, session),
      trace: SearchTrace(client), unread: UnreadCount(client, session), child: MaterialApp.router(theme: keelTheme(), routerConfig: router));
}

void phone(WidgetTester t) {
  t.view.physicalSize = const Size(1170, 2532);
  t.view.devicePixelRatio = 3;
  addTearDown(t.view.reset);
}

void main() {
  testWidgets('自动选最省的券只一次；「不使用」之后不会被改回去', (t) async {
    phone(t);
    final f = Fake();
    await t.pumpWidget(await app(f));
    await t.pumpAndSettle();
    expect(f.previews.map((b) => b['user_coupon_id']), [null, 3], reason: '先不带券拿列表，再带最省的那张');
    expect(t.widget<Text>(find.byKey(const Key('checkout.payable'))).data, '¥40.00');
    expect(find.text('券3 -¥10.00'), findsOneWidget);
    await t.tap(find.byKey(const Key('checkout.coupons')));
    await t.pumpAndSettle();
    await t.tap(find.byKey(const Key('checkout.coupon.none')));
    await t.pumpAndSettle();
    expect(f.previews.last.containsKey('user_coupon_id'), isFalse);
    await t.tap(find.byKey(const Key('checkout.plus')));
    await t.pumpAndSettle();
    expect(f.previews.last.containsKey('user_coupon_id'), isFalse, reason: '改数量重算也不把券选回来');
    expect(f.previews.last['items'], [{'sku_id': 91, 'quantity': 2}]);
  });

  testWidgets('提交：带试算的应付与同一个幂等键；新单跳详情，发起沙箱支付后底栏变「模拟支付完成」，投递后变已支付', (t) async {
    phone(t);
    final f = Fake();
    await t.pumpWidget(await app(f));
    await t.pumpAndSettle();
    await t.tap(find.byKey(const Key('checkout.submit')));
    await t.pumpAndSettle();
    final body = jsonDecode(f.orders.single.body) as Map;
    expect(body['expected_payable_cents'], 4000);
    expect(body['user_coupon_id'], 3);
    expect(body['address_id'], 1);
    expect(find.byKey(const Key('order.status')), findsOneWidget);
    expect(t.widget<Text>(find.byKey(const Key('order.status'))).data, '待支付');
    await t.tap(find.byKey(const Key('order.pay')));
    await t.pumpAndSettle();
    expect(find.byKey(const Key('order.sandbox')), findsOneWidget);
    await t.tap(find.byKey(const Key('order.settle')));
    await t.pumpAndSettle();
    expect(t.widget<Text>(find.byKey(const Key('order.status'))).data, '已支付');
    expect(find.byKey(const Key('order.pay')), findsNothing);
  });

  testWidgets('重放：不跳转，写明之前已提交过', (t) async {
    phone(t);
    final f = Fake()..replay = true;
    await t.pumpWidget(await app(f));
    await t.pumpAndSettle();
    await t.tap(find.byKey(const Key('checkout.submit')));
    await t.pumpAndSettle();
    expect(find.byKey(const Key('order.status')), findsNothing);
    expect(find.textContaining('之前已经提交过'), findsOneWidget);
    expect(find.byKey(const Key('checkout.viewOrder')), findsOneWidget);
  });

  testWidgets('#5 地址 1 的试算还在路上时换成地址 2：只认地址 2 的结果', (t) async {
    phone(t);
    final f = Fake()
      ..slowAddr1 = true
      ..noCoupons = true;
    await t.pumpWidget(await app(f));
    await t.pump();
    await t.pump(const Duration(milliseconds: 100));
    await t.tap(find.byKey(const Key('checkout.address')));
    await t.pumpAndSettle(const Duration(milliseconds: 50), EnginePhase.sendSemanticsUpdate, const Duration(seconds: 1));
    await t.tap(find.byKey(const Key('address.row.2')));
    await t.pumpAndSettle();
    await t.pump(const Duration(seconds: 3));
    await t.pumpAndSettle();
    expect(f.previews.last['address_id'], 2);
    expect(t.widget<Text>(find.byKey(const Key('checkout.freight'))).data, '¥15.00');
    expect(find.byKey(const Key('checkout.address.2')), findsOneWidget);
  });

  testWidgets('选着的券数量加了之后用不了：照实说原因、展开券列表、不给应付', (t) async {
    phone(t);
    final f = Fake();
    await t.pumpWidget(await app(f));
    await t.pumpAndSettle();
    await t.tap(find.byKey(const Key('checkout.plus')));
    await t.pumpAndSettle();
    expect(find.byKey(const Key('checkout.message')), findsOneWidget);
    expect(find.textContaining('包邮券抵不了钱'), findsOneWidget);
    expect(find.byKey(const Key('checkout.coupon.none')), findsOneWidget);
    expect(t.widget<Text>(find.byKey(const Key('checkout.payable'))).data, '—');
  });

  testWidgets('地址在门店围栏外（试算 422 address-out-of-range）：照实说，按这条地址换门店后重新试算', (t) async {
    phone(t);
    final f = Fake()..fence = true;
    await t.pumpWidget(await app(f));
    await t.pumpAndSettle();
    expect(t.widget<Text>(find.byKey(const Key('checkout.message'))).data, '地址不在这家门店配送范围');
    expect(find.byKey(const Key('checkout.changeAddress')), findsOneWidget);
    await t.tap(find.byKey(const Key('checkout.switchStore')));
    await t.pumpAndSettle();
    expect(f.resolves.last, {'lat': '31.23', 'lng': '121.47'});
    expect(f.previews.last['store_id'], 5);
    expect(find.byKey(const Key('checkout.switchStore')), findsNothing);
    expect(find.byKey(const Key('checkout.payable')), findsOneWidget);
  });

  testWidgets('提交时才发现在围栏外：这单没建成，换键；同样给两个出路', (t) async {
    phone(t);
    final f = Fake()
      ..fence = true
      ..fenceOnSubmit = true;
    await t.pumpWidget(await app(f));
    await t.pumpAndSettle();
    await t.tap(find.byKey(const Key('checkout.submit')));
    await t.pumpAndSettle();
    expect(t.widget<Text>(find.byKey(const Key('checkout.message'))).data, '地址不在这家门店配送范围');
    await t.tap(find.byKey(const Key('checkout.switchStore')));
    await t.pumpAndSettle();
    await t.tap(find.byKey(const Key('checkout.submit')));
    await t.pumpAndSettle();
    final keys = f.orders.map((r) => r.headers['Idempotency-Key']).toList();
    expect(keys, hasLength(2));
    expect(keys[0], isNot(keys[1]));
    expect((jsonDecode(f.orders.last.body) as Map)['store_id'], 5);
  });
}
