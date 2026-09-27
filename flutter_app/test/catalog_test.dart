import 'dart:convert';

import 'package:flutter_test/flutter_test.dart';
import 'package:http/http.dart' as http;
import 'package:http/testing.dart';
import 'package:keel_buyer/api/catalog.dart';
import 'package:keel_buyer/api/client.dart';
import 'package:keel_buyer/api/schema.g.dart';
import 'package:keel_buyer/api/session.dart';
import 'package:keel_buyer/api/view.dart';
import 'package:shared_preferences/shared_preferences.dart';

http.Response j(Object body, [int status = 200]) => http.Response(jsonEncode(body), status,
    headers: {'content-type': 'application/json; charset=utf-8'});

Map<String, dynamic> product({int id = 7, int? max, bool? inStock, List<Map<String, dynamic>>? skus}) => {
      'id': id, 'title': '手冲壶', 'min_price_cents': 1990, 'max_price_cents': ?max, 'in_stock': ?inStock, 'status': 1,
      'skus': skus ?? [],
    };

Map<String, dynamic> sku(int id, int qty, {int price = 1990, int? promo, Map<String, String>? spec}) => {
      'id': id, 'sku_code': 'S$id', 'price_cents': price, 'available_qty': qty, 'promo_price_cents': ?promo,
      'spec_values': ?spec,
    };

ProductRow productRowOf(Map<String, dynamic> m) => productRow(ProductSummary.fromJson(m), (s) => s);
ProductDetailView detailOf(Map<String, dynamic> m) => productDetailView(ProductDetail.fromJson(m), (s) => s);

Future<ApiClient> client(MockClient fake) async {
  SharedPreferences.setMockInitialValues({});
  final s = Session();
  await s.load();
  return ApiClient(base: 'http://h/api/v1', session: s, http: fake);
}

void main() {
  group('商品行', () {
    test('多规格：完整区间 + 最低价（首页显示「起」）', () {
      final r = productRowOf(product(max: 2990));
      expect(r.priceText, '¥19.90 ~ ¥29.90');
      expect(r.minPriceText, '¥19.90');
      expect(r.hasRange, isTrue);
    });
    test('缺货只认 in_stock == false；字段缺席不敢说没货', () {
      expect(productRowOf(product(inStock: false)).soldOut, isTrue);
      expect(productRowOf(product()).soldOut, isFalse);
    });
  });

  group('详情', () {
    test('有货的规格在前，无货的排后，各自保持原顺序', () {
      final d = detailOf(product(skus: [sku(1, 0), sku(2, 5), sku(3, 0), sku(4, 1)]));
      expect(d.skus.map((s) => s.id), [2, 4, 1, 3]);
      expect(d.skus[2].soldOut, isTrue);
    });
    test('限时特价：成交价是特价，门店价划线', () {
      final s = detailOf(product(skus: [sku(1, 3, price: 2000, promo: 1500)])).skus.single;
      expect(s.priceText, '¥15.00');
      expect(s.listPriceText, '¥20.00');
    });
    test('规格名 / 值；没有规格值退回 sku_code', () {
      final d = detailOf(product(skus: [sku(1, 3, spec: {'烘焙': '中度'}), sku(2, 3)]));
      expect(d.skus[0].label, '中度');
      expect(d.skus[0].specName, '烘焙');
      expect(d.skus[1].label, 'S2');
      expect(d.skus[1].specName, '规格');
    });
  });

  group('加购', () {
    test('成功返回车里总件数', () async {
      final c = await client(MockClient((r) async {
        expect(r.headers['Idempotency-Key'], isNotEmpty);
        expect(r.url.queryParameters['store_id'], '1');
        return j({'items': [
          for (final (id, q) in [(1, 2), (2, 3)])
            {'id': id, 'sku_id': id, 'quantity': q, 'selected': true, 'available': true, 'status': 'ok'}
        ], 'store': {'match_type': 'default', 'store_id': 1},
          'total_cents': 0, 'selected_total_cents': 0, 'promotion_discount_cents': 0, 'promotions': []});
      }));
      expect(await addToCart(c, skuId: 9, quantity: 1, storeId: 1), 5);
    });
    test('三种问题翻成人话', () async {
      for (final (type, msg) in [
        ('sku-not-sold-in-store', '当前门店不卖这个规格'),
        ('cart-quantity-exceeded', '购物车里这个规格已经到上限了'),
        ('insufficient-stock', '库存不够了'),
      ]) {
        final c = await client(MockClient((r) async =>
            j({'type': 'https://keel.dev/problems/$type', 'title': 'x', 'status': 422}, 422)));
        await expectLater(addToCart(c, skuId: 9, quantity: 1, storeId: 1),
            throwsA(isA<ApiFailure>().having((f) => f.message, 'message', msg)));
      }
    });
  });

  group('搜索归因', () {
    late List<Map<String, dynamic>> sent;
    late SearchTrace trace;
    setUp(() async {
      sent = [];
      final c = await client(MockClient((r) async {
        if (r.url.path.endsWith('/search/events')) sent.add(jsonDecode(r.body) as Map<String, dynamic>);
        return http.Response('', 204);
      }));
      trace = SearchTrace(c);
    });

    test('trace_id 缺席：点击、加购都不报', () async {
      await trace.clicked('', 7);
      await trace.addedFromList('', 7);
      await trace.converted('add_cart', 7);
      expect(sent, isEmpty);
    });
    test('点进去回传 click，之后加购带同一个 trace', () async {
      await trace.clicked('t1', 7);
      await trace.converted('add_cart', 7);
      expect(sent.map((e) => '${e['trace_id']}/${e['event']}/${e['product_id']}'), ['t1/click/7', 't1/add_cart/7']);
    });
    test('列表里原地加购：只报 add_cart，不补 click；下单归到同一次', () async {
      await trace.addedFromList('t2', 8);
      await trace.converted('order', 8);
      expect(sent.map((e) => '${e['trace_id']}/${e['event']}'), ['t2/add_cart', 't2/order']);
    });
    test('不是从搜索来的商品不报', () async {
      await trace.converted('order', 99);
      expect(sent, isEmpty);
    });
    test('最近一次为准；最多记 50 件', () async {
      await trace.clicked('old', 1);
      await trace.clicked('new', 1);
      for (var i = 100; i < 150; i++) {
        await trace.clicked('x', i);
      }
      sent.clear();
      await trace.converted('order', 1);
      expect(sent, isEmpty, reason: '第 1 件被挤出去了');
      await trace.converted('order', 149);
      expect(sent.single['trace_id'], 'x');
    });
  });
}
