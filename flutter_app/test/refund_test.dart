import 'package:flutter_test/flutter_test.dart';
import 'package:keel_buyer/api/refund.dart';
import 'package:keel_buyer/api/schema.g.dart';

Map<String, dynamic> refund({int status = 10, int type = 1, List<Map<String, dynamic>>? items, Map<String, dynamic>? extra}) => {
      'refund_no': 'R1', 'order_no': 'N1', 'refund_type': type, 'status': status, 'amount_cents': 5600,
      'items': items ?? [{'order_item_id': 7, 'title': '亚麻四件套', 'quantity': 1, 'amount_cents': 5600}],
      'created_at': '2026-09-26T00:05:12Z', ...?extra,
    };

Map<String, dynamic> orderDetail(int status, List<Map<String, dynamic>> items) =>
    {'order_no': 'N1', 'store_id': 1, 'status': status, 'refund_status': 0, 'payable_cents': 1, 'created_at': '2026-09-26T00:05:12Z', 'items': items};

void main() {
  test('状态文案与色调：30 退款中不是失败（warn，不用红色），50 已拒绝才是', () {
    expect(refundStateText(30), '退款中');
    expect(refundStateTone(30), 'warn');
    expect(refundStateTone(50), 'err');
    expect(refundStateTone(40), 'ok');
    expect(refundStateTone(60), 'muted');
  });

  test('列表行：一件写 ×N，多件写「等 N 件」', () {
    expect(refundRow(Refund.fromJson(refund())).itemsText, '亚麻四件套 ×1');
    expect(refundRow(Refund.fromJson(refund(items: [
      {'order_item_id': 7, 'title': '亚麻四件套', 'quantity': 1, 'amount_cents': 1},
      {'order_item_id': 8, 'title': '枕套', 'quantity': 2, 'amount_cents': 1},
    ]))).itemsText, '亚麻四件套 等 3 件');
  });

  test('详情：原因拼说明；驳回理由只在 50；寄回物流只在退货退款 + 20；截止时间只在 20', () {
    final d = refundDetailView(Refund.fromJson(refund(status: 20, type: 2, extra: {
      'reason_code': 3, 'reason_text': '外包装破了', 'reject_reason': '不该出现',
      'return_shipment': {'carrier_code': 'SF', 'tracking_no': 'SF123', 'submitted_at': '2026-09-26T01:00:00Z'},
      'return_deadline_at': '2026-10-03T00:00:00Z', 'evidence_urls': ['/api/v1/uploads/5'],
    })), (s) => s);
    expect(d.reasonText, '商品损坏：外包装破了');
    expect(d.rejectReason, '');
    expect(d.canFillReturn, isTrue);
    expect(d.canCancel, isTrue);
    expect(d.returnCarrierText, '顺丰');
    expect(d.returnCarrierCode, 'sf');
    expect(d.returnDeadlineAt, isNotEmpty);
    expect(d.evidenceUrls, ['/api/v1/uploads/5']);
    final only = refundDetailView(Refund.fromJson(refund(status: 20, type: 1)), (s) => s);
    expect(only.canFillReturn, isFalse, reason: '仅退款没有寄回');
    final rej = refundDetailView(Refund.fromJson(refund(status: 50, extra: {'reject_reason': '已超过售后期'})), (s) => s);
    expect(rej.rejectReason, '已超过售后期');
    expect(rej.rejected, isTrue);
    expect(rej.canCancel, isFalse);
    expect(carrierName('zzz'), 'zzz', reason: '不认识的承运商原样显示');
  });

  test('可退的行：买的 − 已退 − 在途；为 0 的不列；发货后才能退货退款', () {
    final o = OrderDetail.fromJson(orderDetail(30, [
      {'id': 1, 'sku_id': 1, 'title': 'A', 'price_cents': 100, 'quantity': 3, 'refunded_qty': 1, 'refunding_qty': 1, 'spec_values': {'色': '白'}},
      {'id': 2, 'sku_id': 2, 'title': 'B', 'price_cents': 100, 'quantity': 1, 'refunded_qty': 1},
    ]));
    final items = refundableItems(o);
    expect(items.map((i) => (i.orderItemId, i.maxQty)), [(1, 1)]);
    expect(items.single.specText, '白');
    expect(refundReturnAllowed(o), isTrue);
    expect(refundReturnAllowed(OrderDetail.fromJson(orderDetail(20, []))), isFalse);
  });

  test('申请请求体不带金额；说明与凭证有才带', () {
    final b = refundBody([(orderItemId: 1, quantity: 2)], refundType: 1, reasonCode: 1, reasonText: '', evidenceUrls: const []);
    expect(b, {'items': [{'order_item_id': 1, 'quantity': 2}], 'refund_type': 1, 'reason_code': 1});
    final b2 = refundBody([(orderItemId: 1, quantity: 1)], refundType: 2, reasonCode: 5, reasonText: '颜色不对', evidenceUrls: const ['/api/v1/uploads/9']);
    expect(b2['reason_text'], '颜色不对');
    expect(b2['evidence_urls'], ['/api/v1/uploads/9']);
  });
}
