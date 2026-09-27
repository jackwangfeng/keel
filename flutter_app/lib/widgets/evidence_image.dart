import 'dart:typed_data';

import 'package:flutter/material.dart';

import '../api/services.dart';
import '../theme.dart';

/// 凭证图：要带令牌读（本人才能看），所以不能直接 Image.network —— 先取字节再显示。
class EvidenceImage extends StatefulWidget {
  const EvidenceImage({super.key, required this.url, this.size = 72});
  final String url;
  final double size;
  @override
  State<EvidenceImage> createState() => _EvidenceImageState();
}

class _EvidenceImageState extends State<EvidenceImage> {
  Uint8List? _bytes;
  bool _failed = false;
  bool _started = false;

  @override
  void didChangeDependencies() {
    super.didChangeDependencies();
    if (_started) return;
    _started = true;
    Services.of(context).client.bytesOf(widget.url).then((b) {
      if (mounted) setState(() => _bytes = Uint8List.fromList(b));
    }, onError: (_) {
      if (mounted) setState(() => _failed = true);
    });
  }

  @override
  Widget build(BuildContext context) => ClipRRect(
        borderRadius: BorderRadius.circular(10),
        child: SizedBox(
          width: widget.size,
          height: widget.size,
          child: _bytes != null
              ? Image.memory(_bytes!, fit: BoxFit.cover)
              : ColoredBox(
                  color: KeelColors.line,
                  child: Center(child: Text(_failed ? '读取失败' : '…', style: KeelText.hint)),
                ),
        ),
      );
}
