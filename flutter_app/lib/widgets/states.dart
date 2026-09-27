import 'package:flutter/material.dart';

import '../theme.dart';

class ErrorCard extends StatelessWidget {
  const ErrorCard({super.key, required this.message, required this.onRetry});
  final String message;
  final VoidCallback onRetry;
  @override
  Widget build(BuildContext context) => Card(
        margin: const EdgeInsets.all(16),
        child: Padding(
          padding: const EdgeInsets.all(18),
          child: Column(crossAxisAlignment: CrossAxisAlignment.start, children: [
            Text(message, style: KeelText.err),
            const SizedBox(height: 16),
            OutlinedButton(onPressed: onRetry, child: const Text('重新加载')),
          ]),
        ),
      );
}

class EmptyState extends StatelessWidget {
  const EmptyState({super.key, required this.text});
  final String text;
  @override
  Widget build(BuildContext context) => Padding(
        padding: const EdgeInsets.symmetric(vertical: 48),
        child: Center(child: Text(text, style: KeelText.sub)),
      );
}
