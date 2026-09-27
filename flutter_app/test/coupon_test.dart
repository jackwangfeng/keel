import 'package:flutter_test/flutter_test.dart';
import 'package:keel_buyer/api/coupon.dart';
import 'package:keel_buyer/api/schema.g.dart';

Map<String, dynamic> tpl({bool can = true, int claimed = 0, int? remaining, int mode = 1, int days = 0}) => {
      'id': 3, 'name': '满99包邮', 'coupon_type': 4, 'threshold_cents': 9900, 'discount_cents': 0, 'discount_rate': 0,
      'max_discount_cents': 0, 'valid_mode': mode, 'valid_start_at': DateTime(2026, 9, 1).toUtc().toIso8601String(),
      'valid_end_at': DateTime(2026, 10, 8).toUtc().toIso8601String(), 'valid_days': days, 'remaining': ?remaining,
      'per_user_limit': 1, 'claimed_count': claimed, 'can_claim': can,
      'scopes': [{'scope_type': 2, 'target_id': 7, 'include': true, 'target_name': '咖啡豆'}, {'scope_type': 3, 'target_id': 9, 'include': false}],
    };

void main() {
  test('领券按钮：能领 / 领过（哪怕已领完）/ 领完 / 其他不可领', () {
    expect(claimRow(ClaimableCouponTemplate.fromJson(tpl())).actionText, '领取');
    expect(claimRow(ClaimableCouponTemplate.fromJson(tpl(can: false, claimed: 1, remaining: 0))).actionText, '已领取');
    expect(claimRow(ClaimableCouponTemplate.fromJson(tpl(can: false, remaining: 0))).actionText, '已领完');
    expect(claimRow(ClaimableCouponTemplate.fromJson(tpl(can: false))).actionText, '不可领');
  });

  test('剩余张数：不限量是空串；券面、规则、范围（缺名字写编号）、有效期', () {
    final r = claimRow(ClaimableCouponTemplate.fromJson(tpl(remaining: 12)));
    expect(r.remainingText, '剩 12 张');
    expect(claimRow(ClaimableCouponTemplate.fromJson(tpl())).remainingText, '');
    expect(r.valueText, '包邮');
    expect(r.ruleText, '满¥99包邮');
    expect(r.scopeText, '限咖啡豆；不含#9');
    expect(r.validText, '2026-09-01 至 2026-10-07');
    expect(claimRow(ClaimableCouponTemplate.fromJson(tpl(mode: 2, days: 7))).validText, '领取后 7 天内有效');
  });

  test('我的券：用过的写使用时间，没用的写有效期', () {
    final base = {'id': 1, 'coupon_code': 'C', 'template_id': 3, 'name': '9 折', 'coupon_type': 2, 'threshold_cents': 0,
      'discount_cents': 0, 'discount_rate': 900, 'max_discount_cents': 0, 'status': 1, 'source': 1,
      'valid_start_at': DateTime(2026, 9, 1).toUtc().toIso8601String(), 'valid_end_at': DateTime(2026, 10, 1).toUtc().toIso8601String(), 'scopes': []};
    final r = couponRow(UserCoupon.fromJson(base));
    expect(r.valueText, '9折');
    expect(r.ruleText, '全场9折');
    expect(r.scopeText, '全场可用');
    expect(r.validText, '2026-09-01 至 2026-09-30');
    expect(couponRow(UserCoupon.fromJson({...base, 'used_at': '2026-09-26T00:05:12Z'})).validText, startsWith('使用于 '));
  });
}
