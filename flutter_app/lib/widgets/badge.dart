import 'package:flutter/material.dart';

import '../theme.dart';

/// 状态徽章：tone 是 warn / ok / refund / muted（订单）或售后的 warn / ok / err / muted。
class ToneBadge extends StatelessWidget {
  const ToneBadge({super.key, required this.text, required this.tone});
  final String text;
  final String tone;
  @override
  Widget build(BuildContext context) {
    final (bg, fg) = switch (tone) {
      'warn' => (const Color(0xFFF6EAD3), KeelColors.warn),
      'ok' => (const Color(0xFFE6EEE3), KeelColors.ok),
      'refund' || 'err' => (const Color(0xFFF8EAE5), KeelColors.err),
      _ => (const Color(0xFFEDE8E2), KeelColors.textSub),
    };
    return Container(
      padding: const EdgeInsets.symmetric(horizontal: 8, vertical: 2),
      decoration: BoxDecoration(color: bg, borderRadius: BorderRadius.circular(6)),
      child: Text(text, style: TextStyle(fontSize: 12, color: fg, fontWeight: FontWeight.w600)),
    );
  }
}
