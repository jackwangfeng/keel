import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';

import 'helpers.dart';

/// 不会和种子撞名的收件人：e2e 买家的种子默认地址叫「e2e 收件人」，按那个名字清理会把种子删掉（uni-app x 踩过）。
const tmpName = 'e2e 临时收件人';

int cents(String yuan) => (double.parse(yuan.replaceAll('¥', '')) * 100).round();

Future<void> cleanTmpAddresses() async {
  for (final a in (await Api.get('/addresses')) as List) {
    if (a['receiver_name'] == tmpName && a['is_default'] != true && a['region_code'] != '330106') {
      await Api.call('DELETE', '/addresses/${a['id']}');
    }
  }
}

void cartAddressTests() {
  e2e('购物车：详情页加购 → 车里这一行勾选着 → 调大数量合计翻倍且与服务端一致', (t) async {
    await startApp(t);
    await loginInApp(t);
    await Api.clearCart();
    final sku = await Api.pickSku(5);
    await open(t, '/product/${sku.productId}');
    await waitFor(t, byKey('detail.price'));
    await tapKey(t, 'detail.sku.${sku.skuId}');
    await tapKey(t, 'detail.add');
    await waitFor(t, byKey('detail.added'));
    await tapKey(t, 'detail.added');
    final rowKeyPrefix = 'cart.row.';
    await waitFor(t, find.byWidgetPredicate((w) => w.key is ValueKey<String> && '${(w.key! as ValueKey).value}'.startsWith(rowKeyPrefix)));
    final item = (await Api.cartItems()).single as Map;
    expect(item['sku_id'], sku.skuId);
    await waitFor(t, find.text('去结算(1)'));
    final one = cents(textOf('cart.total'));
    await tapKey(t, 'cart.${item['id']}.plus');
    await waitFor(t, keyedText('cart.${item['id']}.qty', '2'));
    await waitFor(t, find.byWidgetPredicate((w) => w is Text && w.key == const Key('cart.total') && cents(w.data!) == one * 2));
    final server = (await Api.get('/cart')) as Map;
    expect(server['selected_total_cents'], one * 2);
    expect(find.byKey(const Key('tab.cartBadge')), findsOneWidget);
    await Api.clearCart();
  });

  e2e('地址簿：列出服务端的地址（默认第一）；新建先交错的看标红，再交对的', (t) async {
    await startApp(t);
    await loginInApp(t);
    await cleanTmpAddresses();
    addTearDown(cleanTmpAddresses);
    final server = (await Api.get('/addresses')) as List;
    await tapKey(t, 'me.addresses');
    await waitFor(t, byKey('address.new'));
    if (server.isNotEmpty) {
      await waitFor(t, byKey('address.row.${server.first['id']}'));
      if (server.first['is_default'] == true) expect(byKey('address.default.${server.first['id']}'), findsOneWidget);
    }
    await tapKey(t, 'address.new');
    await waitFor(t, byKey('address.save'));
    for (final (k, v) in [('phone', 'abc'), ('province', '上海市'), ('city', '上海市'), ('district', '徐汇区'), ('detail', '漕溪北路 1 号')]) {
      await t.enterText(byKey('address.field.$k'), v);
    }
    await tapKey(t, 'address.save');
    await waitFor(t, keyedText('address.message', '请检查标红的几项'));
    await t.enterText(byKey('address.field.receiverName'), tmpName);
    await t.enterText(byKey('address.field.phone'), '13900000000');
    await tapKey(t, 'address.save');
    final created = await waitUntil(t, () async {
      final list = (await Api.get('/addresses')) as List;
      return list.cast<Map>().where((a) => a['receiver_name'] == tmpName).firstOrNull;
    }, '新建的地址出现在 GET /addresses');
    expect(created['is_default'], false);
    expect(created['phone'], '13900000000');
    await waitFor(t, byKey('address.row.${created['id']}'));
  });
}
