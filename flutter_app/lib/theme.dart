import 'package:flutter/material.dart';

/// 设计系统：最初照 uni-app x 买家端的 App.uvue 搬过来（uni-app x 已下线，源码见 tag uniapp-final）。
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
  static const display = TextStyle(fontSize: 26, fontWeight: FontWeight.w700, color: KeelColors.text, letterSpacing: 1);
  static const title = TextStyle(fontSize: 18, fontWeight: FontWeight.w700, color: KeelColors.text);
  static const section = TextStyle(fontSize: 15, fontWeight: FontWeight.w700, color: KeelColors.text);
  static const body = TextStyle(fontSize: 14, color: KeelColors.text, height: 22 / 14);
  static const sub = TextStyle(fontSize: 13, color: KeelColors.textSub, height: 20 / 13);
  static const hint = TextStyle(fontSize: 12, color: KeelColors.textHint, height: 18 / 12);
  static const overline = TextStyle(fontSize: 11, fontWeight: FontWeight.w700, letterSpacing: 2, color: KeelColors.accent);
  static const price = TextStyle(fontSize: 16, fontWeight: FontWeight.w700, color: KeelColors.primary);
  /// 底栏合计、详情页价格。
  static const priceL = TextStyle(fontSize: 24, fontWeight: FontWeight.w700, color: KeelColors.primary);
  static const link = TextStyle(fontSize: 13, color: KeelColors.accent);
  /// 凑单 / 活动说明（服务端的 message 原样）。
  static const promo = TextStyle(fontSize: 12, color: KeelColors.err, height: 18 / 12);
  static const err = TextStyle(fontSize: 13, color: KeelColors.err, height: 20 / 13);
  static const ok = TextStyle(fontSize: 13, color: KeelColors.ok, height: 20 / 13);
}

/// actionsRight：顶栏右侧按钮要让出的宽度（小程序的胶囊按钮，见 capsule.dart）。
ThemeData keelTheme({double actionsRight = 0}) {
  return ThemeData(
    useMaterial3: true,
    scaffoldBackgroundColor: KeelColors.bg,
    colorScheme: ColorScheme.fromSeed(
      seedColor: KeelColors.primary,
      primary: KeelColors.primary,
      surface: KeelColors.card,
    ),
    appBarTheme: AppBarTheme(
      backgroundColor: KeelColors.bg,
      foregroundColor: KeelColors.text,
      elevation: 0,
      centerTitle: true,
      // 与 uni-app x 的导航栏同高（44）、标题 17 号。
      toolbarHeight: 44,
      titleTextStyle: const TextStyle(fontSize: 17, fontWeight: FontWeight.w600, color: KeelColors.text),
      surfaceTintColor: KeelColors.bg,
      scrolledUnderElevation: 0,
      actionsPadding: actionsRight > 0 ? EdgeInsets.only(right: actionsRight) : null,
    ),
    cardTheme: CardThemeData(
      color: KeelColors.card,
      elevation: 0,
      shape: RoundedRectangleBorder(borderRadius: BorderRadius.circular(16)),
    ),
    // 桌面 / Web 上默认是紧凑密度，会把按钮整体压扁 8 像素：与 uni-app x 对齐要标准密度。
    visualDensity: VisualDensity.standard,
    // 主按钮：48 高、圆角 24、15 号粗体（uni-app x 的 .btn）；点不了是 0.4 透明，不换颜色（.btn-off）。
    filledButtonTheme: FilledButtonThemeData(
      style: ButtonStyle(
        backgroundColor: WidgetStateProperty.resolveWith(
            (st) => st.contains(WidgetState.disabled) ? KeelColors.primary.withValues(alpha: 0.4) : KeelColors.primary),
        foregroundColor: const WidgetStatePropertyAll(KeelColors.card),
        overlayColor: WidgetStatePropertyAll(KeelColors.card.withValues(alpha: 0.08)),
        minimumSize: const WidgetStatePropertyAll(Size(64, 48)),
        padding: const WidgetStatePropertyAll(EdgeInsets.symmetric(horizontal: 20)),
        shape: WidgetStatePropertyAll(RoundedRectangleBorder(borderRadius: BorderRadius.circular(24))),
        textStyle: const WidgetStatePropertyAll(TextStyle(fontSize: 15, fontWeight: FontWeight.w700, letterSpacing: 1)),
        elevation: const WidgetStatePropertyAll(0),
      ),
    ),
    // 描边按钮：44 高、圆角 22、1 像素深咖边（.btn-ghost）。
    outlinedButtonTheme: OutlinedButtonThemeData(
      style: ButtonStyle(
        foregroundColor: WidgetStateProperty.resolveWith(
            (st) => st.contains(WidgetState.disabled) ? KeelColors.primary.withValues(alpha: 0.4) : KeelColors.primary),
        minimumSize: const WidgetStatePropertyAll(Size(64, 44)),
        padding: const WidgetStatePropertyAll(EdgeInsets.symmetric(horizontal: 18)),
        side: WidgetStateProperty.resolveWith((st) =>
            BorderSide(color: st.contains(WidgetState.disabled) ? KeelColors.primary.withValues(alpha: 0.4) : KeelColors.primary)),
        shape: WidgetStatePropertyAll(RoundedRectangleBorder(borderRadius: BorderRadius.circular(22))),
        textStyle: const WidgetStatePropertyAll(TextStyle(fontSize: 14)),
      ),
    ),
    textButtonTheme: TextButtonThemeData(
      style: TextButton.styleFrom(foregroundColor: KeelColors.accent, textStyle: const TextStyle(fontSize: 13)),
    ),
    dividerColor: KeelColors.line,
  );
}
