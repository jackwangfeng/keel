import 'package:flutter/material.dart';
import 'package:go_router/go_router.dart';

import '../api/client.dart';
import '../api/order.dart';
import '../api/services.dart';
import '../tabs.dart';
import '../theme.dart';
import '../widgets/badge.dart';
import '../widgets/states.dart';

/// 订单 tab：状态与售后状态组合显示；待支付的可以直接取消（点两下）或去支付。
class OrdersPage extends StatefulWidget {
  const OrdersPage({super.key});
  @override
  State<OrdersPage> createState() => _OrdersPageState();
}

class _OrdersPageState extends State<OrdersPage> {
  List<OrderRow> _rows = const [];
  bool _loaded = false;
  String _error = '';
  String _message = '';
  bool _failed = false;
  // 两下取消：第一下点过的订单号。
  String _armed = '';
  // 每笔订单一个取消用的幂等键，这个页面活着期间复用。
  final _keys = <String, String>{};
  bool _started = false;
  late final _session = Services.of(context).session;

  @override
  void didChangeDependencies() {
    super.didChangeDependencies();
    if (!_started) {
      _started = true;
      Tabs.current.addListener(_onTab);
      _session.addListener(_onSession);
      _load();
    }
  }

  @override
  void dispose() {
    Tabs.current.removeListener(_onTab);
    _session.removeListener(_onSession);
    super.dispose();
  }

  void _onTab() {
    if (Tabs.current.value == Tabs.orders && mounted) _load();
  }

  void _onSession() {
    if (!mounted) return;
    if (_session.loggedIn) {
      _load();
    } else {
      setState(() {
        _rows = const [];
        _loaded = false;
      });
    }
  }

  Future<void> _load() async {
    if (!_session.loggedIn) return setState(() {});
    try {
      final rows = await fetchOrders(Services.of(context).client);
      if (mounted) {
        setState(() {
          _rows = rows;
          _loaded = true;
          _error = '';
        });
      }
    } on ApiFailure catch (f) {
      if (mounted) {
        setState(() {
          _error = f.message;
          _loaded = true;
        });
      }
    }
  }

  Future<void> _cancel(String no) async {
    if (_armed != no) return setState(() => _armed = no);
    setState(() {
      _armed = '';
      _message = '';
    });
    try {
      await cancelOrder(Services.of(context).client, no, _keys.putIfAbsent(no, newIdempotencyKey));
      if (mounted) {
        setState(() {
          _failed = false;
          _message = '订单 $no 已取消';
        });
      }
    } on ApiFailure catch (f) {
      if (mounted) {
        setState(() {
          _failed = true;
          _message = f.message;
        });
      }
    }
    _load();
  }

  Future<void> _open(String no) async {
    await context.push('/orders/${Uri.encodeComponent(no)}');
    if (mounted) _load();
  }

  @override
  Widget build(BuildContext context) {
    return Scaffold(
      appBar: AppBar(title: const Text('我的订单'), backgroundColor: KeelColors.bg, surfaceTintColor: KeelColors.bg),
      body: !_session.loggedIn
          ? Center(
              child: Column(mainAxisSize: MainAxisSize.min, children: [
                const Text('登录后查看你的订单', style: KeelText.sub),
                const SizedBox(height: 16),
                FilledButton(key: const Key('orders.login'), onPressed: () => context.push('/login'), child: const Text('登录')),
              ]),
            )
          : RefreshIndicator(
              onRefresh: _load,
              child: ListView(padding: const EdgeInsets.fromLTRB(16, 4, 16, 24), children: [
                if (_error.isNotEmpty) ErrorCard(message: _error, onRetry: _load),
                if (_message.isNotEmpty)
                  Padding(padding: const EdgeInsets.only(bottom: 10),
                      child: Text(_message, key: const Key('orders.message'), style: _failed ? KeelText.err : KeelText.ok)),
                for (final r in _rows) _row(r),
                if (_loaded && _rows.isEmpty && _error.isEmpty)
                  Padding(
                    padding: const EdgeInsets.only(top: 80),
                    child: Column(children: [
                      const Text('还没有订单，去挑点好东西吧', key: Key('orders.empty'), style: KeelText.sub),
                      const SizedBox(height: 12),
                      OutlinedButton(onPressed: () => context.go('/'), child: const Text('去逛逛')),
                    ]),
                  ),
              ]),
            ),
    );
  }

  Widget _row(OrderRow r) => GestureDetector(
        key: Key('orders.row.${r.orderNo}'),
        onTap: () => _open(r.orderNo),
        child: Container(
          margin: const EdgeInsets.only(top: 12),
          padding: const EdgeInsets.all(18),
          decoration: BoxDecoration(color: KeelColors.card, borderRadius: BorderRadius.circular(16)),
          child: Column(crossAxisAlignment: CrossAxisAlignment.start, children: [
            Row(children: [
              ToneBadge(key: Key('orders.status.${r.orderNo}'), text: r.statusText, tone: r.statusTone),
              if (r.refundText.isNotEmpty) ...[const SizedBox(width: 6), ToneBadge(text: r.refundText, tone: 'refund')],
              const Spacer(),
              Text(r.createdAt, style: KeelText.hint),
            ]),
            const SizedBox(height: 12),
            Row(children: [
              Expanded(
                child: Column(crossAxisAlignment: CrossAxisAlignment.start, children: [
                  const Text('订单号', style: KeelText.hint),
                  Text(r.orderNo, style: const TextStyle(fontFamily: 'monospace', fontSize: 13)),
                ]),
              ),
              Text(r.payableText, style: KeelText.price),
            ]),
            if (r.payable) ...[
              const Divider(color: KeelColors.line),
              Row(children: [
                const Text('等待付款', style: KeelText.sub),
                const Spacer(),
                TextButton(key: Key('orders.cancel.${r.orderNo}'), onPressed: () => _cancel(r.orderNo),
                    child: Text(_armed == r.orderNo ? '确认取消？' : '取消订单')),
                TextButton(onPressed: () => _open(r.orderNo), child: const Text('去支付 ›')),
              ]),
            ],
          ]),
        ),
      );
}
