import 'dart:convert';

import 'package:flutter_test/flutter_test.dart';
import 'package:http/http.dart' as http;
import 'package:http/testing.dart';
import 'package:keel_buyer/api/cart.dart';
import 'package:keel_buyer/api/client.dart';
import 'package:keel_buyer/api/schema.g.dart';
import 'package:keel_buyer/api/session.dart';
import 'package:shared_preferences/shared_preferences.dart';

Map<String, dynamic> item(int id, {String status = 'available', bool selected = true, int? price = 1000, int? list,
        int qty = 1, Map<String, dynamic>? undeliverable}) =>
    {'id': id, 'sku_id': 100 + id, 'product_id': 10 + id, 'title': '商品$id', 'spec_values': {'容量': '900ml', '烘焙': '中度'},
      'price_cents': price, 'list_price_cents': list, 'quantity': qty, 'selected': selected,
      'available': status == 'available', 'status': status, 'undeliverable': ?undeliverable};

Map<String, dynamic> cart(List<Map<String, dynamic>> items, {int selectedTotal = 0, Map<String, dynamic>? freight,
        int discount = 0, List<Map<String, dynamic>> promos = const []}) =>
    {'items': items, 'store': {'match_type': 'default', 'store_id': 1}, 'total_cents': 0, 'selected_total_cents': selectedTotal,
      'promotion_discount_cents': discount, 'promotions': promos, 'freight': ?freight};

CartView view(Map<String, dynamic> m) => cartView(Cart.fromJson(m), (s) => s);

Map<String, dynamic> freight({String? free, int threshold = 9900, int freeQty = 0}) => {
      'freight_cents': free == null ? 800 : 0, 'freight_discount_cents': 0,
      'groups': [
        {'sku_ids': [101], 'units': 1, 'fee_cents': 800, 'free_reason': ?free,
          'rule': {'region_codes': [], 'first_unit': 1, 'first_fee_cents': 1500, 'additional_unit': 1,
            'additional_fee_cents': 500, 'free_threshold_cents': threshold, 'free_quantity': freeQty}}
      ],
    };

void main() {
  test('不能买的行：写原因、不显示为勾选（服务端的 selected 可能还是 true）', () {
    final v = view(cart([item(1), item(2, status: 'out_of_stock'), item(3, status: 'not_sold_in_store', price: null)]));
    expect(v.rows[1].selected, isFalse);
    expect(v.rows[1].statusText, '暂时无货');
    expect(v.rows[2].statusText, '当前门店不售');
    expect(v.rows[2].priceText, '—', reason: '价格是 null 不显示 ¥0.00');
    expect(v.availableCount, 1);
    expect(v.selectedCount, 1);
    expect(v.allSelected, isTrue);
  });

  test('规格值拼接；活动价低于门店价时划线门店价', () {
    final r = view(cart([item(1, price: 800, list: 1000)])).rows.single;
    expect(r.specText, '900ml / 中度');
    expect(r.priceText, '¥8.00');
    expect(r.listPriceText, '¥10.00');
  });

  test('合计用 selected_total_cents；没有 freight 时运费是空串（算不了，不是包邮）', () {
    final v = view(cart([item(1)], selectedTotal: 4200));
    expect(v.selectedText, '¥42.00');
    expect(v.freightText, '');
  });

  test('运费说明：已满包邮 / 满件包邮 / 满额门槛 / 首件续件', () {
    expect(view(cart([item(1)], freight: freight(free: 'threshold'))).freightNote, '已满¥99 包邮');
    expect(view(cart([item(1)], freight: freight(free: 'quantity', freeQty: 3))).freightNote, '已满 3 件包邮');
    expect(view(cart([item(1)], freight: freight())).freightNote, '满¥99 包邮');
    expect(view(cart([item(1)], freight: freight(threshold: 0))).freightNote, '首件¥15，续件¥5');
    expect(view(cart([item(1)], freight: freight())).freightText, '¥8.00');
  });

  test('凑单提示用服务端 message 原样，空 message 跳过；满减显示成负数', () {
    final v = view(cart([item(1)], discount: 2000, promos: [
      {'promotion_id': 1, 'name': 'a', 'promotion_type': 1, 'applied': false, 'discount_cents': 0, 'sku_ids': [], 'message': '再买 ¥161 可减 50'},
      {'promotion_id': 2, 'name': 'b', 'promotion_type': 1, 'applied': true, 'discount_cents': 2000, 'sku_ids': [], 'message': ''},
    ]));
    expect(v.promotionNotes, ['再买 ¥161 可减 50']);
    expect(v.promotionDiscountText, '-¥20.00');
  });

  test('送不到当前地址的原因', () {
    final r = view(cart([item(1, undeliverable: {'sku_id': 101, 'reason_code': 'region', 'reason': '新疆不发货'})])).rows.single;
    expect(r.undeliverableText, '新疆不发货');
  });

  test('改数量超库存：抛出服务端原因（409 detail）', () async {
    SharedPreferences.setMockInitialValues({});
    final s = Session();
    await s.load();
    final c = ApiClient(base: 'http://h/api/v1', session: s, http: MockClient((r) async {
      expect(r.method, 'PATCH');
      expect(jsonDecode(r.body), {'quantity': 9});
      return http.Response(jsonEncode({'type': 'https://keel.dev/problems/insufficient-stock', 'title': '库存不足',
        'status': 409, 'detail': '只剩 3 件'}), 409, headers: {'content-type': 'application/problem+json; charset=utf-8'});
    }));
    await expectLater(setCartQuantity(c, 1, 9, storeId: 1), throwsA(isA<ApiFailure>().having((f) => f.message, 'm', '只剩 3 件')));
  });

  group('同城配送（mode = local：有围栏的门店按距离分档）', () {
    Map<String, dynamic> local({int? distance = 3200, int fee = 600, String? free, int freeOver = 0, int minOrder = 0, int shortfall = 0}) => {
          'mode': 'local', 'freight_cents': free == null ? fee : 0, 'freight_discount_cents': 0, 'groups': [],
          'local': {'distance_m': distance, 'tier_within_m': 5000, 'tier_fee_cents': fee, 'free_over_cents': freeOver,
            'free_reason': ?free, 'min_order_cents': minOrder, 'shortfall_cents': shortfall},
        };

    test('叫「配送费」，说明里写距离（公里，一位小数）', () {
      final v = view(cart([item(1)], freight: local()));
      expect(v.freightLabel, '配送费');
      expect(v.freightText, '¥6.00');
      expect(v.freightNote, '距离 3.2 公里');
      expect(v.shortfallText, '');
      expect(v.needsPin, isFalse);
    });

    test('满额免配送费：「已免配送费」；还没满、门店设了免费线：顺带说一句', () {
      expect(view(cart([item(1)], freight: local(free: 'free_over', freeOver: 5000))).freightNote, '已免配送费');
      expect(view(cart([item(1)], freight: local(freeOver: 5000))).freightNote, '距离 3.2 公里，满¥50 免配送费');
    });

    test('地址没有坐标（distance_m 为 null）：按最远一档计，并提示给地址选点', () {
      final v = view(cart([item(1)], freight: local(distance: null)));
      expect(v.freightNote, '按最远一档计');
      expect(v.needsPin, isTrue);
    });

    test('没到起送价：「还差 ¥z 起送」', () {
      final v = view(cart([item(1)], freight: local(minOrder: 3000, shortfall: 1250)));
      expect(v.shortfallText, '还差 ¥12.50 起送');
      expect(v.belowMinimum, isTrue);
    });

    test('express（默认店）照旧叫「运费」', () {
      final v = view(cart([item(1)], freight: freight()));
      expect(v.freightLabel, '运费');
      expect(v.belowMinimum, isFalse);
    });
  });
}
