import 'package:flutter/material.dart';

import '../theme.dart';

/// 券面：左边大字（减多少 / 几折 / 包邮），右边名字、规则、范围与有效期，底部可放按钮。
class Ticket extends StatelessWidget {
  const Ticket({super.key, required this.value, required this.name, required this.rule, required this.meta,
      this.foot, this.dim = false});
  final String value;
  final String name;
  final String rule;
  final String meta;
  final Widget? foot;
  final bool dim;

  @override
  Widget build(BuildContext context) => Opacity(
        opacity: dim ? 0.55 : 1,
        child: Container(
          margin: const EdgeInsets.only(bottom: 10),
          decoration: BoxDecoration(color: KeelColors.card, borderRadius: BorderRadius.circular(14)),
          clipBehavior: Clip.antiAlias,
          child: IntrinsicHeight(
            child: Row(crossAxisAlignment: CrossAxisAlignment.stretch, children: [
              Container(
                width: 96,
                color: const Color(0xFFF3E4D6),
                alignment: Alignment.center,
                child: Text(value, style: const TextStyle(fontSize: 22, fontWeight: FontWeight.w800, color: KeelColors.err)),
              ),
              Expanded(
                child: Padding(
                  padding: const EdgeInsets.all(12),
                  child: Column(crossAxisAlignment: CrossAxisAlignment.start, children: [
                    Text(name, style: KeelText.body.copyWith(fontWeight: FontWeight.w700)),
                    Text(rule, style: KeelText.sub),
                    Text(meta, style: KeelText.hint),
                    if (foot != null) Padding(padding: const EdgeInsets.only(top: 6), child: foot),
                  ]),
                ),
              ),
            ]),
          ),
        ),
      );
}
