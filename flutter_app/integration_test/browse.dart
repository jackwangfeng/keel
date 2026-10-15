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
      await revealChip(t, 'home.cat.${c['id']}'); // 分类栏横向滚动，真机屏窄时后面几个要滑过去才构建
      expect(byKey('home.cat.${c['id']}'), findsOneWidget);
    }
    final target = tree.first as Map;
    await revealChip(t, 'home.cat.${target['id']}');
    await tapKey(t, 'home.cat.${target['id']}');
    await waitFor(t, keyedText('home.section', target['name'] as String));
    final want = ((await Api.get('/products?page_size=1&category_id=${target['id']}', auth: false)) as Map)['total'];
    await waitFor(t, find.text('共 $want 件'));
    await revealChip(t, 'home.cat.all'); // 上面逐个滑过去检查后，「全部」可能已滑出屏幕被回收
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

  // 取有货的第一件（没 SKU / 缺货的会被检索降权，排不上来，且买不了）。同名商品在融合分上可能并列（比如「陶瓷马克杯」
  // 与「陶瓷马克杯 两只装」），所以不断言它排第一，只断言它在结果里、点进去是它。
  e2e('搜索：按商品标题搜得到这件；点进去是它的详情；详情里加购成功', (t) async {
    await startApp(t);
    await loginInApp(t);
    await Api.clearCart();
    await tapKey(t, 'tab.home');
    final items = (((await Api.get('/products?page_size=50', auth: false)) as Map)['items'] as List).cast<Map>();
    // 挑一件「此刻按标题搜得到」的有货商品：检索数据由异步任务生成，刚建的商品（比如同一栈上后台测试
    // 刚建的、或没有推理引擎时新建的）搜不到是正常的，不能碰运气挑中它。
    Map? first;
    for (final p in items.where((p) => p['in_stock'] == true)) {
      final r = await Api.call('POST', '/search', body: {'query': p['title'], 'size': 10}, auth: false);
      final hits = (r is Map ? (r['items'] as List?) ?? const [] : const []).cast<Map>();
      if (hits.any((h) => h['id'] == p['id'])) {
        first = p;
        break;
      }
    }
    if (first == null) fail('列表里没有按标题搜得到的有货商品');
    final title = first['title'] as String;
    await tapKey(t, 'home.search');
    await waitFor(t, byKey('search.input'));
    await t.enterText(byKey('search.input'), title);
    await tapKey(t, 'search.submit');
    await waitFor(t, byKey('search.count'));
    await tapKey(t, 'search.row.${first['id']}');
    await waitFor(t, keyedText('detail.title', title));
    await tapKey(t, 'detail.add');
    await waitFor(t, byKey('detail.added'));
    expect(await Api.cartItems(), isNotEmpty);
    await Api.clearCart();
  });
}

/// 把分类栏里的某个分类滑进屏幕。不用通用的 reveal：它先去滚页面的纵向列表，滚的过程中分类栏被回收重建，
/// 它手里那个旧的 Scrollable 就失效了（类目一多就找不到，CI 上后台测试先建了几个类目时踩到过）。
/// 这里只滚分类栏本身，两个方向都试。
Future<void> revealChip(WidgetTester t, String key) async {
  if (byKey(key).evaluate().isNotEmpty) return;
  final anyChip = find.byWidgetPredicate(
      (w) => w.key is ValueKey<String> && (w.key! as ValueKey<String>).value.startsWith('home.cat.'));
  final bar = find.ancestor(of: anyChip.first, matching: find.byType(Scrollable)).first;
  for (final delta in const [200.0, -200.0]) {
    try {
      await t.scrollUntilVisible(byKey(key), delta, scrollable: bar, maxScrolls: 40);
      return;
    } catch (_) {
      // 这个方向没有：换个方向
    }
  }
  fail('分类栏里滑来滑去也没找到 $key');
}
