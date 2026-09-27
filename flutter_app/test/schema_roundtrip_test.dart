import 'dart:convert';
import 'dart:io';

import 'package:flutter_test/flutter_test.dart';
import 'package:keel_buyer/api/schema.g.dart';

Map<String, dynamic> load(String name) =>
    jsonDecode(File('test/fixtures/$name').readAsStringSync()) as Map<String, dynamic>;

void main() {
  test('商品列表：解析真实响应，往返后字段不丢', () {
    final raw = load('products_page.json');
    final page = ListProductsResponse.fromJson(raw);
    expect(page.items, isNotEmpty);
    expect(page.items.first.minPriceCents, isA<int>());
    final again = ListProductsResponse.fromJson(jsonDecode(jsonEncode(page.toJson())) as Map<String, dynamic>);
    expect(again.items.first.title, page.items.first.title);
    expect(again.total, page.total);
  });

  test('多出的字段忽略、缺的非必填字段为 null', () {
    final raw = load('products_page.json');
    final item = Map<String, dynamic>.from((raw['items'] as List).first as Map);
    item['a_field_from_the_future'] = 1;
    item.remove('subtitle');
    final p = ProductSummary.fromJson(item);
    expect(p.subtitle, isNull);
  });

  test('整数字段收到 12.0 也能进 int', () {
    final raw = load('products_page.json');
    final item = Map<String, dynamic>.from((raw['items'] as List).first as Map);
    item['min_price_cents'] = 4990.0;
    expect(ProductSummary.fromJson(item).minPriceCents, 4990);
  });

  test('登录响应', () {
    final r = LoginResponse.fromJson(load('login.json'));
    expect(r.user.nickname, 'e2e 买家');
    expect(r.refreshToken, 't.r');
  });

  test('门店解析', () {
    final r = StoreResolveResult.fromJson(load('store_resolve.json'));
    expect(r.matchType, isNotEmpty);
  });
}
