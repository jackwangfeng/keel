import 'dart:async';

import 'package:flutter/material.dart';
import 'package:go_router/go_router.dart';

import '../api/address.dart';
import '../api/client.dart';
import '../api/geo.dart';
import '../api/schema.g.dart';
import '../api/services.dart';
import '../theme.dart';
import '../widgets/form_bits.dart';

/// 选出来的送货位置。[device] = 改回用当前定位；[at] 为 null = 选了一条没坐标、也没查到坐标的收货地址。
/// [place] 是搜索地点 / 地图选点选中的完整地点（省市区已补全，填收货地址用）。
class PickedPlace {
  final String label;
  final LatLng? at;
  final GeoPlace? place;
  final bool device;
  const PickedPlace({required this.label, this.at, this.place, this.device = false});
  const PickedPlace.device() : this(label: '', device: true);
  PickedPlace.of(GeoPlace p) : this(label: geoLabel(p), at: (lat: p.lat, lng: p.lng), place: p);
}

/// 选地点：输入提示（/geo/suggest）+ 地图选点（小程序）；首页换地址时另有「使用当前定位」和收货地址列表。
/// 没配地图服务商（501）时搜索框说明暂未开通 —— 降级，不当错误；收货地址照样能选。
class PlacePickerPage extends StatefulWidget {
  const PlacePickerPage({super.key, this.forAddress = false});
  /// 从地址编辑页进来：只要一个地点（省市区、坐标），不列收货地址、不给当前定位。
  final bool forAddress;
  @override
  State<PlacePickerPage> createState() => _PlacePickerPageState();
}

enum _Geo { idle, loading, done, off, down, error }

class _PlacePickerPageState extends State<PlacePickerPage> {
  final _input = TextEditingController();
  Timer? _debounce;
  _Geo _geo = _Geo.idle;
  List<GeoPlace> _hits = const [];
  String _error = '';
  // 连着敲字时只认最后一次发出去的请求。
  int _seq = 0;
  bool _busy = false;

  List<AddressRow>? _addresses;
  String _addressError = '';
  bool _needLogin = false;
  bool _started = false;

  @override
  void didChangeDependencies() {
    super.didChangeDependencies();
    if (!_started) {
      _started = true;
      if (!widget.forAddress) _loadAddresses();
    }
  }

  @override
  void dispose() {
    _debounce?.cancel();
    _input.dispose();
    super.dispose();
  }

  Future<void> _loadAddresses() async {
    try {
      final rows = await fetchAddresses(Services.of(context).client);
      if (mounted) setState(() => _addresses = rows);
    } on ApiFailure catch (f) {
      if (!mounted) return;
      setState(() {
        _needLogin = f.status == 401;
        _addressError = f.message;
      });
    }
  }

  void _onChanged(String _) {
    _debounce?.cancel();
    // 501 是这台服务端没开通，再敲也一样，不再发。
    if (_geo == _Geo.off) return;
    _debounce = Timer(const Duration(milliseconds: 300), _suggest);
  }

  Future<void> _suggest() async {
    final q = _input.text.trim();
    final mine = ++_seq;
    if (q.isEmpty) {
      setState(() {
        _geo = _Geo.idle;
        _hits = const [];
      });
      return;
    }
    final s = Services.of(context);
    setState(() => _geo = _Geo.loading);
    try {
      final hits = await suggestPlaces(s.client, q, near: s.store.at);
      if (!mounted || mine != _seq) return;
      setState(() {
        _hits = hits;
        _geo = _Geo.done;
      });
    } on ApiFailure catch (f) {
      if (!mounted || mine != _seq) return;
      setState(() {
        _geo = geoOff(f) ? _Geo.off : (geoDown(f) ? _Geo.down : _Geo.error);
        _error = f.message;
      });
    }
  }

  Future<void> _pickPlace(GeoPlace p) async {
    if (_busy) return;
    setState(() => _busy = true);
    final full = await completePlace(Services.of(context).client, p);
    if (mounted) context.pop(PickedPlace.of(full));
  }

  Future<void> _pickOnMap() async {
    final p = await pickOnMap(near: Services.of(context).store.at);
    if (p != null && mounted) await _pickPlace(p);
  }

  /// 收货地址：有坐标直接用；老地址没有，先用地址全文搜一次取第一个候选的坐标；拿不到（含 501）就不带坐标。
  Future<void> _pickAddress(AddressRow a) async {
    if (_busy) return;
    var at = a.at;
    if (at == null) {
      setState(() => _busy = true);
      try {
        final hits = await suggestPlaces(Services.of(context).client, a.fullText);
        if (hits.isNotEmpty) at = (lat: hits.first.lat, lng: hits.first.lng);
      } on ApiFailure {
        // 查不到就按没坐标处理：首页回落默认店并照实提示。
      }
    }
    if (mounted) context.pop(PickedPlace(label: a.detail, at: at));
  }

  @override
  Widget build(BuildContext context) {
    return Scaffold(
      appBar: AppBar(title: Text(widget.forAddress ? '搜索地点' : '选择送货地址')),
      body: Column(children: [
        Padding(
          padding: const EdgeInsets.fromLTRB(16, 10, 16, 10),
          child: Container(
            height: 40,
            padding: const EdgeInsets.symmetric(horizontal: 14),
            decoration: BoxDecoration(color: KeelColors.searchBox, borderRadius: BorderRadius.circular(20)),
            child: Row(children: [
              const Icon(Icons.search, size: 17, color: KeelColors.textHint),
              const SizedBox(width: 6),
              Expanded(
                child: TextField(
                  key: const Key('place.input'),
                  controller: _input,
                  autofocus: widget.forAddress,
                  maxLength: 64,
                  onChanged: _onChanged,
                  style: const TextStyle(fontSize: 14, color: KeelColors.text),
                  decoration: const InputDecoration(
                    counterText: '',
                    border: InputBorder.none,
                    isCollapsed: true,
                    hintText: '搜索小区、写字楼、学校',
                    hintStyle: KeelText.hint,
                  ),
                ),
              ),
            ]),
          ),
        ),
        Expanded(
          child: ListView(padding: const EdgeInsets.fromLTRB(16, 0, 16, 24), children: [
            ..._geoSection(),
            if (_geo != _Geo.done && _geo != _Geo.loading) ..._shortcuts(),
          ]),
        ),
      ]),
    );
  }

  List<Widget> _geoSection() => switch (_geo) {
        _Geo.idle => const [],
        _Geo.loading => const [Padding(padding: EdgeInsets.all(12), child: Text('正在搜索…', style: KeelText.hint))],
        _Geo.off => [
            _note('place.off', widget.forAddress ? '地点搜索暂未开通，请返回手动填写地址' : '地点搜索暂未开通，可以从下面的收货地址里选'),
          ],
        _Geo.down => [_note('place.down', '地点搜索暂时不可用，稍后再试${widget.forAddress ? '，或返回手动填写' : ''}')],
        _Geo.error => [_note('place.error', _error)],
        _Geo.done when _hits.isEmpty => [_note('place.empty', '没有找到「${_input.text.trim()}」')],
        _Geo.done => [
            KeelCard(
              padding: EdgeInsets.zero,
              child: Column(children: [
                for (final (i, p) in _hits.indexed)
                  _row(
                    key: Key('place.hit.$i'),
                    icon: Icons.place_outlined,
                    title: p.name.isNotEmpty ? p.name : p.address,
                    sub: [p.district, p.address].where((s) => s.isNotEmpty).join(' · '),
                    onTap: () => _pickPlace(p),
                    last: i == _hits.length - 1,
                  ),
              ]),
            ),
          ],
      };

  List<Widget> _shortcuts() {
    final rows = <Widget>[
      if (canPickOnMap)
        _row(key: const Key('place.map'), icon: Icons.map_outlined, title: '在地图上选点', onTap: _pickOnMap, last: widget.forAddress),
      if (!widget.forAddress)
        _row(key: const Key('place.device'), icon: Icons.my_location, title: '使用当前定位',
            onTap: () => context.pop(const PickedPlace.device()), last: true),
    ];
    return [
      if (rows.isNotEmpty) KeelCard(padding: EdgeInsets.zero, child: Column(children: rows)),
      if (!widget.forAddress) ..._addressSection(),
    ];
  }

  List<Widget> _addressSection() {
    final list = _addresses;
    return [
      const Padding(padding: EdgeInsets.fromLTRB(4, 20, 4, 8), child: Text('我的收货地址', style: KeelText.section)),
      if (_needLogin)
        GestureDetector(
          key: const Key('place.login'),
          onTap: () async {
            await context.push('/login');
            if (!mounted) return;
            setState(() {
              _needLogin = false;
              _addressError = '';
              _addresses = null;
            });
            _loadAddresses();
          },
          child: const Padding(padding: EdgeInsets.all(4), child: Text('登录后可以从收货地址里选 ›', style: KeelText.link)),
        )
      else if (_addressError.isNotEmpty)
        Padding(padding: const EdgeInsets.all(4), child: Text(_addressError, style: KeelText.err))
      else if (list == null)
        const Padding(padding: EdgeInsets.all(4), child: Text('正在加载…', style: KeelText.hint))
      else if (list.isEmpty)
        const Padding(padding: EdgeInsets.all(4), child: Text('还没有收货地址', style: KeelText.hint))
      else
        KeelCard(
          padding: EdgeInsets.zero,
          child: Column(children: [
            for (final (i, a) in list.indexed)
              _row(
                key: Key('place.address.${a.id}'),
                icon: Icons.home_outlined,
                title: a.detail,
                sub: '${a.name} ${a.phone} · ${a.regionText}',
                onTap: () => _pickAddress(a),
                last: i == list.length - 1,
              ),
          ]),
        ),
    ];
  }

  Widget _note(String key, String text) =>
      Padding(padding: const EdgeInsets.fromLTRB(4, 4, 4, 12), child: Text(text, key: Key(key), style: KeelText.sub));

  Widget _row({required Key key, required IconData icon, required String title, String sub = '', required VoidCallback onTap, bool last = false}) =>
      InkWell(
        key: key,
        onTap: _busy ? null : onTap,
        child: Container(
          padding: const EdgeInsets.symmetric(horizontal: 16, vertical: 12),
          decoration: BoxDecoration(border: last ? null : const Border(bottom: BorderSide(color: KeelColors.line))),
          child: Row(children: [
            Icon(icon, size: 18, color: KeelColors.textSub),
            const SizedBox(width: 10),
            Expanded(
              child: Column(crossAxisAlignment: CrossAxisAlignment.start, children: [
                Text(title, style: KeelText.body),
                if (sub.isNotEmpty) Text(sub, style: KeelText.hint, maxLines: 1, overflow: TextOverflow.ellipsis),
              ]),
            ),
          ]),
        ),
      );
}
