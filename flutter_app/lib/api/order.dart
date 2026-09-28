
import 'cart.dart';
import 'client.dart';
import 'coupon.dart';
import 'refund.dart';
import 'schema.g.dart';
import 'view.dart';

/// 结算、下单、支付、订单。照 uni-app x 的 view.uts（previewView / orderRow / orderDetailView …）、
/// client.uts（createOrder / createPayment / settleSandbox …）与 pages/order 移植。
/// 金额一律用服务端算好的数，这里不做加减。

typedef OrderLine = ({int skuId, int quantity});

// ---- 试算 ----

class PreviewView {
  final String goodsAmountText;
  final String freightText;
  final String discountText;
  final String payableText;
  /// 提交订单时回填的 expected_payable_cents（服务端复算对不上回 409 price-changed）。
  final int payableCents;
  final String promotionDiscountText;
  final String couponDiscountText;
  final List<String> promotionNotes;
  final String freightDiscountText;
  final String freightNote;
  /// 「运费」/「配送费」（同城）。
  final String freightLabel;
  /// 同城配送、地址没有坐标：按最远一档计，提示给地址选点。
  final bool needsPin;
  /// 服务端认下的券；0 = 这次没用券。
  final int couponId;
  /// 这一单能用的券，服务端按优惠从大到小排好（第一张最省）。
  final List<CouponOption> coupons;
  const PreviewView({required this.goodsAmountText, required this.freightText, required this.discountText,
      required this.payableText, required this.payableCents, required this.promotionDiscountText,
      required this.couponDiscountText, required this.promotionNotes, required this.freightDiscountText,
      required this.freightNote, this.freightLabel = '运费', this.needsPin = false, required this.couponId, required this.coupons});

  /// 活动 / 券 / 运费抵扣一行都没有时才显示一行「优惠」。
  bool get noDiscountLines => promotionDiscountText.isEmpty && couponDiscountText.isEmpty && freightDiscountText.isEmpty;
}

String _neg(int? c) => c != null && c > 0 ? '-${yuan(c)}' : '';

PreviewView previewView(OrderPreview p) => PreviewView(
      goodsAmountText: yuan(p.goodsAmountCents),
      freightText: yuan(p.freightCents),
      discountText: yuan(p.discountCents),
      payableText: yuan(p.payableCents),
      payableCents: p.payableCents,
      freightDiscountText: _neg(p.freightDiscountCents),
      freightNote: freightNote(p.freight),
      freightLabel: freightLabel(p.freight),
      needsPin: needsPin(p.freight),
      promotionDiscountText: _neg(p.promotionDiscountCents),
      couponDiscountText: _neg(p.couponDiscountCents),
      promotionNotes: promotionNotes(p.promotions),
      couponId: p.userCouponId ?? 0,
      coupons: (p.applicableCoupons ?? const []).map(couponOption).toList(),
    );

/// 试算与下单共用一个请求体。expected 试算过才带；coupon null = 不用券。
Map<String, dynamic> orderBody(List<OrderLine> lines, int addressId, int storeId, int? expected, int? couponId) =>
    OrderCreateRequest(
      items: [for (final l in lines) OrderItemInput(skuId: l.skuId, quantity: l.quantity)],
      storeId: storeId,
      addressId: addressId,
      expectedPayableCents: expected,
      userCouponId: couponId,
    ).toJson();

Future<PreviewView> previewOrder(ApiClient c, {required List<OrderLine> lines, required int addressId, required int storeId,
    required int? couponId}) async {
  final res = await c.send('POST', '/orders/preview', body: orderBody(lines, addressId, storeId, null, couponId),
      decode: (j) => OrderPreview.fromJson(j as Map<String, dynamic>));
  return previewView(res.data);
}

/// 422 region-not-deliverable 里送不到的一行。
class UndeliverableLine {
  final int skuId;
  final String reason;
  /// 收货地址归不到省：引导补全地址，而不是让用户去掉商品。
  final bool provinceUnknown;
  const UndeliverableLine(this.skuId, this.reason, this.provinceUnknown);
}

List<UndeliverableLine> undeliverableLines(ApiFailure f) => [
      for (final it in f.problem?.undeliverableItems ?? const <FreightUndeliverableLine>[])
        UndeliverableLine(it.skuId, it.reason, it.reasonCode == 'province_unknown'),
    ];

// ---- 下单 ----

class Placed {
  final String orderNo;
  /// Idempotency-Replayed：这次什么都没执行，返回的是上一次那单。
  final bool replayed;
  const Placed(this.orderNo, this.replayed);
}

Future<void> _sleep(Duration d) => Future<void>.delayed(d);

/// POST /orders。幂等键是进结算页时生成的那一个（每次点击重新生成等于把幂等废掉）。
/// in-flight 不是业务失败：按 Retry-After 退避，用同一个键重试，最多 3 次。其余失败原样抛给页面。
Future<Placed> placeOrder(ApiClient c, {required List<OrderLine> lines, required int addressId, required int storeId,
    required int? expectedPayableCents, required int? couponId, required String idempotencyKey,
    Future<void> Function(Duration) wait = _sleep, void Function(int seconds)? onRetry}) async {
  for (var attempt = 0;; attempt++) {
    try {
      final res = await c.send('POST', '/orders', body: orderBody(lines, addressId, storeId, expectedPayableCents, couponId),
          idempotencyKey: idempotencyKey, decode: (j) => Order.fromJson(j as Map<String, dynamic>));
      return Placed(res.data.orderNo, res.replayed);
    } on ApiFailure catch (f) {
      if (!f.isType('idempotency-key-in-flight') || attempt >= 3) rethrow;
      final s = f.retryAfter > 0 ? f.retryAfter : 1;
      onRetry?.call(s);
      await wait(Duration(seconds: s));
    }
  }
}

// ---- 订单 ----

String orderStatusText(int s) => switch (s) {
      10 => '待支付',
      20 => '已支付',
      30 => '已发货',
      40 => '已完成',
      50 => '退款中',
      60 => '已退款',
      90 => '已关闭',
      _ => '未知状态 $s',
    };

/// 状态徽章的色调：warn 待支付 / ok 已支付·已发货·已完成 / refund 退款中·已退款 / muted 已关闭。
String orderStatusTone(int s) => switch (s) {
      10 => 'warn',
      20 || 30 || 40 => 'ok',
      50 || 60 => 'refund',
      _ => 'muted',
    };

/// 售后维度，必须和 status 组合显示（「买三退一的已发货单」只看 status 和没售后的一模一样）。
String refundStatusText(int r) => switch (r) { 1 => '退款中', 2 => '部分退款', 3 => '已全额退款', _ => '' };

class OrderRow {
  final String orderNo;
  final String statusText;
  final String statusTone;
  final String refundText;
  final String payableText;
  final String createdAt;
  /// 仅待支付能发起支付 / 取消。
  final bool payable;
  const OrderRow({required this.orderNo, required this.statusText, required this.statusTone, required this.refundText,
      required this.payableText, required this.createdAt, required this.payable});
}

OrderRow _row(String no, int status, int refund, int payable, String created) => OrderRow(
      orderNo: no,
      statusText: orderStatusText(status),
      statusTone: orderStatusTone(status),
      refundText: refundStatusText(refund),
      payableText: yuan(payable),
      createdAt: shortTime(created),
      payable: status == 10,
    );

OrderRow orderRow(Order o) => _row(o.orderNo, o.status, o.refundStatus, o.payableCents, o.createdAt);

class OrderItemRow {
  final int skuId;
  final int productId;
  final String title;
  final String specText;
  final int quantity;
  final String priceText;
  final String amountText;
  final Cover cover;
  const OrderItemRow({required this.skuId, required this.productId, required this.title, required this.specText,
      required this.quantity, required this.priceText, required this.amountText, required this.cover});
}

OrderItemRow orderItemRow(OrderItem it, String Function(String) asset) => OrderItemRow(
      skuId: it.skuId,
      productId: it.productId ?? 0,
      title: it.title,
      specText: (it.specValues ?? const {}).values.join(' / '),
      quantity: it.quantity,
      priceText: yuan(it.priceCents),
      amountText: yuan(it.amountCents),
      cover: coverOf(it.productId ?? it.skuId, it.title, it.imageUrl == null ? '' : asset(it.imageUrl!)),
    );

class PaymentRow {
  final String channelText;
  final String statusText;
  final String amountText;
  const PaymentRow(this.channelText, this.statusText, this.amountText);
}

PaymentRow paymentRow(PaymentRecord p) => PaymentRow(
      switch (p.channel ?? '') { 'wechat' => '微信', 'alipay' => '支付宝', 'balance' => '余额', final x => x },
      switch (p.status) { 0 => '待支付', 1 => '成功', 2 => '失败', 3 => '已关闭', _ => '未知' },
      yuan(p.amountCents),
    );

class OrderDetailView {
  final OrderRow head;
  final String receiver;
  final String goodsAmountText;
  final String freightText;
  final String discountText;
  final String paidText;
  final bool hasDiscount;
  final bool hasPaid;
  final String refundedText;
  final String freightDiscountText;
  final String autoConfirmAt;
  final String promotionDiscountText;
  final List<String> promotionLines;
  final String freightNote;
  final String freightLabel;
  final bool canCancel;
  final bool canConfirm;
  final int couponId;
  final String couponName;
  final List<OrderItemRow> items;
  final List<PaymentRow> payments;
  /// 这一单的售后单。
  final List<RefundRow> refunds;
  /// 多收款退回（契约 OrderDetail.payment_returns，00150）：订单只认一笔到账，其余的系统自动原路退回。
  /// 每行一句「多付的 ¥x（重复支付）已原路退回」；没有时空。
  final List<String> returnLines;
  /// 已支付 / 已发货 / 已完成，且还有没退完、也没在途售后的件数。
  final bool canRefund;
  /// 原始订单：售后那几页（第二阶段）要按行算可退件数。
  final OrderDetail raw;
  const OrderDetailView({required this.head, required this.receiver, required this.goodsAmountText,
      required this.freightText, required this.discountText, required this.paidText, required this.hasDiscount,
      required this.hasPaid, required this.refundedText, required this.freightDiscountText, required this.autoConfirmAt,
      required this.promotionDiscountText, required this.promotionLines, required this.freightNote, this.freightLabel = '运费', required this.canCancel,
      required this.canConfirm, required this.couponId, required this.couponName, required this.items,
      required this.payments, required this.refunds, this.returnLines = const [], required this.canRefund, required this.raw});

  /// 状态下面那一句说明。
  String get statusLine => switch (head.statusText) {
        '待支付' => '订单已创建，完成支付后商家开始备货',
        '已支付' => '支付成功，商家正在准备发货',
        '已发货' => autoConfirmAt.isNotEmpty
            ? '包裹已在路上，$autoConfirmAt 未确认将自动确认收货'
            : '包裹已在路上，发货 7 天后未确认将自动确认收货',
        '已完成' => '订单已完成，感谢光临',
        '已关闭' => '订单已关闭',
        '退款中' => '整单退款处理中',
        '已退款' => '整单已退款，钱已原路退回',
        _ => '',
      };
}

OrderDetailView orderDetailView(OrderDetail o, String Function(String) asset) {
  final r = o.receiver;
  final street = r?.street ?? '';
  return OrderDetailView(
    head: _row(o.orderNo, o.status, o.refundStatus, o.payableCents, o.createdAt),
    // 与地址簿同一个写法：省市区之间留空格，街道不能丢。
    receiver: r == null
        ? ''
        : '${r.receiverName} ${r.phone} ${r.province} ${r.city} ${r.district} ${street.isNotEmpty ? '$street ' : ''}${r.detail}',
    goodsAmountText: yuan(o.goodsAmountCents),
    freightText: yuan(o.freightCents),
    discountText: yuan(o.discountCents),
    paidText: yuan(o.paidCents),
    hasDiscount: (o.discountCents ?? 0) > 0,
    hasPaid: (o.paidCents ?? 0) > 0,
    refundedText: (o.refundedCents ?? 0) > 0 ? yuan(o.refundedCents) : '',
    freightDiscountText: _neg(o.freightDiscountCents),
    freightNote: o.freight == null ? '' : freightNote(o.freight!),
    freightLabel: freightLabel(o.freight),
    promotionDiscountText: _neg(o.promotionDiscountCents),
    autoConfirmAt: o.autoConfirmAt == null ? '' : shortTime(o.autoConfirmAt!),
    promotionLines: [
      for (final p in o.promotions ?? const <OrderPromotion>[]) p.discountCents > 0 ? '${p.name} -${yuan(p.discountCents)}' : p.name,
    ],
    canCancel: o.status == 10,
    canConfirm: o.status == 30,
    couponId: o.userCouponId ?? 0,
    couponName: o.couponName ?? '',
    items: (o.items ?? const <OrderItem>[]).map((it) => orderItemRow(it, asset)).toList(),
    payments: (o.payments ?? const <PaymentRecord>[]).map(paymentRow).toList(),
    refunds: (o.refunds ?? const <Refund>[]).map(refundRow).toList(),
    returnLines: [for (final r in o.paymentReturns ?? const <PaymentReturn>[]) paymentReturnLine(r)],
    canRefund: (o.status == 20 || o.status == 30 || o.status == 40) && refundableItems(o).isNotEmpty,
    raw: o,
  );
}

Future<List<OrderRow>> fetchOrders(ApiClient c, {int page = 1, int pageSize = 20}) async {
  final res = await c.send('GET', '/orders', query: {'page': '$page', 'page_size': '$pageSize'},
      decode: (j) => ListOrdersResponse.fromJson(j as Map<String, dynamic>));
  return res.data.items.map(orderRow).toList();
}

Future<OrderDetailView> fetchOrder(ApiClient c, String orderNo) async {
  final res = await c.send('GET', '/orders/${Uri.encodeComponent(orderNo)}',
      decode: (j) => OrderDetail.fromJson(j as Map<String, dynamic>));
  return orderDetailView(res.data, c.assetUrl);
}

/// 只有待支付的单能取消；库存与锁住的券同一事务退回。
Future<void> cancelOrder(ApiClient c, String orderNo, String idempotencyKey) =>
    c.send('POST', '/orders/${Uri.encodeComponent(orderNo)}/cancel', idempotencyKey: idempotencyKey, decode: (_) => null);

/// 已发货（30）的单才能确认收货。
Future<void> confirmOrder(ApiClient c, String orderNo, String idempotencyKey) =>
    c.send('POST', '/orders/${Uri.encodeComponent(orderNo)}/confirm', idempotencyKey: idempotencyKey, decode: (_) => null);

// ---- 支付 ----

/// 服务端交给客户端的沙箱回调信封：原样投递就是走生产那条完整的入账路径（验签、金额、幂等、状态机）。
class SandboxSettle {
  final String url;
  final Map<String, String> headers;
  /// 已经序列化好的 JSON。签名签的是这串字节：必须原样发出去。
  final String body;
  const SandboxSettle(this.url, this.headers, this.body);
}

class PaymentStart {
  final String paymentNo;
  final String amountText;
  final bool replayed;
  final bool sandbox;
  final String notice;
  /// 沙箱且渠道给了结算指令时才有。
  final SandboxSettle? settle;
  const PaymentStart(this.paymentNo, this.amountText, this.replayed, this.sandbox, this.notice, this.settle);
}

/// POST /orders/{no}/payments（幂等键进详情页生成一次）。payload 在契约里刻意不描述形状（渠道特定），
/// 这里对缺席宽容：拿不到 settle 就是「这个渠道没给沙箱结算指令」，不猜。
Future<PaymentStart> startPayment(ApiClient c, String orderNo, String channel, String idempotencyKey) async {
  final res = await c.send('POST', '/orders/${Uri.encodeComponent(orderNo)}/payments',
      body: PaymentCreateRequest(channel: channel).toJson(), idempotencyKey: idempotencyKey,
      decode: (j) => PaymentIntent.fromJson(j as Map<String, dynamic>));
  final p = res.data.payload ?? const <String, dynamic>{};
  final s = p['settle'];
  SandboxSettle? settle;
  if (s is Map && s['url'] is String && s['body'] is String) {
    settle = SandboxSettle(s['url'] as String,
        {for (final e in ((s['headers'] as Map?) ?? const {}).entries) '${e.key}': '${e.value}'}, s['body'] as String);
  }
  return PaymentStart(res.data.paymentNo, yuan(res.data.amountCents), res.replayed, p['sandbox'] == true,
      (p['sandbox_notice'] as String?) ?? '沙箱支付', settle);
}

/// 投递沙箱回调：URL、头、报文体全由服务端指定；不带令牌（回调的调用方是渠道）。
Future<void> settleSandbox(ApiClient c, SandboxSettle s) async {
  final res = await c.postRaw(c.assetUrl(s.url), s.headers, s.body);
  if (res.status >= 200 && res.status < 300) return;
  throw c.failureOf(res.status, res.text);
}


/// 一张多收款退回单给买家看的那一句。
String paymentReturnLine(PaymentReturn r) {
  final why = switch (r.reason) { 1 => '重复支付', 2 => '订单关闭后到账', _ => '金额与应付不符' };
  final state = r.status == 40 ? '已原路退回' : '正在原路退回';
  return '多付的 ${yuan(r.amountCents)}（$why）$state';
}
