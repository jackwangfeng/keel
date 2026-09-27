import 'package:flutter/material.dart';
import 'package:go_router/go_router.dart';

import '../api/client.dart';
import '../api/refund.dart';
import '../api/services.dart';
import '../theme.dart';
import '../widgets/badge.dart';
import '../widgets/evidence_image.dart';
import '../widgets/states.dart';

/// 售后详情：状态说明、驳回理由（可重新申请）、寄回物流（退货退款 + 待买家退货时可填可改）、
/// 商品与退款金额、凭证图（带令牌读）、撤回（点两下）。
class RefundPage extends StatefulWidget {
  const RefundPage({super.key, required this.refundNo});
  final String refundNo;
  @override
  State<RefundPage> createState() => _RefundPageState();
}

class _RefundPageState extends State<RefundPage> {
  RefundDetailView? _v;
  String _error = '';
  String _message = '';
  bool _failed = false;
  bool _busy = false;
  bool _armed = false;
  final _cancelKey = newIdempotencyKey();
  String _carrier = '';
  final _tracking = TextEditingController();
  bool _shipping = false;
  // 物流表单的幂等键。提交成功后换一个：再改一次是一次新的请求，不是重放。
  String _shipKey = newIdempotencyKey();
  bool _started = false;

  @override
  void didChangeDependencies() {
    super.didChangeDependencies();
    if (!_started) {
      _started = true;
      _load();
    }
  }

  @override
  void dispose() {
    _tracking.dispose();
    super.dispose();
  }

  void _apply(RefundDetailView v) {
    _v = v;
    // 表单预填已提交的那一份，改起来方便。
    if (_carrier.isEmpty && v.returnCarrierCode.isNotEmpty) _carrier = v.returnCarrierCode;
    if (_tracking.text.isEmpty && v.returnTrackingNo.isNotEmpty) _tracking.text = v.returnTrackingNo;
  }

  Future<void> _load() async {
    try {
      final v = await fetchRefund(Services.of(context).client, widget.refundNo);
      if (mounted) {
        setState(() {
          _apply(v);
          _error = '';
        });
      }
    } on ApiFailure catch (f) {
      if (mounted) setState(() => _error = f.message);
    }
  }

  Future<void> _cancel() async {
    if (_busy) return;
    if (!_armed) return setState(() => _armed = true);
    setState(() {
      _armed = false;
      _busy = true;
      _message = '';
    });
    try {
      final v = await cancelRefund(Services.of(context).client, widget.refundNo, _cancelKey);
      if (!mounted) return;
      setState(() {
        _busy = false;
        _failed = false;
        _message = '已撤回';
        _apply(v);
      });
    } on ApiFailure catch (f) {
      // 多半是商家刚审完：照实说，并刷新成现在的状态。
      if (!mounted) return;
      setState(() {
        _busy = false;
        _failed = true;
        _message = f.message;
      });
      _load();
    }
  }

  Future<void> _ship() async {
    final no = _tracking.text.trim();
    if (_shipping || _carrier.isEmpty || no.isEmpty) return;
    setState(() {
      _shipping = true;
      _message = '';
    });
    try {
      final v = await submitReturnShipment(Services.of(context).client, widget.refundNo, _carrier, no, _shipKey);
      if (!mounted) return;
      setState(() {
        _shipping = false;
        _failed = false;
        _message = '物流信息已提交，等待商家确认收货';
        _shipKey = newIdempotencyKey();
        _apply(v);
      });
    } on ApiFailure catch (f) {
      if (!mounted) return;
      setState(() {
        _shipping = false;
        _failed = true;
        _message = f.message;
        if (f.status == 422) _shipKey = newIdempotencyKey();
      });
      // 409 refund-status-not-returnable：商家刚确认收货 / 状态变了，刷新。
      if (f.status == 409) _load();
    }
  }

  @override
  Widget build(BuildContext context) {
    final v = _v;
    return Scaffold(
      appBar: AppBar(title: const Text('售后详情'), backgroundColor: KeelColors.bg, surfaceTintColor: KeelColors.bg),
      body: v == null
          ? (_error.isNotEmpty ? ErrorCard(message: _error, onRetry: _load) : const EmptyState(text: '正在加载…'))
          : ListView(padding: const EdgeInsets.fromLTRB(16, 4, 16, 24), children: [
              Padding(
                padding: const EdgeInsets.fromLTRB(4, 8, 4, 12),
                child: Column(crossAxisAlignment: CrossAxisAlignment.start, children: [
                  Row(children: [
                    Text(v.head.statusText, key: const Key('refund.status'), style: KeelText.display),
                    const SizedBox(width: 8),
                    ToneBadge(text: v.head.typeText, tone: 'muted'),
                  ]),
                  Text(v.statusLine, style: KeelText.sub),
                ]),
              ),
              if (v.rejected)
                _card(color: const Color(0xFFF8EAE5), Column(crossAxisAlignment: CrossAxisAlignment.start, children: [
                  const Text('驳回理由', style: KeelText.overline),
                  Text(v.rejectReason.isNotEmpty ? v.rejectReason : '商家没有填写理由', key: const Key('refund.reject'),
                      style: KeelText.err.copyWith(fontSize: 15)),
                  const SizedBox(height: 10),
                  OutlinedButton(key: const Key('refund.reapply'),
                      onPressed: () => context.pushReplacement('/orders/${Uri.encodeComponent(v.head.orderNo)}/refund'),
                      child: const Text('重新申请')),
                ])),
              if (v.awaitingReturn)
                _card(color: const Color(0xFFF6EAD3), Column(crossAxisAlignment: CrossAxisAlignment.start, children: [
                  const Text('请把商品寄回商家', style: TextStyle(fontWeight: FontWeight.w700, color: KeelColors.warn)),
                  const Text('寄出后填写物流单号，商家确认收到退货后会退款。', style: KeelText.sub),
                  if (v.returnDeadlineAt.isNotEmpty && v.returnTrackingNo.isEmpty)
                    Text('请在 ${v.returnDeadlineAt} 前寄回并填写物流单号，逾期售后将自动关闭', key: const Key('refund.deadline'), style: KeelText.sub),
                ])),
              if (v.canFillReturn)
                _card(Column(crossAxisAlignment: CrossAxisAlignment.start, children: [
                  const Text('寄回物流', style: KeelText.overline),
                  if (v.returnTrackingNo.isNotEmpty)
                    Text('${v.returnCarrierText} ${v.returnTrackingNo}（${v.returnSubmittedAt} 提交）', key: const Key('refund.returnFilled'),
                        style: KeelText.body),
                  const SizedBox(height: 8),
                  Wrap(spacing: 8, runSpacing: 4, children: [
                    for (final c in carriers)
                      ChoiceChip(key: Key('refund.carrier.${c.code}'), label: Text(c.name), selected: _carrier == c.code,
                          onSelected: (_) => setState(() => _carrier = c.code)),
                  ]),
                  TextField(key: const Key('refund.tracking'), controller: _tracking, maxLength: 64, onChanged: (_) => setState(() {}),
                      decoration: const InputDecoration(labelText: '运单号', hintText: '快递单上的单号')),
                  FilledButton(
                    key: const Key('refund.ship'),
                    onPressed: _carrier.isNotEmpty && _tracking.text.trim().isNotEmpty && !_shipping ? _ship : null,
                    child: Text(_shipping ? '提交中…' : (v.returnTrackingNo.isNotEmpty ? '修改物流信息' : '提交物流信息')),
                  ),
                ])),
              _card(Column(crossAxisAlignment: CrossAxisAlignment.start, children: [
                for (final it in v.items)
                  Padding(
                    padding: const EdgeInsets.symmetric(vertical: 4),
                    child: Row(children: [
                      Expanded(child: Text('${it.title} × ${it.quantity}', style: KeelText.body)),
                      Text(it.amountText, style: KeelText.price),
                    ]),
                  ),
                const Divider(height: 29, color: KeelColors.line),
                _kv('退款金额', v.head.amountText, key: 'refund.amount'),
                _kv('售后类型', v.head.typeText),
                if (v.reasonText.isNotEmpty) _kv('原因', v.reasonText),
                if (v.refundedAt.isNotEmpty) _kv('到账时间', v.refundedAt),
                if (v.channelRefundId.isNotEmpty) _kv('退款流水号', v.channelRefundId),
                if (v.evidenceUrls.isNotEmpty)
                  Padding(
                    padding: const EdgeInsets.only(top: 10),
                    child: Wrap(spacing: 10, runSpacing: 10, children: [
                      for (final (i, u) in v.evidenceUrls.indexed) EvidenceImage(key: Key('refund.evidence.$i'), url: u),
                    ]),
                  ),
              ])),
              if (v.canCancel)
                Align(
                  alignment: Alignment.centerRight,
                  child: OutlinedButton(key: const Key('refund.cancel'), onPressed: _busy ? null : _cancel,
                      child: Text(_armed ? '再点一次确认撤回' : '撤回申请')),
                ),
              if (_message.isNotEmpty)
                Padding(padding: const EdgeInsets.only(top: 8),
                    child: Text(_message, key: const Key('refund.message'), style: _failed ? KeelText.err : KeelText.ok)),
              const SizedBox(height: 16),
              _kv('售后单号', v.head.refundNo),
              GestureDetector(
                key: const Key('refund.order'),
                onTap: () => context.push('/orders/${Uri.encodeComponent(v.head.orderNo)}'),
                child: _kv('订单号', '${v.head.orderNo} ›'),
              ),
              _kv('申请时间', v.head.createdAt),
            ]),
    );
  }

  Widget _card(Widget child, {Color color = KeelColors.card}) => Container(
        margin: const EdgeInsets.only(top: 12),
        padding: const EdgeInsets.all(18),
        decoration: BoxDecoration(color: color, borderRadius: BorderRadius.circular(16)),
        child: child,
      );

  Widget _kv(String k, String v, {String? key}) => Padding(
        padding: const EdgeInsets.symmetric(vertical: 6),
        child: Row(crossAxisAlignment: CrossAxisAlignment.start, children: [
          Text(k, style: KeelText.sub),
          const SizedBox(width: 16),
          Expanded(child: Text(v, key: key == null ? null : Key(key), textAlign: TextAlign.right, style: KeelText.body)),
        ]),
      );
}
