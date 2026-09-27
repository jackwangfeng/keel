import 'package:flutter/material.dart';

import 'api/client.dart';
import 'api/services.dart';
import 'api/session.dart';
import 'api/store.dart';
import 'config.dart';
import 'router.dart';
import 'theme.dart';

Future<void> main() async {
  WidgetsFlutterBinding.ensureInitialized();
  final session = Session();
  await session.load();
  final router = buildRouter(session);
  final client = ApiClient(base: apiBase(), session: session,
      onSessionExpired: () => router.go('/login?from=${Uri.encodeComponent(router.state.uri.toString())}'));
  runApp(Services(
    client: client,
    session: session,
    store: StoreService(client),
    child: MaterialApp.router(title: 'Keel', theme: keelTheme(), routerConfig: router, debugShowCheckedModeBanner: false),
  ));
}
