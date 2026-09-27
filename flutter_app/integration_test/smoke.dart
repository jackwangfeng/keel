import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';

import 'e2e_env.dart';
import 'helpers.dart';

void smokeTests() {
  e2e('冒烟：首页显示门店与服务端的商品；登录后「我的」显示昵称；退出', (t) async {
    requireAccount();
    await startApp(t);
    await waitFor(t, find.byWidgetPredicate((w) => w.key is ValueKey<String> &&
        (w.key! as ValueKey<String>).value.startsWith('product.card.')));

    await t.tap(find.byKey(const Key('tab.me')));
    await t.pumpAndSettle();
    if (find.byKey(const Key('me.nickname')).evaluate().isNotEmpty) await logoutInApp(t);
    await t.tap(find.byKey(const Key('me.login')));
    await waitFor(t, find.byKey(const Key('login.phone')));
    await t.enterText(find.byKey(const Key('login.phone')), e2ePhone);
    await t.enterText(find.byKey(const Key('login.password')), e2ePassword);
    await t.tap(find.byKey(const Key('login.submit')));
    await waitFor(t, find.byKey(const Key('me.nickname')));
    await waitGone(t, find.byKey(const Key('login.submit')));
    await logoutInApp(t);
  });
}
