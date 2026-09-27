import 'client.dart';
import 'schema.g.dart';
import 'view.dart';

/// 售后。照 uni-app x 的 view.uts（refundRow / refundDetailView / refundableItems / refundRequest …）
/// 与 pages/refund 移植。请求体不带金额：每行实退多少由服务端按优惠分摊算。

/// 退款单状态（和订单上的 refund_status 不是一个东西）。30「退款中」不是失败：钱在路上，不会凭空消失。
String refundStateText(int s) => switch (s) {
      10 => '待审核',
      20 => '待买家退货',
      30 => '退款中',
      40 => '已退款',
      50 => '已拒绝',
      60 => '已取消',
      _ => '未知状态 $s',
    };

/// 徽章色调：warn 进行中 / ok 已退款 / err 已拒绝 / muted 已取消。
String refundStateTone(int s) => switch (s) { 10 || 20 || 30 => 'warn', 40 => 'ok', 50 => 'err', _ => 'muted' };

String refundTypeText(int t) => t == 2 ? '退货退款' : '仅退款';

String refundReasonText(int code) => switch (code) {
      1 => '不想要了',
      2 => '少发漏发',
      3 => '商品损坏',
      4 => '描述不符',
      5 => '其他',
      _ => '',
    };

class RefundRow {
  final String refundNo;
  final String orderNo;
  final int status;
  final String statusText;
  final String statusTone;
  final String typeText;
  final String amountText;
  final String createdAt;
  /// 「亚麻四件套 ×1」/「亚麻四件套 等 2 件」
  final String itemsText;
  const RefundRow({required this.refundNo, required this.orderNo, required this.status, required this.statusText,
      required this.statusTone, required this.typeText, required this.amountText, required this.createdAt, required this.itemsText});
}

RefundRow refundRow(Refund r) {
  var itemsText = '';
  if (r.items.isNotEmpty) {
    final first = r.items.first;
    final title = first.title ?? '商品';
    final count = r.items.fold(0, (n, it) => n + it.quantity);
    itemsText = r.items.length == 1 ? '$title ×${first.quantity}' : '$title 等 $count 件';
  }
  return RefundRow(
    refundNo: r.refundNo,
    orderNo: r.orderNo,
    status: r.status,
    statusText: refundStateText(r.status),
    statusTone: refundStateTone(r.status),
    typeText: refundTypeText(r.refundType),
    amountText: yuan(r.amountCents),
    createdAt: shortTime(r.createdAt),
    itemsText: itemsText,
  );
}

class RefundItemRow {
  final String title;
  final int quantity;
  /// 这一行实退多少（服务端按优惠分摊算的）。
  final String amountText;
  final Cover cover;
  const RefundItemRow(this.title, this.quantity, this.amountText, this.cover);
}

class Carrier {
  final String code;
  final String name;
  const Carrier(this.code, this.name);
}

/// 承运商。code 与发货的 carrier_code 同一套。
const carriers = [
  Carrier('sf', '顺丰'),
  Carrier('jd', '京东'),
  Carrier('yto', '圆通'),
  Carrier('zto', '中通'),
  Carrier('sto', '申通'),
  Carrier('yd', '韵达'),
  Carrier('ems', 'EMS'),
];

/// 不认识的 code（商家后台录的别家）原样显示。
String carrierName(String code) => carriers.where((c) => c.code == code.toLowerCase()).firstOrNull?.name ?? code;

class RefundDetailView {
  final RefundRow head;
  final List<RefundItemRow> items;
  final String reasonText;
  /// 50 已拒绝时的驳回理由；别的状态是空串。
  final String rejectReason;
  /// 10 待审核 / 20 待买家退货 可以撤回。
  final bool canCancel;
  final bool awaitingReturn;
  final bool rejected;
  final String refundedAt;
  final String channelRefundId;
  /// 凭证图的地址（/api/v1/uploads/{id}）：要带令牌读，不能直接 Image.network。
  final List<String> evidenceUrls;
  /// 退货退款且 20 待买家退货：可以填 / 改寄回物流。
  final bool canFillReturn;
  final String returnCarrierCode;
  final String returnCarrierText;
  final String returnTrackingNo;
  final String returnSubmittedAt;
  /// 待买家退货的寄回截止时间；过了没填物流售后会被自动关闭。
  final String returnDeadlineAt;
  const RefundDetailView({required this.head, required this.items, required this.reasonText, required this.rejectReason,
      required this.canCancel, required this.awaitingReturn, required this.rejected, required this.refundedAt,
      required this.channelRefundId, required this.evidenceUrls, required this.canFillReturn, required this.returnCarrierCode,
      required this.returnCarrierText, required this.returnTrackingNo, required this.returnSubmittedAt, required this.returnDeadlineAt});

  /// 状态下面那一句说明。
  String get statusLine => switch (head.status) {
        10 => '等待商家审核',
        20 => '审核已通过，请寄回商品',
        30 => '商家已同意，退款正在原路退回，到账时间以支付渠道为准',
        40 => '退款已原路退回',
        50 => '商家驳回了这次申请',
        60 => '这次申请已撤回',
        _ => '',
      };
}

RefundDetailView refundDetailView(Refund r, String Function(String) asset) {
  final reason = refundReasonText(r.reasonCode ?? 0);
  final text = r.reasonText ?? '';
  final rs = r.returnShipment;
  return RefundDetailView(
    head: refundRow(r),
    items: [
      for (final it in r.items)
        RefundItemRow(it.title ?? '商品 #${it.orderItemId}', it.quantity, yuan(it.amountCents),
            coverOf(it.orderItemId, it.title ?? '商品', it.imageUrl == null ? '' : asset(it.imageUrl!))),
    ],
    reasonText: reason.isNotEmpty && text.isNotEmpty ? '$reason：$text' : (reason.isNotEmpty ? reason : text),
    rejectReason: r.status == 50 ? (r.rejectReason ?? '') : '',
    canCancel: r.status == 10 || r.status == 20,
    awaitingReturn: r.status == 20,
    rejected: r.status == 50,
    refundedAt: r.refundedAt == null ? '' : shortTime(r.refundedAt!),
    channelRefundId: r.channelRefundId ?? '',
    evidenceUrls: r.evidenceUrls ?? const [],
    canFillReturn: r.refundType == 2 && r.status == 20,
    returnCarrierCode: rs?.carrierCode.toLowerCase() ?? '',
    returnCarrierText: rs == null ? '' : carrierName(rs.carrierCode),
    returnTrackingNo: rs?.trackingNo ?? '',
    returnSubmittedAt: rs == null ? '' : shortTime(rs.submittedAt),
    returnDeadlineAt: r.status == 20 && r.returnDeadlineAt != null ? shortTime(r.returnDeadlineAt!) : '',
  );
}

/// 申请售后时可以选的一行。maxQty = 购买 − 已退 − 在途；为 0 的行不列出来。
class RefundableItem {
  final int orderItemId;
  final String title;
  final String specText;
  final String priceText;
  final int maxQty;
  final Cover cover;
  const RefundableItem({required this.orderItemId, required this.title, required this.specText, required this.priceText,
      required this.maxQty, required this.cover});
}

List<RefundableItem> refundableItems(OrderDetail o, [String Function(String)? asset]) => [
      for (final it in o.items ?? const <OrderItem>[])
        if (it.quantity - (it.refundedQty ?? 0) - (it.refundingQty ?? 0) > 0)
          RefundableItem(
            orderItemId: it.id,
            title: it.title,
            specText: (it.specValues ?? const {}).values.join(' / '),
            priceText: yuan(it.priceCents),
            maxQty: it.quantity - (it.refundedQty ?? 0) - (it.refundingQty ?? 0),
            cover: coverOf(it.productId ?? it.skuId, it.title, it.imageUrl == null || asset == null ? '' : asset(it.imageUrl!)),
          ),
    ];

/// 货发出去了才有货可退：未发货的单服务端只收仅退款。
bool refundReturnAllowed(OrderDetail o) => o.status == 30 || o.status == 40;

typedef RefundLine = ({int orderItemId, int quantity});

/// 售后申请的请求体。不带金额。refundType 由用户选，页面不给默认值。
Map<String, dynamic> refundBody(List<RefundLine> lines, {required int refundType, required int reasonCode,
        required String reasonText, required List<String> evidenceUrls}) =>
    RefundCreateRequest(
      items: [for (final l in lines) RefundItemInput(orderItemId: l.orderItemId, quantity: l.quantity)],
      refundType: refundType,
      reasonCode: reasonCode,
      reasonText: reasonText.isEmpty ? null : reasonText,
      // 只放本人刚用 purpose=3 传的 /api/v1/uploads/{id}，原样回传。
      evidenceUrls: evidenceUrls.isEmpty ? null : evidenceUrls,
    ).toJson();

Refund _refund(dynamic j) => Refund.fromJson(j as Map<String, dynamic>);

Future<String> createRefund(ApiClient c, String orderNo, Map<String, dynamic> body, String idempotencyKey) async =>
    (await c.send('POST', '/orders/${Uri.encodeComponent(orderNo)}/refunds', body: body, idempotencyKey: idempotencyKey,
            decode: _refund))
        .data
        .refundNo;

Future<List<RefundRow>> fetchRefunds(ApiClient c) async => (await c.send('GET', '/refunds',
        query: {'page': '1', 'page_size': '50'}, decode: (j) => ListRefundsResponse.fromJson(j as Map<String, dynamic>)))
    .data
    .items
    .map(refundRow)
    .toList();

/// 订单详情里的售后区。
Future<List<RefundRow>> fetchOrderRefunds(ApiClient c, String orderNo) async => (await c.send(
        'GET', '/orders/${Uri.encodeComponent(orderNo)}/refunds',
        decode: (j) => (j as List).map((e) => _refund(e)).toList()))
    .data
    .map(refundRow)
    .toList();

Future<RefundDetailView> fetchRefund(ApiClient c, String refundNo) async => refundDetailView(
    (await c.send('GET', '/refunds/${Uri.encodeComponent(refundNo)}', decode: _refund)).data, c.assetUrl);

Future<RefundDetailView> cancelRefund(ApiClient c, String refundNo, String idempotencyKey) async => refundDetailView(
    (await c.send('POST', '/refunds/${Uri.encodeComponent(refundNo)}/cancel', idempotencyKey: idempotencyKey, decode: _refund)).data,
    c.assetUrl);

/// 填 / 改寄回物流：只有退货退款且 20 能填，否则 409 refund-status-not-returnable。填完状态仍是 20。
Future<RefundDetailView> submitReturnShipment(ApiClient c, String refundNo, String carrierCode, String trackingNo,
        String idempotencyKey) async =>
    refundDetailView(
        (await c.send('POST', '/refunds/${Uri.encodeComponent(refundNo)}/return-shipment',
                body: ReturnShipmentRequest(carrierCode: carrierCode, trackingNo: trackingNo).toJson(),
                idempotencyKey: idempotencyKey, decode: _refund))
            .data,
        c.assetUrl);
