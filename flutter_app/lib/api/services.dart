import 'package:flutter/widgets.dart';

import 'cart_count.dart';
import 'catalog.dart';
import 'client.dart';
import 'notification.dart';
import 'session.dart';
import 'store.dart';

/// 页面拿依赖的地方：Services.of(context).client / session / store / cart / trace / unread。
class Services extends InheritedWidget {
  const Services({super.key, required this.client, required this.session, required this.store, required this.cart,
      required this.trace, required this.unread, required super.child});
  final ApiClient client;
  final Session session;
  final StoreService store;
  final CartCount cart;
  final SearchTrace trace;
  final UnreadCount unread;

  static Services of(BuildContext context) {
    final s = context.dependOnInheritedWidgetOfExactType<Services>();
    assert(s != null, '上层没有 Services');
    return s!;
  }

  @override
  bool updateShouldNotify(Services old) => client != old.client || session != old.session || store != old.store;
}
