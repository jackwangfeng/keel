import 'dart:convert';
import 'dart:typed_data';

import 'package:image_picker/image_picker.dart';
import 'package:mp_flutter_wechat/mp_flutter_wechat.dart';

import 'client.dart';
import 'schema.g.dart';

/// 售后凭证图：选图与上传。小程序里走微信的 chooseMedia + uploadFile（mp-flutter 的微信桥）；
/// Web / 原生走 image_picker 读出字节再 multipart 上传。上传：POST /uploads，purpose=3，要令牌与幂等键
/// （同一张图的重试复用同一个键）。201 回的 url 形如 /api/v1/uploads/{id}，原样放进售后申请的 evidence_urls。

class PickedImage {
  /// Web / 原生：图片字节（页面拿它显示缩略图）。
  final Uint8List? bytes;
  /// 小程序：微信给的临时文件路径（页面显示不了它，只显示占位）。
  final String? mpPath;
  final String name;
  final String contentType;
  const PickedImage({this.bytes, this.mpPath, required this.name, required this.contentType});
}

String _typeOf(String name) {
  final n = name.toLowerCase();
  if (n.endsWith('.png')) return 'image/png';
  if (n.endsWith('.webp')) return 'image/webp';
  if (n.endsWith('.gif')) return 'image/gif';
  return 'image/jpeg';
}

/// 选最多 max 张。只开相册：凭证图通常本来就在相册里，拍照还要多申请相机权限。用户取消返回空表。
Future<List<PickedImage>> pickImages(int max) async {
  if (max <= 0) return const [];
  if (MpWechat.isAvailable) {
    try {
      final r = await MpWechat.call('chooseMedia', {
        'count': max,
        'mediaType': ['image'],
        'sourceType': ['album'],
        'sizeType': ['compressed'],
      });
      final files = (r['tempFiles'] as List?) ?? const [];
      return [
        for (final f in files.whereType<Map>())
          if (f['tempFilePath'] is String)
            PickedImage(mpPath: f['tempFilePath'] as String, name: '${f['tempFilePath']}'.split('/').last,
                contentType: _typeOf('${f['tempFilePath']}')),
      ];
    } on MpWechatException catch (e) {
      if (e.cancelled) return const [];
      rethrow;
    }
  }
  final xs = await ImagePicker().pickMultiImage(limit: max, imageQuality: 80);
  return [
    for (final x in xs.take(max))
      PickedImage(bytes: await x.readAsBytes(), name: x.name, contentType: x.mimeType ?? _typeOf(x.name)),
  ];
}

/// 上传一张，返回 /api/v1/uploads/{id}。
Future<String> uploadEvidence(ApiClient c, PickedImage img, String idempotencyKey) async {
  if (img.mpPath != null) {
    final r = await MpWechat.call('uploadFile', {
      'url': '${c.base}/uploads',
      'filePath': img.mpPath,
      'name': 'file',
      'header': {
        'Accept': 'application/json, application/problem+json',
        'Idempotency-Key': idempotencyKey,
        if ((c.accessToken ?? '').isNotEmpty) 'Authorization': 'Bearer ${c.accessToken}',
      },
      'formData': {'purpose': '3'},
    });
    final status = (r['statusCode'] as num?)?.toInt() ?? 0;
    final text = '${r['data'] ?? ''}';
    if (status >= 200 && status < 300) return Upload.fromJson(jsonDecode(text) as Map<String, dynamic>).url;
    throw c.failureOf(status, text);
  }
  final res = await c.upload('/uploads', bytes: img.bytes!, filename: img.name, contentType: img.contentType,
      fields: {'purpose': '3'}, idempotencyKey: idempotencyKey, decode: (j) => Upload.fromJson(j as Map<String, dynamic>));
  return res.data.url;
}
