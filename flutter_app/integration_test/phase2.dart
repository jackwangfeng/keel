import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:keel_buyer/widgets/form_bits.dart';

import 'e2e_env.dart';
import 'helpers.dart';

const nineOff = '全场 9 折最高减 30';

Future<Map> waitPaidNote(WidgetTester t, String orderNo) => waitUntil(t, () async {
      final items = (((await Api.get('/me/notifications?page_size=50')) as Map)['items'] as List).cast<Map>();
      return items.where((x) => x['kind'] == 'order_paid' && (x['target'] as Map)['order_no'] == orderNo).firstOrNull;
    }, '付款后的「支付成功」通知', timeout: const Duration(seconds: 30));

Future<void> tapTwice(WidgetTester t, String key) async {
  await tapKey(t, key);
  await waitFor(t, find.descendant(of: byKey(key), matching: find.textContaining('再点一次')));
  await tapKey(t, key);
}

void phase2Tests() {
  group('优惠券', () {
    e2e('领券中心领「9 折」：领到或已领过，最后按钮都是「已领取」；我的优惠券四个 tab 能切、「9 折」在其中一个', (t) async {
      await startApp(t);
      await loginInApp(t);
      await tapKey(t, 'me.couponCenter');
      await waitFor(t, find.text(nineOff));
      final tpl = ((((await Api.get('/coupon-templates?page_size=50')) as Map)['items']) as List).cast<Map>()
          .firstWhere((x) => x['name'] == nineOff);
      final id = tpl['id'];
      if (tpl['can_claim'] == true) {
        await tapKey(t, 'center.claim.$id');
        await waitFor(t, byKey('center.message'));
        final m = textOf('center.message');
        expect(m.contains('已领取') || m.contains('已经领到') || m.contains('已经领过'), isTrue, reason: m);
      }
      await waitFor(t, keyedText('center.action.$id', '已领取'));
      await tapKey(t, 'center.mine');
      var found = false;
      for (final s in ['available', 'locked', 'used', 'expired']) {
        await tapKey(t, 'coupons.tab.$s');
        await t.pump(const Duration(milliseconds: 800));
        await waitUntil(t, () async => find.byWidgetPredicate((w) => '${w.key}'.contains('coupons.row.') || '${w.key}'.contains('coupons.empty')).evaluate().isNotEmpty ? true : null, 'tab $s 加载出来');
        // 列表是懒构建的：这张券可能在首屏以外，往下滚着找。
        if (!found && find.byWidgetPredicate((w) => '${w.key}'.contains('coupons.row.')).evaluate().isNotEmpty) {
          try {
            await t.scrollUntilVisible(find.text(nineOff), 200, scrollable: find.byType(Scrollable).last, maxScrolls: 30);
            found = true;
          } catch (_) {}
        }
      }
      expect(found, isTrue);
    });

    e2e('结算页：有能用的券就自动用上最省的那张；选「不使用」优惠归零', (t) async {
      await startApp(t);
      await loginInApp(t);
      final sku = await Api.pickSku(1);
      final addr = (await Api.addresses()).firstWhere((a) => a['is_default'] == true)['id'] as int;
      final coupons = ((await Api.preview(sku.skuId, 1, addr))['applicable_coupons'] as List? ?? const []).cast<Map>();
      await open(t, '/checkout?sku_id=${sku.skuId}&product_id=${sku.productId}');
      await waitFor(t, find.byWidgetPredicate((w) => w is Text && w.key == const Key('checkout.payable') && w.data != '—'));
      if (coupons.isEmpty) {
        expect(byKey('checkout.couponOff'), findsNothing);
        return;
      }
      expect(textOf('checkout.couponSummary'), startsWith('${coupons.first['name']}'));
      await tapKey(t, 'checkout.coupons');
      await tapKey(t, 'checkout.coupon.none');
      await waitFor(t, byKey('checkout.discount'));
      expect(byKey('checkout.couponOff'), findsNothing);
    });
  });

  group('消息中心', () {
    e2e('付款后消息页出现「支付成功」，点进去是那笔订单并标成已读；「全部标为已读」后未读是 0', (t) async {
      await startApp(t);
      await loginInApp(t);
      await Api.call('POST', '/me/notifications/read-all');
      final no = await Api.placeOrder(pay: true);
      final note = await waitPaidNote(t, no);
      expect(note['read_at'], isNull);
      await tapKey(t, 'me.notifications');
      await waitFor(t, byKey('notes.dot.${note['id']}'));
      await tapKey(t, 'notes.row.${note['id']}');
      await waitFor(t, keyedText('order.status', '已支付'));
      await waitUntil(t, () async {
        final items = (((await Api.get('/me/notifications?page_size=50')) as Map)['items'] as List).cast<Map>();
        return items.firstWhere((x) => x['id'] == note['id'])['read_at'];
      }, '服务端标成已读');

      final no2 = await Api.placeOrder(pay: true);
      await waitPaidNote(t, no2);
      await open(t, '/notifications');
      await waitFor(t, byKey('notes.readAll'), timeout: const Duration(seconds: 30));
      await tapKey(t, 'notes.readAll');
      await waitFor(t, keyedText('notes.unread', '全部已读'));
      expect(((await Api.get('/me/notifications/unread-count')) as Map)['unread_count'], 0);
      expect(byKey('tab.meBadge'), findsNothing);
    });

    if (fixtureRejectedRefund.isNotEmpty) {
      e2e('售后驳回的通知点进去是那张售后单', (t) async {
        await startApp(t);
        await loginInApp(t);
        final note = await Api.findNotification((x) => x['kind'] == 'refund_rejected' && (x['target'] as Map)['refund_no'] == fixtureRejectedRefund);
        if (note == null) fail('没找到售后单 $fixtureRejectedRefund 的驳回通知');
        await tapKey(t, 'me.notifications');
        await waitFor(t, byKey('notes.unread'));
        // 它可能不在第一页（后面的用例又产生了新通知）：一页页往下翻。
        for (var i = 0; i < 60 && byKey('notes.row.${note['id']}').evaluate().isEmpty; i++) {
          if (byKey('notes.more').evaluate().isNotEmpty) {
            await t.ensureVisible(byKey('notes.more'));
            await t.pump();
            await t.tap(byKey('notes.more'));
            await t.pump(const Duration(seconds: 1));
          } else {
            await t.drag(find.byType(Scrollable).first, const Offset(0, -800));
            await t.pump(const Duration(milliseconds: 300));
          }
        }
        await t.ensureVisible(byKey('notes.row.${note['id']}'));
        await t.pump();
        await tapKey(t, 'notes.row.${note['id']}');
        await waitFor(t, keyedText('refund.status', '已拒绝'));
      });
    }
  });

  group('售后', () {
    e2e('已支付的单申请仅退款（未发货只能仅退款）→ 待审核，金额由服务端算；撤回后是已取消', (t) async {
      await startApp(t);
      await loginInApp(t);
      final no = await Api.placeOrder(pay: true);
      await open(t, '/orders/$no');
      await waitFor(t, keyedText('order.status', '已支付'));
      await tapKey(t, 'order.refund');
      await waitFor(t, byKey('apply.submit'));
      expect(t.widget<OptChip>(byKey('apply.type.2')).onTap, isNull);
      await tapKey(t, 'apply.type.1');
      await tapKey(t, 'apply.reason.1');
      await tapKey(t, 'apply.submit');
      await waitFor(t, keyedText('refund.status', '待审核'));
      final refunds = ((await Api.get('/orders/$no/refunds')) as List).cast<Map>();
      final r = refunds.single;
      expect(r['status'], 10);
      expect(r['refund_type'], 1);
      final cents = r['amount_cents'] as int;
      expect(textOf('refund.amount'), '¥${cents ~/ 100}.${(cents % 100).toString().padLeft(2, '0')}');
      await tapTwice(t, 'refund.cancel');
      await waitFor(t, keyedText('refund.status', '已取消'));
      expect(((await Api.get('/refunds/${r['refund_no']}')) as Map)['status'], 60);
    });

    e2e('带凭证的售后单：凭证图带令牌读出来显示', (t) async {
      await startApp(t);
      await loginInApp(t);
      final no = await Api.placeOrder(pay: true);
      final url = await Api.uploadPng();
      final d = (await Api.get('/orders/$no')) as Map;
      final r = await Api.idem('POST', '/orders/$no/refunds', {
        'items': [{'order_item_id': ((d['items'] as List).first as Map)['id'], 'quantity': 1}],
        'refund_type': 1, 'reason_code': 3, 'evidence_urls': [url],
      }) as Map;
      await open(t, '/refunds/${r['refund_no']}');
      await waitFor(t, keyedText('refund.status', '待审核'));
      await waitFor(t, find.descendant(of: byKey('refund.evidence.0'), matching: find.byType(Image)));
      expect(find.text('读取失败'), findsNothing);
      await Api.idem('POST', '/refunds/${r['refund_no']}/cancel', null);
    });

    if (fixtureReturnRefund.isNotEmpty) {
      e2e('待买家退货：填寄回物流，再改一次，状态仍是待买家退货', (t) async {
        await startApp(t);
        await loginInApp(t);
        final before = (await Api.get('/refunds/$fixtureReturnRefund')) as Map;
        await open(t, '/refunds/$fixtureReturnRefund');
        await waitFor(t, keyedText('refund.status', '待买家退货'));
        if (before['return_shipment'] == null) expect(byKey('refund.deadline'), findsOneWidget);
        final no1 = 'E2E${DateTime.now().millisecondsSinceEpoch}';
        await tapKey(t, 'refund.carrier.sf');
        await typeInto(t, 'refund.tracking', no1);
        await tapKey(t, 'refund.ship');
        await waitFor(t, keyedTextContaining('refund.returnFilled', no1));
        var server = (await Api.get('/refunds/$fixtureReturnRefund')) as Map;
        expect(server['status'], 20);
        expect((server['return_shipment'] as Map)['tracking_no'], no1);
        // 回来再改一次（重新打开这一页：表单按服务端已填的那一份预填）。
        // 不在同一页里接着改：Web 上同一个输入框第二次 enterText 收不到（测试输入的连接只接第一次），不是页面的问题 ——
        // 同一流程的页面测试（refund_page_test）里连改两次是好的。
        await open(t, '/refunds/$fixtureReturnRefund');
        await waitFor(t, keyedTextContaining('refund.returnFilled', no1));
        String ctl() => t.widget<EditableText>(find.descendant(of: byKey('refund.tracking').last, matching: find.byType(EditableText))).controller.text;
        expect(ctl(), no1, reason: '表单预填已提交的单号');
        await tapKey(t, 'refund.carrier.jd');
        await typeInto(t, 'refund.tracking', '${no1}B');
        expect(ctl(), '${no1}B', reason: '打完字控制器里的值');
        await tapKey(t, 'refund.ship');
        await waitFor(t, keyedTextContaining('refund.returnFilled', '${no1}B'));
        server = (await Api.get('/refunds/$fixtureReturnRefund')) as Map;
        expect(server['status'], 20);
        expect((server['return_shipment'] as Map)['carrier_code'], 'jd');
      });
    }

    if (fixtureRejectedRefund.isNotEmpty) {
      e2e('商家驳回：显示驳回理由，可以重新申请', (t) async {
        await startApp(t);
        await loginInApp(t);
        final server = (await Api.get('/refunds/$fixtureRejectedRefund')) as Map;
        await open(t, '/refunds/$fixtureRejectedRefund');
        await waitFor(t, keyedText('refund.status', '已拒绝'));
        expect(textOf('refund.reject'), contains('${server['reject_reason']}'));
        expect(byKey('refund.reapply'), findsOneWidget);
      });
    }

    if (fixtureRefundedRefund.isNotEmpty) {
      e2e('商家同意仅退款：已退款，订单详情显示已退金额', (t) async {
        await startApp(t);
        await loginInApp(t);
        final r = (await Api.get('/refunds/$fixtureRefundedRefund')) as Map;
        await open(t, '/refunds/$fixtureRefundedRefund');
        await waitFor(t, keyedText('refund.status', '已退款'));
        await open(t, '/orders/${r['order_no']}');
        await waitFor(t, byKey('order.status'));
        final cents = r['amount_cents'] as int;
        await waitFor(t, find.text('¥${cents ~/ 100}.${(cents % 100).toString().padLeft(2, '0')}'));
      });
    }

    if (fixtureShippedOrder.isNotEmpty) {
      e2e('已发货的单确认收货后是已完成', (t) async {
        await startApp(t);
        await loginInApp(t);
        await open(t, '/orders/$fixtureShippedOrder');
        await waitFor(t, keyedText('order.status', '已发货'));
        expect(textOf('order.statusLine'), contains('未确认将自动确认收货'));
        await tapTwice(t, 'order.confirm');
        await waitFor(t, keyedText('order.status', '已完成'));
      });
    }
  });
}
