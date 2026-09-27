import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';

import 'helpers.dart';

// 演示栈上的固定商品（与 app/e2e/quickcart.test.js 同一组）。
const single = (productId: 23, skuId: 26); // 冷萃咖啡液：一个规格
const multi = (productId: 19, skuIds: [21, 22]); // 挂耳咖啡：两个规格

Future<Map<String, dynamic>?> waitCartHas(int skuId) async {
  final items = await Api.cartItems();
  return items.cast<Map<String, dynamic>>().where((x) => x['sku_id'] == skuId).firstOrNull;
}

void browseTests() {
  e2e('分类：分类栏与服务端一致；切换后标题与件数跟着变，点回「全部」', (t) async {
    await startApp(t);
    final tree = await Api.get('/categories', auth: false);
    if (tree is! List) {
      expect(find.byWidgetPredicate((w) => w.key is ValueKey<String> && '${(w.key! as ValueKey).value}'.startsWith('home.cat.')),
          findsNothing);
      return;
    }
    await waitFor(t, byKey('home.cat.all'));
    for (final c in tree) {
      expect(byKey('home.cat.${c['id']}'), findsOneWidget);
    }
    final target = tree.first as Map;
    await tapKey(t, 'home.cat.${target['id']}');
    await waitFor(t, keyedText('home.section', target['name'] as String));
    final want = ((await Api.get('/products?page_size=1&category_id=${target['id']}', auth: false)) as Map)['total'];
    await waitFor(t, find.text('共 $want 件'));
    await tapKey(t, 'home.cat.all');
    await waitFor(t, keyedText('home.section', '全部商品'));
  });

  e2e('首页原地加购：单规格直接加；多规格弹浮层选第二个规格', (t) async {
    await startApp(t);
    await loginInApp(t);
    await Api.clearCart();
    await tapKey(t, 'tab.home');
    await waitFor(t, byKey('home.total'));
    await scrollTo(t, 'product.add.${single.productId}');
    await tapKey(t, 'product.add.${single.productId}');
    await waitUntil(t, () => waitCartHas(single.skuId), '购物车里出现 sku ${single.skuId}');
    expect(byKey('detail.title'), findsNothing, reason: '不离开首页');
    expect(byKey('tab.home'), findsOneWidget);

    await scrollTo(t, 'product.add.${multi.productId}');
    await tapKey(t, 'product.add.${multi.productId}');
    await waitFor(t, byKey('quick.confirm'));
    for (final id in multi.skuIds) {
      expect(byKey('quick.sku.$id'), findsOneWidget);
    }
    await tapKey(t, 'quick.sku.${multi.skuIds[1]}');
    await tapKey(t, 'quick.confirm');
    await waitUntil(t, () => waitCartHas(multi.skuIds[1]), '购物车里出现 sku ${multi.skuIds[1]}');
    await waitGone(t, byKey('quick.confirm'));
    await Api.clearCart();
  });

  e2e('搜索：按商品标题搜，这件排第一；点进去是它的详情；详情里加购成功', (t) async {
    await startApp(t);
    await loginInApp(t);
    await Api.clearCart();
    await tapKey(t, 'tab.home');
    final first = (((await Api.get('/products?page_size=1', auth: false)) as Map)['items'] as List).first as Map;
    final title = first['title'] as String;
    await tapKey(t, 'home.search');
    await waitFor(t, byKey('search.input'));
    await t.enterText(byKey('search.input'), title);
    await tapKey(t, 'search.submit');
    await waitFor(t, byKey('search.count'));
    final rows = find.byWidgetPredicate((w) => w.key is ValueKey<String> && '${(w.key! as ValueKey).value}'.startsWith('search.row.'));
    expect(rows, findsWidgets);
    expect((rows.evaluate().first.widget.key! as ValueKey).value, 'search.row.${first['id']}');
    await t.tap(rows.first);
    await waitFor(t, keyedText('detail.title', title));
    await tapKey(t, 'detail.add');
    await waitFor(t, byKey('detail.added'));
    expect(await Api.cartItems(), isNotEmpty);
    await Api.clearCart();
  });
}
