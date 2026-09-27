import 'package:flutter/material.dart';
import 'package:go_router/go_router.dart';

import '../api/client.dart';
import '../api/coupon.dart';
import '../api/services.dart';
import '../theme.dart';
import '../widgets/states.dart';
import '../widgets/ticket.dart';

/// 领券中心。
class CouponCenterPage extends StatefulWidget {
  const CouponCenterPage({super.key});
  @override
  State<CouponCenterPage> createState() => _CouponCenterPageState();
}

class _CouponCenterPageState extends State<CouponCenterPage> {
  List<ClaimRow> _rows = const [];
  bool _loaded = false;
  String _error = '';
  String _message = '';
  bool _failed = false;
  // 正在领的那张；领的时候按钮不响应第二下。
  int _claiming = 0;
  // 每张模板一个幂等键，页面活着期间不变。
  final _keys = <int, String>{};
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
    if (!Services.of(context).session.loggedIn) return setState(() {});
    try {
      final rows = await fetchClaimable(Services.of(context).client);
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

  Future<void> _claim(ClaimRow r) async {
    if (_claiming > 0 || !r.canClaim) return;
    setState(() {
      _claiming = r.id;
      _message = '';
    });
    try {
      final got = await claimCoupon(Services.of(context).client, r.id, _keys.putIfAbsent(r.id, newIdempotencyKey));
      if (!mounted) return;
      setState(() {
        _failed = false;
        _message = got.replayed ? '「${got.name}」刚才已经领到了' : '已领取「${got.name}」，下单时自动可用';
      });
    } on ApiFailure catch (f) {
      if (mounted) {
        setState(() {
          _failed = true;
          _message = f.message;
        });
      }
    }
    if (!mounted) return;
    setState(() => _claiming = 0);
    // 按钮与剩余张数都是服务端算的：重新拉一次。
    _load();
  }

  Future<void> _login() async {
    await context.push('/login');
    if (mounted) _load();
  }

  @override
  Widget build(BuildContext context) {
    final loggedIn = Services.of(context).session.loggedIn;
    return Scaffold(
      appBar: AppBar(title: const Text('领券中心'), backgroundColor: KeelColors.bg, surfaceTintColor: KeelColors.bg),
      body: !loggedIn
          ? Center(
              child: Column(mainAxisSize: MainAxisSize.min, children: [
                const Text('登录后领取优惠券', style: KeelText.sub),
                const SizedBox(height: 16),
                FilledButton(onPressed: _login, child: const Text('登录')),
              ]),
            )
          : ListView(padding: const EdgeInsets.fromLTRB(16, 4, 16, 24), children: [
              if (_message.isNotEmpty)
                Padding(padding: const EdgeInsets.only(bottom: 10),
                    child: Text(_message, key: const Key('center.message'), style: _failed ? KeelText.err : KeelText.ok)),
              if (_error.isNotEmpty) ErrorCard(message: _error, onRetry: _load),
              for (final r in _rows)
                Ticket(
                  key: Key('center.row.${r.id}'),
                  value: r.valueText,
                  name: r.name,
                  rule: r.ruleText,
                  meta: '${r.scopeText} · ${r.validText}',
                  foot: Row(children: [
                    Text(r.remainingText, style: KeelText.hint),
                    const Spacer(),
                    // 小胶囊按钮（uni-app x 的 .claim-btn）：能领深咖底白字，不能领浅灰底灰字。
                    GestureDetector(
                      key: Key('center.claim.${r.id}'),
                      onTap: r.canClaim && _claiming == 0 ? () => _claim(r) : null,
                      child: Container(
                        padding: const EdgeInsets.symmetric(horizontal: 16, vertical: 6),
                        decoration: BoxDecoration(
                          color: r.canClaim ? KeelColors.primary : const Color(0xFFEEE9E3),
                          borderRadius: BorderRadius.circular(14),
                        ),
                        child: Text(_claiming == r.id ? '领取中…' : r.actionText, key: Key('center.action.${r.id}'),
                            style: TextStyle(fontSize: 13, color: r.canClaim ? KeelColors.card : KeelColors.textSub)),
                      ),
                    ),
                  ]),
                ),
              if (_loaded && _rows.isEmpty && _error.isEmpty) const EmptyState(text: '暂时没有可以领的券'),
              Padding(
                padding: const EdgeInsets.symmetric(vertical: 24),
                child: Center(child: GestureDetector(key: const Key('center.mine'), onTap: () => context.push('/coupons'),
                    child: const Text('我的优惠券 ›', style: KeelText.link))),
              ),
            ]),
    );
  }
}
