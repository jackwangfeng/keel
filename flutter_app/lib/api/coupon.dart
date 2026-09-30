import 'client.dart';
import 'schema.g.dart';
import 'view.dart';

/// 券的文案。三种券的形状（可领的模板 / 我的券 / 这一单能用的券）字段大半重合，
/// 共用的文案函数只收原始字段。最初从 uni-app x 版的 view.uts 移植（见 tag uniapp-final）。

/// 折扣率（千分比，900 = 9 折）->「9折」「8.5折」「9.95折」：两位小数，去掉末尾的 0。
String rateText(int rate) {
  final whole = rate ~/ 100;
  final rest = rate % 100;
  if (rest == 0) return '$whole折';
  final frac = rest.toString().padLeft(2, '0');
  return '$whole.${frac.endsWith('0') ? frac.substring(0, 1) : frac}折';
}

/// 券面上最大的那几个字：减多少 / 几折 / 包邮。
String couponValueText(int couponType, int discountCents, int discountRate) => switch (couponType) {
      2 => rateText(discountRate),
      4 => '包邮',
      _ => faceYuan(discountCents),
    };

/// 使用规则：满 50 减 10 / 满 100 打 9 折，最高减 30 / 立减 5 / 满 99 包邮。
String couponRuleText(int couponType, int thresholdCents, int discountCents, int discountRate, int maxDiscountCents) {
  final gate = thresholdCents > 0 ? '满${faceYuan(thresholdCents)}' : '';
  switch (couponType) {
    case 1:
      return gate.isNotEmpty ? '$gate减${faceYuan(discountCents)}' : '无门槛减${faceYuan(discountCents)}';
    case 2:
      final t = '${gate.isNotEmpty ? '$gate打' : '全场'}${rateText(discountRate)}';
      return maxDiscountCents > 0 ? '$t，最高减${faceYuan(maxDiscountCents)}' : t;
    case 3:
      return '立减${faceYuan(discountCents)}';
    case 4:
      return gate.isNotEmpty ? '$gate包邮' : '无门槛包邮';
  }
  return '';
}

/// 适用范围：「全场可用」/「限咖啡豆、挂耳；不含 xx 店」。target_name 缺了就写编号。
String couponScopeText(List<CouponScope> scopes) {
  final inc = <String>[];
  final exc = <String>[];
  for (final sc in scopes) {
    if (sc.scopeType == 1) continue;
    var name = sc.targetName ?? '';
    if (name.isEmpty) name = '#${sc.targetId ?? '?'}';
    (sc.include ? inc : exc).add(name);
  }
  var t = inc.isNotEmpty ? '限${inc.join('、')}' : '全场可用';
  if (exc.isNotEmpty) t += '；不含${exc.join('、')}';
  return t;
}

String _p2(int n) => n.toString().padLeft(2, '0');

/// 「2026-09-30T16:00:00Z」->「2026-10-01」：按设备本地时区取日期。
String day(String iso) {
  final d = DateTime.tryParse(iso);
  if (d == null) return iso.length >= 10 ? iso.substring(0, 10) : iso;
  final l = d.toLocal();
  return '${l.year}-${_p2(l.month)}-${_p2(l.day)}';
}

/// 有效期的截止日：valid_end_at 是开区间（到那一刻就过期），往前退 1 毫秒再取日期，写出来的是最后能用的那天。
String endDay(String iso) {
  final d = DateTime.tryParse(iso);
  if (d == null) return day(iso);
  final l = d.subtract(const Duration(milliseconds: 1)).toLocal();
  return '${l.year}-${_p2(l.month)}-${_p2(l.day)}';
}

/// 结算页选券列表里的一项。
class CouponOption {
  final int id;
  final String name;
  final String ruleText;
  final String validText;
  /// 「-¥10」：这一单能省多少（applicable_discount_cents），不是券面额。
  final String saveText;
  const CouponOption({required this.id, required this.name, required this.ruleText, required this.validText, required this.saveText});
}

CouponOption couponOption(ApplicableCoupon c) => CouponOption(
      id: c.id,
      name: c.name,
      ruleText: couponRuleText(c.couponType, c.thresholdCents, c.discountCents, c.discountRate, c.maxDiscountCents),
      validText: '${endDay(c.validEndAt)} 到期',
      saveText: '-${yuan(c.applicableDiscountCents)}',
    );

// ---- 领券中心 / 我的优惠券 ----

class ClaimRow {
  final int id;
  final String name;
  final String valueText;
  final String ruleText;
  final String scopeText;
  final String validText;
  final bool canClaim;
  /// 领取 / 已领取 / 已领完 / 不可领。
  final String actionText;
  /// 「剩 12 张」；不限量是空串。
  final String remainingText;
  const ClaimRow({required this.id, required this.name, required this.valueText, required this.ruleText,
      required this.scopeText, required this.validText, required this.canClaim, required this.actionText,
      required this.remainingText});
}

ClaimRow claimRow(ClaimableCouponTemplate t) {
  final valid = t.validMode == 2
      ? '领取后 ${t.validDays} 天内有效'
      : '${t.validStartAt == null ? '' : day(t.validStartAt!)} 至 ${t.validEndAt == null ? '' : endDay(t.validEndAt!)}';
  // 次序有讲究：领过又领完了的，对这个人来说是「已领取」—— 那是他关心的那件事。
  final action = t.canClaim
      ? '领取'
      : t.claimedCount > 0
          ? '已领取'
          : (t.remaining != null && t.remaining! <= 0)
              ? '已领完'
              : '不可领';
  return ClaimRow(
    id: t.id,
    name: t.name,
    valueText: couponValueText(t.couponType, t.discountCents, t.discountRate),
    ruleText: couponRuleText(t.couponType, t.thresholdCents, t.discountCents, t.discountRate, t.maxDiscountCents),
    scopeText: couponScopeText(t.scopes),
    validText: valid,
    canClaim: t.canClaim,
    actionText: action,
    remainingText: t.remaining != null ? '剩 ${t.remaining} 张' : '',
  );
}

class CouponRow {
  final int id;
  final String name;
  final String valueText;
  final String ruleText;
  final String scopeText;
  final String validText;
  const CouponRow({required this.id, required this.name, required this.valueText, required this.ruleText,
      required this.scopeText, required this.validText});
}

CouponRow couponRow(UserCoupon c) => CouponRow(
      id: c.id,
      name: c.name,
      valueText: couponValueText(c.couponType, c.discountCents, c.discountRate),
      ruleText: couponRuleText(c.couponType, c.thresholdCents, c.discountCents, c.discountRate, c.maxDiscountCents),
      scopeText: couponScopeText(c.scopes),
      validText: c.usedAt != null ? '使用于 ${shortTime(c.usedAt!)}' : '${day(c.validStartAt)} 至 ${endDay(c.validEndAt)}',
    );

Future<List<ClaimRow>> fetchClaimable(ApiClient c) async => (await c.send('GET', '/coupon-templates',
        query: {'page': '1', 'page_size': '50'}, decode: (j) => ListCouponTemplatesResponse.fromJson(j as Map<String, dynamic>)))
    .data
    .items
    .map(claimRow)
    .toList();

/// 领券（幂等键每张模板一个，页面活着期间复用：超时后再点是重放，不会领两张、也不会被说成「已达上限」）。
/// 返回券名与是否重放；常见问题翻成人话。
Future<({String name, bool replayed})> claimCoupon(ApiClient c, int templateId, String idempotencyKey) async {
  try {
    final res = await c.send('POST', '/coupon-templates/$templateId/claim', idempotencyKey: idempotencyKey,
        decode: (j) => UserCoupon.fromJson(j as Map<String, dynamic>));
    return (name: res.data.name, replayed: res.replayed);
  } on ApiFailure catch (f) {
    if (f.isType('coupon-claim-limit-reached')) throw f.withMessage('这张券你已经领过了');
    if (f.isType('coupon-sold-out')) throw f.withMessage('来晚了，这张券已经领完');
    if (f.isType('coupon-claim-ended')) throw f.withMessage('这张券的领取时间已经结束');
    if (f.status == 404) throw f.withMessage('这张券已经下架了');
    rethrow;
  }
}

/// 我的优惠券的四个 tab，与 GET /coupons 的 status 一一对应。
const couponTabs = [('available', '未使用'), ('locked', '使用中'), ('used', '已使用'), ('expired', '已过期')];

Future<List<CouponRow>> fetchMyCoupons(ApiClient c, String status) async => (await c.send('GET', '/coupons',
        query: {'status': status, 'page': '1', 'page_size': '50'},
        decode: (j) => ListCouponsResponse.fromJson(j as Map<String, dynamic>)))
    .data
    .items
    .map(couponRow)
    .toList();
