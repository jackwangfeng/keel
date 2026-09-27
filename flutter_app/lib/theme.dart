import 'package:flutter/material.dart';

/// 设计系统：照 app/src/App.uvue（uni-app x 买家端）搬过来，两端看起来是同一个产品。
/// 改色板只改这一个文件。
class KeelColors {
  static const bg = Color(0xFFF6F1EA);
  static const card = Color(0xFFFFFDF9);
  static const line = Color(0xFFECE4DA);
  static const primary = Color(0xFF3B2A20);
  static const accent = Color(0xFFB5733A);
  static const text = Color(0xFF2A211B);
  static const textSub = Color(0xFF7A6E64);
  static const textHint = Color(0xFFA89C90);
  static const ok = Color(0xFF5E7D5A);
  static const warn = Color(0xFFB7791F);
  static const err = Color(0xFFB4462E);
  static const chipBorder = Color(0xFFE3D9CC);
  static const searchBox = Color(0xFFEFE7DC);
}

class KeelText {
  static const display = TextStyle(fontSize: 28, fontWeight: FontWeight.w700, color: KeelColors.text);
  static const title = TextStyle(fontSize: 18, fontWeight: FontWeight.w700, color: KeelColors.text);
  static const section = TextStyle(fontSize: 16, fontWeight: FontWeight.w700, color: KeelColors.text);
  static const body = TextStyle(fontSize: 14, color: KeelColors.text);
  static const sub = TextStyle(fontSize: 13, color: KeelColors.textSub);
  static const hint = TextStyle(fontSize: 12, color: KeelColors.textHint);
  static const overline = TextStyle(fontSize: 12, fontWeight: FontWeight.w700, letterSpacing: 3, color: KeelColors.accent);
  static const price = TextStyle(fontSize: 16, fontWeight: FontWeight.w700, color: KeelColors.primary);
  static const err = TextStyle(fontSize: 13, color: KeelColors.err, height: 1.5);
  static const ok = TextStyle(fontSize: 13, color: KeelColors.ok, height: 1.5);
}

ThemeData keelTheme() {
  return ThemeData(
    useMaterial3: true,
    scaffoldBackgroundColor: KeelColors.bg,
    colorScheme: ColorScheme.fromSeed(
      seedColor: KeelColors.primary,
      primary: KeelColors.primary,
      surface: KeelColors.card,
    ),
    appBarTheme: const AppBarTheme(
      backgroundColor: KeelColors.bg,
      foregroundColor: KeelColors.text,
      elevation: 0,
      centerTitle: true,
    ),
    cardTheme: CardThemeData(
      color: KeelColors.card,
      elevation: 0,
      shape: RoundedRectangleBorder(borderRadius: BorderRadius.circular(16)),
    ),
    filledButtonTheme: FilledButtonThemeData(
      style: FilledButton.styleFrom(
        backgroundColor: KeelColors.primary,
        foregroundColor: KeelColors.card,
        minimumSize: const Size(64, 48),
        shape: RoundedRectangleBorder(borderRadius: BorderRadius.circular(24)),
      ),
    ),
    dividerColor: KeelColors.line,
  );
}
