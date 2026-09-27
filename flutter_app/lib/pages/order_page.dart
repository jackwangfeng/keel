import 'package:flutter/material.dart';
import 'package:go_router/go_router.dart';

import '../api/client.dart';
import '../api/order.dart';
import '../api/services.dart';
import '../theme.dart';
import '../widgets/badge.dart';
import '../widgets/pay_bar.dart';
import '../widgets/states.dart';
import 'refunds_page.dart';

/// 订单详情：状态说明、收货信息、商品与金额明细、支付记录、沙箱支付、取消 / 确认收货（都是点两下）。
class OrderPage extends StatefulWidget {
  const OrderPage({super.key, required this.orderNo});
  final String orderNo;
  @override
  State<OrderPage> createState() => _OrderPageState();
}

const _channels = [
  (key: 'wechat', name: '微信支付', glyph: '微', color: Color(0xFF6F9A6A)),
  (key: 'alipay', name: '支付宝', glyph: '支', color: Color(0xFF6A8BB0)),
  (key: 'balance', name: '余额', glyph: '余', color: Color(0xFFB5733A)),
];

class _OrderPageState extends State<OrderPage> {
  OrderDetailView? _v;
  String _error = '';
  String _channel = 'wechat';
  // 支付 / 取消 / 确认收货：各自一个幂等键，进页面生成一次，重试复用。
  final _payKey = newIdempotencyKey();
  final _cancelKey = newIdempotencyKey();
  final _confirmKey = newIdempotencyKey();
  bool _paying = false;
  String _payMessage = '';
  bool _payFailed = false;
  String _sandboxNotice = '';
  SandboxSettle? _settle;
  bool _settling = false;
  String _settleMessage = '';
  bool _settleFailed = false;
  String _armed = '';
  bool _acting = false;
  String _actMessage = '';
  bool _actFailed = false;
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
    if (widget.orderNo.isEmpty) return setState(() => _error = '没有指定订单');
    try {
      final v = await fetchOrder(Services.of(context).client, widget.orderNo);
      if (mounted) {
        setState(() {
          _v = v;
          _error = '';
        });
      }
    } on ApiFailure catch (f) {
      if (mounted) setState(() => _error = f.message);
    }
  }

  Future<void> _pay() async {
    if (_paying) return;
    setState(() {
      _paying = true;
      _payFailed = false;
      _payMessage = '';
    });
    try {
      final p = await startPayment(Services.of(context).client, widget.orderNo, _channel, _payKey);
      if (!mounted) return;
      setState(() {
        _paying = false;
        _payMessage = '${p.replayed ? '这笔支付之前已经发起过，本次没有重复发起：' : '已发起支付：'}${p.paymentNo} ${p.amountText}';
        if (!p.sandbox) return;
        // 沙箱写在明面上：看起来像接了真的微信支付就是在骗人。
        _sandboxNotice = p.notice;
        _settle = p.settle;
        if (p.settle == null) _settleMessage = '这个渠道没有沙箱结算指令，只能等真实回调';
      });
    } on ApiFailure catch (f) {
      if (mounted) {
        setState(() {
          _paying = false;
          _payFailed = true;
          _payMessage = f.notImplemented ? '服务器还没有开放支付（${f.status}）' : f.message;
        });
      }
    }
  }

  Future<void> _doSettle() async {
    final s = _settle;
    if (s == null || _settling) return;
    setState(() {
      _settling = true;
      _settleFailed = false;
      _settleMessage = '';
    });
    try {
      await settleSandbox(Services.of(context).client, s);
      if (!mounted) return;
      setState(() {
        _settling = false;
        _settle = null;
        _settleMessage = '回调已入账，订单状态已刷新。';
        _payMessage = '';
      });
      _load();
    } on ApiFailure catch (f) {
      if (mounted) {
        setState(() {
          _settling = false;
          _settleFailed = true;
          _settleMessage = f.message;
        });
      }
    }
  }

  Future<void> _act(String what) async {
    if (_acting) return;
    if (_armed != what) return setState(() => _armed = what);
    final c = Services.of(context).client;
    setState(() {
      _armed = '';
      _acting = true;
      _actMessage = '';
    });
    try {
      if (what == 'cancel') {
        await cancelOrder(c, widget.orderNo, _cancelKey);
      } else {
        await confirmOrder(c, widget.orderNo, _confirmKey);
      }
      if (mounted) {
        setState(() {
          _acting = false;
          _actFailed = false;
          _actMessage = what == 'cancel' ? '订单已取消，库存与优惠券已退回' : '已确认收货';
        });
      }
    } on ApiFailure catch (f) {
      // 多半是刚付完 / 刚超时关闭 / 刚被自动确认：照实说，并刷新成现在的状态。
      if (mounted) {
        setState(() {
          _acting = false;
          _actFailed = true;
          _actMessage = f.message;
        });
      }
    }
    _load();
  }

  @override
  Widget build(BuildContext context) {
    final v = _v;
    final payable = v?.head.payable ?? false;
    return Scaffold(
      appBar: AppBar(title: const Text('订单详情'), backgroundColor: KeelColors.bg, surfaceTintColor: KeelColors.bg),
      body: v == null
          ? (_error.isNotEmpty ? ErrorCard(message: _error, onRetry: _load) : const EmptyState(text: '正在加载…'))
          : ListView(padding: const EdgeInsets.fromLTRB(16, 4, 16, 24), children: [
              Padding(
                padding: const EdgeInsets.fromLTRB(4, 8, 4, 12),
                child: Column(crossAxisAlignment: CrossAxisAlignment.start, children: [
                  Row(children: [
                    Text(v.head.statusText, key: const Key('order.status'), style: KeelText.display),
                    if (v.head.refundText.isNotEmpty) ...[const SizedBox(width: 8), ToneBadge(text: v.head.refundText, tone: 'refund')],
                  ]),
                  Text(v.statusLine, key: const Key('order.statusLine'), style: KeelText.sub),
                ]),
              ),
              if (v.receiver.isNotEmpty)
                _card(Column(crossAxisAlignment: CrossAxisAlignment.start, children: [
                  const Text('收货信息', style: KeelText.overline),
                  const SizedBox(height: 6),
                  Text(v.receiver, key: const Key('order.receiver'), style: KeelText.body),
                ])),
              _card(Column(children: [
                for (final it in v.items)
                  Padding(
                    padding: const EdgeInsets.symmetric(vertical: 6),
                    child: Row(children: [
                      ClipRRect(
                        borderRadius: BorderRadius.circular(10),
                        child: SizedBox(
                          width: 56, height: 56,
                          child: it.cover.imageUrl.isNotEmpty
                              ? Image.network(it.cover.imageUrl, fit: BoxFit.cover, errorBuilder: (_, _, _) => ColoredBox(color: it.cover.color))
                              : ColoredBox(color: it.cover.color,
                                  child: Center(child: Text(it.cover.glyph, style: const TextStyle(fontSize: 20, color: KeelColors.card)))),
                        ),
                      ),
                      const SizedBox(width: 12),
                      Expanded(
                        child: Column(crossAxisAlignment: CrossAxisAlignment.start, children: [
                          Text(it.title, style: KeelText.body.copyWith(fontWeight: FontWeight.w700)),
                          Text('${it.priceText} × ${it.quantity}', style: KeelText.hint),
                        ]),
                      ),
                      Text(it.amountText, style: KeelText.price),
                    ]),
                  ),
                const Divider(height: 29, color: KeelColors.line),
                _kv('商品金额', v.goodsAmountText),
                _kv('运费', v.freightText, note: v.freightNote),
                if (v.promotionDiscountText.isNotEmpty) _kv('活动优惠', v.promotionDiscountText),
                for (final l in v.promotionLines) Align(alignment: Alignment.centerLeft, child: Text(l, style: KeelText.hint)),
                if (v.freightDiscountText.isNotEmpty) _kv('运费抵扣', v.freightDiscountText),
                if (v.couponId > 0)
                  Align(alignment: Alignment.centerLeft,
                      child: Text('用券：${v.couponName.isNotEmpty ? v.couponName : '优惠券'}', key: const Key('order.coupon'), style: KeelText.hint)),
                if (v.hasDiscount) _kv('优惠合计', '-${v.discountText}'),
                if (v.hasPaid) _kv('已付', v.paidText, key: 'order.paid'),
                if (v.refundedText.isNotEmpty) _kv('已退', v.refundedText),
                const Divider(height: 29, color: KeelColors.line),
                Row(children: [
                  const Text('应付', style: KeelText.section),
                  const Spacer(),
                  Text(v.head.payableText, key: const Key('order.payable'), style: KeelText.price),
                ]),
              ])),
              if (v.payments.isNotEmpty)
                _card(Column(crossAxisAlignment: CrossAxisAlignment.start, children: [
                  const Text('支付记录', style: KeelText.overline),
                  for (final p in v.payments) _kv(p.channelText, p.amountText, note: p.statusText),
                ])),
              if (payable)
                _card(Column(crossAxisAlignment: CrossAxisAlignment.start, children: [
                  const Text('支付方式', style: KeelText.overline),
                  for (final c in _channels)
                    ListTile(
                      key: Key('order.channel.${c.key}'),
                      contentPadding: EdgeInsets.zero,
                      leading: Container(
                        width: 28, height: 28, alignment: Alignment.center,
                        decoration: BoxDecoration(color: c.color, borderRadius: BorderRadius.circular(8)),
                        child: Text(c.glyph, style: const TextStyle(fontSize: 13, fontWeight: FontWeight.w700, color: KeelColors.card)),
                      ),
                      title: Text(c.name, style: KeelText.body),
                      trailing: KeelRadio(on: _channel == c.key),
                      onTap: () => setState(() => _channel = c.key),
                    ),
                ])),
              if (_payMessage.isNotEmpty)
                Padding(padding: const EdgeInsets.only(bottom: 10),
                    child: Text(_payMessage, key: const Key('order.payMessage'), style: _payFailed ? KeelText.err : KeelText.ok)),
              if (_sandboxNotice.isNotEmpty)
                Container(
                  key: const Key('order.sandbox'),
                  margin: const EdgeInsets.only(top: 12),
                  padding: const EdgeInsets.symmetric(horizontal: 14, vertical: 12),
                  decoration: BoxDecoration(color: const Color(0xFFFBF1DF), borderRadius: BorderRadius.circular(12)),
                  child: Column(crossAxisAlignment: CrossAxisAlignment.start, children: [
                    const Text('⚠ 沙箱支付，没有真实资金流动', style: TextStyle(fontWeight: FontWeight.w700, color: KeelColors.warn)),
                    Text(_sandboxNotice, style: KeelText.sub),
                    if (_settleMessage.isNotEmpty)
                      Text(_settleMessage, key: const Key('order.settleMessage'), style: _settleFailed ? KeelText.err : KeelText.ok),
                  ]),
                ),
              if (v.refunds.isNotEmpty)
                _card(Column(crossAxisAlignment: CrossAxisAlignment.start, children: [
                  const Text('售后', style: KeelText.overline),
                  const SizedBox(height: 6),
                  for (final r in v.refunds)
                    RefundTile(row: r, onTap: () => context.push('/refunds/${Uri.encodeComponent(r.refundNo)}').then((_) => _load())),
                ])),
              if (v.canCancel || v.canConfirm || v.canRefund)
                Wrap(alignment: WrapAlignment.end, spacing: 8, children: [
                  if (v.canRefund)
                    OutlinedButton(key: const Key('order.refund'),
                        onPressed: () => context.push('/orders/${Uri.encodeComponent(widget.orderNo)}/refund').then((_) => _load()),
                        child: const Text('申请售后')),
                  if (v.canCancel)
                    OutlinedButton(key: const Key('order.cancel'), onPressed: _acting ? null : () => _act('cancel'),
                        child: Text(_armed == 'cancel' ? '再点一次确认取消' : '取消订单')),
                  if (v.canConfirm)
                    FilledButton(key: const Key('order.confirm'), onPressed: _acting ? null : () => _act('confirm'),
                        child: Text(_armed == 'confirm' ? '再点一次确认收货' : '确认收货')),
                ]),
              if (_actMessage.isNotEmpty)
                Padding(padding: const EdgeInsets.only(top: 8),
                    child: Text(_actMessage, key: const Key('order.actMessage'), style: _actFailed ? KeelText.err : KeelText.ok)),
              const SizedBox(height: 16),
              _kv('订单号', widget.orderNo),
              _kv('下单时间', v.head.createdAt),
            ]),
      // 发起支付后底栏直接变「模拟支付完成（沙箱）」：第二个按钮放在首屏以外的话，点完「立即支付」什么都没变。
      bottomNavigationBar: !payable
          ? null
          : PayBar(
              amount: v?.head.payableText ?? '—',
              buttonWidth: 184,
              button: _settle != null
                  ? FilledButton(key: const Key('order.settle'), onPressed: _settling ? null : _doSettle,
                      child: Text(_settling ? '入账中…' : '模拟支付完成（沙箱）', maxLines: 1, overflow: TextOverflow.ellipsis))
                  : FilledButton(key: const Key('order.pay'), onPressed: _paying ? null : _pay, child: Text(_paying ? '处理中…' : '立即支付')),
            ),
    );
  }

  Widget _card(Widget child) => Container(
        margin: const EdgeInsets.only(top: 12),
        padding: const EdgeInsets.all(18),
        decoration: BoxDecoration(color: KeelColors.card, borderRadius: BorderRadius.circular(16)),
        child: child,
      );

  Widget _kv(String k, String v, {String? key, String note = ''}) => Padding(
        padding: const EdgeInsets.symmetric(vertical: 6),
        child: Row(children: [
          Text(k, style: KeelText.sub),
          const Spacer(),
          if (note.isNotEmpty) Padding(padding: const EdgeInsets.only(right: 6), child: Text(note, style: KeelText.hint)),
          Text(v, key: key == null ? null : Key(key), style: KeelText.body),
        ]),
      );
}
