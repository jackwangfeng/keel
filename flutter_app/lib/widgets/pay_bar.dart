import 'package:flutter/material.dart';

import '../theme.dart';

/// 结算 / 订单详情的底栏：左边「应付」小字在上、金额在下，右边一个定宽的主按钮（uni-app x 的 .bar）。
class PayBar extends StatelessWidget {
  const PayBar({super.key, required this.amount, required this.button, this.amountKey, this.buttonWidth = 160});
  final String amount;
  final Widget button;
  final Key? amountKey;
  final double buttonWidth;

  @override
  Widget build(BuildContext context) => Container(
        decoration: const BoxDecoration(color: KeelColors.card, border: Border(top: BorderSide(color: KeelColors.line))),
        child: SafeArea(
          top: false,
          child: Padding(
            padding: const EdgeInsets.fromLTRB(16, 10, 16, 10),
            child: Row(children: [
              Expanded(
                child: Column(mainAxisSize: MainAxisSize.min, crossAxisAlignment: CrossAxisAlignment.start, children: [
                  const Text('应付', style: KeelText.hint),
                  FittedBox(
                    fit: BoxFit.scaleDown,
                    alignment: Alignment.centerLeft,
                    child: Text(amount, key: amountKey, style: KeelText.priceL),
                  ),
                ]),
              ),
              const SizedBox(width: 12),
              SizedBox(width: buttonWidth, child: button),
            ]),
          ),
        ),
      );
}

/// 单选圈：20 的圆，选中时深咖边 + 中间一个点（uni-app x 的 .radio）。
class KeelRadio extends StatelessWidget {
  const KeelRadio({super.key, required this.on});
  final bool on;
  @override
  Widget build(BuildContext context) => Container(
        width: 20, height: 20,
        alignment: Alignment.center,
        decoration: BoxDecoration(shape: BoxShape.circle, border: Border.all(color: on ? KeelColors.primary : const Color(0xFFD3C8BA))),
        child: on
            ? Container(width: 10, height: 10, decoration: const BoxDecoration(color: KeelColors.primary, shape: BoxShape.circle))
            : null,
      );
}
