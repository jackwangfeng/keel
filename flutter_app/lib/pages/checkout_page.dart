import 'package:flutter/material.dart';
import 'package:go_router/go_router.dart';

import '../api/address.dart';
import '../api/cart.dart';
import '../api/catalog.dart';
import '../api/client.dart';
import '../api/coupon.dart';
import '../api/order.dart';
import '../api/services.dart';
import '../api/view.dart';
import '../theme.dart';
import '../widgets/quick_cart.dart';

/// 结算：地址（默认地址；没有默认让用户选；一条都没有引导新建）→ 商品行 → 选券 → 金额拆行 → 提交。
/// 从购物车来（fromCart）结算车里「能买且勾选」的行；从详情来是一个规格（数量可调）。
class CheckoutPage extends StatefulWidget {
  const CheckoutPage({super.key, this.fromCart = false, this.skuId = 0, this.productId = 0, this.addressId = 0});
  final bool fromCart;
  /// 指定收货地址（深链 / e2e）；0 = 用默认地址。
  final int addressId;
  final int skuId;
  final int productId;
  @override
  State<CheckoutPage> createState() => _CheckoutPageState();
}

class _CheckoutPageState extends State<CheckoutPage> {
  int _qty = 1;
  // 收货地址。0 = 还没定。
  int _addressId = 0;
  AddressRow? _address;
  bool _addressLoaded = false;
  int _addressCount = 0;
  String _addressError = '';
  List<CartRow> _cartRows = const [];
  bool _cartLoaded = false;
  // 单品直购的展示信息（只用于展示，下单只认 sku_id）。
  String _itemTitle = '';
  String _itemSpec = '';
  String _unitPrice = '';
  Cover? _cover;
  List<UndeliverableLine> _undeliverable = const [];
  bool _provinceUnknown = false;
  // 幂等键：进页面生成一次（这里，且只有这里）。
  String _key = newIdempotencyKey();
  PreviewView? _pv;
  bool _previewing = false;
  String _message = '';
  bool _failed = false;
  bool _busy = false;
  bool _needNewKey = false;
  String _orderNo = '';
  int? _storeId;
  bool _outOfRange = false;
  int _couponId = 0;
  // 还没替用户选过券：第一次试算回来时自动选最省的那张；选过一次（或用户自己选了）就关掉。
  bool _couponAuto = true;
  bool _couponOpen = false;
  // 最近一次成功试算给出的可用券：选的券用不了时 _pv 是 null，这时候列表恰恰最需要还在。
  List<CouponOption> _coupons = const [];
  bool _started = false;

  @override
  void didChangeDependencies() {
    super.didChangeDependencies();
    if (!_started) {
      _started = true;
      _itemTitle = '规格 #${widget.skuId}';
      _addressId = widget.addressId;
      _start();
    }
  }

  Services get _s => Services.of(context);

  Future<void> _start() async {
    if (!_s.session.loggedIn) return setState(() {});
    try {
      final store = await _s.store.ensure();
      if (!mounted) return;
      if (store.storeId == null) {
        // match_type = none：_pv 保持 null，提交按钮因此是灰的。
        return setState(() {
          _outOfRange = true;
          _failed = true;
          _message = '当前位置暂不在配送范围内，无法下单';
        });
      }
      _storeId = store.storeId;
      if (!widget.fromCart) _loadItem();
      await _loadAddresses();
    } on ApiFailure catch (f) {
      if (mounted) {
        setState(() {
          _failed = true;
          _message = f.message;
        });
      }
    }
  }

  Future<void> _loadItem() async {
    if (widget.productId <= 0) return;
    try {
      final d = await fetchProduct(_s.client, widget.productId, _storeId);
      final sku = d.sku(widget.skuId);
      if (!mounted) return;
      setState(() {
        _itemTitle = d.title;
        _cover = d.cover;
        if (sku != null) {
          _itemSpec = '${sku.specName}：${sku.label}';
          _unitPrice = sku.priceText;
        }
      });
    } on ApiFailure {
      // 展示信息拿不到不影响下单：标题留着「规格 #N」。
    }
  }

  /// 每次回来都重读地址簿：在地址页里可能新建、改了、删了。
  Future<void> _loadAddresses() async {
    try {
      final list = await fetchAddresses(_s.client);
      if (!mounted) return;
      AddressRow? found = list.where((a) => a.id == _addressId).firstOrNull;
      // 还没选过（或选的那条被删了）：用默认地址（契约：默认排第一）。第一条不是默认就不替用户挑。
      if (found == null && list.isNotEmpty && list.first.isDefault) found = list.first;
      final next = found?.id ?? 0;
      setState(() {
        _addressLoaded = true;
        _addressError = '';
        _addressCount = list.length;
        _address = found;
        if (next != _addressId) {
          _addressId = next;
          _invalidate();
        }
      });
      // 在飞的那份试算要是给旧地址算的，回来时会被丢掉（见 _runPreview）：这里照样发一份新的。
      if (_addressId > 0 && _pv == null) _preview();
    } on ApiFailure catch (f) {
      if (mounted) {
        setState(() {
          _addressLoaded = true;
          _addressError = '读取收货地址失败：${f.message}';
        });
      }
    }
  }

  Future<void> _pickAddress() async {
    if (!_s.session.loggedIn) return _login();
    if (_addressLoaded && _addressCount == 0) {
      await context.push('/addresses/new');
    } else {
      final picked = await context.push<int>('/addresses?select=1');
      if (picked != null && picked != _addressId) {
        setState(() {
          _addressId = picked;
          _invalidate();
        });
      }
    }
    if (mounted) _loadAddresses();
  }

  Future<void> _login() async {
    await context.push('/login');
    if (mounted) _start();
  }

  /// 改了订单内容就把试算作废：expected_payable_cents 是上一份内容算的，带着它提交会被 409 打回；
  /// 而且下单要用户先看见应付金额再确认，过期的试算不该还摆着。
  void _invalidate() {
    _pv = null;
    if (!_needNewKey) _message = '';
  }

  List<OrderLine> get _lines => widget.fromCart
      ? [for (final r in _cartRows) (skuId: r.skuId, quantity: r.quantity)]
      : [(skuId: widget.skuId, quantity: _qty)];

  void _changeQty(int d) {
    if (_qty + d < 1) return;
    setState(() {
      _qty += d;
      _invalidate();
    });
    _preview();
  }

  Future<void> _preview() async {
    if (!_s.session.loggedIn || _addressId == 0 || _storeId == null) return;
    if (widget.fromCart && !_cartLoaded) {
      // 按同一家门店重读一次车：车在服务端，上一页那份到这里可能已经过时。
      try {
        final v = await fetchCart(_s.client, storeId: _storeId, addressId: _addressId);
        if (!mounted) return;
        setState(() {
          _cartRows = [for (final r in v.rows) if (r.available && r.selected) r];
          _cartLoaded = true;
        });
      } on ApiFailure catch (f) {
        if (mounted) {
          setState(() {
            _failed = true;
            _message = '读取购物车失败：${f.message}';
          });
        }
        return;
      }
    }
    if (_lines.isEmpty) return;
    await _runPreview();
  }

  Future<void> _runPreview() async {
    setState(() => _previewing = true);
    // 连点 + 号、连着换券、换地址时，只有最后一次的结果算数。
    final qty = _qty;
    final coupon = _couponId;
    final addr = _addressId;
    bool stale() => !mounted || qty != _qty || coupon != _couponId || addr != _addressId;
    try {
      final v = await previewOrder(_s.client, lines: _lines, addressId: _addressId, storeId: _storeId!,
          couponId: coupon > 0 ? coupon : null);
      if (stale()) return;
      _coupons = v.coupons;
      if (_couponAuto) {
        _couponAuto = false;
        if (coupon == 0 && v.coupons.isNotEmpty) {
          // 先不带券算一次拿到可用列表，再带上最省的那张算一次；中间那份不摆出来（应付闪一下大再变小没有意义）。
          _couponId = v.coupons.first.id;
          return _runPreview();
        }
      }
      setState(() {
        _previewing = false;
        _pv = v;
        _undeliverable = const [];
        _provinceUnknown = false;
      });
    } on ApiFailure catch (f) {
      if (stale()) return;
      setState(() {
        _previewing = false;
        if (_notDeliverable(f)) return;
        _failed = true;
        if (f.isType('promotion-limit-exceeded')) {
          _message = '超出活动每人限购，请减少数量（${f.message}）';
          return;
        }
        // coupon-not-applicable：detail 就是原因。展开券列表让用户换一张或不用 —— 由他决定，不替他退回原价。
        if (f.isType('coupon-not-applicable')) _couponOpen = true;
        _message = f.message;
      });
    }
  }

  /// 422 region-not-deliverable：逐行标出，并告诉用户能怎么办。调用方已在 setState 里。
  bool _notDeliverable(ApiFailure f) {
    if (!f.isType('region-not-deliverable')) return false;
    _undeliverable = undeliverableLines(f);
    _provinceUnknown = _undeliverable.any((l) => l.provinceUnknown);
    _failed = true;
    _message = _provinceUnknown
        ? '收货地址缺少省份信息，判断不了能不能配送，请补全地址'
        : '有商品送不到这个收货地址，请换一个地址${widget.fromCart ? '，或回购物车去掉标红的商品' : ''}';
    return true;
  }

  String _undeliverableOf(int skuId) => _undeliverable.where((l) => l.skuId == skuId).firstOrNull?.reason ?? '';

  void _pickCoupon(int id) {
    setState(() {
      _couponAuto = false;
      _couponOpen = false;
      if (id == _couponId && _pv != null) return;
      _couponId = id;
      _failed = false;
      _message = '';
      _invalidate();
    });
    _preview();
  }

  Future<void> _submit() async {
    if (!_s.session.loggedIn) return _login();
    final pv = _pv;
    if (pv == null || _busy || _addressId == 0) return;
    setState(() {
      _busy = true;
      _failed = false;
      _message = '';
    });
    try {
      final r = await placeOrder(_s.client, lines: _lines, addressId: _addressId, storeId: _storeId!,
          expectedPayableCents: pv.payableCents, couponId: _couponId > 0 ? _couponId : null, idempotencyKey: _key,
          onRetry: (sec) {
            // 这不是业务失败：那一单很可能正在成功。
            if (mounted) setState(() => _message = '订单正在处理中，$sec 秒后自动重试…');
          });
      if (!mounted) return;
      setState(() {
        _busy = false;
        _needNewKey = false;
        _orderNo = r.orderNo;
        _message = r.replayed ? '这笔订单之前已经提交过，本次没有重复下单。订单号 ${r.orderNo}' : '下单成功，订单号 ${r.orderNo}';
      });
      _reportSearchOrders();
      _clearOrderedCartRows();
      // 新单直接去订单详情付款；重放不跳：「之前已经提交过」这句话必须让用户看见。
      if (!r.replayed) {
        toast(context, '下单成功');
        context.pushReplacement('/orders/${Uri.encodeComponent(r.orderNo)}');
      }
    } on ApiFailure catch (f) {
      if (!mounted) return;
      setState(() {
        _busy = false;
        _failed = true;
        if (_notDeliverable(f)) {
          // 这一单没建成；换了地址再提交是新请求，换个键免得撞「同键异体」。
          _key = newIdempotencyKey();
          _pv = null;
          return;
        }
        if (f.isType('promotion-sold-out') || f.isType('promotion-limit-exceeded')) {
          // 名额卖完：重新试算会按门店价报价 —— 价格变了，必须让用户看见新应付再按提交。
          _key = newIdempotencyKey();
          _pv = null;
          final sold = f.isType('promotion-sold-out');
          _message = sold ? '活动名额刚刚抢完，已按现价重新计算，请确认应付金额后再提交' : '超出活动每人限购，请减少数量（${f.message}）';
          if (sold) WidgetsBinding.instance.addPostFrameCallback((_) => _preview());
          return;
        }
        if (f.isType('price-changed')) {
          // 价格变了：重算一次，要用户看见新应付再确认。这一单没建成，换键。
          _key = newIdempotencyKey();
          _pv = null;
          _message = '价格有变化，已重新计算，请确认应付金额后再提交';
          WidgetsBinding.instance.addPostFrameCallback((_) => _preview());
          return;
        }
        if (f.isType('idempotency-key-reused')) {
          // 同键异体：服务端宁可显式失败也不把不同的请求当重放吞掉。换新键做成要用户按的按钮。
          _needNewKey = true;
          _message = '订单内容和刚才提交的那笔不一样了。如果要按现在的内容再下一单，请作为新订单提交。';
          return;
        }
        if (f.isType('coupon-not-applicable')) {
          // 提交时券被别的订单锁走了（或刚好过期）。这一单没建成，换键；不自动改成不用券，应付会变。
          _key = newIdempotencyKey();
          _pv = null;
          _couponOpen = true;
          _message = '${f.message}。请换一张优惠券或选择不使用。';
          return;
        }
        _message = f.message;
      });
    }
  }

  void _rotate() {
    setState(() {
      _key = newIdempotencyKey();
      _needNewKey = false;
      _orderNo = '';
      _failed = false;
      _message = '';
    });
    _submit();
  }

  /// 这一单里从搜索结果点进来过的商品回传 order（重放也照发：服务端对重复上报回 204）。
  void _reportSearchOrders() {
    final t = _s.trace;
    if (widget.fromCart) {
      for (final r in _cartRows) {
        t.converted('order', r.productId);
      }
    } else {
      t.converted('order', widget.productId);
    }
  }

  /// 下单成功后把这几行从购物车删掉（服务端下单不碰购物车）。失败不打扰：静默重试一次，同一个幂等键。
  Future<void> _clearOrderedCartRows() async {
    if (!widget.fromCart || _cartRows.isEmpty) return;
    final ids = [for (final r in _cartRows) r.id];
    final c = _s.client;
    final count = _s.cart;
    final key = newIdempotencyKey();
    for (var attempt = 0; attempt < 2; attempt++) {
      try {
        final v = await deleteCartItems(c, ids, storeId: _storeId, idempotencyKey: key);
        count.set(v.quantity);
        return;
      } on ApiFailure {
        await Future<void>.delayed(const Duration(seconds: 1));
      }
    }
  }

  @override
  Widget build(BuildContext context) {
    final loggedIn = _s.session.loggedIn;
    final pv = _pv;
    return Scaffold(
      appBar: AppBar(title: const Text('确认订单', style: KeelText.title), backgroundColor: KeelColors.bg, surfaceTintColor: KeelColors.bg),
      body: ListView(padding: const EdgeInsets.fromLTRB(16, 4, 16, 24), children: [
        _card(GestureDetector(
          key: const Key('checkout.address'),
          behavior: HitTestBehavior.opaque,
          onTap: _pickAddress,
          child: Column(crossAxisAlignment: CrossAxisAlignment.start, children: [
            const Text('收货地址', style: KeelText.overline),
            const SizedBox(height: 6),
            Row(children: [
              Expanded(child: _addressBody(loggedIn)),
              const Icon(Icons.chevron_right, color: KeelColors.textHint),
            ]),
          ]),
        )),
        _card(Column(children: widget.fromCart ? _cartLines() : [_singleLine()])),
        if (loggedIn && (_coupons.isNotEmpty || _couponId > 0)) _card(_couponBox()),
        _card(_amounts(loggedIn, pv)),
        if (_message.isNotEmpty)
          Container(
            margin: const EdgeInsets.only(bottom: 12),
            padding: const EdgeInsets.all(14),
            decoration: BoxDecoration(
                color: _failed ? const Color(0xFFF8EAE5) : const Color(0xFFEAF0E6), borderRadius: BorderRadius.circular(14)),
            child: Column(crossAxisAlignment: CrossAxisAlignment.start, children: [
              Text(_message, key: const Key('checkout.message'), style: _failed ? KeelText.err : KeelText.ok),
              if (_provinceUnknown)
                Padding(
                  padding: const EdgeInsets.only(top: 10),
                  child: OutlinedButton(key: const Key('checkout.fixAddress'),
                      onPressed: () => context.push('/addresses/$_addressId').then((_) => _loadAddresses()),
                      child: const Text('去补全地址')),
                ),
              if (_needNewKey)
                Padding(
                  padding: const EdgeInsets.only(top: 10),
                  child: OutlinedButton(key: const Key('checkout.newOrder'), onPressed: _rotate, child: const Text('作为新订单提交')),
                ),
            ]),
          ),
      ]),
      bottomNavigationBar: SafeArea(
        child: Container(
          color: KeelColors.card,
          padding: const EdgeInsets.fromLTRB(16, 10, 16, 10),
          child: Row(children: [
            const Text('应付 ', style: KeelText.hint),
            Text(pv?.payableText ?? '—', key: const Key('checkout.payable'), style: KeelText.price.copyWith(fontSize: 20)),
            const Spacer(),
            if (_orderNo.isNotEmpty && !_needNewKey)
              FilledButton(key: const Key('checkout.viewOrder'),
                  onPressed: () => context.pushReplacement('/orders/${Uri.encodeComponent(_orderNo)}'), child: const Text('查看订单'))
            else
              FilledButton(
                key: const Key('checkout.submit'),
                onPressed: pv == null || _busy || _addressId == 0 || _outOfRange ? null : _submit,
                child: Text(_busy ? '提交中…' : '提交订单'),
              ),
          ]),
        ),
      ),
    );
  }

  Widget _card(Widget child) => Container(
        margin: const EdgeInsets.only(bottom: 12),
        padding: const EdgeInsets.all(14),
        decoration: BoxDecoration(color: KeelColors.card, borderRadius: BorderRadius.circular(14)),
        child: child,
      );

  Widget _addressBody(bool loggedIn) {
    final a = _address;
    if (a != null) {
      return Column(key: Key('checkout.address.${a.id}'), crossAxisAlignment: CrossAxisAlignment.start, children: [
        Row(children: [
          Text(a.name, style: KeelText.section),
          const SizedBox(width: 8),
          Text(a.phone, style: KeelText.sub),
        ]),
        Text(a.fullText, style: KeelText.hint),
      ]);
    }
    if (!loggedIn) return const Text('登录后选择收货地址', style: KeelText.hint);
    if (_addressError.isNotEmpty) return Text(_addressError, style: KeelText.err);
    if (_addressLoaded) {
      return Text(_addressCount == 0 ? '还没有收货地址，去新建' : '请选择收货地址', key: const Key('checkout.noAddress'),
          style: const TextStyle(fontSize: 14, color: KeelColors.accent));
    }
    return const Text('正在读取地址…', style: KeelText.hint);
  }

  Widget _thumb(Cover? c) => ClipRRect(
        borderRadius: BorderRadius.circular(10),
        child: SizedBox(
          width: 56, height: 56,
          child: c == null
              ? const ColoredBox(color: Color(0xFFD4B896))
              : c.imageUrl.isNotEmpty
                  ? Image.network(c.imageUrl, fit: BoxFit.cover, errorBuilder: (_, _, _) => ColoredBox(color: c.color))
                  : ColoredBox(color: c.color, child: Center(child: Text(c.glyph, style: const TextStyle(fontSize: 20, color: KeelColors.card)))),
        ),
      );

  List<Widget> _cartLines() => [
        for (final r in _cartRows)
          Padding(
            padding: const EdgeInsets.symmetric(vertical: 6),
            child: Row(children: [
              _thumb(r.cover),
              const SizedBox(width: 12),
              Expanded(
                child: Column(crossAxisAlignment: CrossAxisAlignment.start, children: [
                  Text(r.title, style: KeelText.body.copyWith(fontWeight: FontWeight.w700)),
                  if (r.specText.isNotEmpty) Text(r.specText, style: KeelText.hint),
                  if (_undeliverableOf(r.skuId).isNotEmpty)
                    Text(_undeliverableOf(r.skuId), key: Key('checkout.undeliverable.${r.skuId}'), style: KeelText.err),
                  Row(children: [Text(r.priceText, style: KeelText.price), const Spacer(), Text('× ${r.quantity}', style: KeelText.sub)]),
                ]),
              ),
            ]),
          ),
        if (_cartRows.isEmpty && _cartLoaded) const Text('购物车里没有勾选的商品', style: KeelText.hint),
      ];

  Widget _singleLine() => Row(children: [
        _thumb(_cover),
        const SizedBox(width: 12),
        Expanded(
          child: Column(crossAxisAlignment: CrossAxisAlignment.start, children: [
            Text(_itemTitle, style: KeelText.body.copyWith(fontWeight: FontWeight.w700)),
            if (_itemSpec.isNotEmpty) Text(_itemSpec, style: KeelText.hint),
            if (_undeliverableOf(widget.skuId).isNotEmpty)
              Text(_undeliverableOf(widget.skuId), key: Key('checkout.undeliverable.${widget.skuId}'), style: KeelText.err),
            Row(children: [
              Text(_unitPrice, style: KeelText.price),
              const Spacer(),
              QtyStepper(value: _qty, canMinus: _qty > 1, canPlus: true, keyPrefix: 'checkout',
                  onMinus: () => _changeQty(-1), onPlus: () => _changeQty(1)),
            ]),
          ]),
        ),
      ]);

  Widget _couponBox() {
    var summary = _coupons.isNotEmpty ? '${_coupons.length} 张可用' : '暂无可用';
    if (_couponId > 0) {
      final c = _coupons.where((c) => c.id == _couponId).firstOrNull;
      summary = c != null ? '${c.name} ${c.saveText}' : '已选 1 张';
    }
    return Column(children: [
      GestureDetector(
        key: const Key('checkout.coupons'),
        behavior: HitTestBehavior.opaque,
        onTap: () => setState(() => _couponOpen = !_couponOpen),
        child: Row(children: [
          const Text('优惠券', style: KeelText.overline),
          const Spacer(),
          Text(summary, key: const Key('checkout.couponSummary'),
              style: TextStyle(fontSize: 13, color: _couponId > 0 ? KeelColors.err : KeelColors.textHint)),
          Icon(_couponOpen ? Icons.expand_less : Icons.chevron_right, color: KeelColors.textHint),
        ]),
      ),
      if (_couponOpen) ...[
        for (final c in _coupons)
          ListTile(
            key: Key('checkout.coupon.${c.id}'),
            contentPadding: EdgeInsets.zero,
            title: Text(c.name, style: KeelText.body),
            subtitle: Text('${c.ruleText} · ${c.validText}', style: KeelText.hint),
            trailing: Row(mainAxisSize: MainAxisSize.min, children: [
              Text(c.saveText, style: const TextStyle(color: KeelColors.err)),
              Icon(c.id == _couponId ? Icons.radio_button_checked : Icons.radio_button_off, color: KeelColors.primary),
            ]),
            onTap: () => _pickCoupon(c.id),
          ),
        ListTile(
          key: const Key('checkout.coupon.none'),
          contentPadding: EdgeInsets.zero,
          title: const Text('不使用优惠券', style: KeelText.body),
          trailing: Icon(_couponId == 0 ? Icons.radio_button_checked : Icons.radio_button_off, color: KeelColors.primary),
          onTap: () => _pickCoupon(0),
        ),
      ],
    ]);
  }

  Widget _kv(String k, String v, {String? key, String note = ''}) => Padding(
        padding: const EdgeInsets.symmetric(vertical: 4),
        child: Row(children: [
          Text(k, style: KeelText.sub),
          const Spacer(),
          if (note.isNotEmpty)
            Padding(padding: const EdgeInsets.only(right: 6), child: Text(note, key: key == null ? null : Key('$key.note'), style: KeelText.hint)),
          Text(v, key: key == null ? null : Key(key), style: KeelText.body),
        ]),
      );

  Widget _amounts(bool loggedIn, PreviewView? pv) {
    if (!loggedIn) {
      return Column(children: [
        const Text('登录后查看应付金额', style: KeelText.sub),
        const SizedBox(height: 10),
        OutlinedButton(key: const Key('checkout.login'), onPressed: _login, child: const Text('去登录')),
      ]);
    }
    if (pv != null) {
      return Column(crossAxisAlignment: CrossAxisAlignment.start, children: [
        _kv('商品金额', pv.goodsAmountText, key: 'checkout.goods'),
        _kv('运费', pv.freightText, key: 'checkout.freight', note: pv.freightNote),
        if (pv.promotionDiscountText.isNotEmpty) _kv('活动优惠', pv.promotionDiscountText, key: 'checkout.promotion'),
        if (pv.couponDiscountText.isNotEmpty) _kv('优惠券', pv.couponDiscountText, key: 'checkout.couponOff'),
        if (pv.freightDiscountText.isNotEmpty) _kv('运费抵扣', pv.freightDiscountText, key: 'checkout.freightOff'),
        if (pv.noDiscountLines) _kv('优惠', pv.discountText, key: 'checkout.discount'),
        for (final (i, n) in pv.promotionNotes.indexed)
          Text(n, key: Key('checkout.note.$i'), style: const TextStyle(fontSize: 12, color: KeelColors.accent)),
        const Divider(color: KeelColors.line),
        Row(children: [
          const Text('应付', style: KeelText.section),
          const Spacer(),
          Text(pv.payableText, style: KeelText.price),
        ]),
      ]);
    }
    if (_previewing) return const Center(child: Text('正在计算…', key: Key('checkout.computing'), style: KeelText.hint));
    if (_addressLoaded && _addressId == 0) return const Center(child: Text('选好收货地址后计算应付金额', style: KeelText.hint));
    return Center(
      child: Column(children: [
        const Text('金额待计算', key: Key('checkout.pending'), style: KeelText.hint),
        TextButton(key: const Key('checkout.recalc'), onPressed: _preview, child: const Text('重新计算')),
      ]),
    );
  }
}
