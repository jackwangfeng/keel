import 'package:flutter/material.dart';
import 'package:go_router/go_router.dart';

import 'api/cart_count.dart';
import 'api/services.dart';
import 'api/session.dart';
import 'pages/address_edit_page.dart';
import 'pages/address_list_page.dart';
import 'pages/cart_page.dart';
import 'pages/checkout_page.dart';
import 'pages/coupon_center_page.dart';
import 'pages/my_coupons_page.dart';
import 'pages/notifications_page.dart';
import 'pages/profile_page.dart';
import 'pages/refund_apply_page.dart';
import 'pages/refund_page.dart';
import 'pages/refunds_page.dart';
import 'pages/settings_page.dart';
import 'api/notification.dart';
import 'pages/order_page.dart';
import 'pages/orders_page.dart';
import 'pages/home_page.dart';
import 'pages/login_page.dart';
import 'pages/me_page.dart';
import 'pages/product_page.dart';
import 'pages/place_picker_page.dart';
import 'pages/search_page.dart';
import 'tabs.dart';
import 'theme.dart';

/// 底部 tab：首页 / 购物车 / 订单 / 我的。
GoRouter buildRouter(Session session) => GoRouter(
      routes: [
        StatefulShellRoute.indexedStack(
          builder: (context, state, shell) {
            // 切 tab（包括 context.go('/cart') 这类跳转）时通知 tab 页刷新。
            WidgetsBinding.instance.addPostFrameCallback((_) => Tabs.current.value = shell.currentIndex);
            return Scaffold(
              body: shell,
              bottomNavigationBar: _TabBar(
                current: shell.currentIndex,
                onTap: shell.goBranch,
                items: [
                  (key: 'tab.home', label: '首页', icon: const Icon(Icons.home_outlined), on: const Icon(Icons.home)),
                  (key: 'tab.cart', label: '购物车', icon: _CartIcon(count: Services.of(context).cart, on: false),
                      on: _CartIcon(count: Services.of(context).cart, on: true)),
                  (key: 'tab.orders', label: '订单', icon: const Icon(Icons.receipt_long_outlined), on: const Icon(Icons.receipt_long)),
                  (key: 'tab.me', label: '我的', icon: _MeIcon(unread: Services.of(context).unread, on: false),
                      on: _MeIcon(unread: Services.of(context).unread, on: true)),
                ],
              ),
            );
          },
          branches: [
            StatefulShellBranch(routes: [GoRoute(path: '/', builder: (_, _) => const HomePage())]),
            StatefulShellBranch(routes: [GoRoute(path: '/cart', builder: (_, _) => const CartPage())]),
            StatefulShellBranch(routes: [GoRoute(path: '/orders', builder: (_, _) => const OrdersPage())]),
            StatefulShellBranch(routes: [GoRoute(path: '/me', builder: (_, _) => const MePage())]),
          ],
        ),
        // 外壳之上的页（push 进来，返回回到原 tab）。
        GoRoute(path: '/search', builder: (_, _) => const SearchPage()),
        GoRoute(path: '/place', builder: (_, st) => PlacePickerPage(forAddress: st.uri.queryParameters['for'] == 'address')),
        GoRoute(path: '/product/:id', builder: (_, st) => ProductPage(productId: int.tryParse(st.pathParameters['id'] ?? '') ?? 0)),
        GoRoute(path: '/addresses', builder: (_, st) => AddressListPage(select: st.uri.queryParameters['select'] == '1')),
        GoRoute(path: '/addresses/new', builder: (_, _) => const AddressEditPage()),
        GoRoute(path: '/addresses/:id', builder: (_, st) => AddressEditPage(addressId: int.tryParse(st.pathParameters['id'] ?? '') ?? 0)),
        GoRoute(
          path: '/checkout',
          builder: (_, st) => CheckoutPage(
            fromCart: st.uri.queryParameters['from'] == 'cart',
            skuId: int.tryParse(st.uri.queryParameters['sku_id'] ?? '') ?? 0,
            productId: int.tryParse(st.uri.queryParameters['product_id'] ?? '') ?? 0,
            addressId: int.tryParse(st.uri.queryParameters['address_id'] ?? '') ?? 0,
          ),
        ),
        GoRoute(path: '/orders/:no', builder: (_, st) => OrderPage(orderNo: st.pathParameters['no'] ?? '')),
        GoRoute(path: '/orders/:no/refund', builder: (_, st) => RefundApplyPage(orderNo: st.pathParameters['no'] ?? '')),
        GoRoute(path: '/refunds', builder: (_, _) => const RefundsPage()),
        GoRoute(path: '/refunds/:no', builder: (_, st) => RefundPage(refundNo: st.pathParameters['no'] ?? '')),
        GoRoute(path: '/coupon-center', builder: (_, _) => const CouponCenterPage()),
        GoRoute(path: '/coupons', builder: (_, _) => const MyCouponsPage()),
        GoRoute(path: '/notifications', builder: (_, _) => const NotificationsPage()),
        GoRoute(path: '/profile', builder: (_, _) => const ProfilePage()),
        GoRoute(path: '/settings', builder: (_, _) => const SettingsPage()),
        GoRoute(path: '/login', builder: (_, st) => LoginPage(from: st.uri.queryParameters['from'] ?? '')),
      ],
    );

/// 底部 tab 栏：与 uni-app x 的 tabBar 同一套样子（米白底、选中深咖、未选中灰，没有胶囊底）。
class _TabBar extends StatelessWidget {
  const _TabBar({required this.current, required this.onTap, required this.items});
  final int current;
  final void Function(int) onTap;
  final List<({String key, String label, Widget icon, Widget on})> items;

  @override
  Widget build(BuildContext context) => Container(
        decoration: const BoxDecoration(color: KeelColors.card, border: Border(top: BorderSide(color: KeelColors.line, width: 0.5))),
        child: SafeArea(
          top: false,
          child: ConstrainedBox(
            constraints: const BoxConstraints(minHeight: 50),
            child: Row(children: [
              for (final (i, it) in items.indexed)
                Expanded(
                  child: InkResponse(
                    key: Key(it.key),
                    onTap: () => onTap(i),
                    child: IconTheme(
                      data: IconThemeData(size: 24, color: i == current ? KeelColors.primary : KeelColors.textHint),
                      child: Column(mainAxisSize: MainAxisSize.min, mainAxisAlignment: MainAxisAlignment.center, children: [
                        const SizedBox(height: 5),
                        i == current ? it.on : it.icon,
                        const SizedBox(height: 2),
                        Text(it.label, style: TextStyle(fontSize: 10, color: i == current ? KeelColors.primary : KeelColors.textHint)),
                        const SizedBox(height: 5),
                      ]),
                    ),
                  ),
                ),
            ]),
          ),
        ),
      );
}

class _CartIcon extends StatelessWidget {
  const _CartIcon({required this.count, required this.on});
  final CartCount count;
  final bool on;
  @override
  Widget build(BuildContext context) => ListenableBuilder(
        listenable: count,
        builder: (_, _) => Badge(
          isLabelVisible: count.n > 0,
          label: Text(count.n > 99 ? '99+' : '${count.n}', key: const Key('tab.cartBadge')),
          child: Icon(on ? Icons.shopping_cart : Icons.shopping_cart_outlined),
        ),
      );
}

class _MeIcon extends StatelessWidget {
  const _MeIcon({required this.unread, required this.on});
  final UnreadCount unread;
  final bool on;
  @override
  Widget build(BuildContext context) => ListenableBuilder(
        listenable: unread,
        builder: (_, _) => Badge(
          isLabelVisible: unread.n > 0,
          label: Text(unread.n > 99 ? '99+' : '${unread.n}', key: const Key('tab.meBadge')),
          child: Icon(on ? Icons.person : Icons.person_outline),
        ),
      );
}
