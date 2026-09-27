import 'package:flutter/material.dart';

import '../theme.dart';

/// 状态徽章：tone 是 warn / ok / refund / muted（订单）或售后的 warn / ok / err / muted。
class ToneBadge extends StatelessWidget {
  const ToneBadge({super.key, required this.text, required this.tone});
  final String text;
  final String tone;
  @override
  Widget build(BuildContext context) {
    // 与 uni-app x 的 .badge-warn / -ok / -refund / -muted 同色。
    final (bg, fg) = switch (tone) {
      'warn' => (const Color(0xFFF6E7CF), const Color(0xFF9A6414)),
      'ok' => (const Color(0xFFE3EBDF), const Color(0xFF4D6A49)),
      'refund' || 'err' => (const Color(0xFFF3DDD5), const Color(0xFF9C3F28)),
      _ => (const Color(0xFFEEE9E3), KeelColors.textSub),
    };
    return Container(
      padding: const EdgeInsets.symmetric(horizontal: 10, vertical: 3),
      decoration: BoxDecoration(color: bg, borderRadius: BorderRadius.circular(10)),
      child: Text(text, style: TextStyle(fontSize: 12, color: fg)),
    );
  }
}
