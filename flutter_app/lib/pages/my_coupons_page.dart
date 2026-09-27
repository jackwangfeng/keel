import 'package:flutter/material.dart';
import 'package:go_router/go_router.dart';

import '../api/client.dart';
import '../api/coupon.dart';
import '../api/services.dart';
import '../theme.dart';
import '../widgets/states.dart';
import '../widgets/ticket.dart';

/// 我的优惠券：未使用 / 使用中（待支付订单占着）/ 已使用 / 已过期。
class MyCouponsPage extends StatefulWidget {
  const MyCouponsPage({super.key});
  @override
  State<MyCouponsPage> createState() => _MyCouponsPageState();
}

class _MyCouponsPageState extends State<MyCouponsPage> {
  int _tab = 0;
  List<CouponRow> _rows = const [];
  bool _loaded = false;
  String _error = '';
  bool _started = false;

  static const _empty = ['没有可用的券', '没有待支付订单占用的券', '还没有用过券', '没有过期的券'];

  @override
  void didChangeDependencies() {
    super.didChangeDependencies();
    if (!_started) {
      _started = true;
      _load();
    }
  }

  Future<void> _load() async {
    // 连着切两个 tab 时只认最后一次。
    final asked = _tab;
    try {
      final rows = await fetchMyCoupons(Services.of(context).client, couponTabs[asked].$1);
      if (!mounted || asked != _tab) return;
      setState(() {
        _rows = rows;
        _loaded = true;
        _error = '';
      });
    } on ApiFailure catch (f) {
      if (!mounted || asked != _tab) return;
      setState(() {
        _error = f.message;
        _loaded = true;
      });
    }
  }

  void _pick(int i) {
    if (i == _tab) return;
    setState(() {
      _tab = i;
      _rows = const [];
      _loaded = false;
    });
    _load();
  }

  @override
  Widget build(BuildContext context) {
    return Scaffold(
      appBar: AppBar(
        title: const Text('我的优惠券'),
        backgroundColor: KeelColors.bg,
        surfaceTintColor: KeelColors.bg,
      ),
      body: Column(children: [
        // 白底 tab 栏，选中的下面一条 2 像素深咖线（uni-app x 的 .tabs / .tab-on）。
        Container(
          padding: const EdgeInsets.symmetric(horizontal: 8),
          decoration: const BoxDecoration(color: KeelColors.card, border: Border(bottom: BorderSide(color: KeelColors.line))),
          child: Row(children: [
            for (final (i, t) in couponTabs.indexed)
              Expanded(
                child: InkWell(
                  key: Key('coupons.tab.${t.$1}'),
                  onTap: () => _pick(i),
                  child: Container(
                    padding: const EdgeInsets.fromLTRB(0, 12, 0, 10),
                    alignment: Alignment.center,
                    decoration: BoxDecoration(
                        border: Border(bottom: BorderSide(width: 2, color: i == _tab ? KeelColors.primary : KeelColors.card))),
                    child: Text(t.$2,
                        style: TextStyle(fontSize: 14, fontWeight: i == _tab ? FontWeight.w700 : FontWeight.w400,
                            color: i == _tab ? KeelColors.text : KeelColors.textSub)),
                  ),
                ),
              ),
          ]),
        ),
        Expanded(
          child: ListView(padding: const EdgeInsets.fromLTRB(16, 4, 16, 24), children: [
            if (_error.isNotEmpty) ErrorCard(message: _error, onRetry: _load),
            if (_tab == 1 && _rows.isNotEmpty)
              const Padding(padding: EdgeInsets.only(top: 12), child: Text('这些券已用在待支付的订单上，订单取消后会退回', style: KeelText.hint)),
            for (final r in _rows)
              Ticket(key: Key('coupons.row.${r.id}'), value: r.valueText, name: r.name, rule: r.ruleText,
                  meta: r.scopeText, meta2: r.validText, dim: _tab >= 2),
            if (_loaded && _rows.isEmpty && _error.isEmpty) EmptyState(key: const Key('coupons.empty'), text: _empty[_tab]),
            if (_loaded)
              Padding(
                padding: const EdgeInsets.symmetric(vertical: 24),
                child: Center(child: GestureDetector(key: const Key('coupons.center'), onTap: () => context.push('/coupon-center'),
                    child: const Text('领券中心 ›', style: KeelText.link))),
              ),
          ]),
        ),
      ]),
    );
  }
}
