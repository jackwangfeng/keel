import 'package:flutter/material.dart';
import 'package:go_router/go_router.dart';

import 'api/cart_count.dart';
import 'api/catalog.dart';
import 'api/client.dart';
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
  final client = ApiClient(base: apiBase(), session: session,
      onSessionExpired: () => router.push('/login?from=${Uri.encodeComponent(router.state.uri.toString())}'));
  runApp(Services(
    client: client,
    session: session,
    store: StoreService(client),
    cart: CartCount(client, session)..refresh(),
    trace: SearchTrace(client),
    child: MaterialApp.router(title: 'Keel', theme: keelTheme(), routerConfig: router, debugShowCheckedModeBanner: false),
  ));
}
