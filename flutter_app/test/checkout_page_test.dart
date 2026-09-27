import 'dart:convert';

import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:http/http.dart' as http;
import 'package:http/testing.dart';
import 'package:keel_buyer/api/cart_count.dart';
import 'package:keel_buyer/api/catalog.dart';
import 'package:keel_buyer/api/client.dart';
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
  String status = '10';
  late final client = MockClient((r) async {
    final p = r.url.path.replaceFirst('/api/v1', '');
    if (p == '/stores/resolve') return j({'match_type': 'default', 'stores': [{'id': 1, 'name': '示例小店', 'is_default': true}]});
    if (p == '/products' ) return j({'page': 1, 'page_size': 20, 'total': 0, 'store': {'match_type': 'default', 'store_id': 1}, 'items': []});
    if (p == '/categories') return j([]);
    if (p == '/cart') return j({'items': [], 'store': {'match_type': 'default', 'store_id': 1}, 'total_cents': 0, 'selected_total_cents': 0, 'promotion_discount_cents': 0, 'promotions': []});
    if (p == '/products/9') {
      return j({'id': 9, 'title': '挂耳', 'min_price_cents': 5000, 'status': 1,
        'skus': [{'id': 91, 'sku_code': 'S', 'price_cents': 5000, 'available_qty': 9, 'spec_values': {'规格': '10 包'}}]});
    }
    if (p == '/addresses') {
      return j([{'id': 1, 'receiver_name': '张三', 'phone': '139', 'province': '浙江省', 'city': '杭州市', 'district': '西湖区', 'detail': '1 号', 'is_default': true}]);
    }
    if (p == '/orders/preview') {
      final b = jsonDecode(r.body) as Map;
      previews.add(b);
      final c = b['user_coupon_id'] as int?;
      final off = c == 3 ? 1000 : (c == 4 ? 500 : 0);
      return j({'store_id': 1, 'goods_amount_cents': 5000, 'freight_cents': 0, 'freight_discount_cents': 0,
        'freight': {'freight_cents': 0, 'freight_discount_cents': 0, 'groups': []}, 'payable_cents': 5000 - off,
        'promotion_discount_cents': 0, 'coupon_discount_cents': off, 'items': [], 'promotions': [],
        'user_coupon_id': ?c, 'applicable_coupons': [coupon(3, 1000), coupon(4, 500)]});
    }
    if (p == '/orders') {
      orders.add(r);
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

Future<Widget> app(Fake f) async {
  SharedPreferences.setMockInitialValues({'keel.access': 'a', 'keel.refresh': 'r', 'keel.nickname': 'e2e'});
  final session = Session();
  await session.load();
  final client = ApiClient(base: 'http://h/api/v1', session: session, http: f.client);
  final router = buildRouter(session);
  router.go('/checkout?sku_id=91&product_id=9');
  return Services(client: client, session: session, store: StoreService(client), cart: CartCount(client, session),
      trace: SearchTrace(client), child: MaterialApp.router(theme: keelTheme(), routerConfig: router));
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
}
