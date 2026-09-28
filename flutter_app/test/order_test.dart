import 'dart:convert';

import 'package:flutter_test/flutter_test.dart';
import 'package:http/http.dart' as http;
import 'package:http/testing.dart';
import 'package:keel_buyer/api/client.dart';
import 'package:keel_buyer/api/coupon.dart';
import 'package:keel_buyer/api/order.dart';
import 'package:keel_buyer/api/schema.g.dart';
import 'package:keel_buyer/api/session.dart';
import 'package:shared_preferences/shared_preferences.dart';

http.Response j(Object body, [int status = 200, Map<String, String> h = const {}]) => http.Response(jsonEncode(body), status,
    headers: {'content-type': 'application/json; charset=utf-8', ...h});

Future<(ApiClient, List<http.Request>)> client(Future<http.Response> Function(http.Request) h, {String base = 'http://h/api/v1'}) async {
  SharedPreferences.setMockInitialValues({'keel.access': 'a'});
  final s = Session();
  await s.load();
  final seen = <http.Request>[];
  return (ApiClient(base: base, session: s, http: MockClient((r) async {
    seen.add(r);
    return h(r);
  })), seen);
}

Map<String, dynamic> coupon(int id, int save, {int type = 1, int rate = 0}) => {
      'id': id, 'coupon_code': 'C$id', 'template_id': 1, 'name': '券$id', 'coupon_type': type, 'threshold_cents': 5000,
      'discount_cents': type == 2 ? 0 : 1000, 'discount_rate': rate, 'max_discount_cents': 0, 'status': 1, 'source': 1,
      'valid_start_at': '2026-09-01T00:00:00Z', 'valid_end_at': DateTime(2026, 10, 7).toUtc().toIso8601String(),
      'scopes': [], 'applicable_discount_cents': save,
    };

Map<String, dynamic> preview({int? couponId, List<Map<String, dynamic>> coupons = const [], int couponOff = 0}) => {
      'store_id': 1, 'goods_amount_cents': 10000, 'freight_cents': 800, 'freight_discount_cents': 800,
      'freight': {'freight_cents': 800, 'freight_discount_cents': 800, 'groups': []}, 'payable_cents': 10000 - couponOff,
      'promotion_discount_cents': 0, 'coupon_discount_cents': couponOff, 'items': [], 'promotions': [],
      'user_coupon_id': ?couponId, 'applicable_coupons': coupons,
    };

Map<String, dynamic> order(String no, int status) => {'order_no': no, 'store_id': 1, 'status': status, 'refund_status': 0,
      'payable_cents': 9200, 'created_at': '2026-09-26T00:05:12Z'};

void main() {
  group('券的文案', () {
    test('折扣率：两位精度、去掉末尾 0（995 是 9.95 折，不是 10 折）', () {
      expect(rateText(900), '9折');
      expect(rateText(850), '8.5折');
      expect(rateText(995), '9.95折');
    });
    test('规则：满减 / 无门槛 / 折扣最高减 / 立减 / 包邮', () {
      expect(couponRuleText(1, 5000, 1000, 0, 0), '满¥50减¥10');
      expect(couponRuleText(1, 0, 500, 0, 0), '无门槛减¥5');
      expect(couponRuleText(2, 10000, 0, 900, 3000), '满¥100打9折，最高减¥30');
      expect(couponRuleText(3, 0, 500, 0, 0), '立减¥5');
      expect(couponRuleText(4, 9900, 0, 0, 0), '满¥99包邮');
    });
    test('截止是开区间：10-07 0 点过期写成「10-06 到期」', () {
      expect(couponOption(ApplicableCoupon.fromJson(coupon(1, 1000))).validText, '2026-10-06 到期');
    });
    test('省多少用这一单的 applicable_discount_cents（9 折券面额是 0）', () {
      expect(couponOption(ApplicableCoupon.fromJson(coupon(1, 730, type: 2, rate: 900))).saveText, '-¥7.30');
    });
  });

  test('试算：各行金额与说明；券列表服务端排好（第一张最省）', () {
    final v = previewView(OrderPreview.fromJson(preview(couponId: 3, couponOff: 1000, coupons: [coupon(3, 1000), coupon(4, 500)])));
    expect(v.goodsAmountText, '¥100.00');
    expect(v.freightDiscountText, '-¥8.00');
    expect(v.couponDiscountText, '-¥10.00');
    expect(v.promotionDiscountText, '');
    expect(v.payableText, '¥90.00');
    expect(v.payableCents, 9000);
    expect(v.couponId, 3);
    expect(v.coupons.map((c) => c.id), [3, 4]);
  });

  test('订单详情：收货人带街道；活动行；已发货的自动确认时间；待支付能取消', () {
    final d = orderDetailView(OrderDetail.fromJson({
      ...order('N1', 30),
      'receiver': {'receiver_name': '张三', 'phone': '139', 'province': '浙江省', 'city': '杭州市', 'district': '西湖区',
        'street': '文三路', 'detail': '1 号楼'},
      'promotions': [{'promotion_id': 1, 'name': '满199减20', 'promotion_type': 1, 'discount_cents': 2000, 'sku_ids': []}],
      'discount_cents': 2000, 'paid_cents': 9200, 'auto_confirm_at': '2026-09-30T06:05:00Z',
      'items': [{'id': 1, 'sku_id': 5, 'product_id': 9, 'title': '豆子', 'price_cents': 5600, 'quantity': 2, 'amount_cents': 11200}],
      'payments': [{'channel': 'wechat', 'status': 1, 'amount_cents': 9200}],
    }), (s) => s);
    expect(d.receiver, '张三 139 浙江省 杭州市 西湖区 文三路 1 号楼');
    expect(d.promotionLines, ['满199减20 -¥20.00']);
    expect(d.head.statusText, '已发货');
    expect(d.canConfirm, isTrue);
    expect(d.canCancel, isFalse);
    expect(d.autoConfirmAt, isNotEmpty);
    expect(d.payments.single.channelText, '微信');
    expect(d.payments.single.statusText, '成功');
    expect(d.hasDiscount, isTrue);
    expect(d.items.single.amountText, '¥112.00');
  });

  group('下单', () {
    test('in-flight：按 Retry-After 等一会儿、同一个键重试', () async {
      var n = 0;
      final (c, seen) = await client((r) async {
        if (n++ == 0) {
          return j({'type': 'https://keel.dev/problems/idempotency-key-in-flight', 'title': '处理中', 'status': 409}, 409, {'retry-after': '1'});
        }
        return j(order('N2', 10), 201);
      });
      final waits = <Duration>[];
      final r = await placeOrder(c, lines: [(skuId: 1, quantity: 1)], addressId: 1, storeId: 1, expectedPayableCents: 9200,
          couponId: null, idempotencyKey: 'k', wait: (d) async => waits.add(d));
      expect(r.orderNo, 'N2');
      expect(r.replayed, isFalse);
      expect(waits, [const Duration(seconds: 1)]);
      expect(seen.map((s) => s.headers['Idempotency-Key']).toSet(), {'k'});
      final body = jsonDecode(seen.last.body) as Map;
      expect(body['expected_payable_cents'], 9200);
      expect(body.containsKey('user_coupon_id'), isFalse);
    });
    test('重放：Idempotency-Replayed 带出来', () async {
      final (c, _) = await client((r) async => j(order('N3', 10), 201, {'idempotency-replayed': 'true'}));
      final r = await placeOrder(c, lines: [(skuId: 1, quantity: 1)], addressId: 1, storeId: 1, expectedPayableCents: null,
          couponId: 7, idempotencyKey: 'k');
      expect(r.replayed, isTrue);
    });
  });

  test('沙箱：信封原样投到服务地址的 origin + settle.url，不带令牌', () async {
    final (c, seen) = await client((r) async {
      if (r.url.path.endsWith('/payments')) {
        return j({'payment_no': 'P1', 'channel': 'wechat', 'amount_cents': 9200, 'payload': {
          'sandbox': true, 'sandbox_notice': '沙箱', 'settle': {'method': 'POST', 'url': '/api/v1/webhooks/payments/wechat',
            'headers': {'X-Sign': 'abc', 'Content-Type': 'application/json'}, 'body': '{"a":1}'}}}, 201);
      }
      return http.Response('', 204);
    }, base: 'https://shop.example/api/v1');
    final intent = await startPayment(c, 'N1', 'wechat', 'pk');
    expect(intent.sandbox, isTrue);
    expect(intent.notice, '沙箱');
    await settleSandbox(c, intent.settle!);
    final hook = seen.last;
    expect(hook.url.toString(), 'https://shop.example/api/v1/webhooks/payments/wechat');
    expect(hook.body, '{"a":1}');
    expect(hook.headers['X-Sign'], 'abc');
    expect(hook.headers.containsKey('Authorization'), isFalse);
  });

  test('同城配送的试算：叫配送费、写距离；地址没坐标时提示选点', () {
    Map<String, dynamic> local(int? d) => {...preview(), 'freight': {'mode': 'local', 'freight_cents': 600, 'freight_discount_cents': 0,
      'groups': [], 'local': {'distance_m': d, 'tier_fee_cents': 600, 'free_over_cents': 0, 'min_order_cents': 0, 'shortfall_cents': 0}}};
    final v = previewView(OrderPreview.fromJson(local(1500)));
    expect((v.freightLabel, v.freightNote, v.needsPin), ('配送费', '距离 1.5 公里', false));
    final w = previewView(OrderPreview.fromJson(local(null)));
    expect((w.freightNote, w.needsPin), ('按最远一档计', true));
    expect(previewView(OrderPreview.fromJson(preview())).freightLabel, '运费');
  });

  test('多收款退回：订单详情上每张一句（已退回 / 正在退回）', () {
    const r1 = PaymentReturn(returnNo: 'PR1', amountCents: 6880, reason: 1, status: 40, createdAt: '2026-09-28T00:00:00Z');
    const r2 = PaymentReturn(returnNo: 'PR2', amountCents: 100, reason: 2, status: 30, createdAt: '2026-09-28T00:00:00Z');
    expect(paymentReturnLine(r1), '多付的 ¥68.80（重复支付）已原路退回');
    expect(paymentReturnLine(r2), '多付的 ¥1.00（订单关闭后到账）正在原路退回');
  });

  test('试算：available_qty 小于 quantity 的行记为缺货，文案区分售罄', () {
    final p = OrderPreview.fromJson({
      'store_id': 1, 'goods_amount_cents': 100, 'freight_cents': 0, 'freight_discount_cents': 0,
      'freight': {'freight_cents': 0, 'freight_discount_cents': 0, 'groups': []},
      'payable_cents': 100, 'promotion_discount_cents': 0, 'coupon_discount_cents': 0, 'promotions': [],
      'items': [
        {'sku_id': 1, 'quantity': 2, 'price_cents': 50, 'list_price_cents': 50, 'amount_cents': 100, 'discount_cents': 0,
         'promotion_discount_cents': 0, 'available_qty': 1},
        {'sku_id': 2, 'quantity': 1, 'price_cents': 50, 'list_price_cents': 50, 'amount_cents': 50, 'discount_cents': 0,
         'promotion_discount_cents': 0, 'available_qty': 5},
        {'sku_id': 3, 'quantity': 1, 'price_cents': 50, 'list_price_cents': 50, 'amount_cents': 50, 'discount_cents': 0,
         'promotion_discount_cents': 0},
      ],
    });
    expect(previewView(p).shortages, {1: 1});
    expect(shortageText(1), '库存不足，仅剩 1 件');
    expect(shortageText(0), '库存不足，已售罄');
  });

  test('售后期：截止之前可申请，过了不给入口', () {
    final now = DateTime.utc(2026, 10, 1, 12);
    expect(afterSaleExpired(null, now), isFalse);
    expect(afterSaleExpired('2026-10-02T00:00:00Z', now), isFalse);
    expect(afterSaleExpired('2026-10-01T00:00:00Z', now), isTrue);
  });
}
