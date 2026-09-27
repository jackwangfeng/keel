import 'package:flutter/material.dart';

import '../theme.dart';

/// 券面：左边大字（减多少 / 几折 / 包邮），右边名字、规则、范围与有效期，底部可放按钮。
class Ticket extends StatelessWidget {
  const Ticket({super.key, required this.value, required this.name, required this.rule, required this.meta,
      this.foot, this.dim = false, this.meta2 = ''});
  final String value;
  final String name;
  final String rule;
  final String meta;
  /// 第二行说明（我的优惠券：范围一行、有效期一行）。
  final String meta2;
  final Widget? foot;
  final bool dim;

  @override
  Widget build(BuildContext context) => Opacity(
        opacity: dim ? 0.55 : 1,
        child: Container(
          margin: const EdgeInsets.only(top: 12),
          decoration: BoxDecoration(color: KeelColors.card, borderRadius: BorderRadius.circular(14)),
          clipBehavior: Clip.antiAlias,
          child: IntrinsicHeight(
            child: Row(crossAxisAlignment: CrossAxisAlignment.stretch, children: [
              // 左边深咖底、米金色的大字（uni-app x 的 .ticket-value）。
              Container(
                width: 96,
                color: KeelColors.primary,
                alignment: Alignment.center,
                padding: const EdgeInsets.symmetric(horizontal: 6, vertical: 16),
                child: FittedBox(
                  fit: BoxFit.scaleDown,
                  child: Text(value, style: const TextStyle(fontSize: 24, fontWeight: FontWeight.w700, color: Color(0xFFF3D9B1))),
                ),
              ),
              Expanded(
                child: Padding(
                  padding: const EdgeInsets.all(14),
                  child: Column(crossAxisAlignment: CrossAxisAlignment.start, children: [
                    Padding(
                      padding: const EdgeInsets.only(bottom: 4),
                      child: Text(name, style: const TextStyle(fontSize: 15, fontWeight: FontWeight.w700, color: KeelColors.text)),
                    ),
                    Text(rule, style: KeelText.sub),
                    Text(meta, style: KeelText.hint),
                    if (meta2.isNotEmpty) Text(meta2, style: KeelText.hint),
                    if (foot != null) Padding(padding: const EdgeInsets.only(top: 10), child: foot),
                  ]),
                ),
              ),
            ]),
          ),
        ),
      );
}
