import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';

import 'helpers.dart';

// 演示栈上的固定商品（与 app/e2e/promotion.test.js 同一组）。
const guaer = (productId: 19, skuId: 21); // 限时特价 49.9（门店价 69），限购 2
const mill = (productId: 22, skuId: 25); // 238 元：命中满 199 减 20，且满 99 包邮
const cheap = (productId: 23, skuId: 26); // 55.8 元：运费 8 元
const freeShipTemplate = 3;
const freightName = 'e2e 运费';

Future<void> cleanFreightAddresses() async {
  for (final a in await Api.addresses()) {
    if (a['receiver_name'] == freightName && a['is_default'] != true && a['region_code'] != '330106') {
      await Api.call('DELETE', '/addresses/${a['id']}');
    }
  }
}

/// 打开结算页并等到试算出应付（或出错）。
Future<void> openCheckout(WidgetTester t, ({int productId, int skuId}) sku, {int addressId = 0}) async {
  await open(t, '/checkout?sku_id=${sku.skuId}&product_id=${sku.productId}${addressId > 0 ? '&address_id=$addressId' : ''}');
  await waitFor(t, byKey('checkout.submit'));
}

Future<void> waitPayable(WidgetTester t) =>
    waitFor(t, find.byWidgetPredicate((w) => w is Text && w.key == const Key('checkout.payable') && w.data != '—'));

/// 应付 = 商品 + 运费 − 各项优惠（金额都是服务端的，这里只核对页面把它们摆对了）。
void expectPayableAddsUp() {
  var want = centsOf(textOf('checkout.goods')) + centsOf(textOf('checkout.freight'));
  for (final k in ['checkout.promotion', 'checkout.couponOff', 'checkout.freightOff']) {
    if (byKey(k).evaluate().isNotEmpty) want += centsOf(textOf(k));
  }
  if (byKey('checkout.discount').evaluate().isNotEmpty) want -= centsOf(textOf('checkout.discount'));
  expect(centsOf(textOf('checkout.payable')), want);
}

Future<void> pickNoCoupon(WidgetTester t) async {
  if (byKey('checkout.coupons').evaluate().isEmpty) return;
  await tapKey(t, 'checkout.coupons');
  await tapKey(t, 'checkout.coupon.none');
  await waitPayable(t);
}

void orderTests() {
  e2e('下单主链路：试算出应付 → 提交 → 跳订单详情（待支付）→ 沙箱支付 → 已支付；订单 tab 里有这一单', (t) async {
    await startApp(t);
    await loginInApp(t);
    final sku = await Api.pickSku(1);
    await openCheckout(t, (productId: sku.productId, skuId: sku.skuId));
    await waitPayable(t);
    expectPayableAddsUp();
    await tapKey(t, 'checkout.submit');
    await waitFor(t, keyedText('order.status', '待支付'));
    await tapKey(t, 'order.pay');
    await waitFor(t, byKey('order.sandbox'));
    await tapKey(t, 'order.settle');
    await waitFor(t, keyedText('order.status', '已支付'), timeout: const Duration(seconds: 30));
    expect(byKey('order.pay'), findsNothing);
  });

  e2e('购物车去结算：试算的商品金额 = 购物车合计；下单后车里这行被删掉；详情里取消订单（点两下）', (t) async {
    await startApp(t);
    await loginInApp(t);
    await Api.clearCart();
    final sku = await Api.pickSku(3);
    await Api.idem('POST', '/cart/items?store_id=${await Api.storeId()}', {'sku_id': sku.skuId, 'quantity': 1});
    await tapKey(t, 'tab.cart');
    await waitFor(t, find.text('去结算(1)'));
    final total = textOf('cart.total');
    await tapKey(t, 'cart.checkout');
    await waitPayable(t);
    expect(textOf('checkout.goods'), total);
    await tapKey(t, 'checkout.submit');
    await waitFor(t, keyedText('order.status', '待支付'));
    await waitUntil(t, () async => (await Api.cartItems()).isEmpty ? true : null, '下单后购物车清掉这一行');
    await tapKey(t, 'order.cancel');
    await tapKey(t, 'order.cancel');
    await waitFor(t, keyedText('order.status', '已关闭'));
    expect(byKey('order.pay'), findsNothing);
  });

  group('运费', () {
    late ({int productId, int skuId}) under99;
    late ({int productId, int skuId}) over99;
    late int hz;
    setUpAll(() async {
      ({int productId, int skuId})? a;
      ({int productId, int skuId})? b;
      for (final p in ((await Api.get('/products?page_size=50', auth: false)) as Map)['items'] as List) {
        if (p['status'] != 1) continue;
        final s = (((await Api.get('/products/${p['id']}', auth: false)) as Map)['skus'] as List)
            .cast<Map>().where((x) => (x['available_qty'] as int) > 5).firstOrNull;
        if (s == null) continue;
        final price = s['price_cents'] as int;
        if (a == null && price < 9900) a = (productId: p['id'] as int, skuId: s['id'] as int);
        if (b == null && price >= 9900) b = (productId: p['id'] as int, skuId: s['id'] as int);
      }
      if (a == null || b == null) fail('演示库里凑不齐 99 元上下各一个有货的规格');
      under99 = a;
      over99 = b;
      await cleanFreightAddresses();
      final seed = (await Api.addresses()).where((x) => x['region_code'] == '330106').firstOrNull;
      if (seed == null) fail('演示买家名下没有种子里的杭州地址（region_code 330106），请重置演示库');
      hz = seed['id'] as int;
    });
    tearDownAll(cleanFreightAddresses);

    e2e('杭州：99 元以下运费 8 元并提示满 99 包邮；99 元以上包邮写「已满¥99 包邮」', (t) async {
      await startApp(t);
      await loginInApp(t);
      await openCheckout(t, under99, addressId: hz);
      await waitFor(t, byKey('checkout.address.$hz'));
      await waitPayable(t);
      expect(textOf('checkout.freight'), '¥8.00');
      expect(textOf('checkout.freight.note'), '满¥99 包邮');
      expectPayableAddsUp();
      await open(t, '/checkout?sku_id=${over99.skuId}&product_id=${over99.productId}&address_id=$hz');
      await waitFor(t, keyedText('checkout.freight.note', '已满¥99 包邮'));
      expect(textOf('checkout.freight'), '¥0.00');
    });

    e2e('新疆：首件 15 元、不包邮；香港：送不到、逐行标原因、提交灰掉；省份写错：引导补全地址', (t) async {
      await startApp(t);
      await loginInApp(t);
      final xj = await Api.makeAddress({'receiver_name': freightName, 'province': '新疆维吾尔自治区', 'city': '乌鲁木齐市', 'district': '天山区', 'region_code': '650102'});
      await openCheckout(t, over99, addressId: xj);
      await waitFor(t, keyedText('checkout.freight', '¥15.00'));
      expect(textOf('checkout.freight.note'), '首件¥15，续件¥5');

      final hk = await Api.makeAddress({'receiver_name': freightName, 'province': '香港特别行政区', 'city': '香港', 'district': '中西区', 'region_code': '810000'});
      await openCheckout(t, under99, addressId: hk);
      await waitFor(t, byKey('checkout.undeliverable.${under99.skuId}'));
      expect(textOf('checkout.message'), contains('送不到这个收货地址'));
      expect(textOf('checkout.undeliverable.${under99.skuId}'), allOf(contains('香港'), contains('配送范围')));
      expect(t.widget<FilledButton>(byKey('checkout.submit')).onPressed, isNull);
      expect(byKey('checkout.fixAddress'), findsNothing);

      final mars = await Api.makeAddress({'receiver_name': freightName, 'province': '火星', 'city': 'x', 'district': 'y'});
      await openCheckout(t, under99, addressId: mars);
      await waitFor(t, byKey('checkout.fixAddress'));
      expect(textOf('checkout.message'), contains('补全地址'));
    });

    e2e('购物车按默认地址显示预估运费', (t) async {
      await startApp(t);
      await loginInApp(t);
      await Api.clearCart();
      await Api.idem('POST', '/cart/items?store_id=${await Api.storeId()}', {'sku_id': under99.skuId, 'quantity': 1});
      await tapKey(t, 'tab.cart');
      await waitFor(t, byKey('cart.freight'));
      final server = (await Api.get('/cart?store_id=${await Api.storeId()}')) as Map;
      final want = '¥${((server['freight'] as Map)['freight_cents'] as int) ~/ 100}.${(((server['freight'] as Map)['freight_cents'] as int) % 100).toString().padLeft(2, '0')}';
      expect(textOf('cart.freight'), contains(want));
      await Api.clearCart();
    });
  });

  group('营销活动与包邮券', () {
    setUpAll(Api.clearCart);
    tearDownAll(Api.clearCart);

    e2e('详情按特价显示并划线门店价；限时特价结算按特价算，买 3 件超过限购提示减数量', (t) async {
      await startApp(t);
      await loginInApp(t);
      await open(t, '/product/${guaer.productId}');
      await tapKey(t, 'detail.sku.${guaer.skuId}');
      await waitFor(t, keyedText('detail.price', '¥49.90'));
      expect(textOf('detail.listPrice'), '¥69.00');

      final addr = (await Api.addresses()).first['id'] as int;
      final probe = await Api.preview(guaer.skuId, 1, addr);
      await openCheckout(t, guaer);
      if (probe['_status'] == 409) {
        // 这个买家的限购额度已经用完（之前的运行买过）：页面要照实说限购，不给应付。
        await waitFor(t, byKey('checkout.message'));
        expect(textOf('checkout.message'), contains('限购'));
        return;
      }
      await waitPayable(t);
      expect(textOf('checkout.goods'), '¥49.90');
      expect(find.byWidgetPredicate((w) => w is Text && (w.data ?? '').contains('限时特价') && '${w.key}'.contains('checkout.note')), findsWidgets);
      await pickNoCoupon(t);
      await tapKey(t, 'checkout.plus');
      await waitFor(t, keyedText('checkout.goods', '¥99.80'));
      await tapKey(t, 'checkout.plus');
      await waitFor(t, byKey('checkout.message'));
      expect(textOf('checkout.message'), contains('限购'));
      expect(textOf('checkout.payable'), '—');
    });

    e2e('满 199 减 20：活动优惠一行、凑单说明，应付加得起来', (t) async {
      await startApp(t);
      await loginInApp(t);
      await openCheckout(t, mill);
      await waitPayable(t);
      expect(textOf('checkout.promotion'), '-¥20.00');
      expect(find.byWidgetPredicate((w) => w is Text && (w.data ?? '').contains('已减 20 元') && '${w.key}'.contains('checkout.note')), findsWidgets);
      expectPayableAddsUp();
    });

    e2e('包邮券：99 元以下的单自动选最省的券、选包邮券抵掉 8 元运费；已包邮的单里不出现；加到满 99 时照实报错并展开券列表', (t) async {
      await startApp(t);
      await loginInApp(t);
      final claim = await Api.idem('POST', '/coupon-templates/$freeShipTemplate/claim', null);
      expect(claim is Map && claim['_status'] == null, isTrue, reason: '领包邮券：$claim');
      final addr = (await Api.addresses()).firstWhere((a) => a['is_default'] == true)['id'] as int;
      final coupons = ((await Api.preview(cheap.skuId, 1, addr))['applicable_coupons'] as List).cast<Map>();
      final freeShip = coupons.firstWhere((c) => '${c['name']}'.contains('包邮'));
      await openCheckout(t, cheap);
      await waitPayable(t);
      // 自动选的是服务端排在第一的（最省的）那张。
      expect(textOf('checkout.couponSummary'), startsWith('${coupons.first['name']}'));
      if (coupons.first['id'] != freeShip['id']) {
        await tapKey(t, 'checkout.coupons');
        await tapKey(t, 'checkout.coupon.${freeShip['id']}');
        await waitFor(t, byKey('checkout.freightOff'));
      }
      expect(textOf('checkout.freight'), '¥8.00');
      expect(textOf('checkout.freightOff'), '-¥8.00');
      expect(centsOf(textOf('checkout.payable')), centsOf(textOf('checkout.goods')));

      // 选着包邮券把数量加到满 99：包邮券抵不了钱，照实说并展开券列表，不悄悄换掉。
      await tapKey(t, 'checkout.plus');
      // 券列表展开后提示在列表下面（每跑一轮多领一张包邮券，列表越来越长）：滚过去再看。
      await waitFor(t, find.byKey(const Key('checkout.coupon.none')));
      await scrollTo(t, 'checkout.message');
      expect(textOf('checkout.message'), contains('包邮券'));
      expect(byKey('checkout.coupon.none'), findsOneWidget);
      expect(textOf('checkout.payable'), '—');

      await openCheckout(t, mill);
      await waitPayable(t);
      expect(textOf('checkout.freight'), '¥0.00');
      expect(find.byWidgetPredicate((w) => w is ListTile && '${w.key}'.contains('checkout.coupon.${freeShip['id']}')), findsNothing);
    });

    e2e('购物车：特价行划线门店价；满减显示已减金额与凑单说明', (t) async {
      await startApp(t);
      await loginInApp(t);
      await Api.clearCart();
      for (final s in [guaer, mill]) {
        await Api.idem('POST', '/cart/items?store_id=${await Api.storeId()}', {'sku_id': s.skuId, 'quantity': 1});
      }
      await tapKey(t, 'tab.cart');
      await waitFor(t, byKey('cart.discount'));
      expect(textOf('cart.discount'), '活动已减 ¥20.00');
      expect(find.text('¥69.00'), findsOneWidget);
      expect(find.text('¥49.90'), findsWidgets);
      await Api.clearCart();
    });
  });
}
