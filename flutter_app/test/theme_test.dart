import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:keel_buyer/theme.dart';

void main() {
  test('色板与 uni-app x 的 App.uvue 一致', () {
    expect(KeelColors.bg, const Color(0xFFF6F1EA));
    expect(KeelColors.card, const Color(0xFFFFFDF9));
    expect(KeelColors.primary, const Color(0xFF3B2A20));
    expect(KeelColors.err, const Color(0xFFB4462E));
  });

  test('主题：底色与主色', () {
    final t = keelTheme();
    expect(t.scaffoldBackgroundColor, KeelColors.bg);
    expect(t.colorScheme.primary, KeelColors.primary);
  });
}
