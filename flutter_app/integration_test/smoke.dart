import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:keel_buyer/main.dart' as app;

import 'e2e_env.dart';
import 'helpers.dart';

void smokeTests() {
  testWidgets('冒烟：首页显示门店与服务端的商品；登录后「我的」显示昵称；退出', (t) async {
    requireAccount();
    await app.main();
    await waitFor(t, find.byKey(const Key('home.store')));
    await waitFor(t, find.byWidgetPredicate((w) => w.key is ValueKey<String> &&
        (w.key! as ValueKey<String>).value.startsWith('product.card.')));

    await t.tap(find.byKey(const Key('tab.me')));
    await t.pumpAndSettle();
    if (find.byKey(const Key('me.logout')).evaluate().isNotEmpty) {
      await t.tap(find.byKey(const Key('me.logout')));
      await waitFor(t, find.byKey(const Key('me.login')));
    }
    await t.tap(find.byKey(const Key('me.login')));
    await waitFor(t, find.byKey(const Key('login.phone')));
    await t.enterText(find.byKey(const Key('login.phone')), e2ePhone);
    await t.enterText(find.byKey(const Key('login.password')), e2ePassword);
    await t.tap(find.byKey(const Key('login.submit')));
    await waitFor(t, find.byKey(const Key('me.nickname')));
    await waitGone(t, find.byKey(const Key('login.submit')));
    await t.tap(find.byKey(const Key('me.logout')));
    await waitFor(t, find.byKey(const Key('me.login')));
  });
}
