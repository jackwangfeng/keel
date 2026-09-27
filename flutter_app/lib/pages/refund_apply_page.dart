import 'package:flutter/material.dart';
import 'package:go_router/go_router.dart';

import '../api/client.dart';
import '../api/evidence.dart';
import '../api/order.dart';
import '../api/refund.dart';
import '../api/services.dart';
import '../theme.dart';
import '../widgets/net_image.dart';
import '../widgets/form_bits.dart';
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
        // in-flight：上一次可能正在建这张售后单 —— 键不换，再点就是重放。其余 409 / 422 都是没建成，换键。
        final inFlight = f.isType('idempotency-key-in-flight');
        if (inFlight) _message = '申请正在处理中，稍等几秒再点一次提交';
        if (!inFlight && (f.status == 409 || f.status == 422)) _key = newIdempotencyKey();
      });
      // 409（件数超了 / 已有在途售后 / 订单状态不能退）：可退件数变了，重读一次。
      if (f.status == 409 && !f.isType('idempotency-key-in-flight')) _load();
    }
  }


  @override
  Widget build(BuildContext context) {
    return Scaffold(
      appBar: AppBar(title: const Text('申请售后'), backgroundColor: KeelColors.bg, surfaceTintColor: KeelColors.bg),
      body: !_loaded
          ? const EmptyState(text: '正在加载…')
          : _error.isNotEmpty
              ? ErrorCard(message: _error, onRetry: _load)
              : ListView(padding: const EdgeInsets.fromLTRB(16, 0, 16, 24), children: [
                  if (_items.isEmpty) const EmptyState(key: Key('apply.nothing'), text: '这一单没有可以申请售后的商品了'),
                  if (_items.isNotEmpty)
                    KeelCard(
                      title: '退哪几件',
                      child: Column(children: [
                        for (final (i, it) in _items.indexed)
                          Padding(
                            padding: EdgeInsets.only(top: i == 0 ? 0 : 14),
                            child: Row(crossAxisAlignment: CrossAxisAlignment.start, children: [
                              GestureDetector(
                                key: Key('apply.item.${it.orderItemId}'),
                                onTap: () => setState(() => _qty = [..._qty]..[i] = _qty[i] > 0 ? 0 : it.maxQty),
                                child: Padding(
                                  padding: const EdgeInsets.only(top: 19, right: 12),
                                  child: _check(_qty[i] > 0),
                                ),
                              ),
                              ClipRRect(
                                borderRadius: BorderRadius.circular(10),
                                child: SizedBox(
                                  width: 60, height: 60,
                                  child: it.cover.imageUrl.isNotEmpty
                                      ? NetImage(url: it.cover.imageUrl, fallback: ColoredBox(color: it.cover.color))
                                      : ColoredBox(color: it.cover.color,
                                          child: Center(child: Text(it.cover.glyph, style: const TextStyle(fontSize: 24, color: KeelColors.card)))),
                                ),
                              ),
                              const SizedBox(width: 12),
                              Expanded(
                                child: Column(crossAxisAlignment: CrossAxisAlignment.start, children: [
                                  Text(it.title, style: KeelText.body),
                                  Text('${it.specText.isEmpty ? '' : '${it.specText} · '}${it.priceText} · 可退 ${it.maxQty} 件', style: KeelText.hint),
                                  if (_qty[i] > 0)
                                    Padding(
                                      padding: const EdgeInsets.only(top: 8),
                                      child: QtyStepper(value: _qty[i], canMinus: _qty[i] > 1, canPlus: _qty[i] < it.maxQty,
                                          keyPrefix: 'apply.${it.orderItemId}',
                                          onMinus: () => setState(() => _qty = [..._qty]..[i] -= 1),
                                          onPlus: () => setState(() => _qty = [..._qty]..[i] += 1)),
                                    ),
                                ]),
                              ),
                            ]),
                          ),
                      ]),
                    ),
                  if (_items.isNotEmpty) ...[
                    KeelCard(
                      title: '售后类型',
                      child: Column(crossAxisAlignment: CrossAxisAlignment.start, children: [
                        Wrap(spacing: 10, runSpacing: 10, children: [
                          OptChip(key: const Key('apply.type.1'), label: '仅退款', on: _type == 1, onTap: () => setState(() => _type = 1)),
                          OptChip(key: const Key('apply.type.2'), label: '退货退款', on: _type == 2,
                              onTap: _returnAllowed ? () => setState(() => _type = 2) : null),
                        ]),
                        if (!_returnAllowed)
                          const Padding(padding: EdgeInsets.only(top: 10), child: Text('订单还没发货，只能申请仅退款', style: KeelText.hint)),
                      ]),
                    ),
                    KeelCard(
                      title: '原因',
                      child: Column(crossAxisAlignment: CrossAxisAlignment.start, children: [
                        Wrap(spacing: 10, runSpacing: 10, children: [
                          for (var c = 1; c <= 5; c++)
                            OptChip(key: Key('apply.reason.$c'), label: refundReasonText(c), on: _reason == c, onTap: () => setState(() => _reason = c)),
                        ]),
                        const SizedBox(height: 8),
                        TextField(
                          key: const Key('apply.text'),
                          controller: _text,
                          maxLength: 200,
                          onChanged: (_) => setState(() {}),
                          style: inlineInputStyle,
                          decoration: InputDecoration(
                            hintText: _reason == 5 ? '补充说明（选「其他」时必填）' : '补充说明（选填）',
                            hintStyle: KeelText.hint.copyWith(fontSize: 15),
                            counterText: '',
                            enabledBorder: const UnderlineInputBorder(borderSide: BorderSide(color: KeelColors.line)),
                            focusedBorder: const UnderlineInputBorder(borderSide: BorderSide(color: KeelColors.primary)),
                          ),
                        ),
                      ]),
                    ),
                    KeelCard(
                      title: '凭证图片（选填，最多 9 张）',
                      trailing: Text('${_evidence.length}/9', style: KeelText.hint),
                      child: Wrap(spacing: 10, runSpacing: 10, children: [
                        for (final e in _evidence) _evidenceTile(e),
                        if (_evidence.length < 9)
                          GestureDetector(
                            key: const Key('apply.addImage'),
                            onTap: _pick,
                            child: CustomPaint(
                              painter: _Dashed(),
                              child: Container(
                                width: 72, height: 72,
                                alignment: Alignment.center,
                                decoration: BoxDecoration(color: const Color(0xFFF3ECE3), borderRadius: BorderRadius.circular(10)),
                                child: const Text('+', style: TextStyle(fontSize: 26, color: KeelColors.textHint)),
                              ),
                            ),
                          ),
                      ]),
                    ),
                  ],
                  if (_message.isNotEmpty)
                    KeelCard(color: const Color(0xFFF8EAE5), child: Text(_message, key: const Key('apply.message'), style: KeelText.err)),
                ]),
      bottomNavigationBar: _items.isEmpty
          ? null
          : Container(
              decoration: const BoxDecoration(color: KeelColors.card, border: Border(top: BorderSide(color: KeelColors.line))),
              child: SafeArea(
                top: false,
                child: Padding(
                  padding: const EdgeInsets.fromLTRB(16, 10, 16, 10),
                  child: Row(children: [
                    // 请求体不带金额：每行实退多少由服务端按优惠分摊算。
                    const Expanded(child: Text('退款金额以审核结果为准', style: KeelText.hint)),
                    SizedBox(
                      width: 140,
                      child: FilledButton(key: const Key('apply.submit'), onPressed: _canSubmit && !_busy ? _submit : null,
                          child: Text(_busy ? '提交中…' : '提交申请')),
                    ),
                  ]),
                ),
              ),
            ),
    );
  }

  Widget _check(bool on) => Container(
        width: 22, height: 22,
        decoration: BoxDecoration(
          shape: BoxShape.circle,
          color: on ? KeelColors.primary : null,
          border: Border.all(color: on ? KeelColors.primary : const Color(0xFFCFC3B5)),
        ),
        child: on ? const Icon(Icons.check, size: 14, color: KeelColors.card) : null,
      );

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

/// 加图格子的虚线框（uni-app x 的 .ev-add：1 像素虚线）。
class _Dashed extends CustomPainter {
  @override
  void paint(Canvas canvas, Size size) {
    final p = Paint()
      ..color = const Color(0xFFCFC3B5)
      ..style = PaintingStyle.stroke
      ..strokeWidth = 1;
    final path = Path()..addRRect(RRect.fromRectAndRadius(Offset.zero & size, const Radius.circular(10)));
    for (final m in path.computeMetrics()) {
      for (var d = 0.0; d < m.length; d += 7) {
        canvas.drawPath(m.extractPath(d, d + 4), p);
      }
    }
  }

  @override
  bool shouldRepaint(covariant CustomPainter oldDelegate) => false;
}
