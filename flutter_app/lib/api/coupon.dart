import 'schema.g.dart';
import 'view.dart';

/// 券的文案。三种券的形状（可领的模板 / 我的券 / 这一单能用的券）字段大半重合，
/// 共用的文案函数只收原始字段。照 uni-app x 的 view.uts 移植。

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
