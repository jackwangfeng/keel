import 'package:flutter/material.dart';
import 'package:go_router/go_router.dart';

import 'api/session.dart';
import 'pages/home_page.dart';
import 'pages/login_page.dart';
import 'pages/me_page.dart';
import 'theme.dart';

/// 底部 tab：第 1 步只有 首页 / 我的；购物车、订单在第 3、4 步加进来。
GoRouter buildRouter(Session session) => GoRouter(
      refreshListenable: session,
      routes: [
        StatefulShellRoute.indexedStack(
          builder: (context, state, shell) => Scaffold(
            body: shell,
            bottomNavigationBar: NavigationBar(
              backgroundColor: KeelColors.card,
              selectedIndex: shell.currentIndex,
              onDestinationSelected: shell.goBranch,
              destinations: const [
                NavigationDestination(key: Key('tab.home'), icon: Icon(Icons.home_outlined), label: '首页'),
                NavigationDestination(key: Key('tab.me'), icon: Icon(Icons.person_outline), label: '我的'),
              ],
            ),
          ),
          branches: [
            StatefulShellBranch(routes: [GoRoute(path: '/', builder: (_, _) => const HomePage())]),
            StatefulShellBranch(routes: [GoRoute(path: '/me', builder: (_, _) => const MePage())]),
          ],
        ),
        GoRoute(path: '/login', builder: (_, st) => LoginPage(from: st.uri.queryParameters['from'] ?? '')),
      ],
    );
