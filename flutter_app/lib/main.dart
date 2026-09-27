import 'package:flutter/material.dart';
import 'package:go_router/go_router.dart';

import 'api/cart_count.dart';
import 'api/catalog.dart';
import 'api/client.dart';
import 'api/notification.dart';
import 'api/profile.dart';
import 'capsule.dart';
import 'api/services.dart';
import 'api/session.dart';
import 'api/store.dart';
import 'config.dart';
import 'router.dart';
import 'theme.dart';

/// 整个 App 的路由（e2e 用它直接打开某一页，相当于深链）。
late GoRouter appRouter;

Future<void> main() async {
  WidgetsFlutterBinding.ensureInitialized();
  final session = Session();
  await session.load();
  final router = appRouter = buildRouter(session);
  // 小程序：顶栏右侧按钮让出胶囊按钮（见 capsule.dart）。
  final view = WidgetsBinding.instance.platformDispatcher.views.first;
  final capsule = await capsuleInset(view.physicalSize.width / view.devicePixelRatio);
  // 「服务地址」页设过的优先（换了店），没有就用编译时注入的。
  final client = ApiClient(base: await savedBase() ?? apiBase(), session: session,
      onSessionExpired: () => router.push('/login?from=${Uri.encodeComponent(router.state.uri.toString())}'));
  runApp(Services(
    client: client,
    session: session,
    store: StoreService(client),
    cart: CartCount(client, session)..refresh(),
    trace: SearchTrace(client),
    unread: UnreadCount(client, session)..refresh(),
    child: MaterialApp.router(title: 'Keel', theme: keelTheme(actionsRight: capsule), routerConfig: router, debugShowCheckedModeBanner: false),
  ));
}
