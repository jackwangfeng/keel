import 'package:flutter/material.dart';
import 'package:go_router/go_router.dart';

import '../api/client.dart';
import '../api/refund.dart';
import '../api/services.dart';
import '../theme.dart';
import '../widgets/badge.dart';
import '../widgets/states.dart';

/// 售后 / 退款列表。
class RefundsPage extends StatefulWidget {
  const RefundsPage({super.key});
  @override
  State<RefundsPage> createState() => _RefundsPageState();
}

class _RefundsPageState extends State<RefundsPage> {
  List<RefundRow> _rows = const [];
  bool _loaded = false;
  String _error = '';
  bool _started = false;

  @override
  void didChangeDependencies() {
    super.didChangeDependencies();
    if (!_started) {
      _started = true;
      _load();
    }
  }

  Future<void> _load() async {
    try {
      final rows = await fetchRefunds(Services.of(context).client);
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

  @override
  Widget build(BuildContext context) => Scaffold(
        appBar: AppBar(title: const Text('售后 / 退款', style: KeelText.title), backgroundColor: KeelColors.bg, surfaceTintColor: KeelColors.bg),
        body: RefreshIndicator(
          onRefresh: _load,
          child: ListView(padding: const EdgeInsets.fromLTRB(16, 4, 16, 24), children: [
            if (_error.isNotEmpty) ErrorCard(message: _error, onRetry: _load),
            for (final r in _rows) RefundTile(row: r, onTap: () => context.push('/refunds/${Uri.encodeComponent(r.refundNo)}').then((_) => _load())),
            if (_loaded && _rows.isEmpty && _error.isEmpty) const EmptyState(key: Key('refunds.empty'), text: '还没有售后申请'),
          ]),
        ),
      );
}

class RefundTile extends StatelessWidget {
  const RefundTile({super.key, required this.row, required this.onTap});
  final RefundRow row;
  final VoidCallback onTap;
  @override
  Widget build(BuildContext context) => GestureDetector(
        key: Key('refunds.row.${row.refundNo}'),
        onTap: onTap,
        child: Container(
          margin: const EdgeInsets.only(top: 12),
          padding: const EdgeInsets.all(18),
          decoration: BoxDecoration(color: KeelColors.card, borderRadius: BorderRadius.circular(16)),
          child: Column(crossAxisAlignment: CrossAxisAlignment.start, children: [
            Row(children: [
              Expanded(child: Text(row.typeText, style: KeelText.body)),
              ToneBadge(key: Key('refunds.status.${row.refundNo}'), text: row.statusText, tone: row.statusTone),
            ]),
            const SizedBox(height: 6),
            Text(row.itemsText, style: KeelText.sub),
            const SizedBox(height: 6),
            Row(children: [
              Expanded(child: Text(row.createdAt, style: KeelText.hint)),
              Text(row.amountText, style: KeelText.price),
            ]),
          ]),
        ),
      );
}
