import 'dart:async';

import 'package:flutter/material.dart';
import 'package:flutter_map/flutter_map.dart';
import 'package:latlong2/latlong.dart' as ll;

import '../api/client.dart';
import '../api/geo.dart';
import '../api/locate.dart';
import '../api/services.dart';
import '../theme.dart';

/// 地图选点（App / Web；小程序用微信自带的选点页）：中心固定一枚图钉，拖动 / 缩放地图，停下 400ms 后
/// reverse 一次显示「当前位置」；确定返回省市区已补全的 [Place]。底图走服务端代理（GET /geo/tiles），
/// 坐标 WGS-84，不做 GCJ-02 转换。
///
/// 初始中心：[near]（首页当前的送货坐标）→ 设备定位 → 默认城市中心。
class MapPickerPage extends StatefulWidget {
  const MapPickerPage({super.key, required this.config, this.near, this.locate = locateDevice, this.tileProvider});
  final MapConfig config;
  final LatLng? near;
  final Future<LatLng?> Function() locate;
  /// 测试里换成不发网络请求的；默认按网络拉瓦片。
  final TileProvider? tileProvider;

  /// 没有任何坐标时的中心（北京市中心）。
  static const fallback = (lat: 39.9087, lng: 116.3975);

  @override
  State<MapPickerPage> createState() => _MapPickerPageState();
}

enum _At { loading, done, off, down, error }

class _MapPickerPageState extends State<MapPickerPage> {
  final _map = MapController();
  Timer? _debounce;
  int _seq = 0;
  bool _started = false;

  ll.LatLng? _start;
  double _startZoom = 16;
  ll.LatLng? _center;
  LatLng? _mine;

  _At _at = _At.loading;
  Place? _here;
  bool _busy = false;

  @override
  void didChangeDependencies() {
    super.didChangeDependencies();
    if (!_started) {
      _started = true;
      _init();
    }
  }

  @override
  void dispose() {
    _debounce?.cancel();
    _map.dispose();
    super.dispose();
  }

  Future<LatLng?> _locate() async {
    try {
      return await widget.locate().timeout(const Duration(seconds: 8));
    } catch (_) {
      // 拒绝授权、没开定位、超时都按拿不到：地图照样能拖。
      return null;
    }
  }

  Future<void> _init() async {
    var at = widget.near;
    if (at == null) {
      at = _mine = await _locate();
    } else {
      // 有初始坐标也顺手定一次位，拿到了才给「回到我的位置」。
      unawaited(_locate().then((m) {
        if (mounted && m != null) setState(() => _mine = m);
      }));
    }
    if (!mounted) return;
    final c = at ?? MapPickerPage.fallback;
    setState(() {
      _start = _center = ll.LatLng(c.lat, c.lng);
      _startZoom = at == null ? 11 : 16;
    });
    _reverseNow();
  }

  void _moved(MapCamera camera, bool _) {
    _center = camera.center;
    _debounce?.cancel();
    ++_seq; // 还在动：之前发出去的查询作废
    if (_at != _At.loading || _here != null) setState(() => _at = _At.loading);
    _debounce = Timer(const Duration(milliseconds: 400), _reverseNow);
  }

  Future<void> _reverseNow() async {
    final c = _center;
    if (c == null) return;
    final mine = ++_seq;
    setState(() {
      _at = _At.loading;
      _here = null;
    });
    try {
      final p = await reverseGeocode(Services.of(context).client, c.latitude, c.longitude);
      if (!mounted || mine != _seq) return;
      setState(() {
        _here = p;
        _at = _At.done;
      });
    } on ApiFailure catch (f) {
      if (!mounted || mine != _seq) return;
      setState(() => _at = geoOff(f) ? _At.off : (geoDown(f) ? _At.down : _At.error));
    }
  }

  Future<void> _toMine() async {
    final m = await _locate() ?? _mine;
    if (!mounted || m == null) return;
    setState(() => _mine = m);
    _map.move(ll.LatLng(m.lat, m.lng), _map.camera.zoom < 15 ? 16 : _map.camera.zoom);
  }

  Future<void> _confirm() async {
    final c = _center;
    if (c == null || _busy) return;
    setState(() => _busy = true);
    final h = _here;
    // 查到了就用它（坐标用图钉的）；没查到再试一次补全，还不行就只带坐标，省市区留给用户手填。
    var p = h != null
        ? Place(name: h.name, address: h.address, province: h.province, city: h.city, district: h.district,
            adcode: h.adcode, street: h.street, lat: c.latitude, lng: c.longitude)
        : await completePlace(Services.of(context).client, Place(name: '', address: '', lat: c.latitude, lng: c.longitude));
    if (p.name.isEmpty && p.address.isEmpty) {
      p = Place(name: '地图上选的位置', address: '', province: p.province, city: p.city, district: p.district,
          adcode: p.adcode, street: p.street, lat: p.lat, lng: p.lng);
    }
    if (mounted) Navigator.of(context).pop(p);
  }

  @override
  Widget build(BuildContext context) {
    final start = _start;
    return Scaffold(
      appBar: AppBar(title: const Text('在地图上选点')),
      body: start == null
          ? const Center(child: Text('正在定位…', key: Key('map.locating'), style: KeelText.hint))
          : Stack(children: [
              FlutterMap(
                key: const Key('map.view'),
                mapController: _map,
                options: MapOptions(
                  initialCenter: start,
                  initialZoom: _startZoom,
                  minZoom: 3,
                  maxZoom: widget.config.maxZoom.toDouble(),
                  interactionOptions: const InteractionOptions(flags: InteractiveFlag.all & ~InteractiveFlag.rotate),
                  onPositionChanged: _moved,
                ),
                children: [
                  for (final layer in widget.config.layers)
                    TileLayer(
                      key: Key('map.layer.$layer'),
                      urlTemplate: mapTileUrl(Services.of(context).client, layer),
                      maxNativeZoom: widget.config.maxZoom,
                      userAgentPackageName: 'dev.keel.keel_buyer',
                      tileProvider: widget.tileProvider,
                    ),
                  // 不用 SimpleAttributionWidget：它会自己在前面拼「flutter_map | ©」，而服务商的署名本身带 ©，
                  // 显示出来是「© © 天地图」。署名照服务端给的原样显示。
                  if (widget.config.attribution.isNotEmpty)
                    Align(
                      alignment: Alignment.bottomRight,
                      child: ColoredBox(
                        color: KeelColors.card.withValues(alpha: 0.8),
                        child: Padding(
                          padding: const EdgeInsets.symmetric(horizontal: 6, vertical: 2),
                          child: Text(widget.config.attribution, key: const Key('map.attribution'), style: KeelText.hint),
                        ),
                      ),
                    ),
                ],
              ),
              // 图钉的尖对着地图中心。
              const IgnorePointer(
                child: Center(
                  child: Padding(
                    padding: EdgeInsets.only(bottom: 40),
                    child: Icon(Icons.location_on, key: Key('map.pin'), size: 40, color: KeelColors.accent),
                  ),
                ),
              ),
              if (_mine != null)
                Positioned(
                  right: 12,
                  bottom: 36,
                  child: Material(
                    color: KeelColors.card,
                    shape: const CircleBorder(side: BorderSide(color: KeelColors.line)),
                    child: IconButton(
                      key: const Key('map.mine'),
                      tooltip: '回到我的位置',
                      onPressed: _toMine,
                      icon: const Icon(Icons.my_location, size: 20, color: KeelColors.textSub),
                    ),
                  ),
                ),
            ]),
      bottomNavigationBar: Container(
        decoration: const BoxDecoration(color: KeelColors.card, border: Border(top: BorderSide(color: KeelColors.line))),
        child: SafeArea(
          top: false,
          child: Padding(
            padding: const EdgeInsets.fromLTRB(16, 12, 16, 10),
            child: Column(mainAxisSize: MainAxisSize.min, crossAxisAlignment: CrossAxisAlignment.stretch, children: [
              Row(crossAxisAlignment: CrossAxisAlignment.start, children: [
                const Padding(padding: EdgeInsets.only(top: 2), child: Icon(Icons.place_outlined, size: 18, color: KeelColors.textSub)),
                const SizedBox(width: 8),
                Expanded(child: _hereText()),
              ]),
              const SizedBox(height: 12),
              FilledButton(
                key: const Key('map.confirm'),
                onPressed: start == null || _busy || _at == _At.loading ? null : _confirm,
                child: Text(_busy ? '处理中…' : '确定'),
              ),
            ]),
          ),
        ),
      ),
    );
  }

  Widget _hereText() {
    final h = _here;
    final (line, sub) = switch (_at) {
      _At.loading => ('正在获取位置…', ''),
      _At.done when h != null => (
          '当前位置：${geoLabel(h)}',
          [h.province, h.city, h.district, if (h.name.isNotEmpty) h.address].where((s) => s.isNotEmpty).join(' '),
        ),
      _At.off || _At.done => ('当前位置：地图上选的点', '地址查询暂未开通，确定后请手动补全省市区'),
      _At.down => ('当前位置：地图上选的点', '地址查询暂时不可用，确定后请手动补全省市区'),
      _At.error => ('当前位置：地图上选的点', '暂时查不到这里的地址，可以挪一下再试'),
    };
    return Column(crossAxisAlignment: CrossAxisAlignment.start, children: [
      Text(line, key: const Key('map.here'), style: KeelText.body, maxLines: 2, overflow: TextOverflow.ellipsis),
      if (sub.isNotEmpty) Text(sub, style: KeelText.hint, maxLines: 1, overflow: TextOverflow.ellipsis),
    ]);
  }
}
