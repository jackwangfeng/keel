import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:integration_test/integration_test.dart';
import 'package:keel_buyer/main.dart' as app;

import 'e2e_env.dart';

/// 等一个 finder 出现：接口有网络延迟，按条件等，不 sleep 固定秒数。
Future<void> waitFor(WidgetTester t, Finder f, {Duration timeout = const Duration(seconds: 20)}) async {
  final end = DateTime.now().add(timeout);
  while (DateTime.now().isBefore(end)) {
    await t.pump(const Duration(milliseconds: 200));
    if (f.evaluate().isNotEmpty) return;
  }
  throw TestFailure('等 $f 超时');
}

/// 等一个 finder 消失：比如登录页退场动画没走完时，底下的页已经能找到，但点击会落在登录页上。
Future<void> waitGone(WidgetTester t, Finder f, {Duration timeout = const Duration(seconds: 20)}) async {
  final end = DateTime.now().add(timeout);
  while (DateTime.now().isBefore(end)) {
    await t.pump(const Duration(milliseconds: 200));
    if (f.evaluate().isEmpty) return;
  }
  throw TestFailure('等 $f 消失超时');
}

void main() {
  IntegrationTestWidgetsFlutterBinding.ensureInitialized();

  testWidgets('冒烟：首页显示门店与服务端的商品；登录后「我的」显示昵称；退出', (t) async {
    if (e2ePhone.isEmpty || e2ePassword.isEmpty) {
      fail('没有 e2e 账号：用 tool/e2e_web.sh 跑（读 ~/.config/keel/e2e.env），或传 --dart-define=KEEL_E2E_PHONE/KEEL_E2E_PASSWORD');
    }
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
