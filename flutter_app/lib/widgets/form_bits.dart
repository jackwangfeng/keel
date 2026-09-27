import 'package:flutter/material.dart';

import '../theme.dart';

/// 表单的一行：左边 72 宽的标签、右边内容，52 高，底下一条细线；出错时细线变红（uni-app x 的 .field / .field-bad）。
class FieldRow extends StatelessWidget {
  const FieldRow({super.key, required this.label, required this.child, this.bad = false, this.last = false});
  final String label;
  final Widget child;
  final bool bad;
  /// 卡片里的最后一行不画底线。
  final bool last;

  @override
  Widget build(BuildContext context) => Container(
        constraints: const BoxConstraints(minHeight: 52),
        decoration: BoxDecoration(
          border: last && !bad ? null : Border(bottom: BorderSide(color: bad ? KeelColors.err : KeelColors.line)),
        ),
        child: Row(children: [
          SizedBox(width: 72, child: Text(label, style: const TextStyle(fontSize: 14, color: KeelColors.textSub))),
          Expanded(child: child),
        ]),
      );
}

/// 行内输入框：无边框、15 号字（配合 FieldRow）。
InputDecoration inlineInput(String hint) => InputDecoration(
      border: InputBorder.none,
      isCollapsed: true,
      hintText: hint,
      hintStyle: KeelText.hint.copyWith(fontSize: 15),
      counterText: '',
    );

const inlineInputStyle = TextStyle(fontSize: 15, color: KeelColors.text);

/// 选项胶囊（uni-app x 的 .opt / .chip）：选中深咖底白字；不可选变淡。small 是资料页性别那种小号。
class OptChip extends StatelessWidget {
  const OptChip({super.key, required this.label, required this.on, required this.onTap, this.small = false});
  final String label;
  final bool on;
  /// null = 不可选。
  final VoidCallback? onTap;
  final bool small;

  @override
  Widget build(BuildContext context) => Opacity(
        opacity: onTap == null && !on ? 0.4 : 1,
        child: GestureDetector(
          onTap: onTap,
          child: Container(
            padding: small ? const EdgeInsets.symmetric(horizontal: 14, vertical: 5) : const EdgeInsets.symmetric(horizontal: 16, vertical: 8),
            decoration: BoxDecoration(
              color: on ? KeelColors.primary : null,
              border: Border.all(color: on ? KeelColors.primary : KeelColors.chipBorder),
              borderRadius: BorderRadius.circular(small ? 15 : 16),
            ),
            child: Text(label, style: TextStyle(fontSize: small ? 13 : 14, color: on ? KeelColors.card : KeelColors.text)),
          ),
        ),
      );
}

/// 开关：44×26 的胶囊，开是深咖（uni-app x 的 .switch）。
class KeelSwitch extends StatelessWidget {
  const KeelSwitch({super.key, required this.value, required this.onChanged});
  final bool value;
  final ValueChanged<bool> onChanged;
  @override
  Widget build(BuildContext context) => GestureDetector(
        onTap: () => onChanged(!value),
        child: AnimatedContainer(
          duration: const Duration(milliseconds: 150),
          width: 44, height: 26,
          padding: const EdgeInsets.all(3),
          alignment: value ? Alignment.centerRight : Alignment.centerLeft,
          decoration: BoxDecoration(color: value ? KeelColors.primary : KeelColors.chipBorder, borderRadius: BorderRadius.circular(13)),
          child: Container(width: 20, height: 20, decoration: const BoxDecoration(color: KeelColors.card, shape: BoxShape.circle)),
        ),
      );
}

/// 白色卡片：圆角 16、内边距 18、上面 12（uni-app x 的 .card）；可带一个小标题（.t-overline）和右上角的字。
class KeelCard extends StatelessWidget {
  const KeelCard({super.key, required this.child, this.title, this.trailing, this.padding = const EdgeInsets.all(18), this.color});
  final Widget child;
  final String? title;
  final Widget? trailing;
  final EdgeInsets padding;
  final Color? color;

  @override
  Widget build(BuildContext context) => Container(
        margin: const EdgeInsets.only(top: 12),
        padding: padding,
        decoration: BoxDecoration(color: color ?? KeelColors.card, borderRadius: BorderRadius.circular(16)),
        child: Column(crossAxisAlignment: CrossAxisAlignment.start, children: [
          if (title != null)
            Padding(
              padding: const EdgeInsets.only(bottom: 12),
              child: Row(children: [Expanded(child: Text(title!, style: KeelText.overline)), ?trailing]),
            ),
          child,
        ]),
      );
}
