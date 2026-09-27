import 'package:flutter/material.dart';
import 'package:go_router/go_router.dart';

import '../api/client.dart';
import '../api/evidence.dart';
import '../api/order.dart';
import '../api/refund.dart';
import '../api/services.dart';
import '../theme.dart';
import '../widgets/quick_cart.dart';
import '../widgets/states.dart';

/// 一张凭证图：uploading / done / failed。key 是这张图的幂等键，重试复用它。
class _Evidence {
  _Evidence(this.img) : key = newIdempotencyKey();
  final PickedImage img;
  final String key;
  String url = '';
  String state = 'uploading';
}

/// 申请售后：选行与件数、仅退款 / 退货退款（未发货只能仅退款）、原因（「其他」要写说明）、凭证图（最多 9 张）。
class RefundApplyPage extends StatefulWidget {
  const RefundApplyPage({super.key, required this.orderNo});
  final String orderNo;
  @override
  State<RefundApplyPage> createState() => _RefundApplyPageState();
}

class _RefundApplyPageState extends State<RefundApplyPage> {
  List<RefundableItem> _items = const [];
  // _qty[i] 对应 _items[i]：0 = 这一行不退。
  List<int> _qty = const [];
  bool _returnAllowed = false;
  // 0 = 还没选：页面不给默认值（服务端不猜，客户端也不猜）。
  int _type = 0;
  int _reason = 0;
  final _text = TextEditingController();
  final _evidence = <_Evidence>[];
  bool _loaded = false;
  String _error = '';
  String _message = '';
  bool _busy = false;
  // 这张表单的幂等键：超时后再点提交是重放；改了内容再提交是同键异体 422，那时换一个。
  String _key = newIdempotencyKey();
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
    _text.dispose();
    super.dispose();
  }

  Future<void> _load() async {
    try {
      final o = await fetchOrder(Services.of(context).client, widget.orderNo);
      if (!mounted) return;
      final items = refundableItems(o.raw, Services.of(context).client.assetUrl);
      setState(() {
        _items = items;
        _returnAllowed = refundReturnAllowed(o.raw);
        if (!_returnAllowed && _type == 2) _type = 0;
        // 只有一行时默认勾上全部可退件数；多行时让用户自己勾。
        _qty = [for (final it in items) items.length == 1 ? it.maxQty : 0];
        _loaded = true;
      });
    } on ApiFailure catch (f) {
      if (mounted) {
        setState(() {
          _error = f.message;
          _loaded = true;
        });
      }
    }
  }

  bool get _canSubmit {
    if (!_qty.any((q) => q > 0) || _type == 0 || _reason == 0) return false;
    if (_reason == 5 && _text.text.trim().isEmpty) return false;
    // 还有图在传 / 传失败：等它传完，或者让用户删掉失败的那张 —— 不悄悄少交一张凭证。
    return _evidence.every((e) => e.state == 'done');
  }

  Future<void> _pick() async {
    final left = 9 - _evidence.length;
    if (left <= 0) return;
    try {
      final imgs = await pickImages(left);
      if (!mounted) return;
      for (final img in imgs.take(9 - _evidence.length)) {
        final e = _Evidence(img);
        setState(() => _evidence.add(e));
        _upload(e);
      }
    } catch (e) {
      if (mounted) toast(context, '选图失败：$e');
    }
  }

  Future<void> _upload(_Evidence e) async {
    setState(() => e.state = 'uploading');
    try {
      final url = await uploadEvidence(Services.of(context).client, e.img, e.key);
      if (mounted) {
        setState(() {
          e.url = url;
          e.state = 'done';
        });
      }
    } on ApiFailure catch (f) {
      if (mounted) {
        setState(() {
          e.state = 'failed';
          _message = '有一张图片没传上去：${f.message}';
        });
      }
    }
  }

  Future<void> _submit() async {
    if (!_canSubmit || _busy) return;
    final lines = [for (final (i, it) in _items.indexed) if (_qty[i] > 0) (orderItemId: it.orderItemId, quantity: _qty[i])];
    setState(() {
      _busy = true;
      _message = '';
    });
    try {
      final no = await createRefund(Services.of(context).client, widget.orderNo,
          refundBody(lines, refundType: _type, reasonCode: _reason, reasonText: _text.text.trim(),
              evidenceUrls: [for (final e in _evidence) e.url]),
          _key);
      // 替换掉这一页：返回键回到订单详情，不回到这张已经交掉的表单。
      if (mounted) context.pushReplacement('/refunds/${Uri.encodeComponent(no)}');
    } on ApiFailure catch (f) {
      if (!mounted) return;
      setState(() {
        _busy = false;
        _message = f.message;
        if (f.status == 409 || f.status == 422) _key = newIdempotencyKey();
      });
      // 409（件数超了 / 已有在途售后 / 订单状态不能退）：可退件数变了，重读一次。
      if (f.status == 409) _load();
    }
  }

  Widget _chip(String key, String label, bool on, VoidCallback? onTap) => ChoiceChip(
        key: Key(key),
        label: Text(label),
        selected: on,
        onSelected: onTap == null ? null : (_) => onTap(),
      );

  @override
  Widget build(BuildContext context) {
    return Scaffold(
      appBar: AppBar(title: const Text('申请售后', style: KeelText.title), backgroundColor: KeelColors.bg, surfaceTintColor: KeelColors.bg),
      body: !_loaded
          ? const EmptyState(text: '正在加载…')
          : _error.isNotEmpty
              ? ErrorCard(message: _error, onRetry: _load)
              : ListView(padding: const EdgeInsets.fromLTRB(16, 4, 16, 24), children: [
                  if (_items.isEmpty) const EmptyState(key: Key('apply.nothing'), text: '这一单没有可以申请售后的商品了'),
                  for (final (i, it) in _items.indexed)
                    Container(
                      margin: const EdgeInsets.only(bottom: 10),
                      padding: const EdgeInsets.all(12),
                      decoration: BoxDecoration(color: KeelColors.card, borderRadius: BorderRadius.circular(14)),
                      child: Row(children: [
                        Checkbox(key: Key('apply.item.${it.orderItemId}'), value: _qty[i] > 0,
                            onChanged: (_) => setState(() => _qty = [..._qty]..[i] = _qty[i] > 0 ? 0 : it.maxQty)),
                        Expanded(
                          child: Column(crossAxisAlignment: CrossAxisAlignment.start, children: [
                            Text(it.title, style: KeelText.body.copyWith(fontWeight: FontWeight.w700)),
                            if (it.specText.isNotEmpty) Text(it.specText, style: KeelText.hint),
                            Text('${it.priceText} · 最多可退 ${it.maxQty} 件', style: KeelText.hint),
                          ]),
                        ),
                        if (_qty[i] > 0)
                          QtyStepper(value: _qty[i], canMinus: _qty[i] > 1, canPlus: _qty[i] < it.maxQty, keyPrefix: 'apply.${it.orderItemId}',
                              onMinus: () => setState(() => _qty = [..._qty]..[i] -= 1),
                              onPlus: () => setState(() => _qty = [..._qty]..[i] += 1)),
                      ]),
                    ),
                  if (_items.isNotEmpty) ...[
                    const Text('售后类型', style: KeelText.section),
                    const SizedBox(height: 6),
                    Wrap(spacing: 8, children: [
                      _chip('apply.type.1', '仅退款', _type == 1, () => setState(() => _type = 1)),
                      _chip('apply.type.2', '退货退款', _type == 2, _returnAllowed ? () => setState(() => _type = 2) : null),
                    ]),
                    if (!_returnAllowed) const Text('还没发货的订单只能申请仅退款', style: KeelText.hint),
                    const SizedBox(height: 14),
                    const Text('原因', style: KeelText.section),
                    const SizedBox(height: 6),
                    Wrap(spacing: 8, runSpacing: 4, children: [
                      for (var c = 1; c <= 5; c++) _chip('apply.reason.$c', refundReasonText(c), _reason == c, () => setState(() => _reason = c)),
                    ]),
                    TextField(
                      key: const Key('apply.text'),
                      controller: _text,
                      maxLength: 200,
                      maxLines: 3,
                      onChanged: (_) => setState(() {}),
                      decoration: InputDecoration(hintText: _reason == 5 ? '请写明原因（必填）' : '补充说明（选填）'),
                    ),
                    const Text('凭证图片（最多 9 张）', style: KeelText.section),
                    const SizedBox(height: 8),
                    Wrap(spacing: 10, runSpacing: 10, children: [
                      for (final e in _evidence) _evidenceTile(e),
                      if (_evidence.length < 9)
                        GestureDetector(
                          key: const Key('apply.addImage'),
                          onTap: _pick,
                          child: Container(
                            width: 72, height: 72,
                            decoration: BoxDecoration(border: Border.all(color: KeelColors.chipBorder), borderRadius: BorderRadius.circular(10)),
                            child: const Icon(Icons.add_photo_alternate_outlined, color: KeelColors.textHint),
                          ),
                        ),
                    ]),
                  ],
                  if (_message.isNotEmpty)
                    Padding(padding: const EdgeInsets.only(top: 12), child: Text(_message, key: const Key('apply.message'), style: KeelText.err)),
                ]),
      bottomNavigationBar: _items.isEmpty
          ? null
          : SafeArea(
              child: Padding(
                padding: const EdgeInsets.fromLTRB(16, 8, 16, 10),
                child: FilledButton(key: const Key('apply.submit'), onPressed: _canSubmit && !_busy ? _submit : null,
                    child: Text(_busy ? '提交中…' : '提交申请')),
              ),
            ),
    );
  }

  Widget _evidenceTile(_Evidence e) {
    final b = e.img.bytes;
    return Stack(children: [
      ClipRRect(
        borderRadius: BorderRadius.circular(10),
        child: SizedBox(
          width: 72, height: 72,
          child: b != null ? Image.memory(b, fit: BoxFit.cover) : const ColoredBox(color: KeelColors.line, child: Icon(Icons.image_outlined)),
        ),
      ),
      if (e.state != 'done')
        Positioned.fill(
          child: GestureDetector(
            onTap: e.state == 'failed' ? () => _upload(e) : null,
            child: Container(
              decoration: BoxDecoration(color: Colors.black45, borderRadius: BorderRadius.circular(10)),
              alignment: Alignment.center,
              child: Text(e.state == 'failed' ? '重传' : '上传中', style: const TextStyle(color: Colors.white, fontSize: 12)),
            ),
          ),
        ),
      Positioned(
        right: 0,
        top: 0,
        child: GestureDetector(
          onTap: () => setState(() => _evidence.remove(e)),
          child: const CircleAvatar(radius: 9, backgroundColor: Colors.black54, child: Icon(Icons.close, size: 12, color: Colors.white)),
        ),
      ),
    ]);
  }
}
