// 由 scripts/gen_dart_schema.py 从 docs/电商系统-OpenAPI.yaml 生成。**请勿手改。**
//
// 手改会被 scripts/check_dart_contract.py 抓住：改契约 -> `make flutter-generate` -> 一起提交。
// 只有类型与 JSON 编解码，没有网络代码 —— 网络在 lib/api/client.dart。
// ignore_for_file: non_constant_identifier_names, unnecessary_cast, prefer_null_aware_operators

class Address {
  final double? lat;
  final double? lng;
  final String receiverName;
  final String phone;
  final String province;
  final String city;
  final String district;
  final String? street;
  final String detail;
  final String? regionCode;
  final String? postalCode;
  final int? tag;
  final bool isDefault;
  final int id;
  final String? createdAt;
  final String? updatedAt;
  const Address({this.lat, this.lng, required this.receiverName, required this.phone, required this.province, required this.city, required this.district, this.street, required this.detail, this.regionCode, this.postalCode, this.tag, required this.isDefault, required this.id, this.createdAt, this.updatedAt});
  factory Address.fromJson(Map<String, dynamic> j) => Address(
        lat: (j['lat'] as num?)?.toDouble(),
        lng: (j['lng'] as num?)?.toDouble(),
        receiverName: j['receiver_name'] as String,
        phone: j['phone'] as String,
        province: j['province'] as String,
        city: j['city'] as String,
        district: j['district'] as String,
        street: j['street'] as String?,
        detail: j['detail'] as String,
        regionCode: j['region_code'] as String?,
        postalCode: j['postal_code'] as String?,
        tag: (j['tag'] as num?)?.toInt(),
        isDefault: j['is_default'] as bool,
        id: (j['id'] as num).toInt(),
        createdAt: j['created_at'] as String?,
        updatedAt: j['updated_at'] as String?,
      );
  Map<String, dynamic> toJson() => {
        if (lat != null) 'lat': lat,
        if (lng != null) 'lng': lng,
        'receiver_name': receiverName,
        'phone': phone,
        'province': province,
        'city': city,
        'district': district,
        if (street != null) 'street': street,
        'detail': detail,
        if (regionCode != null) 'region_code': regionCode,
        if (postalCode != null) 'postal_code': postalCode,
        if (tag != null) 'tag': tag,
        'is_default': isDefault,
        'id': id,
        if (createdAt != null) 'created_at': createdAt,
        if (updatedAt != null) 'updated_at': updatedAt,
      };
}

class AddressInput {
  final double? lat;
  final double? lng;
  final String receiverName;
  final String phone;
  final String province;
  final String city;
  final String district;
  final String? street;
  final String detail;
  final String? regionCode;
  final String? postalCode;
  final int? tag;
  final bool? isDefault;
  const AddressInput({this.lat, this.lng, required this.receiverName, required this.phone, required this.province, required this.city, required this.district, this.street, required this.detail, this.regionCode, this.postalCode, this.tag, this.isDefault});
  factory AddressInput.fromJson(Map<String, dynamic> j) => AddressInput(
        lat: (j['lat'] as num?)?.toDouble(),
        lng: (j['lng'] as num?)?.toDouble(),
        receiverName: j['receiver_name'] as String,
        phone: j['phone'] as String,
        province: j['province'] as String,
        city: j['city'] as String,
        district: j['district'] as String,
        street: j['street'] as String?,
        detail: j['detail'] as String,
        regionCode: j['region_code'] as String?,
        postalCode: j['postal_code'] as String?,
        tag: (j['tag'] as num?)?.toInt(),
        isDefault: j['is_default'] as bool?,
      );
  Map<String, dynamic> toJson() => {
        if (lat != null) 'lat': lat,
        if (lng != null) 'lng': lng,
        'receiver_name': receiverName,
        'phone': phone,
        'province': province,
        'city': city,
        'district': district,
        if (street != null) 'street': street,
        'detail': detail,
        if (regionCode != null) 'region_code': regionCode,
        if (postalCode != null) 'postal_code': postalCode,
        if (tag != null) 'tag': tag,
        if (isDefault != null) 'is_default': isDefault,
      };
}

class AgentKey {
  final int id;
  final String name;
  final String prefix;
  final String? expiresAt;
  final String? revokedAt;
  final String? lastUsedAt;
  final String createdAt;
  const AgentKey({required this.id, required this.name, required this.prefix, this.expiresAt, this.revokedAt, this.lastUsedAt, required this.createdAt});
  factory AgentKey.fromJson(Map<String, dynamic> j) => AgentKey(
        id: (j['id'] as num).toInt(),
        name: j['name'] as String,
        prefix: j['prefix'] as String,
        expiresAt: j['expires_at'] as String?,
        revokedAt: j['revoked_at'] as String?,
        lastUsedAt: j['last_used_at'] as String?,
        createdAt: j['created_at'] as String,
      );
  Map<String, dynamic> toJson() => {
        'id': id,
        'name': name,
        'prefix': prefix,
        if (expiresAt != null) 'expires_at': expiresAt,
        if (revokedAt != null) 'revoked_at': revokedAt,
        if (lastUsedAt != null) 'last_used_at': lastUsedAt,
        'created_at': createdAt,
      };
}

class AdminAgent {
  final int id;
  final String name;
  final int role;
  final int status;
  final List<int> regionIds;
  final List<int> storeIds;
  final int liveKeys;
  final String? lastUsedAt;
  final String createdAt;
  final List<AgentKey>? keys;
  const AdminAgent({required this.id, required this.name, required this.role, required this.status, required this.regionIds, required this.storeIds, required this.liveKeys, this.lastUsedAt, required this.createdAt, this.keys});
  factory AdminAgent.fromJson(Map<String, dynamic> j) => AdminAgent(
        id: (j['id'] as num).toInt(),
        name: j['name'] as String,
        role: (j['role'] as num).toInt(),
        status: (j['status'] as num).toInt(),
        regionIds: (j['region_ids'] as List).map((e) => (e as num).toInt()).toList(),
        storeIds: (j['store_ids'] as List).map((e) => (e as num).toInt()).toList(),
        liveKeys: (j['live_keys'] as num).toInt(),
        lastUsedAt: j['last_used_at'] as String?,
        createdAt: j['created_at'] as String,
        keys: (j['keys'] as List?)?.map((e) => AgentKey.fromJson(e as Map<String, dynamic>)).toList(),
      );
  Map<String, dynamic> toJson() => {
        'id': id,
        'name': name,
        'role': role,
        'status': status,
        'region_ids': regionIds,
        'store_ids': storeIds,
        'live_keys': liveKeys,
        if (lastUsedAt != null) 'last_used_at': lastUsedAt,
        'created_at': createdAt,
        if (keys != null) 'keys': keys!.map((e) => e.toJson()).toList(),
      };
}

/// 后台视角的分类，**扁平**。与前台的 `Category` 不同，它不嵌 `children`：
class AdminCategory {
  final int id;
  final int? parentId;
  final String name;
  final String path;
  final int level;
  final int sortOrder;
  final int status;
  final String? deletedAt;
  final String createdAt;
  final String? updatedAt;
  const AdminCategory({required this.id, this.parentId, required this.name, required this.path, required this.level, required this.sortOrder, required this.status, this.deletedAt, required this.createdAt, this.updatedAt});
  factory AdminCategory.fromJson(Map<String, dynamic> j) => AdminCategory(
        id: (j['id'] as num).toInt(),
        parentId: (j['parent_id'] as num?)?.toInt(),
        name: j['name'] as String,
        path: j['path'] as String,
        level: (j['level'] as num).toInt(),
        sortOrder: (j['sort_order'] as num).toInt(),
        status: (j['status'] as num).toInt(),
        deletedAt: j['deleted_at'] as String?,
        createdAt: j['created_at'] as String,
        updatedAt: j['updated_at'] as String?,
      );
  Map<String, dynamic> toJson() => {
        'id': id,
        if (parentId != null) 'parent_id': parentId,
        'name': name,
        'path': path,
        'level': level,
        'sort_order': sortOrder,
        'status': status,
        if (deletedAt != null) 'deleted_at': deletedAt,
        'created_at': createdAt,
        if (updatedAt != null) 'updated_at': updatedAt,
      };
}

/// 1 满减 · 2 折扣 · 3 立减 · 4 包邮。
typedef CouponType = int;

/// 金额，单位「分」。禁止使用浮点。
typedef Money = int;

/// 一条适用范围规则（数据模型 §7 `coupon_scopes`）。1–4 决定哪几行商品参与计算，
class CouponScope {
  final int scopeType;
  final int? targetId;
  final bool include;
  final String? targetName;
  const CouponScope({required this.scopeType, this.targetId, required this.include, this.targetName});
  factory CouponScope.fromJson(Map<String, dynamic> j) => CouponScope(
        scopeType: (j['scope_type'] as num).toInt(),
        targetId: (j['target_id'] as num?)?.toInt(),
        include: j['include'] as bool,
        targetName: j['target_name'] as String?,
      );
  Map<String, dynamic> toJson() => {
        'scope_type': scopeType,
        'target_id': targetId,
        'include': include,
        if (targetName != null) 'target_name': targetName,
      };
}

/// 发放与核销统计。`issued = claimed + granted = unused + locked + used + expired`。
class CouponTemplateStats {
  final int issued;
  final int claimed;
  final int granted;
  final int unused;
  final int locked;
  final int used;
  final int expired;
  const CouponTemplateStats({required this.issued, required this.claimed, required this.granted, required this.unused, required this.locked, required this.used, required this.expired});
  factory CouponTemplateStats.fromJson(Map<String, dynamic> j) => CouponTemplateStats(
        issued: (j['issued'] as num).toInt(),
        claimed: (j['claimed'] as num).toInt(),
        granted: (j['granted'] as num).toInt(),
        unused: (j['unused'] as num).toInt(),
        locked: (j['locked'] as num).toInt(),
        used: (j['used'] as num).toInt(),
        expired: (j['expired'] as num).toInt(),
      );
  Map<String, dynamic> toJson() => {
        'issued': issued,
        'claimed': claimed,
        'granted': granted,
        'unused': unused,
        'locked': locked,
        'used': used,
        'expired': expired,
      };
}

class AdminCouponTemplate {
  final int id;
  final String name;
  final CouponType couponType;
  final Money thresholdCents;
  final Money discountCents;
  final int discountRate;
  final Money maxDiscountCents;
  final int validMode;
  final String? validStartAt;
  final String? validEndAt;
  final int validDays;
  final int totalCount;
  final int issuedCount;
  final int perUserLimit;
  final bool claimable;
  final int status;
  final bool locked;
  final List<CouponScope> scopes;
  final CouponTemplateStats stats;
  final String createdAt;
  final String updatedAt;
  const AdminCouponTemplate({required this.id, required this.name, required this.couponType, required this.thresholdCents, required this.discountCents, required this.discountRate, required this.maxDiscountCents, required this.validMode, this.validStartAt, this.validEndAt, required this.validDays, required this.totalCount, required this.issuedCount, required this.perUserLimit, required this.claimable, required this.status, required this.locked, required this.scopes, required this.stats, required this.createdAt, required this.updatedAt});
  factory AdminCouponTemplate.fromJson(Map<String, dynamic> j) => AdminCouponTemplate(
        id: (j['id'] as num).toInt(),
        name: j['name'] as String,
        couponType: (j['coupon_type'] as num).toInt(),
        thresholdCents: (j['threshold_cents'] as num).toInt(),
        discountCents: (j['discount_cents'] as num).toInt(),
        discountRate: (j['discount_rate'] as num).toInt(),
        maxDiscountCents: (j['max_discount_cents'] as num).toInt(),
        validMode: (j['valid_mode'] as num).toInt(),
        validStartAt: j['valid_start_at'] as String?,
        validEndAt: j['valid_end_at'] as String?,
        validDays: (j['valid_days'] as num).toInt(),
        totalCount: (j['total_count'] as num).toInt(),
        issuedCount: (j['issued_count'] as num).toInt(),
        perUserLimit: (j['per_user_limit'] as num).toInt(),
        claimable: j['claimable'] as bool,
        status: (j['status'] as num).toInt(),
        locked: j['locked'] as bool,
        scopes: (j['scopes'] as List).map((e) => CouponScope.fromJson(e as Map<String, dynamic>)).toList(),
        stats: CouponTemplateStats.fromJson(j['stats'] as Map<String, dynamic>),
        createdAt: j['created_at'] as String,
        updatedAt: j['updated_at'] as String,
      );
  Map<String, dynamic> toJson() => {
        'id': id,
        'name': name,
        'coupon_type': couponType,
        'threshold_cents': thresholdCents,
        'discount_cents': discountCents,
        'discount_rate': discountRate,
        'max_discount_cents': maxDiscountCents,
        'valid_mode': validMode,
        if (validStartAt != null) 'valid_start_at': validStartAt,
        if (validEndAt != null) 'valid_end_at': validEndAt,
        'valid_days': validDays,
        'total_count': totalCount,
        'issued_count': issuedCount,
        'per_user_limit': perUserLimit,
        'claimable': claimable,
        'status': status,
        'locked': locked,
        'scopes': scopes.map((e) => e.toJson()).toList(),
        'stats': stats.toJson(),
        'created_at': createdAt,
        'updated_at': updatedAt,
      };
}

/// 1 按件 · 2 按重量（单位克，取 SKU 的 `weight_gram`）
typedef FreightChargeMode = int;

/// 省级行政区划码（GB/T 2260 的 6 位码，后四位为 0），如 `110000` 北京、`440000` 广东、
typedef ProvinceCode = String;

/// 一条计费规则：管哪些省、首件（首重）多少钱、续件（续重）多少钱、满什么条件包邮。
class FreightRule {
  final List<ProvinceCode> regionCodes;
  final int firstUnit;
  final Money firstFeeCents;
  final int additionalUnit;
  final Money additionalFeeCents;
  final Money freeThresholdCents;
  final int freeQuantity;
  const FreightRule({required this.regionCodes, required this.firstUnit, required this.firstFeeCents, required this.additionalUnit, required this.additionalFeeCents, required this.freeThresholdCents, required this.freeQuantity});
  factory FreightRule.fromJson(Map<String, dynamic> j) => FreightRule(
        regionCodes: (j['region_codes'] as List).map((e) => e as String).toList(),
        firstUnit: (j['first_unit'] as num).toInt(),
        firstFeeCents: (j['first_fee_cents'] as num).toInt(),
        additionalUnit: (j['additional_unit'] as num).toInt(),
        additionalFeeCents: (j['additional_fee_cents'] as num).toInt(),
        freeThresholdCents: (j['free_threshold_cents'] as num).toInt(),
        freeQuantity: (j['free_quantity'] as num).toInt(),
      );
  Map<String, dynamic> toJson() => {
        'region_codes': regionCodes,
        'first_unit': firstUnit,
        'first_fee_cents': firstFeeCents,
        'additional_unit': additionalUnit,
        'additional_fee_cents': additionalFeeCents,
        'free_threshold_cents': freeThresholdCents,
        'free_quantity': freeQuantity,
      };
}

class AdminFreightTemplate {
  final int id;
  final String name;
  final int? storeId;
  final FreightChargeMode chargeMode;
  final bool isDefault;
  final List<FreightRule> rules;
  final List<ProvinceCode> undeliverableRegionCodes;
  final int productCount;
  final String createdAt;
  final String updatedAt;
  const AdminFreightTemplate({required this.id, required this.name, this.storeId, required this.chargeMode, required this.isDefault, required this.rules, required this.undeliverableRegionCodes, required this.productCount, required this.createdAt, required this.updatedAt});
  factory AdminFreightTemplate.fromJson(Map<String, dynamic> j) => AdminFreightTemplate(
        id: (j['id'] as num).toInt(),
        name: j['name'] as String,
        storeId: (j['store_id'] as num?)?.toInt(),
        chargeMode: (j['charge_mode'] as num).toInt(),
        isDefault: j['is_default'] as bool,
        rules: (j['rules'] as List).map((e) => FreightRule.fromJson(e as Map<String, dynamic>)).toList(),
        undeliverableRegionCodes: (j['undeliverable_region_codes'] as List).map((e) => e as String).toList(),
        productCount: (j['product_count'] as num).toInt(),
        createdAt: j['created_at'] as String,
        updatedAt: j['updated_at'] as String,
      );
  Map<String, dynamic> toJson() => {
        'id': id,
        'name': name,
        'store_id': storeId,
        'charge_mode': chargeMode,
        'is_default': isDefault,
        'rules': rules.map((e) => e.toJson()).toList(),
        'undeliverable_region_codes': undeliverableRegionCodes,
        'product_count': productCount,
        'created_at': createdAt,
        'updated_at': updatedAt,
      };
}

class AdminInventory {
  final int skuId;
  final String? skuCode;
  final int storeId;
  final int availableQty;
  final int warningQty;
  final String updatedAt;
  const AdminInventory({required this.skuId, this.skuCode, required this.storeId, required this.availableQty, required this.warningQty, required this.updatedAt});
  factory AdminInventory.fromJson(Map<String, dynamic> j) => AdminInventory(
        skuId: (j['sku_id'] as num).toInt(),
        skuCode: j['sku_code'] as String?,
        storeId: (j['store_id'] as num).toInt(),
        availableQty: (j['available_qty'] as num).toInt(),
        warningQty: (j['warning_qty'] as num).toInt(),
        updatedAt: j['updated_at'] as String,
      );
  Map<String, dynamic> toJson() => {
        'sku_id': skuId,
        if (skuCode != null) 'sku_code': skuCode,
        'store_id': storeId,
        'available_qty': availableQty,
        'warning_qty': warningQty,
        'updated_at': updatedAt,
      };
}

class DeliveryTier {
  final int withinM;
  final Money feeCents;
  const DeliveryTier({required this.withinM, required this.feeCents});
  factory DeliveryTier.fromJson(Map<String, dynamic> j) => DeliveryTier(
        withinM: (j['within_m'] as num).toInt(),
        feeCents: (j['fee_cents'] as num).toInt(),
      );
  Map<String, dynamic> toJson() => {
        'within_m': withinM,
        'fee_cents': feeCents,
      };
}

class LocalDeliveryConfig {
  final Money minOrderCents;
  final Money freeOverCents;
  final List<DeliveryTier> feeTiers;
  const LocalDeliveryConfig({required this.minOrderCents, required this.freeOverCents, required this.feeTiers});
  factory LocalDeliveryConfig.fromJson(Map<String, dynamic> j) => LocalDeliveryConfig(
        minOrderCents: (j['min_order_cents'] as num).toInt(),
        freeOverCents: (j['free_over_cents'] as num).toInt(),
        feeTiers: (j['fee_tiers'] as List).map((e) => DeliveryTier.fromJson(e as Map<String, dynamic>)).toList(),
      );
  Map<String, dynamic> toJson() => {
        'min_order_cents': minOrderCents,
        'free_over_cents': freeOverCents,
        'fee_tiers': feeTiers.map((e) => e.toJson()).toList(),
      };
}

/// 顶层的三个数是**生效的**规则（按 `source` 取自模板、门店自定义或默认模板）。
class AdminLocalDelivery {
  final Money minOrderCents;
  final Money freeOverCents;
  final List<DeliveryTier> feeTiers;
  final bool active;
  final String source;
  final int? templateId;
  final String? templateName;
  final LocalDeliveryConfig? custom;
  final String? updatedAt;
  const AdminLocalDelivery({required this.minOrderCents, required this.freeOverCents, required this.feeTiers, required this.active, required this.source, this.templateId, this.templateName, this.custom, this.updatedAt});
  factory AdminLocalDelivery.fromJson(Map<String, dynamic> j) => AdminLocalDelivery(
        minOrderCents: (j['min_order_cents'] as num).toInt(),
        freeOverCents: (j['free_over_cents'] as num).toInt(),
        feeTiers: (j['fee_tiers'] as List).map((e) => DeliveryTier.fromJson(e as Map<String, dynamic>)).toList(),
        active: j['active'] as bool,
        source: j['source'] as String,
        templateId: (j['template_id'] as num?)?.toInt(),
        templateName: j['template_name'] as String?,
        custom: j['custom'] == null ? null : LocalDeliveryConfig.fromJson(j['custom'] as Map<String, dynamic>),
        updatedAt: j['updated_at'] as String?,
      );
  Map<String, dynamic> toJson() => {
        'min_order_cents': minOrderCents,
        'free_over_cents': freeOverCents,
        'fee_tiers': feeTiers.map((e) => e.toJson()).toList(),
        'active': active,
        'source': source,
        if (templateId != null) 'template_id': templateId,
        if (templateName != null) 'template_name': templateName,
        if (custom != null) 'custom': custom!.toJson(),
        if (updatedAt != null) 'updated_at': updatedAt,
      };
}

/// **履约维度** —— 货走到哪儿了。资金维度另见 `Order.refund_status`。
typedef OrderStatus = int;

/// **资金维度** —— 钱退了多少，与 `status` 正交，由退款单驱动。
typedef OrderRefundStatus = int;

/// 1 满减 · 2 满折 · 3 限时折扣（特价） · 4 秒杀 · 5 新人礼。
typedef PromotionType = int;

/// 订单上命中的活动，**下单时的快照**（活动之后改名、改规则、下线都不影响它）。
class OrderPromotion {
  final int promotionId;
  final String name;
  final PromotionType promotionType;
  final Money discountCents;
  final List<int> skuIds;
  const OrderPromotion({required this.promotionId, required this.name, required this.promotionType, required this.discountCents, required this.skuIds});
  factory OrderPromotion.fromJson(Map<String, dynamic> j) => OrderPromotion(
        promotionId: (j['promotion_id'] as num).toInt(),
        name: j['name'] as String,
        promotionType: (j['promotion_type'] as num).toInt(),
        discountCents: (j['discount_cents'] as num).toInt(),
        skuIds: (j['sku_ids'] as List).map((e) => (e as num).toInt()).toList(),
      );
  Map<String, dynamic> toJson() => {
        'promotion_id': promotionId,
        'name': name,
        'promotion_type': promotionType,
        'discount_cents': discountCents,
        'sku_ids': skuIds,
      };
}

/// 下单瞬间从 `user_addresses` 拷贝的收货信息快照，落在
class ReceiverSnapshot {
  final String receiverName;
  final String phone;
  final String province;
  final String city;
  final String district;
  final String? street;
  final String detail;
  final String? regionCode;
  final String? postalCode;
  const ReceiverSnapshot({required this.receiverName, required this.phone, required this.province, required this.city, required this.district, this.street, required this.detail, this.regionCode, this.postalCode});
  factory ReceiverSnapshot.fromJson(Map<String, dynamic> j) => ReceiverSnapshot(
        receiverName: j['receiver_name'] as String,
        phone: j['phone'] as String,
        province: j['province'] as String,
        city: j['city'] as String,
        district: j['district'] as String,
        street: j['street'] as String?,
        detail: j['detail'] as String,
        regionCode: j['region_code'] as String?,
        postalCode: j['postal_code'] as String?,
      );
  Map<String, dynamic> toJson() => {
        'receiver_name': receiverName,
        'phone': phone,
        'province': province,
        'city': city,
        'district': district,
        if (street != null) 'street': street,
        'detail': detail,
        if (regionCode != null) 'region_code': regionCode,
        if (postalCode != null) 'postal_code': postalCode,
      };
}

/// 下单那一瞬间的门店与大区展示信息，取自 `orders.store_snapshot`。
class OrderStoreSnapshot {
  final String storeName;
  final String? storePhone;
  final String? storeAddress;
  final String? regionName;
  const OrderStoreSnapshot({required this.storeName, this.storePhone, this.storeAddress, this.regionName});
  factory OrderStoreSnapshot.fromJson(Map<String, dynamic> j) => OrderStoreSnapshot(
        storeName: j['store_name'] as String,
        storePhone: j['store_phone'] as String?,
        storeAddress: j['store_address'] as String?,
        regionName: j['region_name'] as String?,
      );
  Map<String, dynamic> toJson() => {
        'store_name': storeName,
        if (storePhone != null) 'store_phone': storePhone,
        if (storeAddress != null) 'store_address': storeAddress,
        if (regionName != null) 'region_name': regionName,
      };
}

/// 同城配送怎么算的（`FreightBreakdown.mode` = `local` 时出现）。口径见 `PUT /admin/stores/{store_id}/local-delivery`。
class LocalDeliveryQuote {
  final int? distanceM;
  final int? tierWithinM;
  final Money tierFeeCents;
  final Money freeOverCents;
  final String? freeReason;
  final Money minOrderCents;
  final Money shortfallCents;
  const LocalDeliveryQuote({this.distanceM, this.tierWithinM, required this.tierFeeCents, required this.freeOverCents, this.freeReason, required this.minOrderCents, required this.shortfallCents});
  factory LocalDeliveryQuote.fromJson(Map<String, dynamic> j) => LocalDeliveryQuote(
        distanceM: (j['distance_m'] as num?)?.toInt(),
        tierWithinM: (j['tier_within_m'] as num?)?.toInt(),
        tierFeeCents: (j['tier_fee_cents'] as num).toInt(),
        freeOverCents: (j['free_over_cents'] as num).toInt(),
        freeReason: j['free_reason'] as String?,
        minOrderCents: (j['min_order_cents'] as num).toInt(),
        shortfallCents: (j['shortfall_cents'] as num).toInt(),
      );
  Map<String, dynamic> toJson() => {
        'distance_m': distanceM,
        if (tierWithinM != null) 'tier_within_m': tierWithinM,
        'tier_fee_cents': tierFeeCents,
        'free_over_cents': freeOverCents,
        if (freeReason != null) 'free_reason': freeReason,
        'min_order_cents': minOrderCents,
        'shortfall_cents': shortfallCents,
      };
}

/// 这一组为什么免运费：`threshold` 满额包邮、`quantity` 满件包邮、
typedef FreightFreeReason = String;

/// 按运费模板分的一组商品及其运费。一单里的商品挂不同模板时分成几组，
class FreightGroup {
  final int? templateId;
  final String? templateName;
  final FreightChargeMode? chargeMode;
  final List<int> skuIds;
  final int units;
  final FreightRule? rule;
  final Money feeCents;
  final FreightFreeReason? freeReason;
  const FreightGroup({this.templateId, this.templateName, this.chargeMode, required this.skuIds, required this.units, this.rule, required this.feeCents, this.freeReason});
  factory FreightGroup.fromJson(Map<String, dynamic> j) => FreightGroup(
        templateId: (j['template_id'] as num?)?.toInt(),
        templateName: j['template_name'] as String?,
        chargeMode: (j['charge_mode'] as num?)?.toInt(),
        skuIds: (j['sku_ids'] as List).map((e) => (e as num).toInt()).toList(),
        units: (j['units'] as num).toInt(),
        rule: j['rule'] == null ? null : FreightRule.fromJson(j['rule'] as Map<String, dynamic>),
        feeCents: (j['fee_cents'] as num).toInt(),
        freeReason: j['free_reason'] as String?,
      );
  Map<String, dynamic> toJson() => {
        if (templateId != null) 'template_id': templateId,
        if (templateName != null) 'template_name': templateName,
        if (chargeMode != null) 'charge_mode': chargeMode,
        'sku_ids': skuIds,
        'units': units,
        if (rule != null) 'rule': rule!.toJson(),
        'fee_cents': feeCents,
        if (freeReason != null) 'free_reason': freeReason,
      };
}

/// 运费是怎么算出来的。试算与购物车里是**现算**的，订单上是**下单那一刻的快照**
class FreightBreakdown {
  final String? mode;
  final LocalDeliveryQuote? local;
  final ProvinceCode? provinceCode;
  final Money freightCents;
  final Money freightDiscountCents;
  final List<FreightGroup> groups;
  const FreightBreakdown({this.mode, this.local, this.provinceCode, required this.freightCents, required this.freightDiscountCents, required this.groups});
  factory FreightBreakdown.fromJson(Map<String, dynamic> j) => FreightBreakdown(
        mode: j['mode'] as String?,
        local: j['local'] == null ? null : LocalDeliveryQuote.fromJson(j['local'] as Map<String, dynamic>),
        provinceCode: j['province_code'] as String?,
        freightCents: (j['freight_cents'] as num).toInt(),
        freightDiscountCents: (j['freight_discount_cents'] as num).toInt(),
        groups: (j['groups'] as List).map((e) => FreightGroup.fromJson(e as Map<String, dynamic>)).toList(),
      );
  Map<String, dynamic> toJson() => {
        if (mode != null) 'mode': mode,
        if (local != null) 'local': local!.toJson(),
        if (provinceCode != null) 'province_code': provinceCode,
        'freight_cents': freightCents,
        'freight_discount_cents': freightDiscountCents,
        'groups': groups.map((e) => e.toJson()).toList(),
      };
}

class OrderItem {
  final int id;
  final int skuId;
  final int? productId;
  final String title;
  final Map<String, String>? specValues;
  final String? imageUrl;
  final Money priceCents;
  final Money? listPriceCents;
  final int? pricePromotionId;
  final int quantity;
  final Money? amountCents;
  final Money? discountCents;
  final Money? promotionDiscountCents;
  final int? refundedQty;
  final int? refundingQty;
  const OrderItem({required this.id, required this.skuId, this.productId, required this.title, this.specValues, this.imageUrl, required this.priceCents, this.listPriceCents, this.pricePromotionId, required this.quantity, this.amountCents, this.discountCents, this.promotionDiscountCents, this.refundedQty, this.refundingQty});
  factory OrderItem.fromJson(Map<String, dynamic> j) => OrderItem(
        id: (j['id'] as num).toInt(),
        skuId: (j['sku_id'] as num).toInt(),
        productId: (j['product_id'] as num?)?.toInt(),
        title: j['title'] as String,
        specValues: (j['spec_values'] as Map?)?.map((k, e) => MapEntry(k as String, e as String)),
        imageUrl: j['image_url'] as String?,
        priceCents: (j['price_cents'] as num).toInt(),
        listPriceCents: (j['list_price_cents'] as num?)?.toInt(),
        pricePromotionId: (j['price_promotion_id'] as num?)?.toInt(),
        quantity: (j['quantity'] as num).toInt(),
        amountCents: (j['amount_cents'] as num?)?.toInt(),
        discountCents: (j['discount_cents'] as num?)?.toInt(),
        promotionDiscountCents: (j['promotion_discount_cents'] as num?)?.toInt(),
        refundedQty: (j['refunded_qty'] as num?)?.toInt(),
        refundingQty: (j['refunding_qty'] as num?)?.toInt(),
      );
  Map<String, dynamic> toJson() => {
        'id': id,
        'sku_id': skuId,
        if (productId != null) 'product_id': productId,
        'title': title,
        if (specValues != null) 'spec_values': specValues,
        if (imageUrl != null) 'image_url': imageUrl,
        'price_cents': priceCents,
        if (listPriceCents != null) 'list_price_cents': listPriceCents,
        if (pricePromotionId != null) 'price_promotion_id': pricePromotionId,
        'quantity': quantity,
        if (amountCents != null) 'amount_cents': amountCents,
        if (discountCents != null) 'discount_cents': discountCents,
        if (promotionDiscountCents != null) 'promotion_discount_cents': promotionDiscountCents,
        if (refundedQty != null) 'refunded_qty': refundedQty,
        if (refundingQty != null) 'refunding_qty': refundingQty,
      };
}

class PaymentRecord {
  final String? paymentNo;
  final String? channel;
  final Money? amountCents;
  final int? status;
  final String? paidAt;
  const PaymentRecord({this.paymentNo, this.channel, this.amountCents, this.status, this.paidAt});
  factory PaymentRecord.fromJson(Map<String, dynamic> j) => PaymentRecord(
        paymentNo: j['payment_no'] as String?,
        channel: j['channel'] as String?,
        amountCents: (j['amount_cents'] as num?)?.toInt(),
        status: (j['status'] as num?)?.toInt(),
        paidAt: j['paid_at'] as String?,
      );
  Map<String, dynamic> toJson() => {
        if (paymentNo != null) 'payment_no': paymentNo,
        if (channel != null) 'channel': channel,
        if (amountCents != null) 'amount_cents': amountCents,
        if (status != null) 'status': status,
        if (paidAt != null) 'paid_at': paidAt,
      };
}

class Shipment {
  final int id;
  final String carrierCode;
  final String trackingNo;
  final int status;
  final String shippedAt;
  final String? deliveredAt;
  const Shipment({required this.id, required this.carrierCode, required this.trackingNo, required this.status, required this.shippedAt, this.deliveredAt});
  factory Shipment.fromJson(Map<String, dynamic> j) => Shipment(
        id: (j['id'] as num).toInt(),
        carrierCode: j['carrier_code'] as String,
        trackingNo: j['tracking_no'] as String,
        status: (j['status'] as num).toInt(),
        shippedAt: j['shipped_at'] as String,
        deliveredAt: j['delivered_at'] as String?,
      );
  Map<String, dynamic> toJson() => {
        'id': id,
        'carrier_code': carrierCode,
        'tracking_no': trackingNo,
        'status': status,
        'shipped_at': shippedAt,
        if (deliveredAt != null) 'delivered_at': deliveredAt,
      };
}

/// 1 仅退款 / 2 退货退款。对应 `refunds.refund_type`。
typedef RefundType = int;

/// 退款单状态机，与 `refunds.status` 的 SMALLINT 取值逐值一致。
typedef RefundStatus = int;

class RefundItem {
  final int orderItemId;
  final String? title;
  final String? imageUrl;
  final int quantity;
  final Money amountCents;
  const RefundItem({required this.orderItemId, this.title, this.imageUrl, required this.quantity, required this.amountCents});
  factory RefundItem.fromJson(Map<String, dynamic> j) => RefundItem(
        orderItemId: (j['order_item_id'] as num).toInt(),
        title: j['title'] as String?,
        imageUrl: j['image_url'] as String?,
        quantity: (j['quantity'] as num).toInt(),
        amountCents: (j['amount_cents'] as num).toInt(),
      );
  Map<String, dynamic> toJson() => {
        'order_item_id': orderItemId,
        if (title != null) 'title': title,
        if (imageUrl != null) 'image_url': imageUrl,
        'quantity': quantity,
        'amount_cents': amountCents,
      };
}

/// 对应 `refunds.reason_code`：
typedef RefundReasonCode = int;

class ReturnShipment {
  final String carrierCode;
  final String trackingNo;
  final String submittedAt;
  const ReturnShipment({required this.carrierCode, required this.trackingNo, required this.submittedAt});
  factory ReturnShipment.fromJson(Map<String, dynamic> j) => ReturnShipment(
        carrierCode: j['carrier_code'] as String,
        trackingNo: j['tracking_no'] as String,
        submittedAt: j['submitted_at'] as String,
      );
  Map<String, dynamic> toJson() => {
        'carrier_code': carrierCode,
        'tracking_no': trackingNo,
        'submitted_at': submittedAt,
      };
}

/// 审计字段里的「谁」。`name` 取员工此刻的名字；平台级员工（不属于这家店）
class StaffRef {
  final int id;
  final String? name;
  const StaffRef({required this.id, this.name});
  factory StaffRef.fromJson(Map<String, dynamic> j) => StaffRef(
        id: (j['id'] as num).toInt(),
        name: j['name'] as String?,
      );
  Map<String, dynamic> toJson() => {
        'id': id,
        if (name != null) 'name': name,
      };
}

/// 后台视角的退款单：买家侧 `Refund` 的全部字段，加上履约门店与审核记录。
class AdminRefund {
  final String refundNo;
  final String orderNo;
  final String? paymentNo;
  final RefundType refundType;
  final RefundStatus status;
  final List<RefundItem> items;
  final Money? goodsAmountCents;
  final Money? freightCents;
  final Money amountCents;
  final String? channel;
  final String? channelRefundId;
  final RefundReasonCode? reasonCode;
  final String? reasonText;
  final List<String>? evidenceUrls;
  final ReturnShipment? returnShipment;
  final String? rejectReason;
  final String? auditedAt;
  final String? returnDeadlineAt;
  final String? refundedAt;
  final String createdAt;
  final String? updatedAt;
  final int storeId;
  final OrderStoreSnapshot store;
  final OrderStatus orderStatus;
  final StaffRef? auditedBy;
  final String? receivedAt;
  final StaffRef? receivedBy;
  const AdminRefund({required this.refundNo, required this.orderNo, this.paymentNo, required this.refundType, required this.status, required this.items, this.goodsAmountCents, this.freightCents, required this.amountCents, this.channel, this.channelRefundId, this.reasonCode, this.reasonText, this.evidenceUrls, this.returnShipment, this.rejectReason, this.auditedAt, this.returnDeadlineAt, this.refundedAt, required this.createdAt, this.updatedAt, required this.storeId, required this.store, required this.orderStatus, this.auditedBy, this.receivedAt, this.receivedBy});
  factory AdminRefund.fromJson(Map<String, dynamic> j) => AdminRefund(
        refundNo: j['refund_no'] as String,
        orderNo: j['order_no'] as String,
        paymentNo: j['payment_no'] as String?,
        refundType: (j['refund_type'] as num).toInt(),
        status: (j['status'] as num).toInt(),
        items: (j['items'] as List).map((e) => RefundItem.fromJson(e as Map<String, dynamic>)).toList(),
        goodsAmountCents: (j['goods_amount_cents'] as num?)?.toInt(),
        freightCents: (j['freight_cents'] as num?)?.toInt(),
        amountCents: (j['amount_cents'] as num).toInt(),
        channel: j['channel'] as String?,
        channelRefundId: j['channel_refund_id'] as String?,
        reasonCode: (j['reason_code'] as num?)?.toInt(),
        reasonText: j['reason_text'] as String?,
        evidenceUrls: (j['evidence_urls'] as List?)?.map((e) => e as String).toList(),
        returnShipment: j['return_shipment'] == null ? null : ReturnShipment.fromJson(j['return_shipment'] as Map<String, dynamic>),
        rejectReason: j['reject_reason'] as String?,
        auditedAt: j['audited_at'] as String?,
        returnDeadlineAt: j['return_deadline_at'] as String?,
        refundedAt: j['refunded_at'] as String?,
        createdAt: j['created_at'] as String,
        updatedAt: j['updated_at'] as String?,
        storeId: (j['store_id'] as num).toInt(),
        store: OrderStoreSnapshot.fromJson(j['store'] as Map<String, dynamic>),
        orderStatus: (j['order_status'] as num).toInt(),
        auditedBy: j['audited_by'] == null ? null : StaffRef.fromJson(j['audited_by'] as Map<String, dynamic>),
        receivedAt: j['received_at'] as String?,
        receivedBy: j['received_by'] == null ? null : StaffRef.fromJson(j['received_by'] as Map<String, dynamic>),
      );
  Map<String, dynamic> toJson() => {
        'refund_no': refundNo,
        'order_no': orderNo,
        if (paymentNo != null) 'payment_no': paymentNo,
        'refund_type': refundType,
        'status': status,
        'items': items.map((e) => e.toJson()).toList(),
        if (goodsAmountCents != null) 'goods_amount_cents': goodsAmountCents,
        if (freightCents != null) 'freight_cents': freightCents,
        'amount_cents': amountCents,
        if (channel != null) 'channel': channel,
        if (channelRefundId != null) 'channel_refund_id': channelRefundId,
        if (reasonCode != null) 'reason_code': reasonCode,
        if (reasonText != null) 'reason_text': reasonText,
        if (evidenceUrls != null) 'evidence_urls': evidenceUrls,
        if (returnShipment != null) 'return_shipment': returnShipment!.toJson(),
        if (rejectReason != null) 'reject_reason': rejectReason,
        if (auditedAt != null) 'audited_at': auditedAt,
        if (returnDeadlineAt != null) 'return_deadline_at': returnDeadlineAt,
        if (refundedAt != null) 'refunded_at': refundedAt,
        'created_at': createdAt,
        if (updatedAt != null) 'updated_at': updatedAt,
        'store_id': storeId,
        'store': store.toJson(),
        'order_status': orderStatus,
        if (auditedBy != null) 'audited_by': auditedBy!.toJson(),
        if (receivedAt != null) 'received_at': receivedAt,
        if (receivedBy != null) 'received_by': receivedBy!.toJson(),
      };
}

class AdminOrderDetail {
  final String orderNo;
  final int storeId;
  final int? regionId;
  final OrderStatus status;
  final OrderRefundStatus refundStatus;
  final Money? goodsAmountCents;
  final Money? freightCents;
  final Money? freightDiscountCents;
  final Money? discountCents;
  final int? userCouponId;
  final String? couponName;
  final Money? promotionDiscountCents;
  final List<OrderPromotion>? promotions;
  final Money payableCents;
  final Money? paidCents;
  final Money? refundedCents;
  final String? expireAt;
  final String createdAt;
  final String? paidAt;
  final String? shippedAt;
  final String? finishedAt;
  final ReceiverSnapshot receiver;
  final OrderStoreSnapshot store;
  final bool hasOpenRefund;
  final FreightBreakdown? freight;
  final List<OrderItem> items;
  final List<PaymentRecord> payments;
  final List<Shipment> shipments;
  final List<AdminRefund> refunds;
  const AdminOrderDetail({required this.orderNo, required this.storeId, this.regionId, required this.status, required this.refundStatus, this.goodsAmountCents, this.freightCents, this.freightDiscountCents, this.discountCents, this.userCouponId, this.couponName, this.promotionDiscountCents, this.promotions, required this.payableCents, this.paidCents, this.refundedCents, this.expireAt, required this.createdAt, this.paidAt, this.shippedAt, this.finishedAt, required this.receiver, required this.store, required this.hasOpenRefund, this.freight, required this.items, required this.payments, required this.shipments, required this.refunds});
  factory AdminOrderDetail.fromJson(Map<String, dynamic> j) => AdminOrderDetail(
        orderNo: j['order_no'] as String,
        storeId: (j['store_id'] as num).toInt(),
        regionId: (j['region_id'] as num?)?.toInt(),
        status: (j['status'] as num).toInt(),
        refundStatus: (j['refund_status'] as num).toInt(),
        goodsAmountCents: (j['goods_amount_cents'] as num?)?.toInt(),
        freightCents: (j['freight_cents'] as num?)?.toInt(),
        freightDiscountCents: (j['freight_discount_cents'] as num?)?.toInt(),
        discountCents: (j['discount_cents'] as num?)?.toInt(),
        userCouponId: (j['user_coupon_id'] as num?)?.toInt(),
        couponName: j['coupon_name'] as String?,
        promotionDiscountCents: (j['promotion_discount_cents'] as num?)?.toInt(),
        promotions: (j['promotions'] as List?)?.map((e) => OrderPromotion.fromJson(e as Map<String, dynamic>)).toList(),
        payableCents: (j['payable_cents'] as num).toInt(),
        paidCents: (j['paid_cents'] as num?)?.toInt(),
        refundedCents: (j['refunded_cents'] as num?)?.toInt(),
        expireAt: j['expire_at'] as String?,
        createdAt: j['created_at'] as String,
        paidAt: j['paid_at'] as String?,
        shippedAt: j['shipped_at'] as String?,
        finishedAt: j['finished_at'] as String?,
        receiver: ReceiverSnapshot.fromJson(j['receiver'] as Map<String, dynamic>),
        store: OrderStoreSnapshot.fromJson(j['store'] as Map<String, dynamic>),
        hasOpenRefund: j['has_open_refund'] as bool,
        freight: j['freight'] == null ? null : FreightBreakdown.fromJson(j['freight'] as Map<String, dynamic>),
        items: (j['items'] as List).map((e) => OrderItem.fromJson(e as Map<String, dynamic>)).toList(),
        payments: (j['payments'] as List).map((e) => PaymentRecord.fromJson(e as Map<String, dynamic>)).toList(),
        shipments: (j['shipments'] as List).map((e) => Shipment.fromJson(e as Map<String, dynamic>)).toList(),
        refunds: (j['refunds'] as List).map((e) => AdminRefund.fromJson(e as Map<String, dynamic>)).toList(),
      );
  Map<String, dynamic> toJson() => {
        'order_no': orderNo,
        'store_id': storeId,
        if (regionId != null) 'region_id': regionId,
        'status': status,
        'refund_status': refundStatus,
        if (goodsAmountCents != null) 'goods_amount_cents': goodsAmountCents,
        if (freightCents != null) 'freight_cents': freightCents,
        if (freightDiscountCents != null) 'freight_discount_cents': freightDiscountCents,
        if (discountCents != null) 'discount_cents': discountCents,
        if (userCouponId != null) 'user_coupon_id': userCouponId,
        if (couponName != null) 'coupon_name': couponName,
        if (promotionDiscountCents != null) 'promotion_discount_cents': promotionDiscountCents,
        if (promotions != null) 'promotions': promotions!.map((e) => e.toJson()).toList(),
        'payable_cents': payableCents,
        if (paidCents != null) 'paid_cents': paidCents,
        if (refundedCents != null) 'refunded_cents': refundedCents,
        if (expireAt != null) 'expire_at': expireAt,
        'created_at': createdAt,
        if (paidAt != null) 'paid_at': paidAt,
        if (shippedAt != null) 'shipped_at': shippedAt,
        if (finishedAt != null) 'finished_at': finishedAt,
        'receiver': receiver.toJson(),
        'store': store.toJson(),
        'has_open_refund': hasOpenRefund,
        if (freight != null) 'freight': freight!.toJson(),
        'items': items.map((e) => e.toJson()).toList(),
        'payments': payments.map((e) => e.toJson()).toList(),
        'shipments': shipments.map((e) => e.toJson()).toList(),
        'refunds': refunds.map((e) => e.toJson()).toList(),
      };
}

/// 后台视角的订单摘要：买家侧 `Order` 的全部字段，加上收货人与门店的**下单时快照**，
class AdminOrderSummary {
  final String orderNo;
  final int storeId;
  final int? regionId;
  final OrderStatus status;
  final OrderRefundStatus refundStatus;
  final Money? goodsAmountCents;
  final Money? freightCents;
  final Money? freightDiscountCents;
  final Money? discountCents;
  final int? userCouponId;
  final String? couponName;
  final Money? promotionDiscountCents;
  final List<OrderPromotion>? promotions;
  final Money payableCents;
  final Money? paidCents;
  final Money? refundedCents;
  final String? expireAt;
  final String createdAt;
  final String? paidAt;
  final String? shippedAt;
  final String? finishedAt;
  final ReceiverSnapshot receiver;
  final OrderStoreSnapshot store;
  final bool hasOpenRefund;
  const AdminOrderSummary({required this.orderNo, required this.storeId, this.regionId, required this.status, required this.refundStatus, this.goodsAmountCents, this.freightCents, this.freightDiscountCents, this.discountCents, this.userCouponId, this.couponName, this.promotionDiscountCents, this.promotions, required this.payableCents, this.paidCents, this.refundedCents, this.expireAt, required this.createdAt, this.paidAt, this.shippedAt, this.finishedAt, required this.receiver, required this.store, required this.hasOpenRefund});
  factory AdminOrderSummary.fromJson(Map<String, dynamic> j) => AdminOrderSummary(
        orderNo: j['order_no'] as String,
        storeId: (j['store_id'] as num).toInt(),
        regionId: (j['region_id'] as num?)?.toInt(),
        status: (j['status'] as num).toInt(),
        refundStatus: (j['refund_status'] as num).toInt(),
        goodsAmountCents: (j['goods_amount_cents'] as num?)?.toInt(),
        freightCents: (j['freight_cents'] as num?)?.toInt(),
        freightDiscountCents: (j['freight_discount_cents'] as num?)?.toInt(),
        discountCents: (j['discount_cents'] as num?)?.toInt(),
        userCouponId: (j['user_coupon_id'] as num?)?.toInt(),
        couponName: j['coupon_name'] as String?,
        promotionDiscountCents: (j['promotion_discount_cents'] as num?)?.toInt(),
        promotions: (j['promotions'] as List?)?.map((e) => OrderPromotion.fromJson(e as Map<String, dynamic>)).toList(),
        payableCents: (j['payable_cents'] as num).toInt(),
        paidCents: (j['paid_cents'] as num?)?.toInt(),
        refundedCents: (j['refunded_cents'] as num?)?.toInt(),
        expireAt: j['expire_at'] as String?,
        createdAt: j['created_at'] as String,
        paidAt: j['paid_at'] as String?,
        shippedAt: j['shipped_at'] as String?,
        finishedAt: j['finished_at'] as String?,
        receiver: ReceiverSnapshot.fromJson(j['receiver'] as Map<String, dynamic>),
        store: OrderStoreSnapshot.fromJson(j['store'] as Map<String, dynamic>),
        hasOpenRefund: j['has_open_refund'] as bool,
      );
  Map<String, dynamic> toJson() => {
        'order_no': orderNo,
        'store_id': storeId,
        if (regionId != null) 'region_id': regionId,
        'status': status,
        'refund_status': refundStatus,
        if (goodsAmountCents != null) 'goods_amount_cents': goodsAmountCents,
        if (freightCents != null) 'freight_cents': freightCents,
        if (freightDiscountCents != null) 'freight_discount_cents': freightDiscountCents,
        if (discountCents != null) 'discount_cents': discountCents,
        if (userCouponId != null) 'user_coupon_id': userCouponId,
        if (couponName != null) 'coupon_name': couponName,
        if (promotionDiscountCents != null) 'promotion_discount_cents': promotionDiscountCents,
        if (promotions != null) 'promotions': promotions!.map((e) => e.toJson()).toList(),
        'payable_cents': payableCents,
        if (paidCents != null) 'paid_cents': paidCents,
        if (refundedCents != null) 'refunded_cents': refundedCents,
        if (expireAt != null) 'expire_at': expireAt,
        'created_at': createdAt,
        if (paidAt != null) 'paid_at': paidAt,
        if (shippedAt != null) 'shipped_at': shippedAt,
        if (finishedAt != null) 'finished_at': finishedAt,
        'receiver': receiver.toJson(),
        'store': store.toJson(),
        'has_open_refund': hasOpenRefund,
      };
}

/// 后台视角的商品。与 `ProductSummary` 的差别是状态面：
class AdminProduct {
  final int id;
  final String title;
  final String? subtitle;
  final String? description;
  final int categoryId;
  final int? brandId;
  final int? freightTemplateId;
  final int status;
  final String? publishedAt;
  final String? deletedAt;
  final Money minPriceCents;
  final Money maxPriceCents;
  final int totalStock;
  final int salesCount;
  final String createdAt;
  final String? updatedAt;
  const AdminProduct({required this.id, required this.title, this.subtitle, this.description, required this.categoryId, this.brandId, this.freightTemplateId, required this.status, this.publishedAt, this.deletedAt, required this.minPriceCents, required this.maxPriceCents, required this.totalStock, required this.salesCount, required this.createdAt, this.updatedAt});
  factory AdminProduct.fromJson(Map<String, dynamic> j) => AdminProduct(
        id: (j['id'] as num).toInt(),
        title: j['title'] as String,
        subtitle: j['subtitle'] as String?,
        description: j['description'] as String?,
        categoryId: (j['category_id'] as num).toInt(),
        brandId: (j['brand_id'] as num?)?.toInt(),
        freightTemplateId: (j['freight_template_id'] as num?)?.toInt(),
        status: (j['status'] as num).toInt(),
        publishedAt: j['published_at'] as String?,
        deletedAt: j['deleted_at'] as String?,
        minPriceCents: (j['min_price_cents'] as num).toInt(),
        maxPriceCents: (j['max_price_cents'] as num).toInt(),
        totalStock: (j['total_stock'] as num).toInt(),
        salesCount: (j['sales_count'] as num).toInt(),
        createdAt: j['created_at'] as String,
        updatedAt: j['updated_at'] as String?,
      );
  Map<String, dynamic> toJson() => {
        'id': id,
        'title': title,
        if (subtitle != null) 'subtitle': subtitle,
        if (description != null) 'description': description,
        'category_id': categoryId,
        if (brandId != null) 'brand_id': brandId,
        if (freightTemplateId != null) 'freight_template_id': freightTemplateId,
        'status': status,
        if (publishedAt != null) 'published_at': publishedAt,
        if (deletedAt != null) 'deleted_at': deletedAt,
        'min_price_cents': minPriceCents,
        'max_price_cents': maxPriceCents,
        'total_stock': totalStock,
        'sales_count': salesCount,
        'created_at': createdAt,
        if (updatedAt != null) 'updated_at': updatedAt,
      };
}

/// 后台视角的 SKU。比前台的 `Sku` 多出成本、重量、售卖开关与库存预警位——
class AdminSku {
  final int id;
  final int productId;
  final String skuCode;
  final Map<String, String>? specValues;
  final Money priceCents;
  final Money? costCents;
  final int? weightGram;
  final String? imageUrl;
  final int status;
  final int availableQty;
  final int? warningQty;
  final String? createdAt;
  final String? updatedAt;
  const AdminSku({required this.id, required this.productId, required this.skuCode, this.specValues, required this.priceCents, this.costCents, this.weightGram, this.imageUrl, required this.status, required this.availableQty, this.warningQty, this.createdAt, this.updatedAt});
  factory AdminSku.fromJson(Map<String, dynamic> j) => AdminSku(
        id: (j['id'] as num).toInt(),
        productId: (j['product_id'] as num).toInt(),
        skuCode: j['sku_code'] as String,
        specValues: (j['spec_values'] as Map?)?.map((k, e) => MapEntry(k as String, e as String)),
        priceCents: (j['price_cents'] as num).toInt(),
        costCents: (j['cost_cents'] as num?)?.toInt(),
        weightGram: (j['weight_gram'] as num?)?.toInt(),
        imageUrl: j['image_url'] as String?,
        status: (j['status'] as num).toInt(),
        availableQty: (j['available_qty'] as num).toInt(),
        warningQty: (j['warning_qty'] as num?)?.toInt(),
        createdAt: j['created_at'] as String?,
        updatedAt: j['updated_at'] as String?,
      );
  Map<String, dynamic> toJson() => {
        'id': id,
        'product_id': productId,
        'sku_code': skuCode,
        if (specValues != null) 'spec_values': specValues,
        'price_cents': priceCents,
        if (costCents != null) 'cost_cents': costCents,
        if (weightGram != null) 'weight_gram': weightGram,
        if (imageUrl != null) 'image_url': imageUrl,
        'status': status,
        'available_qty': availableQty,
        if (warningQty != null) 'warning_qty': warningQty,
        if (createdAt != null) 'created_at': createdAt,
        if (updatedAt != null) 'updated_at': updatedAt,
      };
}

/// 商品图与商品的关联。落地在数据模型 §3 的 `product_images` 表上——
class ProductImage {
  final int uploadId;
  final String url;
  final int sortOrder;
  const ProductImage({required this.uploadId, required this.url, required this.sortOrder});
  factory ProductImage.fromJson(Map<String, dynamic> j) => ProductImage(
        uploadId: (j['upload_id'] as num).toInt(),
        url: j['url'] as String,
        sortOrder: (j['sort_order'] as num).toInt(),
      );
  Map<String, dynamic> toJson() => {
        'upload_id': uploadId,
        'url': url,
        'sort_order': sortOrder,
      };
}

class AdminProductDetail {
  final int id;
  final String title;
  final String? subtitle;
  final String? description;
  final int categoryId;
  final int? brandId;
  final int? freightTemplateId;
  final int status;
  final String? publishedAt;
  final String? deletedAt;
  final Money minPriceCents;
  final Money maxPriceCents;
  final int totalStock;
  final int salesCount;
  final String createdAt;
  final String? updatedAt;
  final List<AdminSku> skus;
  final List<ProductImage> images;
  const AdminProductDetail({required this.id, required this.title, this.subtitle, this.description, required this.categoryId, this.brandId, this.freightTemplateId, required this.status, this.publishedAt, this.deletedAt, required this.minPriceCents, required this.maxPriceCents, required this.totalStock, required this.salesCount, required this.createdAt, this.updatedAt, required this.skus, required this.images});
  factory AdminProductDetail.fromJson(Map<String, dynamic> j) => AdminProductDetail(
        id: (j['id'] as num).toInt(),
        title: j['title'] as String,
        subtitle: j['subtitle'] as String?,
        description: j['description'] as String?,
        categoryId: (j['category_id'] as num).toInt(),
        brandId: (j['brand_id'] as num?)?.toInt(),
        freightTemplateId: (j['freight_template_id'] as num?)?.toInt(),
        status: (j['status'] as num).toInt(),
        publishedAt: j['published_at'] as String?,
        deletedAt: j['deleted_at'] as String?,
        minPriceCents: (j['min_price_cents'] as num).toInt(),
        maxPriceCents: (j['max_price_cents'] as num).toInt(),
        totalStock: (j['total_stock'] as num).toInt(),
        salesCount: (j['sales_count'] as num).toInt(),
        createdAt: j['created_at'] as String,
        updatedAt: j['updated_at'] as String?,
        skus: (j['skus'] as List).map((e) => AdminSku.fromJson(e as Map<String, dynamic>)).toList(),
        images: (j['images'] as List).map((e) => ProductImage.fromJson(e as Map<String, dynamic>)).toList(),
      );
  Map<String, dynamic> toJson() => {
        'id': id,
        'title': title,
        if (subtitle != null) 'subtitle': subtitle,
        if (description != null) 'description': description,
        'category_id': categoryId,
        if (brandId != null) 'brand_id': brandId,
        if (freightTemplateId != null) 'freight_template_id': freightTemplateId,
        'status': status,
        if (publishedAt != null) 'published_at': publishedAt,
        if (deletedAt != null) 'deleted_at': deletedAt,
        'min_price_cents': minPriceCents,
        'max_price_cents': maxPriceCents,
        'total_stock': totalStock,
        'sales_count': salesCount,
        'created_at': createdAt,
        if (updatedAt != null) 'updated_at': updatedAt,
        'skus': skus.map((e) => e.toJson()).toList(),
        'images': images.map((e) => e.toJson()).toList(),
      };
}

/// 满减 / 满折的一档。`threshold` 的单位由活动的 `threshold_unit` 决定（1 分 / 2 件）。
class PromotionTier {
  final int threshold;
  final Money discountCents;
  final int discountRate;
  const PromotionTier({required this.threshold, required this.discountCents, required this.discountRate});
  factory PromotionTier.fromJson(Map<String, dynamic> j) => PromotionTier(
        threshold: (j['threshold'] as num).toInt(),
        discountCents: (j['discount_cents'] as num).toInt(),
        discountRate: (j['discount_rate'] as num).toInt(),
      );
  Map<String, dynamic> toJson() => {
        'threshold': threshold,
        'discount_cents': discountCents,
        'discount_rate': discountRate,
      };
}

/// 限时折扣 / 秒杀的一个 SKU。`promo_price_cents`（特价，分）与 `discount_rate`（千分比）二选一，
class PromotionSku {
  final int skuId;
  final Money promoPriceCents;
  final int discountRate;
  final int perUserLimit;
  final int stockQty;
  final int soldQty;
  final String? title;
  final String? skuCode;
  const PromotionSku({required this.skuId, required this.promoPriceCents, required this.discountRate, required this.perUserLimit, required this.stockQty, required this.soldQty, this.title, this.skuCode});
  factory PromotionSku.fromJson(Map<String, dynamic> j) => PromotionSku(
        skuId: (j['sku_id'] as num).toInt(),
        promoPriceCents: (j['promo_price_cents'] as num).toInt(),
        discountRate: (j['discount_rate'] as num).toInt(),
        perUserLimit: (j['per_user_limit'] as num).toInt(),
        stockQty: (j['stock_qty'] as num).toInt(),
        soldQty: (j['sold_qty'] as num).toInt(),
        title: j['title'] as String?,
        skuCode: j['sku_code'] as String?,
      );
  Map<String, dynamic> toJson() => {
        'sku_id': skuId,
        'promo_price_cents': promoPriceCents,
        'discount_rate': discountRate,
        'per_user_limit': perUserLimit,
        'stock_qty': stockQty,
        'sold_qty': soldQty,
        if (title != null) 'title': title,
        if (skuCode != null) 'sku_code': skuCode,
      };
}

class AdminPromotion {
  final int id;
  final String name;
  final PromotionType promotionType;
  final int thresholdUnit;
  final bool stackWithCoupon;
  final String startsAt;
  final String endsAt;
  final int status;
  final String phase;
  final List<PromotionTier> tiers;
  final List<CouponScope> scopes;
  final List<PromotionSku> skus;
  final int? giftCouponTemplateId;
  final int? giftGrantedCount;
  final String createdAt;
  final String updatedAt;
  const AdminPromotion({required this.id, required this.name, required this.promotionType, required this.thresholdUnit, required this.stackWithCoupon, required this.startsAt, required this.endsAt, required this.status, required this.phase, required this.tiers, required this.scopes, required this.skus, this.giftCouponTemplateId, this.giftGrantedCount, required this.createdAt, required this.updatedAt});
  factory AdminPromotion.fromJson(Map<String, dynamic> j) => AdminPromotion(
        id: (j['id'] as num).toInt(),
        name: j['name'] as String,
        promotionType: (j['promotion_type'] as num).toInt(),
        thresholdUnit: (j['threshold_unit'] as num).toInt(),
        stackWithCoupon: j['stack_with_coupon'] as bool,
        startsAt: j['starts_at'] as String,
        endsAt: j['ends_at'] as String,
        status: (j['status'] as num).toInt(),
        phase: j['phase'] as String,
        tiers: (j['tiers'] as List).map((e) => PromotionTier.fromJson(e as Map<String, dynamic>)).toList(),
        scopes: (j['scopes'] as List).map((e) => CouponScope.fromJson(e as Map<String, dynamic>)).toList(),
        skus: (j['skus'] as List).map((e) => PromotionSku.fromJson(e as Map<String, dynamic>)).toList(),
        giftCouponTemplateId: (j['gift_coupon_template_id'] as num?)?.toInt(),
        giftGrantedCount: (j['gift_granted_count'] as num?)?.toInt(),
        createdAt: j['created_at'] as String,
        updatedAt: j['updated_at'] as String,
      );
  Map<String, dynamic> toJson() => {
        'id': id,
        'name': name,
        'promotion_type': promotionType,
        'threshold_unit': thresholdUnit,
        'stack_with_coupon': stackWithCoupon,
        'starts_at': startsAt,
        'ends_at': endsAt,
        'status': status,
        'phase': phase,
        'tiers': tiers.map((e) => e.toJson()).toList(),
        'scopes': scopes.map((e) => e.toJson()).toList(),
        'skus': skus.map((e) => e.toJson()).toList(),
        if (giftCouponTemplateId != null) 'gift_coupon_template_id': giftCouponTemplateId,
        if (giftGrantedCount != null) 'gift_granted_count': giftGrantedCount,
        'created_at': createdAt,
        'updated_at': updatedAt,
      };
}

class AdminRefundDetail {
  final String refundNo;
  final String orderNo;
  final String? paymentNo;
  final RefundType refundType;
  final RefundStatus status;
  final List<RefundItem> items;
  final Money? goodsAmountCents;
  final Money? freightCents;
  final Money amountCents;
  final String? channel;
  final String? channelRefundId;
  final RefundReasonCode? reasonCode;
  final String? reasonText;
  final List<String>? evidenceUrls;
  final ReturnShipment? returnShipment;
  final String? rejectReason;
  final String? auditedAt;
  final String? returnDeadlineAt;
  final String? refundedAt;
  final String createdAt;
  final String? updatedAt;
  final int storeId;
  final OrderStoreSnapshot store;
  final OrderStatus orderStatus;
  final StaffRef? auditedBy;
  final String? receivedAt;
  final StaffRef? receivedBy;
  final AdminOrderSummary order;
  const AdminRefundDetail({required this.refundNo, required this.orderNo, this.paymentNo, required this.refundType, required this.status, required this.items, this.goodsAmountCents, this.freightCents, required this.amountCents, this.channel, this.channelRefundId, this.reasonCode, this.reasonText, this.evidenceUrls, this.returnShipment, this.rejectReason, this.auditedAt, this.returnDeadlineAt, this.refundedAt, required this.createdAt, this.updatedAt, required this.storeId, required this.store, required this.orderStatus, this.auditedBy, this.receivedAt, this.receivedBy, required this.order});
  factory AdminRefundDetail.fromJson(Map<String, dynamic> j) => AdminRefundDetail(
        refundNo: j['refund_no'] as String,
        orderNo: j['order_no'] as String,
        paymentNo: j['payment_no'] as String?,
        refundType: (j['refund_type'] as num).toInt(),
        status: (j['status'] as num).toInt(),
        items: (j['items'] as List).map((e) => RefundItem.fromJson(e as Map<String, dynamic>)).toList(),
        goodsAmountCents: (j['goods_amount_cents'] as num?)?.toInt(),
        freightCents: (j['freight_cents'] as num?)?.toInt(),
        amountCents: (j['amount_cents'] as num).toInt(),
        channel: j['channel'] as String?,
        channelRefundId: j['channel_refund_id'] as String?,
        reasonCode: (j['reason_code'] as num?)?.toInt(),
        reasonText: j['reason_text'] as String?,
        evidenceUrls: (j['evidence_urls'] as List?)?.map((e) => e as String).toList(),
        returnShipment: j['return_shipment'] == null ? null : ReturnShipment.fromJson(j['return_shipment'] as Map<String, dynamic>),
        rejectReason: j['reject_reason'] as String?,
        auditedAt: j['audited_at'] as String?,
        returnDeadlineAt: j['return_deadline_at'] as String?,
        refundedAt: j['refunded_at'] as String?,
        createdAt: j['created_at'] as String,
        updatedAt: j['updated_at'] as String?,
        storeId: (j['store_id'] as num).toInt(),
        store: OrderStoreSnapshot.fromJson(j['store'] as Map<String, dynamic>),
        orderStatus: (j['order_status'] as num).toInt(),
        auditedBy: j['audited_by'] == null ? null : StaffRef.fromJson(j['audited_by'] as Map<String, dynamic>),
        receivedAt: j['received_at'] as String?,
        receivedBy: j['received_by'] == null ? null : StaffRef.fromJson(j['received_by'] as Map<String, dynamic>),
        order: AdminOrderSummary.fromJson(j['order'] as Map<String, dynamic>),
      );
  Map<String, dynamic> toJson() => {
        'refund_no': refundNo,
        'order_no': orderNo,
        if (paymentNo != null) 'payment_no': paymentNo,
        'refund_type': refundType,
        'status': status,
        'items': items.map((e) => e.toJson()).toList(),
        if (goodsAmountCents != null) 'goods_amount_cents': goodsAmountCents,
        if (freightCents != null) 'freight_cents': freightCents,
        'amount_cents': amountCents,
        if (channel != null) 'channel': channel,
        if (channelRefundId != null) 'channel_refund_id': channelRefundId,
        if (reasonCode != null) 'reason_code': reasonCode,
        if (reasonText != null) 'reason_text': reasonText,
        if (evidenceUrls != null) 'evidence_urls': evidenceUrls,
        if (returnShipment != null) 'return_shipment': returnShipment!.toJson(),
        if (rejectReason != null) 'reject_reason': rejectReason,
        if (auditedAt != null) 'audited_at': auditedAt,
        if (returnDeadlineAt != null) 'return_deadline_at': returnDeadlineAt,
        if (refundedAt != null) 'refunded_at': refundedAt,
        'created_at': createdAt,
        if (updatedAt != null) 'updated_at': updatedAt,
        'store_id': storeId,
        'store': store.toJson(),
        'order_status': orderStatus,
        if (auditedBy != null) 'audited_by': auditedBy!.toJson(),
        if (receivedAt != null) 'received_at': receivedAt,
        if (receivedBy != null) 'received_by': receivedBy!.toJson(),
        'order': order.toJson(),
      };
}

/// 后台视角的大区。**没有几何**——大区是门店的分组，
class AdminRegion {
  final int id;
  final String code;
  final String name;
  final int status;
  final int storeCount;
  final String? deletedAt;
  final String createdAt;
  final String? updatedAt;
  const AdminRegion({required this.id, required this.code, required this.name, required this.status, required this.storeCount, this.deletedAt, required this.createdAt, this.updatedAt});
  factory AdminRegion.fromJson(Map<String, dynamic> j) => AdminRegion(
        id: (j['id'] as num).toInt(),
        code: j['code'] as String,
        name: j['name'] as String,
        status: (j['status'] as num).toInt(),
        storeCount: (j['store_count'] as num).toInt(),
        deletedAt: j['deleted_at'] as String?,
        createdAt: j['created_at'] as String,
        updatedAt: j['updated_at'] as String?,
      );
  Map<String, dynamic> toJson() => {
        'id': id,
        'code': code,
        'name': name,
        'status': status,
        'store_count': storeCount,
        if (deletedAt != null) 'deleted_at': deletedAt,
        'created_at': createdAt,
        if (updatedAt != null) 'updated_at': updatedAt,
      };
}

/// 后台视角的门店，**含围栏**。与买家侧的 `Store` 分成两个 schema，
class AdminStore {
  final int id;
  final int regionId;
  final String? regionName;
  final String code;
  final String name;
  final String? phone;
  final String? province;
  final String? city;
  final String? district;
  final String? address;
  final double? lat;
  final double? lng;
  final dynamic fence;
  final bool isDefault;
  final int status;
  final String? deletedAt;
  final String createdAt;
  final String? updatedAt;
  const AdminStore({required this.id, required this.regionId, this.regionName, required this.code, required this.name, this.phone, this.province, this.city, this.district, this.address, this.lat, this.lng, this.fence, required this.isDefault, required this.status, this.deletedAt, required this.createdAt, this.updatedAt});
  factory AdminStore.fromJson(Map<String, dynamic> j) => AdminStore(
        id: (j['id'] as num).toInt(),
        regionId: (j['region_id'] as num).toInt(),
        regionName: j['region_name'] as String?,
        code: j['code'] as String,
        name: j['name'] as String,
        phone: j['phone'] as String?,
        province: j['province'] as String?,
        city: j['city'] as String?,
        district: j['district'] as String?,
        address: j['address'] as String?,
        lat: (j['lat'] as num?)?.toDouble(),
        lng: (j['lng'] as num?)?.toDouble(),
        fence: j['fence'],
        isDefault: j['is_default'] as bool,
        status: (j['status'] as num).toInt(),
        deletedAt: j['deleted_at'] as String?,
        createdAt: j['created_at'] as String,
        updatedAt: j['updated_at'] as String?,
      );
  Map<String, dynamic> toJson() => {
        'id': id,
        'region_id': regionId,
        if (regionName != null) 'region_name': regionName,
        'code': code,
        'name': name,
        if (phone != null) 'phone': phone,
        if (province != null) 'province': province,
        if (city != null) 'city': city,
        if (district != null) 'district': district,
        if (address != null) 'address': address,
        if (lat != null) 'lat': lat,
        if (lng != null) 'lng': lng,
        if (fence != null) 'fence': fence,
        'is_default': isDefault,
        'status': status,
        if (deletedAt != null) 'deleted_at': deletedAt,
        'created_at': createdAt,
        if (updatedAt != null) 'updated_at': updatedAt,
      };
}

/// 门店列表。比一般的分页响应多一个 `has_default`——
class AdminStoreList {
  final int page;
  final int pageSize;
  final int total;
  final List<AdminStore> items;
  final bool hasDefault;
  const AdminStoreList({required this.page, required this.pageSize, required this.total, required this.items, required this.hasDefault});
  factory AdminStoreList.fromJson(Map<String, dynamic> j) => AdminStoreList(
        page: (j['page'] as num).toInt(),
        pageSize: (j['page_size'] as num).toInt(),
        total: (j['total'] as num).toInt(),
        items: (j['items'] as List).map((e) => AdminStore.fromJson(e as Map<String, dynamic>)).toList(),
        hasDefault: j['has_default'] as bool,
      );
  Map<String, dynamic> toJson() => {
        'page': page,
        'page_size': pageSize,
        'total': total,
        'items': items.map((e) => e.toJson()).toList(),
        'has_default': hasDefault,
      };
}

class AgentBrief {
  final int id;
  final int agentStaffId;
  final String agentName;
  final String title;
  final String body;
  final String periodStart;
  final String periodEnd;
  final String createdAt;
  const AgentBrief({required this.id, required this.agentStaffId, required this.agentName, required this.title, required this.body, required this.periodStart, required this.periodEnd, required this.createdAt});
  factory AgentBrief.fromJson(Map<String, dynamic> j) => AgentBrief(
        id: (j['id'] as num).toInt(),
        agentStaffId: (j['agent_staff_id'] as num).toInt(),
        agentName: j['agent_name'] as String,
        title: j['title'] as String,
        body: j['body'] as String,
        periodStart: j['period_start'] as String,
        periodEnd: j['period_end'] as String,
        createdAt: j['created_at'] as String,
      );
  Map<String, dynamic> toJson() => {
        'id': id,
        'agent_staff_id': agentStaffId,
        'agent_name': agentName,
        'title': title,
        'body': body,
        'period_start': periodStart,
        'period_end': periodEnd,
        'created_at': createdAt,
      };
}

class AgentCreateRequest {
  final String name;
  final int role;
  final List<int>? regionIds;
  final List<int>? storeIds;
  const AgentCreateRequest({required this.name, required this.role, this.regionIds, this.storeIds});
  factory AgentCreateRequest.fromJson(Map<String, dynamic> j) => AgentCreateRequest(
        name: j['name'] as String,
        role: (j['role'] as num).toInt(),
        regionIds: (j['region_ids'] as List?)?.map((e) => (e as num).toInt()).toList(),
        storeIds: (j['store_ids'] as List?)?.map((e) => (e as num).toInt()).toList(),
      );
  Map<String, dynamic> toJson() => {
        'name': name,
        'role': role,
        if (regionIds != null) 'region_ids': regionIds,
        if (storeIds != null) 'store_ids': storeIds,
      };
}

class AgentKeyCreateRequest {
  final String name;
  final int? expiresInDays;
  const AgentKeyCreateRequest({required this.name, this.expiresInDays});
  factory AgentKeyCreateRequest.fromJson(Map<String, dynamic> j) => AgentKeyCreateRequest(
        name: j['name'] as String,
        expiresInDays: (j['expires_in_days'] as num?)?.toInt(),
      );
  Map<String, dynamic> toJson() => {
        'name': name,
        if (expiresInDays != null) 'expires_in_days': expiresInDays,
      };
}

class AgentKeyCreated {
  final int id;
  final String name;
  final String prefix;
  final String? expiresAt;
  final String? revokedAt;
  final String? lastUsedAt;
  final String createdAt;
  final String secret;
  const AgentKeyCreated({required this.id, required this.name, required this.prefix, this.expiresAt, this.revokedAt, this.lastUsedAt, required this.createdAt, required this.secret});
  factory AgentKeyCreated.fromJson(Map<String, dynamic> j) => AgentKeyCreated(
        id: (j['id'] as num).toInt(),
        name: j['name'] as String,
        prefix: j['prefix'] as String,
        expiresAt: j['expires_at'] as String?,
        revokedAt: j['revoked_at'] as String?,
        lastUsedAt: j['last_used_at'] as String?,
        createdAt: j['created_at'] as String,
        secret: j['secret'] as String,
      );
  Map<String, dynamic> toJson() => {
        'id': id,
        'name': name,
        'prefix': prefix,
        if (expiresAt != null) 'expires_at': expiresAt,
        if (revokedAt != null) 'revoked_at': revokedAt,
        if (lastUsedAt != null) 'last_used_at': lastUsedAt,
        'created_at': createdAt,
        'secret': secret,
      };
}

class AgentProposal {
  final int id;
  final int agentStaffId;
  final String agentName;
  final String kind;
  final int? storeId;
  final String? storeName;
  final int? skuId;
  final Map<String, dynamic> payload;
  final String title;
  final String evidence;
  final String expectedImpact;
  final int status;
  final int? decidedBy;
  final String? decidedByName;
  final String? decidedAt;
  final String? rejectReason;
  final Map<String, dynamic>? result;
  final String expiresAt;
  final String createdAt;
  final String updatedAt;
  final String? executedAt;
  final Map<String, dynamic>? outcome;
  final String? outcomeAt;
  const AgentProposal({required this.id, required this.agentStaffId, required this.agentName, required this.kind, this.storeId, this.storeName, this.skuId, required this.payload, required this.title, required this.evidence, required this.expectedImpact, required this.status, this.decidedBy, this.decidedByName, this.decidedAt, this.rejectReason, this.result, required this.expiresAt, required this.createdAt, required this.updatedAt, this.executedAt, this.outcome, this.outcomeAt});
  factory AgentProposal.fromJson(Map<String, dynamic> j) => AgentProposal(
        id: (j['id'] as num).toInt(),
        agentStaffId: (j['agent_staff_id'] as num).toInt(),
        agentName: j['agent_name'] as String,
        kind: j['kind'] as String,
        storeId: (j['store_id'] as num?)?.toInt(),
        storeName: j['store_name'] as String?,
        skuId: (j['sku_id'] as num?)?.toInt(),
        payload: j['payload'] as Map<String, dynamic>,
        title: j['title'] as String,
        evidence: j['evidence'] as String,
        expectedImpact: j['expected_impact'] as String,
        status: (j['status'] as num).toInt(),
        decidedBy: (j['decided_by'] as num?)?.toInt(),
        decidedByName: j['decided_by_name'] as String?,
        decidedAt: j['decided_at'] as String?,
        rejectReason: j['reject_reason'] as String?,
        result: j['result'] as Map<String, dynamic>?,
        expiresAt: j['expires_at'] as String,
        createdAt: j['created_at'] as String,
        updatedAt: j['updated_at'] as String,
        executedAt: j['executed_at'] as String?,
        outcome: j['outcome'] as Map<String, dynamic>?,
        outcomeAt: j['outcome_at'] as String?,
      );
  Map<String, dynamic> toJson() => {
        'id': id,
        'agent_staff_id': agentStaffId,
        'agent_name': agentName,
        'kind': kind,
        if (storeId != null) 'store_id': storeId,
        if (storeName != null) 'store_name': storeName,
        if (skuId != null) 'sku_id': skuId,
        'payload': payload,
        'title': title,
        'evidence': evidence,
        'expected_impact': expectedImpact,
        'status': status,
        if (decidedBy != null) 'decided_by': decidedBy,
        if (decidedByName != null) 'decided_by_name': decidedByName,
        if (decidedAt != null) 'decided_at': decidedAt,
        if (rejectReason != null) 'reject_reason': rejectReason,
        if (result != null) 'result': result,
        'expires_at': expiresAt,
        'created_at': createdAt,
        'updated_at': updatedAt,
        if (executedAt != null) 'executed_at': executedAt,
        if (outcome != null) 'outcome': outcome,
        if (outcomeAt != null) 'outcome_at': outcomeAt,
      };
}

class AgentProposalRejectRequest {
  final String reason;
  const AgentProposalRejectRequest({required this.reason});
  factory AgentProposalRejectRequest.fromJson(Map<String, dynamic> j) => AgentProposalRejectRequest(
        reason: j['reason'] as String,
      );
  Map<String, dynamic> toJson() => {
        'reason': reason,
      };
}

class AgentScorecardKind {
  final String kind;
  final int proposed;
  final int approved;
  final int executed;
  final int failed;
  final int rejected;
  final int expired;
  final int open;
  final int positive;
  final int neutral;
  final int negative;
  const AgentScorecardKind({required this.kind, required this.proposed, required this.approved, required this.executed, required this.failed, required this.rejected, required this.expired, required this.open, required this.positive, required this.neutral, required this.negative});
  factory AgentScorecardKind.fromJson(Map<String, dynamic> j) => AgentScorecardKind(
        kind: j['kind'] as String,
        proposed: (j['proposed'] as num).toInt(),
        approved: (j['approved'] as num).toInt(),
        executed: (j['executed'] as num).toInt(),
        failed: (j['failed'] as num).toInt(),
        rejected: (j['rejected'] as num).toInt(),
        expired: (j['expired'] as num).toInt(),
        open: (j['open'] as num).toInt(),
        positive: (j['positive'] as num).toInt(),
        neutral: (j['neutral'] as num).toInt(),
        negative: (j['negative'] as num).toInt(),
      );
  Map<String, dynamic> toJson() => {
        'kind': kind,
        'proposed': proposed,
        'approved': approved,
        'executed': executed,
        'failed': failed,
        'rejected': rejected,
        'expired': expired,
        'open': open,
        'positive': positive,
        'neutral': neutral,
        'negative': negative,
      };
}

class AgentScorecardEntry {
  final int proposalId;
  final String kind;
  final String title;
  final Map<String, dynamic> outcome;
  final String outcomeAt;
  const AgentScorecardEntry({required this.proposalId, required this.kind, required this.title, required this.outcome, required this.outcomeAt});
  factory AgentScorecardEntry.fromJson(Map<String, dynamic> j) => AgentScorecardEntry(
        proposalId: (j['proposal_id'] as num).toInt(),
        kind: j['kind'] as String,
        title: j['title'] as String,
        outcome: j['outcome'] as Map<String, dynamic>,
        outcomeAt: j['outcome_at'] as String,
      );
  Map<String, dynamic> toJson() => {
        'proposal_id': proposalId,
        'kind': kind,
        'title': title,
        'outcome': outcome,
        'outcome_at': outcomeAt,
      };
}

class AgentScorecard {
  final int agentStaffId;
  final String since;
  final List<AgentScorecardKind> kinds;
  final List<AgentScorecardEntry> recent;
  const AgentScorecard({required this.agentStaffId, required this.since, required this.kinds, required this.recent});
  factory AgentScorecard.fromJson(Map<String, dynamic> j) => AgentScorecard(
        agentStaffId: (j['agent_staff_id'] as num).toInt(),
        since: j['since'] as String,
        kinds: (j['kinds'] as List).map((e) => AgentScorecardKind.fromJson(e as Map<String, dynamic>)).toList(),
        recent: (j['recent'] as List).map((e) => AgentScorecardEntry.fromJson(e as Map<String, dynamic>)).toList(),
      );
  Map<String, dynamic> toJson() => {
        'agent_staff_id': agentStaffId,
        'since': since,
        'kinds': kinds.map((e) => e.toJson()).toList(),
        'recent': recent.map((e) => e.toJson()).toList(),
      };
}

class AgentUpdateRequest {
  final String? name;
  final int? role;
  final int? status;
  final List<int>? regionIds;
  final List<int>? storeIds;
  const AgentUpdateRequest({this.name, this.role, this.status, this.regionIds, this.storeIds});
  factory AgentUpdateRequest.fromJson(Map<String, dynamic> j) => AgentUpdateRequest(
        name: j['name'] as String?,
        role: (j['role'] as num?)?.toInt(),
        status: (j['status'] as num?)?.toInt(),
        regionIds: (j['region_ids'] as List?)?.map((e) => (e as num).toInt()).toList(),
        storeIds: (j['store_ids'] as List?)?.map((e) => (e as num).toInt()).toList(),
      );
  Map<String, dynamic> toJson() => {
        if (name != null) 'name': name,
        if (role != null) 'role': role,
        if (status != null) 'status': status,
        if (regionIds != null) 'region_ids': regionIds,
        if (storeIds != null) 'store_ids': storeIds,
      };
}

class AgentWhoAmI {
  final int staffId;
  final String name;
  final int role;
  final List<int> regionIds;
  final List<int> storeIds;
  final int keyId;
  const AgentWhoAmI({required this.staffId, required this.name, required this.role, required this.regionIds, required this.storeIds, required this.keyId});
  factory AgentWhoAmI.fromJson(Map<String, dynamic> j) => AgentWhoAmI(
        staffId: (j['staff_id'] as num).toInt(),
        name: j['name'] as String,
        role: (j['role'] as num).toInt(),
        regionIds: (j['region_ids'] as List).map((e) => (e as num).toInt()).toList(),
        storeIds: (j['store_ids'] as List).map((e) => (e as num).toInt()).toList(),
        keyId: (j['key_id'] as num).toInt(),
      );
  Map<String, dynamic> toJson() => {
        'staff_id': staffId,
        'name': name,
        'role': role,
        'region_ids': regionIds,
        'store_ids': storeIds,
        'key_id': keyId,
      };
}

class ApplicableCoupon {
  final int id;
  final String couponCode;
  final int templateId;
  final String name;
  final CouponType couponType;
  final Money thresholdCents;
  final Money discountCents;
  final int discountRate;
  final Money maxDiscountCents;
  final int status;
  final int source;
  final String validStartAt;
  final String validEndAt;
  final String? usedAt;
  final List<CouponScope> scopes;
  final Money applicableDiscountCents;
  const ApplicableCoupon({required this.id, required this.couponCode, required this.templateId, required this.name, required this.couponType, required this.thresholdCents, required this.discountCents, required this.discountRate, required this.maxDiscountCents, required this.status, required this.source, required this.validStartAt, required this.validEndAt, this.usedAt, required this.scopes, required this.applicableDiscountCents});
  factory ApplicableCoupon.fromJson(Map<String, dynamic> j) => ApplicableCoupon(
        id: (j['id'] as num).toInt(),
        couponCode: j['coupon_code'] as String,
        templateId: (j['template_id'] as num).toInt(),
        name: j['name'] as String,
        couponType: (j['coupon_type'] as num).toInt(),
        thresholdCents: (j['threshold_cents'] as num).toInt(),
        discountCents: (j['discount_cents'] as num).toInt(),
        discountRate: (j['discount_rate'] as num).toInt(),
        maxDiscountCents: (j['max_discount_cents'] as num).toInt(),
        status: (j['status'] as num).toInt(),
        source: (j['source'] as num).toInt(),
        validStartAt: j['valid_start_at'] as String,
        validEndAt: j['valid_end_at'] as String,
        usedAt: j['used_at'] as String?,
        scopes: (j['scopes'] as List).map((e) => CouponScope.fromJson(e as Map<String, dynamic>)).toList(),
        applicableDiscountCents: (j['applicable_discount_cents'] as num).toInt(),
      );
  Map<String, dynamic> toJson() => {
        'id': id,
        'coupon_code': couponCode,
        'template_id': templateId,
        'name': name,
        'coupon_type': couponType,
        'threshold_cents': thresholdCents,
        'discount_cents': discountCents,
        'discount_rate': discountRate,
        'max_discount_cents': maxDiscountCents,
        'status': status,
        'source': source,
        'valid_start_at': validStartAt,
        'valid_end_at': validEndAt,
        if (usedAt != null) 'used_at': usedAt,
        'scopes': scopes.map((e) => e.toJson()).toList(),
        'applicable_discount_cents': applicableDiscountCents,
      };
}

/// 这一行此刻能不能买，按响应里 `store` 那家门店判。判定顺序即下表顺序，
typedef CartItemStatus = String;

class FreightUndeliverableLine {
  final int skuId;
  final String reasonCode;
  final String reason;
  const FreightUndeliverableLine({required this.skuId, required this.reasonCode, required this.reason});
  factory FreightUndeliverableLine.fromJson(Map<String, dynamic> j) => FreightUndeliverableLine(
        skuId: (j['sku_id'] as num).toInt(),
        reasonCode: j['reason_code'] as String,
        reason: j['reason'] as String,
      );
  Map<String, dynamic> toJson() => {
        'sku_id': skuId,
        'reason_code': reasonCode,
        'reason': reason,
      };
}

class CartItem {
  final int id;
  final int skuId;
  final int? productId;
  final String? title;
  final Map<String, String>? specValues;
  final String? imageUrl;
  final int? priceCents;
  final int? listPriceCents;
  final int? pricePromotionId;
  final int quantity;
  final bool selected;
  final bool available;
  final CartItemStatus status;
  final FreightUndeliverableLine? undeliverable;
  const CartItem({required this.id, required this.skuId, this.productId, this.title, this.specValues, this.imageUrl, this.priceCents, this.listPriceCents, this.pricePromotionId, required this.quantity, required this.selected, required this.available, required this.status, this.undeliverable});
  factory CartItem.fromJson(Map<String, dynamic> j) => CartItem(
        id: (j['id'] as num).toInt(),
        skuId: (j['sku_id'] as num).toInt(),
        productId: (j['product_id'] as num?)?.toInt(),
        title: j['title'] as String?,
        specValues: (j['spec_values'] as Map?)?.map((k, e) => MapEntry(k as String, e as String)),
        imageUrl: j['image_url'] as String?,
        priceCents: (j['price_cents'] as num?)?.toInt(),
        listPriceCents: (j['list_price_cents'] as num?)?.toInt(),
        pricePromotionId: (j['price_promotion_id'] as num?)?.toInt(),
        quantity: (j['quantity'] as num).toInt(),
        selected: j['selected'] as bool,
        available: j['available'] as bool,
        status: j['status'] as String,
        undeliverable: j['undeliverable'] == null ? null : FreightUndeliverableLine.fromJson(j['undeliverable'] as Map<String, dynamic>),
      );
  Map<String, dynamic> toJson() => {
        'id': id,
        'sku_id': skuId,
        if (productId != null) 'product_id': productId,
        if (title != null) 'title': title,
        if (specValues != null) 'spec_values': specValues,
        if (imageUrl != null) 'image_url': imageUrl,
        'price_cents': priceCents,
        if (listPriceCents != null) 'list_price_cents': listPriceCents,
        if (pricePromotionId != null) 'price_promotion_id': pricePromotionId,
        'quantity': quantity,
        'selected': selected,
        'available': available,
        'status': status,
        if (undeliverable != null) 'undeliverable': undeliverable!.toJson(),
      };
}

/// 「当前门店」是怎么定下来的。这三个值是产品规则的对外形状，
typedef StoreMatchType = String;

/// 每一条会受门店影响的读接口都回显它。**没有它，客户端拿到的
class StoreContext {
  final int? storeId;
  final int? regionId;
  final StoreMatchType matchType;
  const StoreContext({this.storeId, this.regionId, required this.matchType});
  factory StoreContext.fromJson(Map<String, dynamic> j) => StoreContext(
        storeId: (j['store_id'] as num?)?.toInt(),
        regionId: (j['region_id'] as num?)?.toInt(),
        matchType: j['match_type'] as String,
      );
  Map<String, dynamic> toJson() => {
        if (storeId != null) 'store_id': storeId,
        if (regionId != null) 'region_id': regionId,
        'match_type': matchType,
      };
}

/// 一个活动在这一单（或购物车已勾选的行）上的结果。试算、购物车、下单用的是同一份计算
class PromotionHit {
  final int promotionId;
  final String name;
  final PromotionType promotionType;
  final bool applied;
  final Money discountCents;
  final bool? stackWithCoupon;
  final int? thresholdUnit;
  final int? reachedThreshold;
  final int? nextThreshold;
  final int? shortfall;
  final List<int> skuIds;
  final String message;
  const PromotionHit({required this.promotionId, required this.name, required this.promotionType, required this.applied, required this.discountCents, this.stackWithCoupon, this.thresholdUnit, this.reachedThreshold, this.nextThreshold, this.shortfall, required this.skuIds, required this.message});
  factory PromotionHit.fromJson(Map<String, dynamic> j) => PromotionHit(
        promotionId: (j['promotion_id'] as num).toInt(),
        name: j['name'] as String,
        promotionType: (j['promotion_type'] as num).toInt(),
        applied: j['applied'] as bool,
        discountCents: (j['discount_cents'] as num).toInt(),
        stackWithCoupon: j['stack_with_coupon'] as bool?,
        thresholdUnit: (j['threshold_unit'] as num?)?.toInt(),
        reachedThreshold: (j['reached_threshold'] as num?)?.toInt(),
        nextThreshold: (j['next_threshold'] as num?)?.toInt(),
        shortfall: (j['shortfall'] as num?)?.toInt(),
        skuIds: (j['sku_ids'] as List).map((e) => (e as num).toInt()).toList(),
        message: j['message'] as String,
      );
  Map<String, dynamic> toJson() => {
        'promotion_id': promotionId,
        'name': name,
        'promotion_type': promotionType,
        'applied': applied,
        'discount_cents': discountCents,
        if (stackWithCoupon != null) 'stack_with_coupon': stackWithCoupon,
        if (thresholdUnit != null) 'threshold_unit': thresholdUnit,
        if (reachedThreshold != null) 'reached_threshold': reachedThreshold,
        if (nextThreshold != null) 'next_threshold': nextThreshold,
        if (shortfall != null) 'shortfall': shortfall,
        'sku_ids': skuIds,
        'message': message,
      };
}

class Cart {
  final List<CartItem> items;
  final StoreContext store;
  final Money totalCents;
  final Money selectedTotalCents;
  final Money promotionDiscountCents;
  final List<PromotionHit> promotions;
  final int? addressId;
  final FreightBreakdown? freight;
  const Cart({required this.items, required this.store, required this.totalCents, required this.selectedTotalCents, required this.promotionDiscountCents, required this.promotions, this.addressId, this.freight});
  factory Cart.fromJson(Map<String, dynamic> j) => Cart(
        items: (j['items'] as List).map((e) => CartItem.fromJson(e as Map<String, dynamic>)).toList(),
        store: StoreContext.fromJson(j['store'] as Map<String, dynamic>),
        totalCents: (j['total_cents'] as num).toInt(),
        selectedTotalCents: (j['selected_total_cents'] as num).toInt(),
        promotionDiscountCents: (j['promotion_discount_cents'] as num).toInt(),
        promotions: (j['promotions'] as List).map((e) => PromotionHit.fromJson(e as Map<String, dynamic>)).toList(),
        addressId: (j['address_id'] as num?)?.toInt(),
        freight: j['freight'] == null ? null : FreightBreakdown.fromJson(j['freight'] as Map<String, dynamic>),
      );
  Map<String, dynamic> toJson() => {
        'items': items.map((e) => e.toJson()).toList(),
        'store': store.toJson(),
        'total_cents': totalCents,
        'selected_total_cents': selectedTotalCents,
        'promotion_discount_cents': promotionDiscountCents,
        'promotions': promotions.map((e) => e.toJson()).toList(),
        if (addressId != null) 'address_id': addressId,
        if (freight != null) 'freight': freight!.toJson(),
      };
}

class Category {
  final int id;
  final String name;
  final int level;
  final List<Category>? children;
  const Category({required this.id, required this.name, required this.level, this.children});
  factory Category.fromJson(Map<String, dynamic> j) => Category(
        id: (j['id'] as num).toInt(),
        name: j['name'] as String,
        level: (j['level'] as num).toInt(),
        children: (j['children'] as List?)?.map((e) => Category.fromJson(e as Map<String, dynamic>)).toList(),
      );
  Map<String, dynamic> toJson() => {
        'id': id,
        'name': name,
        'level': level,
        if (children != null) 'children': children!.map((e) => e.toJson()).toList(),
      };
}

/// **没有 `path` 与 `level`**，它们由服务端从 `parent_id` 算出。
class CategoryCreateRequest {
  final String name;
  final int? parentId;
  final int? sortOrder;
  const CategoryCreateRequest({required this.name, this.parentId, this.sortOrder});
  factory CategoryCreateRequest.fromJson(Map<String, dynamic> j) => CategoryCreateRequest(
        name: j['name'] as String,
        parentId: (j['parent_id'] as num?)?.toInt(),
        sortOrder: (j['sort_order'] as num?)?.toInt(),
      );
  Map<String, dynamic> toJson() => {
        'name': name,
        if (parentId != null) 'parent_id': parentId,
        if (sortOrder != null) 'sort_order': sortOrder,
      };
}

/// 传 `parent_id` 就是**移动子树**，服务端会在同一事务里重写整棵子树的
class CategoryUpdateRequest {
  final String? name;
  final int? parentId;
  final int? sortOrder;
  final int? status;
  const CategoryUpdateRequest({this.name, this.parentId, this.sortOrder, this.status});
  factory CategoryUpdateRequest.fromJson(Map<String, dynamic> j) => CategoryUpdateRequest(
        name: j['name'] as String?,
        parentId: (j['parent_id'] as num?)?.toInt(),
        sortOrder: (j['sort_order'] as num?)?.toInt(),
        status: (j['status'] as num?)?.toInt(),
      );
  Map<String, dynamic> toJson() => {
        if (name != null) 'name': name,
        if (parentId != null) 'parent_id': parentId,
        if (sortOrder != null) 'sort_order': sortOrder,
        if (status != null) 'status': status,
      };
}

/// 商品当前生效的活动标签（商品列表 / 详情）。按响应里 `store` 那家门店判：
class PromotionTag {
  final int promotionId;
  final PromotionType promotionType;
  final String label;
  final String? endsAt;
  const PromotionTag({required this.promotionId, required this.promotionType, required this.label, this.endsAt});
  factory PromotionTag.fromJson(Map<String, dynamic> j) => PromotionTag(
        promotionId: (j['promotion_id'] as num).toInt(),
        promotionType: (j['promotion_type'] as num).toInt(),
        label: j['label'] as String,
        endsAt: j['ends_at'] as String?,
      );
  Map<String, dynamic> toJson() => {
        'promotion_id': promotionId,
        'promotion_type': promotionType,
        'label': label,
        if (endsAt != null) 'ends_at': endsAt,
      };
}

class ProductSummary {
  final int id;
  final String title;
  final String? subtitle;
  final String? imageUrl;
  final Money minPriceCents;
  final Money? maxPriceCents;
  final bool? inStock;
  final int? salesCount;
  final int status;
  final List<PromotionTag>? promotionTags;
  const ProductSummary({required this.id, required this.title, this.subtitle, this.imageUrl, required this.minPriceCents, this.maxPriceCents, this.inStock, this.salesCount, required this.status, this.promotionTags});
  factory ProductSummary.fromJson(Map<String, dynamic> j) => ProductSummary(
        id: (j['id'] as num).toInt(),
        title: j['title'] as String,
        subtitle: j['subtitle'] as String?,
        imageUrl: j['image_url'] as String?,
        minPriceCents: (j['min_price_cents'] as num).toInt(),
        maxPriceCents: (j['max_price_cents'] as num?)?.toInt(),
        inStock: j['in_stock'] as bool?,
        salesCount: (j['sales_count'] as num?)?.toInt(),
        status: (j['status'] as num).toInt(),
        promotionTags: (j['promotion_tags'] as List?)?.map((e) => PromotionTag.fromJson(e as Map<String, dynamic>)).toList(),
      );
  Map<String, dynamic> toJson() => {
        'id': id,
        'title': title,
        if (subtitle != null) 'subtitle': subtitle,
        if (imageUrl != null) 'image_url': imageUrl,
        'min_price_cents': minPriceCents,
        if (maxPriceCents != null) 'max_price_cents': maxPriceCents,
        if (inStock != null) 'in_stock': inStock,
        if (salesCount != null) 'sales_count': salesCount,
        'status': status,
        if (promotionTags != null) 'promotion_tags': promotionTags!.map((e) => e.toJson()).toList(),
      };
}

class ChatReplyAction {
  final String? type;
  final Map<String, dynamic>? params;
  const ChatReplyAction({this.type, this.params});
  factory ChatReplyAction.fromJson(Map<String, dynamic> j) => ChatReplyAction(
        type: j['type'] as String?,
        params: j['params'] as Map<String, dynamic>?,
      );
  Map<String, dynamic> toJson() => {
        if (type != null) 'type': type,
        if (params != null) 'params': params,
      };
}

class ChatReply {
  final String sessionId;
  final String reply;
  final List<ProductSummary>? products;
  final Map<String, dynamic>? intent;
  final ChatReplyAction? action;
  const ChatReply({required this.sessionId, required this.reply, this.products, this.intent, this.action});
  factory ChatReply.fromJson(Map<String, dynamic> j) => ChatReply(
        sessionId: j['session_id'] as String,
        reply: j['reply'] as String,
        products: (j['products'] as List?)?.map((e) => ProductSummary.fromJson(e as Map<String, dynamic>)).toList(),
        intent: j['intent'] as Map<String, dynamic>?,
        action: j['action'] == null ? null : ChatReplyAction.fromJson(j['action'] as Map<String, dynamic>),
      );
  Map<String, dynamic> toJson() => {
        'session_id': sessionId,
        'reply': reply,
        if (products != null) 'products': products!.map((e) => e.toJson()).toList(),
        if (intent != null) 'intent': intent,
        if (action != null) 'action': action!.toJson(),
      };
}

class ClaimableCouponTemplate {
  final int id;
  final String name;
  final CouponType couponType;
  final Money thresholdCents;
  final Money discountCents;
  final int discountRate;
  final Money maxDiscountCents;
  final int validMode;
  final String? validStartAt;
  final String? validEndAt;
  final int validDays;
  final int? remaining;
  final int perUserLimit;
  final int claimedCount;
  final bool canClaim;
  final List<CouponScope> scopes;
  const ClaimableCouponTemplate({required this.id, required this.name, required this.couponType, required this.thresholdCents, required this.discountCents, required this.discountRate, required this.maxDiscountCents, required this.validMode, this.validStartAt, this.validEndAt, required this.validDays, this.remaining, required this.perUserLimit, required this.claimedCount, required this.canClaim, required this.scopes});
  factory ClaimableCouponTemplate.fromJson(Map<String, dynamic> j) => ClaimableCouponTemplate(
        id: (j['id'] as num).toInt(),
        name: j['name'] as String,
        couponType: (j['coupon_type'] as num).toInt(),
        thresholdCents: (j['threshold_cents'] as num).toInt(),
        discountCents: (j['discount_cents'] as num).toInt(),
        discountRate: (j['discount_rate'] as num).toInt(),
        maxDiscountCents: (j['max_discount_cents'] as num).toInt(),
        validMode: (j['valid_mode'] as num).toInt(),
        validStartAt: j['valid_start_at'] as String?,
        validEndAt: j['valid_end_at'] as String?,
        validDays: (j['valid_days'] as num).toInt(),
        remaining: (j['remaining'] as num?)?.toInt(),
        perUserLimit: (j['per_user_limit'] as num).toInt(),
        claimedCount: (j['claimed_count'] as num).toInt(),
        canClaim: j['can_claim'] as bool,
        scopes: (j['scopes'] as List).map((e) => CouponScope.fromJson(e as Map<String, dynamic>)).toList(),
      );
  Map<String, dynamic> toJson() => {
        'id': id,
        'name': name,
        'coupon_type': couponType,
        'threshold_cents': thresholdCents,
        'discount_cents': discountCents,
        'discount_rate': discountRate,
        'max_discount_cents': maxDiscountCents,
        'valid_mode': validMode,
        if (validStartAt != null) 'valid_start_at': validStartAt,
        if (validEndAt != null) 'valid_end_at': validEndAt,
        'valid_days': validDays,
        'remaining': remaining,
        'per_user_limit': perUserLimit,
        'claimed_count': claimedCount,
        'can_claim': canClaim,
        'scopes': scopes.map((e) => e.toJson()).toList(),
      };
}

class OrderItemInput {
  final int skuId;
  final int quantity;
  const OrderItemInput({required this.skuId, required this.quantity});
  factory OrderItemInput.fromJson(Map<String, dynamic> j) => OrderItemInput(
        skuId: (j['sku_id'] as num).toInt(),
        quantity: (j['quantity'] as num).toInt(),
      );
  Map<String, dynamic> toJson() => {
        'sku_id': skuId,
        'quantity': quantity,
      };
}

class CouponApplicableRequest {
  final List<OrderItemInput> items;
  final int storeId;
  final int? addressId;
  const CouponApplicableRequest({required this.items, required this.storeId, this.addressId});
  factory CouponApplicableRequest.fromJson(Map<String, dynamic> j) => CouponApplicableRequest(
        items: (j['items'] as List).map((e) => OrderItemInput.fromJson(e as Map<String, dynamic>)).toList(),
        storeId: (j['store_id'] as num).toInt(),
        addressId: (j['address_id'] as num?)?.toInt(),
      );
  Map<String, dynamic> toJson() => {
        'items': items.map((e) => e.toJson()).toList(),
        'store_id': storeId,
        if (addressId != null) 'address_id': addressId,
      };
}

class CouponGrantItem {
  final String phone;
  final int userId;
  final int userCouponId;
  final String couponCode;
  const CouponGrantItem({required this.phone, required this.userId, required this.userCouponId, required this.couponCode});
  factory CouponGrantItem.fromJson(Map<String, dynamic> j) => CouponGrantItem(
        phone: j['phone'] as String,
        userId: (j['user_id'] as num).toInt(),
        userCouponId: (j['user_coupon_id'] as num).toInt(),
        couponCode: j['coupon_code'] as String,
      );
  Map<String, dynamic> toJson() => {
        'phone': phone,
        'user_id': userId,
        'user_coupon_id': userCouponId,
        'coupon_code': couponCode,
      };
}

class CouponGrantRequest {
  final List<String> phones;
  const CouponGrantRequest({required this.phones});
  factory CouponGrantRequest.fromJson(Map<String, dynamic> j) => CouponGrantRequest(
        phones: (j['phones'] as List).map((e) => e as String).toList(),
      );
  Map<String, dynamic> toJson() => {
        'phones': phones,
      };
}

class CouponGrantResult {
  final int templateId;
  final int granted;
  final List<CouponGrantItem> coupons;
  const CouponGrantResult({required this.templateId, required this.granted, required this.coupons});
  factory CouponGrantResult.fromJson(Map<String, dynamic> j) => CouponGrantResult(
        templateId: (j['template_id'] as num).toInt(),
        granted: (j['granted'] as num).toInt(),
        coupons: (j['coupons'] as List).map((e) => CouponGrantItem.fromJson(e as Map<String, dynamic>)).toList(),
      );
  Map<String, dynamic> toJson() => {
        'template_id': templateId,
        'granted': granted,
        'coupons': coupons.map((e) => e.toJson()).toList(),
      };
}

class CouponScopeInput {
  final int scopeType;
  final int? targetId;
  final bool? include;
  const CouponScopeInput({required this.scopeType, this.targetId, this.include});
  factory CouponScopeInput.fromJson(Map<String, dynamic> j) => CouponScopeInput(
        scopeType: (j['scope_type'] as num).toInt(),
        targetId: (j['target_id'] as num?)?.toInt(),
        include: j['include'] as bool?,
      );
  Map<String, dynamic> toJson() => {
        'scope_type': scopeType,
        if (targetId != null) 'target_id': targetId,
        if (include != null) 'include': include,
      };
}

class CouponScopesSetRequest {
  final List<CouponScopeInput> scopes;
  const CouponScopesSetRequest({required this.scopes});
  factory CouponScopesSetRequest.fromJson(Map<String, dynamic> j) => CouponScopesSetRequest(
        scopes: (j['scopes'] as List).map((e) => CouponScopeInput.fromJson(e as Map<String, dynamic>)).toList(),
      );
  Map<String, dynamic> toJson() => {
        'scopes': scopes.map((e) => e.toJson()).toList(),
      };
}

class CouponTemplateCreateRequest {
  final String name;
  final CouponType couponType;
  final Money? thresholdCents;
  final Money? discountCents;
  final int? discountRate;
  final Money? maxDiscountCents;
  final int validMode;
  final String? validStartAt;
  final String? validEndAt;
  final int? validDays;
  final int? totalCount;
  final int? perUserLimit;
  final bool? claimable;
  const CouponTemplateCreateRequest({required this.name, required this.couponType, this.thresholdCents, this.discountCents, this.discountRate, this.maxDiscountCents, required this.validMode, this.validStartAt, this.validEndAt, this.validDays, this.totalCount, this.perUserLimit, this.claimable});
  factory CouponTemplateCreateRequest.fromJson(Map<String, dynamic> j) => CouponTemplateCreateRequest(
        name: j['name'] as String,
        couponType: (j['coupon_type'] as num).toInt(),
        thresholdCents: (j['threshold_cents'] as num?)?.toInt(),
        discountCents: (j['discount_cents'] as num?)?.toInt(),
        discountRate: (j['discount_rate'] as num?)?.toInt(),
        maxDiscountCents: (j['max_discount_cents'] as num?)?.toInt(),
        validMode: (j['valid_mode'] as num).toInt(),
        validStartAt: j['valid_start_at'] as String?,
        validEndAt: j['valid_end_at'] as String?,
        validDays: (j['valid_days'] as num?)?.toInt(),
        totalCount: (j['total_count'] as num?)?.toInt(),
        perUserLimit: (j['per_user_limit'] as num?)?.toInt(),
        claimable: j['claimable'] as bool?,
      );
  Map<String, dynamic> toJson() => {
        'name': name,
        'coupon_type': couponType,
        if (thresholdCents != null) 'threshold_cents': thresholdCents,
        if (discountCents != null) 'discount_cents': discountCents,
        if (discountRate != null) 'discount_rate': discountRate,
        if (maxDiscountCents != null) 'max_discount_cents': maxDiscountCents,
        'valid_mode': validMode,
        if (validStartAt != null) 'valid_start_at': validStartAt,
        if (validEndAt != null) 'valid_end_at': validEndAt,
        if (validDays != null) 'valid_days': validDays,
        if (totalCount != null) 'total_count': totalCount,
        if (perUserLimit != null) 'per_user_limit': perUserLimit,
        if (claimable != null) 'claimable': claimable,
      };
}

/// 只改传了的字段。有效期整组替换：改 `valid_mode` 时要把那一模式的字段一起给。
class CouponTemplatePatchRequest {
  final String? name;
  final CouponType? couponType;
  final Money? thresholdCents;
  final Money? discountCents;
  final int? discountRate;
  final Money? maxDiscountCents;
  final int? validMode;
  final String? validStartAt;
  final String? validEndAt;
  final int? validDays;
  final int? totalCount;
  final int? perUserLimit;
  final bool? claimable;
  final int? status;
  const CouponTemplatePatchRequest({this.name, this.couponType, this.thresholdCents, this.discountCents, this.discountRate, this.maxDiscountCents, this.validMode, this.validStartAt, this.validEndAt, this.validDays, this.totalCount, this.perUserLimit, this.claimable, this.status});
  factory CouponTemplatePatchRequest.fromJson(Map<String, dynamic> j) => CouponTemplatePatchRequest(
        name: j['name'] as String?,
        couponType: (j['coupon_type'] as num?)?.toInt(),
        thresholdCents: (j['threshold_cents'] as num?)?.toInt(),
        discountCents: (j['discount_cents'] as num?)?.toInt(),
        discountRate: (j['discount_rate'] as num?)?.toInt(),
        maxDiscountCents: (j['max_discount_cents'] as num?)?.toInt(),
        validMode: (j['valid_mode'] as num?)?.toInt(),
        validStartAt: j['valid_start_at'] as String?,
        validEndAt: j['valid_end_at'] as String?,
        validDays: (j['valid_days'] as num?)?.toInt(),
        totalCount: (j['total_count'] as num?)?.toInt(),
        perUserLimit: (j['per_user_limit'] as num?)?.toInt(),
        claimable: j['claimable'] as bool?,
        status: (j['status'] as num?)?.toInt(),
      );
  Map<String, dynamic> toJson() => {
        if (name != null) 'name': name,
        if (couponType != null) 'coupon_type': couponType,
        if (thresholdCents != null) 'threshold_cents': thresholdCents,
        if (discountCents != null) 'discount_cents': discountCents,
        if (discountRate != null) 'discount_rate': discountRate,
        if (maxDiscountCents != null) 'max_discount_cents': maxDiscountCents,
        if (validMode != null) 'valid_mode': validMode,
        if (validStartAt != null) 'valid_start_at': validStartAt,
        if (validEndAt != null) 'valid_end_at': validEndAt,
        if (validDays != null) 'valid_days': validDays,
        if (totalCount != null) 'total_count': totalCount,
        if (perUserLimit != null) 'per_user_limit': perUserLimit,
        if (claimable != null) 'claimable': claimable,
        if (status != null) 'status': status,
      };
}

/// 一条字段级错误。`field` 是请求体（或商品对象）里的字段名。
class FieldError {
  final String? field;
  final String? message;
  final int? offset;
  final int? length;
  const FieldError({this.field, this.message, this.offset, this.length});
  factory FieldError.fromJson(Map<String, dynamic> j) => FieldError(
        field: j['field'] as String?,
        message: j['message'] as String?,
        offset: (j['offset'] as num?)?.toInt(),
        length: (j['length'] as num?)?.toInt(),
      );
  Map<String, dynamic> toJson() => {
        if (field != null) 'field': field,
        if (message != null) 'message': message,
        if (offset != null) 'offset': offset,
        if (length != null) 'length': length,
      };
}

/// 新建与整体替换共用。字段含义见 `POST /admin/freight-templates`。
class FreightTemplateInput {
  final String name;
  final int? storeId;
  final FreightChargeMode chargeMode;
  final bool? isDefault;
  final List<FreightRule> rules;
  final List<ProvinceCode>? undeliverableRegionCodes;
  const FreightTemplateInput({required this.name, this.storeId, required this.chargeMode, this.isDefault, required this.rules, this.undeliverableRegionCodes});
  factory FreightTemplateInput.fromJson(Map<String, dynamic> j) => FreightTemplateInput(
        name: j['name'] as String,
        storeId: (j['store_id'] as num?)?.toInt(),
        chargeMode: (j['charge_mode'] as num).toInt(),
        isDefault: j['is_default'] as bool?,
        rules: (j['rules'] as List).map((e) => FreightRule.fromJson(e as Map<String, dynamic>)).toList(),
        undeliverableRegionCodes: (j['undeliverable_region_codes'] as List?)?.map((e) => e as String).toList(),
      );
  Map<String, dynamic> toJson() => {
        'name': name,
        if (storeId != null) 'store_id': storeId,
        'charge_mode': chargeMode,
        if (isDefault != null) 'is_default': isDefault,
        'rules': rules.map((e) => e.toJson()).toList(),
        if (undeliverableRegionCodes != null) 'undeliverable_region_codes': undeliverableRegionCodes,
      };
}

/// 一个地点，坐标 WGS-84。字段与收货地址对齐：adcode 即地址的 region_code（运费按它算）。
class GeoPlace {
  final String name;
  final String address;
  final String province;
  final String city;
  final String district;
  final String adcode;
  final String street;
  final double lat;
  final double lng;
  const GeoPlace({required this.name, required this.address, required this.province, required this.city, required this.district, required this.adcode, required this.street, required this.lat, required this.lng});
  factory GeoPlace.fromJson(Map<String, dynamic> j) => GeoPlace(
        name: j['name'] as String,
        address: j['address'] as String,
        province: j['province'] as String,
        city: j['city'] as String,
        district: j['district'] as String,
        adcode: j['adcode'] as String,
        street: j['street'] as String,
        lat: (j['lat'] as num).toDouble(),
        lng: (j['lng'] as num).toDouble(),
      );
  Map<String, dynamic> toJson() => {
        'name': name,
        'address': address,
        'province': province,
        'city': city,
        'district': district,
        'adcode': adcode,
        'street': street,
        'lat': lat,
        'lng': lng,
      };
}

/// GeoJSON Polygon，SRID 固定 4326。落库成 `GEOGRAPHY(POLYGON, 4326)`。
class GeoPolygon {
  final String type;
  final List<List<List<double>>> coordinates;
  const GeoPolygon({required this.type, required this.coordinates});
  factory GeoPolygon.fromJson(Map<String, dynamic> j) => GeoPolygon(
        type: j['type'] as String,
        coordinates: (j['coordinates'] as List).map((e) => (e as List).map((e) => (e as List).map((e) => (e as num).toDouble()).toList()).toList()).toList(),
      );
  Map<String, dynamic> toJson() => {
        'type': type,
        'coordinates': coordinates,
      };
}

/// 第三方身份来源，对应 `user_identities.provider`：
typedef IdentityProvider = int;

/// 相对调整。只有一个必填字段：加减多少。**不带「我看到的那个值」**——
class InventoryAdjustRequest {
  final int delta;
  final String? reason;
  const InventoryAdjustRequest({required this.delta, this.reason});
  factory InventoryAdjustRequest.fromJson(Map<String, dynamic> j) => InventoryAdjustRequest(
        delta: (j['delta'] as num).toInt(),
        reason: j['reason'] as String?,
      );
  Map<String, dynamic> toJson() => {
        'delta': delta,
        if (reason != null) 'reason': reason,
      };
}

class InventoryConflict {
  final String type;
  final String title;
  final int status;
  final String? detail;
  final String? instance;
  final String? traceId;
  final List<FieldError>? errors;
  final List<FreightUndeliverableLine>? undeliverableItems;
  final AdminInventory current;
  const InventoryConflict({required this.type, required this.title, required this.status, this.detail, this.instance, this.traceId, this.errors, this.undeliverableItems, required this.current});
  factory InventoryConflict.fromJson(Map<String, dynamic> j) => InventoryConflict(
        type: j['type'] as String,
        title: j['title'] as String,
        status: (j['status'] as num).toInt(),
        detail: j['detail'] as String?,
        instance: j['instance'] as String?,
        traceId: j['trace_id'] as String?,
        errors: (j['errors'] as List?)?.map((e) => FieldError.fromJson(e as Map<String, dynamic>)).toList(),
        undeliverableItems: (j['undeliverable_items'] as List?)?.map((e) => FreightUndeliverableLine.fromJson(e as Map<String, dynamic>)).toList(),
        current: AdminInventory.fromJson(j['current'] as Map<String, dynamic>),
      );
  Map<String, dynamic> toJson() => {
        'type': type,
        'title': title,
        'status': status,
        if (detail != null) 'detail': detail,
        if (instance != null) 'instance': instance,
        if (traceId != null) 'trace_id': traceId,
        if (errors != null) 'errors': errors!.map((e) => e.toJson()).toList(),
        if (undeliverableItems != null) 'undeliverable_items': undeliverableItems!.map((e) => e.toJson()).toList(),
        'current': current.toJson(),
      };
}

/// 比较并设置。两个数量都是必填，缺一不可——只给 `available_qty` 就退化成
class InventorySetRequest {
  final int expectedAvailableQty;
  final int availableQty;
  final int? warningQty;
  const InventorySetRequest({required this.expectedAvailableQty, required this.availableQty, this.warningQty});
  factory InventorySetRequest.fromJson(Map<String, dynamic> j) => InventorySetRequest(
        expectedAvailableQty: (j['expected_available_qty'] as num).toInt(),
        availableQty: (j['available_qty'] as num).toInt(),
        warningQty: (j['warning_qty'] as num?)?.toInt(),
      );
  Map<String, dynamic> toJson() => {
        'expected_available_qty': expectedAvailableQty,
        'available_qty': availableQty,
        if (warningQty != null) 'warning_qty': warningQty,
      };
}

class LocalDeliveryTemplate {
  final Money minOrderCents;
  final Money freeOverCents;
  final List<DeliveryTier> feeTiers;
  final int id;
  final String name;
  final bool isDefault;
  final int? storeCount;
  final String createdAt;
  final String updatedAt;
  const LocalDeliveryTemplate({required this.minOrderCents, required this.freeOverCents, required this.feeTiers, required this.id, required this.name, required this.isDefault, this.storeCount, required this.createdAt, required this.updatedAt});
  factory LocalDeliveryTemplate.fromJson(Map<String, dynamic> j) => LocalDeliveryTemplate(
        minOrderCents: (j['min_order_cents'] as num).toInt(),
        freeOverCents: (j['free_over_cents'] as num).toInt(),
        feeTiers: (j['fee_tiers'] as List).map((e) => DeliveryTier.fromJson(e as Map<String, dynamic>)).toList(),
        id: (j['id'] as num).toInt(),
        name: j['name'] as String,
        isDefault: j['is_default'] as bool,
        storeCount: (j['store_count'] as num?)?.toInt(),
        createdAt: j['created_at'] as String,
        updatedAt: j['updated_at'] as String,
      );
  Map<String, dynamic> toJson() => {
        'min_order_cents': minOrderCents,
        'free_over_cents': freeOverCents,
        'fee_tiers': feeTiers.map((e) => e.toJson()).toList(),
        'id': id,
        'name': name,
        'is_default': isDefault,
        if (storeCount != null) 'store_count': storeCount,
        'created_at': createdAt,
        'updated_at': updatedAt,
      };
}

class LocalDeliveryTemplateInput {
  final Money minOrderCents;
  final Money freeOverCents;
  final List<DeliveryTier> feeTiers;
  final String name;
  final bool isDefault;
  const LocalDeliveryTemplateInput({required this.minOrderCents, required this.freeOverCents, required this.feeTiers, required this.name, required this.isDefault});
  factory LocalDeliveryTemplateInput.fromJson(Map<String, dynamic> j) => LocalDeliveryTemplateInput(
        minOrderCents: (j['min_order_cents'] as num).toInt(),
        freeOverCents: (j['free_over_cents'] as num).toInt(),
        feeTiers: (j['fee_tiers'] as List).map((e) => DeliveryTier.fromJson(e as Map<String, dynamic>)).toList(),
        name: j['name'] as String,
        isDefault: j['is_default'] as bool,
      );
  Map<String, dynamic> toJson() => {
        'min_order_cents': minOrderCents,
        'free_over_cents': freeOverCents,
        'fee_tiers': feeTiers.map((e) => e.toJson()).toList(),
        'name': name,
        'is_default': isDefault,
      };
}

class User {
  final int id;
  final String nickname;
  final String? avatarUrl;
  final String? phone;
  final int? gender;
  final bool? hasPassword;
  final String? createdAt;
  final String? lastLoginAt;
  const User({required this.id, required this.nickname, this.avatarUrl, this.phone, this.gender, this.hasPassword, this.createdAt, this.lastLoginAt});
  factory User.fromJson(Map<String, dynamic> j) => User(
        id: (j['id'] as num).toInt(),
        nickname: j['nickname'] as String,
        avatarUrl: j['avatar_url'] as String?,
        phone: j['phone'] as String?,
        gender: (j['gender'] as num?)?.toInt(),
        hasPassword: j['has_password'] as bool?,
        createdAt: j['created_at'] as String?,
        lastLoginAt: j['last_login_at'] as String?,
      );
  Map<String, dynamic> toJson() => {
        'id': id,
        'nickname': nickname,
        if (avatarUrl != null) 'avatar_url': avatarUrl,
        if (phone != null) 'phone': phone,
        if (gender != null) 'gender': gender,
        if (hasPassword != null) 'has_password': hasPassword,
        if (createdAt != null) 'created_at': createdAt,
        if (lastLoginAt != null) 'last_login_at': lastLoginAt,
      };
}

class LoginResponse {
  final String accessToken;
  final String? refreshToken;
  final String tokenType;
  final int expiresIn;
  final User user;
  final bool? isNewUser;
  const LoginResponse({required this.accessToken, this.refreshToken, required this.tokenType, required this.expiresIn, required this.user, this.isNewUser});
  factory LoginResponse.fromJson(Map<String, dynamic> j) => LoginResponse(
        accessToken: j['access_token'] as String,
        refreshToken: j['refresh_token'] as String?,
        tokenType: j['token_type'] as String,
        expiresIn: (j['expires_in'] as num).toInt(),
        user: User.fromJson(j['user'] as Map<String, dynamic>),
        isNewUser: j['is_new_user'] as bool?,
      );
  Map<String, dynamic> toJson() => {
        'access_token': accessToken,
        if (refreshToken != null) 'refresh_token': refreshToken,
        'token_type': tokenType,
        'expires_in': expiresIn,
        'user': user.toJson(),
        if (isNewUser != null) 'is_new_user': isNewUser,
      };
}

class Merchant {
  final int id;
  final String code;
  final String name;
  final int status;
  final String? domain;
  final String createdAt;
  final String? updatedAt;
  const Merchant({required this.id, required this.code, required this.name, required this.status, this.domain, required this.createdAt, this.updatedAt});
  factory Merchant.fromJson(Map<String, dynamic> j) => Merchant(
        id: (j['id'] as num).toInt(),
        code: j['code'] as String,
        name: j['name'] as String,
        status: (j['status'] as num).toInt(),
        domain: j['domain'] as String?,
        createdAt: j['created_at'] as String,
        updatedAt: j['updated_at'] as String?,
      );
  Map<String, dynamic> toJson() => {
        'id': id,
        'code': code,
        'name': name,
        'status': status,
        if (domain != null) 'domain': domain,
        'created_at': createdAt,
        if (updatedAt != null) 'updated_at': updatedAt,
      };
}

class MerchantCreateRequest {
  final String code;
  final String name;
  final String adminEmail;
  const MerchantCreateRequest({required this.code, required this.name, required this.adminEmail});
  factory MerchantCreateRequest.fromJson(Map<String, dynamic> j) => MerchantCreateRequest(
        code: j['code'] as String,
        name: j['name'] as String,
        adminEmail: j['admin_email'] as String,
      );
  Map<String, dynamic> toJson() => {
        'code': code,
        'name': name,
        'admin_email': adminEmail,
      };
}

class MerchantList {
  final int page;
  final int pageSize;
  final int total;
  final List<Merchant> items;
  final bool singleMerchantMode;
  const MerchantList({required this.page, required this.pageSize, required this.total, required this.items, required this.singleMerchantMode});
  factory MerchantList.fromJson(Map<String, dynamic> j) => MerchantList(
        page: (j['page'] as num).toInt(),
        pageSize: (j['page_size'] as num).toInt(),
        total: (j['total'] as num).toInt(),
        items: (j['items'] as List).map((e) => Merchant.fromJson(e as Map<String, dynamic>)).toList(),
        singleMerchantMode: j['single_merchant_mode'] as bool,
      );
  Map<String, dynamic> toJson() => {
        'page': page,
        'page_size': pageSize,
        'total': total,
        'items': items.map((e) => e.toJson()).toList(),
        'single_merchant_mode': singleMerchantMode,
      };
}

class MerchantUpdateRequest {
  final String? name;
  final int? status;
  const MerchantUpdateRequest({this.name, this.status});
  factory MerchantUpdateRequest.fromJson(Map<String, dynamic> j) => MerchantUpdateRequest(
        name: j['name'] as String?,
        status: (j['status'] as num?)?.toInt(),
      );
  Map<String, dynamic> toJson() => {
        if (name != null) 'name': name,
        if (status != null) 'status': status,
      };
}

/// 通知的种类。**文案不要按它拼**（服务端已渲染好 `title` / `body`），
typedef NotificationKind = String;

/// 点了这条通知跳到哪里。四个定位字段都一定出现，用不上的是 `null`：
class NotificationTarget {
  final String type;
  final String? orderNo;
  final String? refundNo;
  final int? storeId;
  final int? skuId;
  const NotificationTarget({required this.type, this.orderNo, this.refundNo, this.storeId, this.skuId});
  factory NotificationTarget.fromJson(Map<String, dynamic> j) => NotificationTarget(
        type: j['type'] as String,
        orderNo: j['order_no'] as String?,
        refundNo: j['refund_no'] as String?,
        storeId: (j['store_id'] as num?)?.toInt(),
        skuId: (j['sku_id'] as num?)?.toInt(),
      );
  Map<String, dynamic> toJson() => {
        'type': type,
        'order_no': orderNo,
        'refund_no': refundNo,
        'store_id': storeId,
        'sku_id': skuId,
      };
}

class Notification {
  final int id;
  final NotificationKind kind;
  final String title;
  final String body;
  final NotificationTarget target;
  final String? readAt;
  final String createdAt;
  const Notification({required this.id, required this.kind, required this.title, required this.body, required this.target, this.readAt, required this.createdAt});
  factory Notification.fromJson(Map<String, dynamic> j) => Notification(
        id: (j['id'] as num).toInt(),
        kind: j['kind'] as String,
        title: j['title'] as String,
        body: j['body'] as String,
        target: NotificationTarget.fromJson(j['target'] as Map<String, dynamic>),
        readAt: j['read_at'] as String?,
        createdAt: j['created_at'] as String,
      );
  Map<String, dynamic> toJson() => {
        'id': id,
        'kind': kind,
        'title': title,
        'body': body,
        'target': target.toJson(),
        'read_at': readAt,
        'created_at': createdAt,
      };
}

class NotificationList {
  final int page;
  final int pageSize;
  final int total;
  final List<Notification> items;
  final int unreadCount;
  const NotificationList({required this.page, required this.pageSize, required this.total, required this.items, required this.unreadCount});
  factory NotificationList.fromJson(Map<String, dynamic> j) => NotificationList(
        page: (j['page'] as num).toInt(),
        pageSize: (j['page_size'] as num).toInt(),
        total: (j['total'] as num).toInt(),
        items: (j['items'] as List).map((e) => Notification.fromJson(e as Map<String, dynamic>)).toList(),
        unreadCount: (j['unread_count'] as num).toInt(),
      );
  Map<String, dynamic> toJson() => {
        'page': page,
        'page_size': pageSize,
        'total': total,
        'items': items.map((e) => e.toJson()).toList(),
        'unread_count': unreadCount,
      };
}

class NotificationUnreadCount {
  final int unreadCount;
  const NotificationUnreadCount({required this.unreadCount});
  factory NotificationUnreadCount.fromJson(Map<String, dynamic> j) => NotificationUnreadCount(
        unreadCount: (j['unread_count'] as num).toInt(),
      );
  Map<String, dynamic> toJson() => {
        'unread_count': unreadCount,
      };
}

class Order {
  final String orderNo;
  final int storeId;
  final int? regionId;
  final OrderStatus status;
  final OrderRefundStatus refundStatus;
  final Money? goodsAmountCents;
  final Money? freightCents;
  final Money? freightDiscountCents;
  final Money? discountCents;
  final int? userCouponId;
  final String? couponName;
  final Money? promotionDiscountCents;
  final List<OrderPromotion>? promotions;
  final Money payableCents;
  final Money? paidCents;
  final Money? refundedCents;
  final String? expireAt;
  final String createdAt;
  final String? paidAt;
  final String? shippedAt;
  final String? finishedAt;
  const Order({required this.orderNo, required this.storeId, this.regionId, required this.status, required this.refundStatus, this.goodsAmountCents, this.freightCents, this.freightDiscountCents, this.discountCents, this.userCouponId, this.couponName, this.promotionDiscountCents, this.promotions, required this.payableCents, this.paidCents, this.refundedCents, this.expireAt, required this.createdAt, this.paidAt, this.shippedAt, this.finishedAt});
  factory Order.fromJson(Map<String, dynamic> j) => Order(
        orderNo: j['order_no'] as String,
        storeId: (j['store_id'] as num).toInt(),
        regionId: (j['region_id'] as num?)?.toInt(),
        status: (j['status'] as num).toInt(),
        refundStatus: (j['refund_status'] as num).toInt(),
        goodsAmountCents: (j['goods_amount_cents'] as num?)?.toInt(),
        freightCents: (j['freight_cents'] as num?)?.toInt(),
        freightDiscountCents: (j['freight_discount_cents'] as num?)?.toInt(),
        discountCents: (j['discount_cents'] as num?)?.toInt(),
        userCouponId: (j['user_coupon_id'] as num?)?.toInt(),
        couponName: j['coupon_name'] as String?,
        promotionDiscountCents: (j['promotion_discount_cents'] as num?)?.toInt(),
        promotions: (j['promotions'] as List?)?.map((e) => OrderPromotion.fromJson(e as Map<String, dynamic>)).toList(),
        payableCents: (j['payable_cents'] as num).toInt(),
        paidCents: (j['paid_cents'] as num?)?.toInt(),
        refundedCents: (j['refunded_cents'] as num?)?.toInt(),
        expireAt: j['expire_at'] as String?,
        createdAt: j['created_at'] as String,
        paidAt: j['paid_at'] as String?,
        shippedAt: j['shipped_at'] as String?,
        finishedAt: j['finished_at'] as String?,
      );
  Map<String, dynamic> toJson() => {
        'order_no': orderNo,
        'store_id': storeId,
        if (regionId != null) 'region_id': regionId,
        'status': status,
        'refund_status': refundStatus,
        if (goodsAmountCents != null) 'goods_amount_cents': goodsAmountCents,
        if (freightCents != null) 'freight_cents': freightCents,
        if (freightDiscountCents != null) 'freight_discount_cents': freightDiscountCents,
        if (discountCents != null) 'discount_cents': discountCents,
        if (userCouponId != null) 'user_coupon_id': userCouponId,
        if (couponName != null) 'coupon_name': couponName,
        if (promotionDiscountCents != null) 'promotion_discount_cents': promotionDiscountCents,
        if (promotions != null) 'promotions': promotions!.map((e) => e.toJson()).toList(),
        'payable_cents': payableCents,
        if (paidCents != null) 'paid_cents': paidCents,
        if (refundedCents != null) 'refunded_cents': refundedCents,
        if (expireAt != null) 'expire_at': expireAt,
        'created_at': createdAt,
        if (paidAt != null) 'paid_at': paidAt,
        if (shippedAt != null) 'shipped_at': shippedAt,
        if (finishedAt != null) 'finished_at': finishedAt,
      };
}

class OrderCreateRequest {
  final List<OrderItemInput> items;
  final int storeId;
  final int addressId;
  final int? userCouponId;
  final String? remark;
  final Money? expectedPayableCents;
  const OrderCreateRequest({required this.items, required this.storeId, required this.addressId, this.userCouponId, this.remark, this.expectedPayableCents});
  factory OrderCreateRequest.fromJson(Map<String, dynamic> j) => OrderCreateRequest(
        items: (j['items'] as List).map((e) => OrderItemInput.fromJson(e as Map<String, dynamic>)).toList(),
        storeId: (j['store_id'] as num).toInt(),
        addressId: (j['address_id'] as num).toInt(),
        userCouponId: (j['user_coupon_id'] as num?)?.toInt(),
        remark: j['remark'] as String?,
        expectedPayableCents: (j['expected_payable_cents'] as num?)?.toInt(),
      );
  Map<String, dynamic> toJson() => {
        'items': items.map((e) => e.toJson()).toList(),
        'store_id': storeId,
        'address_id': addressId,
        if (userCouponId != null) 'user_coupon_id': userCouponId,
        if (remark != null) 'remark': remark,
        if (expectedPayableCents != null) 'expected_payable_cents': expectedPayableCents,
      };
}

class Refund {
  final String refundNo;
  final String orderNo;
  final String? paymentNo;
  final RefundType refundType;
  final RefundStatus status;
  final List<RefundItem> items;
  final Money? goodsAmountCents;
  final Money? freightCents;
  final Money amountCents;
  final String? channel;
  final String? channelRefundId;
  final RefundReasonCode? reasonCode;
  final String? reasonText;
  final List<String>? evidenceUrls;
  final ReturnShipment? returnShipment;
  final String? rejectReason;
  final String? auditedAt;
  final String? returnDeadlineAt;
  final String? refundedAt;
  final String createdAt;
  final String? updatedAt;
  const Refund({required this.refundNo, required this.orderNo, this.paymentNo, required this.refundType, required this.status, required this.items, this.goodsAmountCents, this.freightCents, required this.amountCents, this.channel, this.channelRefundId, this.reasonCode, this.reasonText, this.evidenceUrls, this.returnShipment, this.rejectReason, this.auditedAt, this.returnDeadlineAt, this.refundedAt, required this.createdAt, this.updatedAt});
  factory Refund.fromJson(Map<String, dynamic> j) => Refund(
        refundNo: j['refund_no'] as String,
        orderNo: j['order_no'] as String,
        paymentNo: j['payment_no'] as String?,
        refundType: (j['refund_type'] as num).toInt(),
        status: (j['status'] as num).toInt(),
        items: (j['items'] as List).map((e) => RefundItem.fromJson(e as Map<String, dynamic>)).toList(),
        goodsAmountCents: (j['goods_amount_cents'] as num?)?.toInt(),
        freightCents: (j['freight_cents'] as num?)?.toInt(),
        amountCents: (j['amount_cents'] as num).toInt(),
        channel: j['channel'] as String?,
        channelRefundId: j['channel_refund_id'] as String?,
        reasonCode: (j['reason_code'] as num?)?.toInt(),
        reasonText: j['reason_text'] as String?,
        evidenceUrls: (j['evidence_urls'] as List?)?.map((e) => e as String).toList(),
        returnShipment: j['return_shipment'] == null ? null : ReturnShipment.fromJson(j['return_shipment'] as Map<String, dynamic>),
        rejectReason: j['reject_reason'] as String?,
        auditedAt: j['audited_at'] as String?,
        returnDeadlineAt: j['return_deadline_at'] as String?,
        refundedAt: j['refunded_at'] as String?,
        createdAt: j['created_at'] as String,
        updatedAt: j['updated_at'] as String?,
      );
  Map<String, dynamic> toJson() => {
        'refund_no': refundNo,
        'order_no': orderNo,
        if (paymentNo != null) 'payment_no': paymentNo,
        'refund_type': refundType,
        'status': status,
        'items': items.map((e) => e.toJson()).toList(),
        if (goodsAmountCents != null) 'goods_amount_cents': goodsAmountCents,
        if (freightCents != null) 'freight_cents': freightCents,
        'amount_cents': amountCents,
        if (channel != null) 'channel': channel,
        if (channelRefundId != null) 'channel_refund_id': channelRefundId,
        if (reasonCode != null) 'reason_code': reasonCode,
        if (reasonText != null) 'reason_text': reasonText,
        if (evidenceUrls != null) 'evidence_urls': evidenceUrls,
        if (returnShipment != null) 'return_shipment': returnShipment!.toJson(),
        if (rejectReason != null) 'reject_reason': rejectReason,
        if (auditedAt != null) 'audited_at': auditedAt,
        if (returnDeadlineAt != null) 'return_deadline_at': returnDeadlineAt,
        if (refundedAt != null) 'refunded_at': refundedAt,
        'created_at': createdAt,
        if (updatedAt != null) 'updated_at': updatedAt,
      };
}

class OrderDetail {
  final String orderNo;
  final int storeId;
  final int? regionId;
  final OrderStatus status;
  final OrderRefundStatus refundStatus;
  final Money? goodsAmountCents;
  final Money? freightCents;
  final Money? freightDiscountCents;
  final Money? discountCents;
  final int? userCouponId;
  final String? couponName;
  final Money? promotionDiscountCents;
  final List<OrderPromotion>? promotions;
  final Money payableCents;
  final Money? paidCents;
  final Money? refundedCents;
  final String? expireAt;
  final String createdAt;
  final String? paidAt;
  final String? shippedAt;
  final String? finishedAt;
  final ReceiverSnapshot? receiver;
  final OrderStoreSnapshot? store;
  final FreightBreakdown? freight;
  final List<OrderItem>? items;
  final List<PaymentRecord>? payments;
  final List<Refund>? refunds;
  final String? autoConfirmAt;
  const OrderDetail({required this.orderNo, required this.storeId, this.regionId, required this.status, required this.refundStatus, this.goodsAmountCents, this.freightCents, this.freightDiscountCents, this.discountCents, this.userCouponId, this.couponName, this.promotionDiscountCents, this.promotions, required this.payableCents, this.paidCents, this.refundedCents, this.expireAt, required this.createdAt, this.paidAt, this.shippedAt, this.finishedAt, this.receiver, this.store, this.freight, this.items, this.payments, this.refunds, this.autoConfirmAt});
  factory OrderDetail.fromJson(Map<String, dynamic> j) => OrderDetail(
        orderNo: j['order_no'] as String,
        storeId: (j['store_id'] as num).toInt(),
        regionId: (j['region_id'] as num?)?.toInt(),
        status: (j['status'] as num).toInt(),
        refundStatus: (j['refund_status'] as num).toInt(),
        goodsAmountCents: (j['goods_amount_cents'] as num?)?.toInt(),
        freightCents: (j['freight_cents'] as num?)?.toInt(),
        freightDiscountCents: (j['freight_discount_cents'] as num?)?.toInt(),
        discountCents: (j['discount_cents'] as num?)?.toInt(),
        userCouponId: (j['user_coupon_id'] as num?)?.toInt(),
        couponName: j['coupon_name'] as String?,
        promotionDiscountCents: (j['promotion_discount_cents'] as num?)?.toInt(),
        promotions: (j['promotions'] as List?)?.map((e) => OrderPromotion.fromJson(e as Map<String, dynamic>)).toList(),
        payableCents: (j['payable_cents'] as num).toInt(),
        paidCents: (j['paid_cents'] as num?)?.toInt(),
        refundedCents: (j['refunded_cents'] as num?)?.toInt(),
        expireAt: j['expire_at'] as String?,
        createdAt: j['created_at'] as String,
        paidAt: j['paid_at'] as String?,
        shippedAt: j['shipped_at'] as String?,
        finishedAt: j['finished_at'] as String?,
        receiver: j['receiver'] == null ? null : ReceiverSnapshot.fromJson(j['receiver'] as Map<String, dynamic>),
        store: j['store'] == null ? null : OrderStoreSnapshot.fromJson(j['store'] as Map<String, dynamic>),
        freight: j['freight'] == null ? null : FreightBreakdown.fromJson(j['freight'] as Map<String, dynamic>),
        items: (j['items'] as List?)?.map((e) => OrderItem.fromJson(e as Map<String, dynamic>)).toList(),
        payments: (j['payments'] as List?)?.map((e) => PaymentRecord.fromJson(e as Map<String, dynamic>)).toList(),
        refunds: (j['refunds'] as List?)?.map((e) => Refund.fromJson(e as Map<String, dynamic>)).toList(),
        autoConfirmAt: j['auto_confirm_at'] as String?,
      );
  Map<String, dynamic> toJson() => {
        'order_no': orderNo,
        'store_id': storeId,
        if (regionId != null) 'region_id': regionId,
        'status': status,
        'refund_status': refundStatus,
        if (goodsAmountCents != null) 'goods_amount_cents': goodsAmountCents,
        if (freightCents != null) 'freight_cents': freightCents,
        if (freightDiscountCents != null) 'freight_discount_cents': freightDiscountCents,
        if (discountCents != null) 'discount_cents': discountCents,
        if (userCouponId != null) 'user_coupon_id': userCouponId,
        if (couponName != null) 'coupon_name': couponName,
        if (promotionDiscountCents != null) 'promotion_discount_cents': promotionDiscountCents,
        if (promotions != null) 'promotions': promotions!.map((e) => e.toJson()).toList(),
        'payable_cents': payableCents,
        if (paidCents != null) 'paid_cents': paidCents,
        if (refundedCents != null) 'refunded_cents': refundedCents,
        if (expireAt != null) 'expire_at': expireAt,
        'created_at': createdAt,
        if (paidAt != null) 'paid_at': paidAt,
        if (shippedAt != null) 'shipped_at': shippedAt,
        if (finishedAt != null) 'finished_at': finishedAt,
        if (receiver != null) 'receiver': receiver!.toJson(),
        if (store != null) 'store': store!.toJson(),
        if (freight != null) 'freight': freight!.toJson(),
        if (items != null) 'items': items!.map((e) => e.toJson()).toList(),
        if (payments != null) 'payments': payments!.map((e) => e.toJson()).toList(),
        if (refunds != null) 'refunds': refunds!.map((e) => e.toJson()).toList(),
        if (autoConfirmAt != null) 'auto_confirm_at': autoConfirmAt,
      };
}

/// 试算的一行。**本轮从内联 schema 提成了具名类型**（数据模型 §15 记过的那笔债）：
class OrderPreviewItem {
  final int skuId;
  final int quantity;
  final Money priceCents;
  final Money listPriceCents;
  final int? pricePromotionId;
  final Money amountCents;
  final Money discountCents;
  final Money promotionDiscountCents;
  const OrderPreviewItem({required this.skuId, required this.quantity, required this.priceCents, required this.listPriceCents, this.pricePromotionId, required this.amountCents, required this.discountCents, required this.promotionDiscountCents});
  factory OrderPreviewItem.fromJson(Map<String, dynamic> j) => OrderPreviewItem(
        skuId: (j['sku_id'] as num).toInt(),
        quantity: (j['quantity'] as num).toInt(),
        priceCents: (j['price_cents'] as num).toInt(),
        listPriceCents: (j['list_price_cents'] as num).toInt(),
        pricePromotionId: (j['price_promotion_id'] as num?)?.toInt(),
        amountCents: (j['amount_cents'] as num).toInt(),
        discountCents: (j['discount_cents'] as num).toInt(),
        promotionDiscountCents: (j['promotion_discount_cents'] as num).toInt(),
      );
  Map<String, dynamic> toJson() => {
        'sku_id': skuId,
        'quantity': quantity,
        'price_cents': priceCents,
        'list_price_cents': listPriceCents,
        if (pricePromotionId != null) 'price_promotion_id': pricePromotionId,
        'amount_cents': amountCents,
        'discount_cents': discountCents,
        'promotion_discount_cents': promotionDiscountCents,
      };
}

class OrderPreview {
  final int storeId;
  final int? regionId;
  final Money goodsAmountCents;
  final Money freightCents;
  final Money freightDiscountCents;
  final FreightBreakdown freight;
  final Money? discountCents;
  final Money payableCents;
  final Money promotionDiscountCents;
  final Money couponDiscountCents;
  final List<OrderPreviewItem> items;
  final List<PromotionHit> promotions;
  final int? userCouponId;
  final List<ApplicableCoupon>? applicableCoupons;
  const OrderPreview({required this.storeId, this.regionId, required this.goodsAmountCents, required this.freightCents, required this.freightDiscountCents, required this.freight, this.discountCents, required this.payableCents, required this.promotionDiscountCents, required this.couponDiscountCents, required this.items, required this.promotions, this.userCouponId, this.applicableCoupons});
  factory OrderPreview.fromJson(Map<String, dynamic> j) => OrderPreview(
        storeId: (j['store_id'] as num).toInt(),
        regionId: (j['region_id'] as num?)?.toInt(),
        goodsAmountCents: (j['goods_amount_cents'] as num).toInt(),
        freightCents: (j['freight_cents'] as num).toInt(),
        freightDiscountCents: (j['freight_discount_cents'] as num).toInt(),
        freight: FreightBreakdown.fromJson(j['freight'] as Map<String, dynamic>),
        discountCents: (j['discount_cents'] as num?)?.toInt(),
        payableCents: (j['payable_cents'] as num).toInt(),
        promotionDiscountCents: (j['promotion_discount_cents'] as num).toInt(),
        couponDiscountCents: (j['coupon_discount_cents'] as num).toInt(),
        items: (j['items'] as List).map((e) => OrderPreviewItem.fromJson(e as Map<String, dynamic>)).toList(),
        promotions: (j['promotions'] as List).map((e) => PromotionHit.fromJson(e as Map<String, dynamic>)).toList(),
        userCouponId: (j['user_coupon_id'] as num?)?.toInt(),
        applicableCoupons: (j['applicable_coupons'] as List?)?.map((e) => ApplicableCoupon.fromJson(e as Map<String, dynamic>)).toList(),
      );
  Map<String, dynamic> toJson() => {
        'store_id': storeId,
        if (regionId != null) 'region_id': regionId,
        'goods_amount_cents': goodsAmountCents,
        'freight_cents': freightCents,
        'freight_discount_cents': freightDiscountCents,
        'freight': freight.toJson(),
        if (discountCents != null) 'discount_cents': discountCents,
        'payable_cents': payableCents,
        'promotion_discount_cents': promotionDiscountCents,
        'coupon_discount_cents': couponDiscountCents,
        'items': items.map((e) => e.toJson()).toList(),
        'promotions': promotions.map((e) => e.toJson()).toList(),
        if (userCouponId != null) 'user_coupon_id': userCouponId,
        if (applicableCoupons != null) 'applicable_coupons': applicableCoupons!.map((e) => e.toJson()).toList(),
      };
}

class PageMeta {
  final int page;
  final int pageSize;
  final int total;
  const PageMeta({required this.page, required this.pageSize, required this.total});
  factory PageMeta.fromJson(Map<String, dynamic> j) => PageMeta(
        page: (j['page'] as num).toInt(),
        pageSize: (j['page_size'] as num).toInt(),
        total: (j['total'] as num).toInt(),
      );
  Map<String, dynamic> toJson() => {
        'page': page,
        'page_size': pageSize,
        'total': total,
      };
}

class PaymentCreateRequest {
  final String channel;
  const PaymentCreateRequest({required this.channel});
  factory PaymentCreateRequest.fromJson(Map<String, dynamic> j) => PaymentCreateRequest(
        channel: j['channel'] as String,
      );
  Map<String, dynamic> toJson() => {
        'channel': channel,
      };
}

class PaymentIntent {
  final String paymentNo;
  final String channel;
  final Money amountCents;
  final Map<String, dynamic>? payload;
  const PaymentIntent({required this.paymentNo, required this.channel, required this.amountCents, this.payload});
  factory PaymentIntent.fromJson(Map<String, dynamic> j) => PaymentIntent(
        paymentNo: j['payment_no'] as String,
        channel: j['channel'] as String,
        amountCents: (j['amount_cents'] as num).toInt(),
        payload: j['payload'] as Map<String, dynamic>?,
      );
  Map<String, dynamic> toJson() => {
        'payment_no': paymentNo,
        'channel': channel,
        'amount_cents': amountCents,
        if (payload != null) 'payload': payload,
      };
}

/// RFC 9457 Problem Details
class Problem {
  final String type;
  final String title;
  final int status;
  final String? detail;
  final String? instance;
  final String? traceId;
  final List<FieldError>? errors;
  final List<FreightUndeliverableLine>? undeliverableItems;
  const Problem({required this.type, required this.title, required this.status, this.detail, this.instance, this.traceId, this.errors, this.undeliverableItems});
  factory Problem.fromJson(Map<String, dynamic> j) => Problem(
        type: j['type'] as String,
        title: j['title'] as String,
        status: (j['status'] as num).toInt(),
        detail: j['detail'] as String?,
        instance: j['instance'] as String?,
        traceId: j['trace_id'] as String?,
        errors: (j['errors'] as List?)?.map((e) => FieldError.fromJson(e as Map<String, dynamic>)).toList(),
        undeliverableItems: (j['undeliverable_items'] as List?)?.map((e) => FreightUndeliverableLine.fromJson(e as Map<String, dynamic>)).toList(),
      );
  Map<String, dynamic> toJson() => {
        'type': type,
        'title': title,
        'status': status,
        if (detail != null) 'detail': detail,
        if (instance != null) 'instance': instance,
        if (traceId != null) 'trace_id': traceId,
        if (errors != null) 'errors': errors!.map((e) => e.toJson()).toList(),
        if (undeliverableItems != null) 'undeliverable_items': undeliverableItems!.map((e) => e.toJson()).toList(),
      };
}

/// **没有 `status` 也没有 `merchant_id`。** 前者因为创建与发布是两个动作，
class ProductCreateRequest {
  final int categoryId;
  final String title;
  final String? subtitle;
  final String? description;
  final int? brandId;
  final int? freightTemplateId;
  const ProductCreateRequest({required this.categoryId, required this.title, this.subtitle, this.description, this.brandId, this.freightTemplateId});
  factory ProductCreateRequest.fromJson(Map<String, dynamic> j) => ProductCreateRequest(
        categoryId: (j['category_id'] as num).toInt(),
        title: j['title'] as String,
        subtitle: j['subtitle'] as String?,
        description: j['description'] as String?,
        brandId: (j['brand_id'] as num?)?.toInt(),
        freightTemplateId: (j['freight_template_id'] as num?)?.toInt(),
      );
  Map<String, dynamic> toJson() => {
        'category_id': categoryId,
        'title': title,
        if (subtitle != null) 'subtitle': subtitle,
        if (description != null) 'description': description,
        if (brandId != null) 'brand_id': brandId,
        if (freightTemplateId != null) 'freight_template_id': freightTemplateId,
      };
}

class Sku {
  final int id;
  final String skuCode;
  final Map<String, String>? specValues;
  final Money priceCents;
  final int availableQty;
  final String? imageUrl;
  final Money? promoPriceCents;
  final int? promotionId;
  const Sku({required this.id, required this.skuCode, this.specValues, required this.priceCents, required this.availableQty, this.imageUrl, this.promoPriceCents, this.promotionId});
  factory Sku.fromJson(Map<String, dynamic> j) => Sku(
        id: (j['id'] as num).toInt(),
        skuCode: j['sku_code'] as String,
        specValues: (j['spec_values'] as Map?)?.map((k, e) => MapEntry(k as String, e as String)),
        priceCents: (j['price_cents'] as num).toInt(),
        availableQty: (j['available_qty'] as num).toInt(),
        imageUrl: j['image_url'] as String?,
        promoPriceCents: (j['promo_price_cents'] as num?)?.toInt(),
        promotionId: (j['promotion_id'] as num?)?.toInt(),
      );
  Map<String, dynamic> toJson() => {
        'id': id,
        'sku_code': skuCode,
        if (specValues != null) 'spec_values': specValues,
        'price_cents': priceCents,
        'available_qty': availableQty,
        if (imageUrl != null) 'image_url': imageUrl,
        if (promoPriceCents != null) 'promo_price_cents': promoPriceCents,
        if (promotionId != null) 'promotion_id': promotionId,
      };
}

class ProductDetail {
  final int id;
  final String title;
  final String? subtitle;
  final String? imageUrl;
  final Money minPriceCents;
  final Money? maxPriceCents;
  final bool? inStock;
  final int? salesCount;
  final int status;
  final List<PromotionTag>? promotionTags;
  final String? description;
  final int? categoryId;
  final List<String>? images;
  final List<Sku> skus;
  const ProductDetail({required this.id, required this.title, this.subtitle, this.imageUrl, required this.minPriceCents, this.maxPriceCents, this.inStock, this.salesCount, required this.status, this.promotionTags, this.description, this.categoryId, this.images, required this.skus});
  factory ProductDetail.fromJson(Map<String, dynamic> j) => ProductDetail(
        id: (j['id'] as num).toInt(),
        title: j['title'] as String,
        subtitle: j['subtitle'] as String?,
        imageUrl: j['image_url'] as String?,
        minPriceCents: (j['min_price_cents'] as num).toInt(),
        maxPriceCents: (j['max_price_cents'] as num?)?.toInt(),
        inStock: j['in_stock'] as bool?,
        salesCount: (j['sales_count'] as num?)?.toInt(),
        status: (j['status'] as num).toInt(),
        promotionTags: (j['promotion_tags'] as List?)?.map((e) => PromotionTag.fromJson(e as Map<String, dynamic>)).toList(),
        description: j['description'] as String?,
        categoryId: (j['category_id'] as num?)?.toInt(),
        images: (j['images'] as List?)?.map((e) => e as String).toList(),
        skus: (j['skus'] as List).map((e) => Sku.fromJson(e as Map<String, dynamic>)).toList(),
      );
  Map<String, dynamic> toJson() => {
        'id': id,
        'title': title,
        if (subtitle != null) 'subtitle': subtitle,
        if (imageUrl != null) 'image_url': imageUrl,
        'min_price_cents': minPriceCents,
        if (maxPriceCents != null) 'max_price_cents': maxPriceCents,
        if (inStock != null) 'in_stock': inStock,
        if (salesCount != null) 'sales_count': salesCount,
        'status': status,
        if (promotionTags != null) 'promotion_tags': promotionTags!.map((e) => e.toJson()).toList(),
        if (description != null) 'description': description,
        if (categoryId != null) 'category_id': categoryId,
        if (images != null) 'images': images,
        'skus': skus.map((e) => e.toJson()).toList(),
      };
}

/// 只收 `upload_id`，不收 URL。收 URL 意味着客户端能往商品上挂任意地址，
class ProductImageInput {
  final int uploadId;
  const ProductImageInput({required this.uploadId});
  factory ProductImageInput.fromJson(Map<String, dynamic> j) => ProductImageInput(
        uploadId: (j['upload_id'] as num).toInt(),
      );
  Map<String, dynamic> toJson() => {
        'upload_id': uploadId,
      };
}

class ProductImagesReplaceRequest {
  final List<ProductImageInput> images;
  const ProductImagesReplaceRequest({required this.images});
  factory ProductImagesReplaceRequest.fromJson(Map<String, dynamic> j) => ProductImagesReplaceRequest(
        images: (j['images'] as List).map((e) => ProductImageInput.fromJson(e as Map<String, dynamic>)).toList(),
      );
  Map<String, dynamic> toJson() => {
        'images': images.map((e) => e.toJson()).toList(),
      };
}

class ProductImportCategoryCandidate {
  final int categoryId;
  final String pathName;
  final double score;
  const ProductImportCategoryCandidate({required this.categoryId, required this.pathName, required this.score});
  factory ProductImportCategoryCandidate.fromJson(Map<String, dynamic> j) => ProductImportCategoryCandidate(
        categoryId: (j['category_id'] as num).toInt(),
        pathName: j['path_name'] as String,
        score: (j['score'] as num).toDouble(),
      );
  Map<String, dynamic> toJson() => {
        'category_id': categoryId,
        'path_name': pathName,
        'score': score,
      };
}

class ProductImportCategoryChoice {
  final int firstRow;
  final int categoryId;
  const ProductImportCategoryChoice({required this.firstRow, required this.categoryId});
  factory ProductImportCategoryChoice.fromJson(Map<String, dynamic> j) => ProductImportCategoryChoice(
        firstRow: (j['first_row'] as num).toInt(),
        categoryId: (j['category_id'] as num).toInt(),
      );
  Map<String, dynamic> toJson() => {
        'first_row': firstRow,
        'category_id': categoryId,
      };
}

/// 一件商品的类目怎么定。
class ProductImportCategoryDecision {
  final String status;
  final int? categoryId;
  final String? pathName;
  final List<ProductImportCategoryCandidate> candidates;
  const ProductImportCategoryDecision({required this.status, this.categoryId, this.pathName, required this.candidates});
  factory ProductImportCategoryDecision.fromJson(Map<String, dynamic> j) => ProductImportCategoryDecision(
        status: j['status'] as String,
        categoryId: (j['category_id'] as num?)?.toInt(),
        pathName: j['path_name'] as String?,
        candidates: (j['candidates'] as List).map((e) => ProductImportCategoryCandidate.fromJson(e as Map<String, dynamic>)).toList(),
      );
  Map<String, dynamic> toJson() => {
        'status': status,
        if (categoryId != null) 'category_id': categoryId,
        if (pathName != null) 'path_name': pathName,
        'candidates': candidates.map((e) => e.toJson()).toList(),
      };
}

/// 导入文件格式。服务端按内容判断，不按文件名。
typedef ProductImportFormat = String;

/// 一行里的一条问题。`code` 给程序认，`message` 给人看。
class ProductImportIssue {
  final String? column;
  final String code;
  final String message;
  const ProductImportIssue({this.column, required this.code, required this.message});
  factory ProductImportIssue.fromJson(Map<String, dynamic> j) => ProductImportIssue(
        column: j['column'] as String?,
        code: j['code'] as String,
        message: j['message'] as String,
      );
  Map<String, dynamic> toJson() => {
        if (column != null) 'column': column,
        'code': code,
        'message': message,
      };
}

class ProductImportOutcome {
  final int firstRow;
  final List<int> rows;
  final String title;
  final String status;
  final int? productId;
  final int? skuCount;
  final int? categoryId;
  final List<String>? reasons;
  final List<String>? imageUrls;
  const ProductImportOutcome({required this.firstRow, required this.rows, required this.title, required this.status, this.productId, this.skuCount, this.categoryId, this.reasons, this.imageUrls});
  factory ProductImportOutcome.fromJson(Map<String, dynamic> j) => ProductImportOutcome(
        firstRow: (j['first_row'] as num).toInt(),
        rows: (j['rows'] as List).map((e) => (e as num).toInt()).toList(),
        title: j['title'] as String,
        status: j['status'] as String,
        productId: (j['product_id'] as num?)?.toInt(),
        skuCount: (j['sku_count'] as num?)?.toInt(),
        categoryId: (j['category_id'] as num?)?.toInt(),
        reasons: (j['reasons'] as List?)?.map((e) => e as String).toList(),
        imageUrls: (j['image_urls'] as List?)?.map((e) => e as String).toList(),
      );
  Map<String, dynamic> toJson() => {
        'first_row': firstRow,
        'rows': rows,
        'title': title,
        'status': status,
        if (productId != null) 'product_id': productId,
        if (skuCount != null) 'sku_count': skuCount,
        if (categoryId != null) 'category_id': categoryId,
        if (reasons != null) 'reasons': reasons,
        if (imageUrls != null) 'image_urls': imageUrls,
      };
}

/// 文件里的一件商品（同一标题的若干行）
class ProductImportProduct {
  final int firstRow;
  final List<int> rows;
  final String title;
  final bool importable;
  final ProductImportCategoryDecision category;
  const ProductImportProduct({required this.firstRow, required this.rows, required this.title, required this.importable, required this.category});
  factory ProductImportProduct.fromJson(Map<String, dynamic> j) => ProductImportProduct(
        firstRow: (j['first_row'] as num).toInt(),
        rows: (j['rows'] as List).map((e) => (e as num).toInt()).toList(),
        title: j['title'] as String,
        importable: j['importable'] as bool,
        category: ProductImportCategoryDecision.fromJson(j['category'] as Map<String, dynamic>),
      );
  Map<String, dynamic> toJson() => {
        'first_row': firstRow,
        'rows': rows,
        'title': title,
        'importable': importable,
        'category': category.toJson(),
      };
}

/// 文件里的一行（一个 SKU）。有错的格对应的字段缺席（比如价格写错了就没有 `price_cents`）。
class ProductImportRow {
  final int row;
  final int firstRow;
  final String? title;
  final String? subtitle;
  final String? category;
  final String? description;
  final String skuCode;
  final Map<String, String> specValues;
  final Money? priceCents;
  final int? stock;
  final int? weightGram;
  final List<String>? imageUrls;
  final List<ProductImportIssue> errors;
  final List<ProductImportIssue> warnings;
  final List<FieldError> violations;
  const ProductImportRow({required this.row, required this.firstRow, this.title, this.subtitle, this.category, this.description, required this.skuCode, required this.specValues, this.priceCents, this.stock, this.weightGram, this.imageUrls, required this.errors, required this.warnings, required this.violations});
  factory ProductImportRow.fromJson(Map<String, dynamic> j) => ProductImportRow(
        row: (j['row'] as num).toInt(),
        firstRow: (j['first_row'] as num).toInt(),
        title: j['title'] as String?,
        subtitle: j['subtitle'] as String?,
        category: j['category'] as String?,
        description: j['description'] as String?,
        skuCode: j['sku_code'] as String,
        specValues: (j['spec_values'] as Map).map((k, e) => MapEntry(k as String, e as String)),
        priceCents: (j['price_cents'] as num?)?.toInt(),
        stock: (j['stock'] as num?)?.toInt(),
        weightGram: (j['weight_gram'] as num?)?.toInt(),
        imageUrls: (j['image_urls'] as List?)?.map((e) => e as String).toList(),
        errors: (j['errors'] as List).map((e) => ProductImportIssue.fromJson(e as Map<String, dynamic>)).toList(),
        warnings: (j['warnings'] as List).map((e) => ProductImportIssue.fromJson(e as Map<String, dynamic>)).toList(),
        violations: (j['violations'] as List).map((e) => FieldError.fromJson(e as Map<String, dynamic>)).toList(),
      );
  Map<String, dynamic> toJson() => {
        'row': row,
        'first_row': firstRow,
        if (title != null) 'title': title,
        if (subtitle != null) 'subtitle': subtitle,
        if (category != null) 'category': category,
        if (description != null) 'description': description,
        'sku_code': skuCode,
        'spec_values': specValues,
        if (priceCents != null) 'price_cents': priceCents,
        if (stock != null) 'stock': stock,
        if (weightGram != null) 'weight_gram': weightGram,
        if (imageUrls != null) 'image_urls': imageUrls,
        'errors': errors.map((e) => e.toJson()).toList(),
        'warnings': warnings.map((e) => e.toJson()).toList(),
        'violations': violations.map((e) => e.toJson()).toList(),
      };
}

class ProductImportPreviewCategoryGate {
  final double minScore;
  final double minMargin;
  const ProductImportPreviewCategoryGate({required this.minScore, required this.minMargin});
  factory ProductImportPreviewCategoryGate.fromJson(Map<String, dynamic> j) => ProductImportPreviewCategoryGate(
        minScore: (j['min_score'] as num).toDouble(),
        minMargin: (j['min_margin'] as num).toDouble(),
      );
  Map<String, dynamic> toJson() => {
        'min_score': minScore,
        'min_margin': minMargin,
      };
}

class ProductImportPreviewPreviousImport {
  final int importId;
  final String createdAt;
  const ProductImportPreviewPreviousImport({required this.importId, required this.createdAt});
  factory ProductImportPreviewPreviousImport.fromJson(Map<String, dynamic> j) => ProductImportPreviewPreviousImport(
        importId: (j['import_id'] as num).toInt(),
        createdAt: j['created_at'] as String,
      );
  Map<String, dynamic> toJson() => {
        'import_id': importId,
        'created_at': createdAt,
      };
}

class ProductImportPreview {
  final String fileSha256;
  final ProductImportFormat format;
  final int totalRows;
  final int errorRows;
  final List<ProductImportProduct> products;
  final List<ProductImportRow> rows;
  final List<String> notices;
  final String categoryEngine;
  final ProductImportPreviewCategoryGate categoryGate;
  final ProductImportPreviewPreviousImport? previousImport;
  const ProductImportPreview({required this.fileSha256, required this.format, required this.totalRows, required this.errorRows, required this.products, required this.rows, required this.notices, required this.categoryEngine, required this.categoryGate, this.previousImport});
  factory ProductImportPreview.fromJson(Map<String, dynamic> j) => ProductImportPreview(
        fileSha256: j['file_sha256'] as String,
        format: j['format'] as String,
        totalRows: (j['total_rows'] as num).toInt(),
        errorRows: (j['error_rows'] as num).toInt(),
        products: (j['products'] as List).map((e) => ProductImportProduct.fromJson(e as Map<String, dynamic>)).toList(),
        rows: (j['rows'] as List).map((e) => ProductImportRow.fromJson(e as Map<String, dynamic>)).toList(),
        notices: (j['notices'] as List).map((e) => e as String).toList(),
        categoryEngine: j['category_engine'] as String,
        categoryGate: ProductImportPreviewCategoryGate.fromJson(j['category_gate'] as Map<String, dynamic>),
        previousImport: j['previous_import'] == null ? null : ProductImportPreviewPreviousImport.fromJson(j['previous_import'] as Map<String, dynamic>),
      );
  Map<String, dynamic> toJson() => {
        'file_sha256': fileSha256,
        'format': format,
        'total_rows': totalRows,
        'error_rows': errorRows,
        'products': products.map((e) => e.toJson()).toList(),
        'rows': rows.map((e) => e.toJson()).toList(),
        'notices': notices,
        'category_engine': categoryEngine,
        'category_gate': categoryGate.toJson(),
        if (previousImport != null) 'previous_import': previousImport!.toJson(),
      };
}

class ProductImportResult {
  final int importId;
  final String fileSha256;
  final bool alreadyImported;
  final int totalRows;
  final int createdProducts;
  final int createdSkus;
  final int failedRows;
  final List<ProductImportOutcome> products;
  final String createdAt;
  const ProductImportResult({required this.importId, required this.fileSha256, required this.alreadyImported, required this.totalRows, required this.createdProducts, required this.createdSkus, required this.failedRows, required this.products, required this.createdAt});
  factory ProductImportResult.fromJson(Map<String, dynamic> j) => ProductImportResult(
        importId: (j['import_id'] as num).toInt(),
        fileSha256: j['file_sha256'] as String,
        alreadyImported: j['already_imported'] as bool,
        totalRows: (j['total_rows'] as num).toInt(),
        createdProducts: (j['created_products'] as num).toInt(),
        createdSkus: (j['created_skus'] as num).toInt(),
        failedRows: (j['failed_rows'] as num).toInt(),
        products: (j['products'] as List).map((e) => ProductImportOutcome.fromJson(e as Map<String, dynamic>)).toList(),
        createdAt: j['created_at'] as String,
      );
  Map<String, dynamic> toJson() => {
        'import_id': importId,
        'file_sha256': fileSha256,
        'already_imported': alreadyImported,
        'total_rows': totalRows,
        'created_products': createdProducts,
        'created_skus': createdSkus,
        'failed_rows': failedRows,
        'products': products.map((e) => e.toJson()).toList(),
        'created_at': createdAt,
      };
}

/// `listed = false` 写一行排除，`listed = true` 删掉那一行。
class ProductListingRequest {
  final bool listed;
  const ProductListingRequest({required this.listed});
  factory ProductListingRequest.fromJson(Map<String, dynamic> j) => ProductListingRequest(
        listed: j['listed'] as bool,
      );
  Map<String, dynamic> toJson() => {
        'listed': listed,
      };
}

class ProductPublicationRequest {
  final String action;
  const ProductPublicationRequest({required this.action});
  factory ProductPublicationRequest.fromJson(Map<String, dynamic> j) => ProductPublicationRequest(
        action: j['action'] as String,
      );
  Map<String, dynamic> toJson() => {
        'action': action,
      };
}

/// 只改文案与归属。**不含 `status` / `published_at` / `deleted_at`**，
class ProductUpdateRequest {
  final String? title;
  final String? subtitle;
  final String? description;
  final int? categoryId;
  final int? brandId;
  final int? freightTemplateId;
  const ProductUpdateRequest({this.title, this.subtitle, this.description, this.categoryId, this.brandId, this.freightTemplateId});
  factory ProductUpdateRequest.fromJson(Map<String, dynamic> j) => ProductUpdateRequest(
        title: j['title'] as String?,
        subtitle: j['subtitle'] as String?,
        description: j['description'] as String?,
        categoryId: (j['category_id'] as num?)?.toInt(),
        brandId: (j['brand_id'] as num?)?.toInt(),
        freightTemplateId: (j['freight_template_id'] as num?)?.toInt(),
      );
  Map<String, dynamic> toJson() => {
        if (title != null) 'title': title,
        if (subtitle != null) 'subtitle': subtitle,
        if (description != null) 'description': description,
        if (categoryId != null) 'category_id': categoryId,
        if (brandId != null) 'brand_id': brandId,
        if (freightTemplateId != null) 'freight_template_id': freightTemplateId,
      };
}

class PromotionSkuInput {
  final int skuId;
  final Money? promoPriceCents;
  final int? discountRate;
  final int? perUserLimit;
  final int? stockQty;
  const PromotionSkuInput({required this.skuId, this.promoPriceCents, this.discountRate, this.perUserLimit, this.stockQty});
  factory PromotionSkuInput.fromJson(Map<String, dynamic> j) => PromotionSkuInput(
        skuId: (j['sku_id'] as num).toInt(),
        promoPriceCents: (j['promo_price_cents'] as num?)?.toInt(),
        discountRate: (j['discount_rate'] as num?)?.toInt(),
        perUserLimit: (j['per_user_limit'] as num?)?.toInt(),
        stockQty: (j['stock_qty'] as num?)?.toInt(),
      );
  Map<String, dynamic> toJson() => {
        'sku_id': skuId,
        if (promoPriceCents != null) 'promo_price_cents': promoPriceCents,
        if (discountRate != null) 'discount_rate': discountRate,
        if (perUserLimit != null) 'per_user_limit': perUserLimit,
        if (stockQty != null) 'stock_qty': stockQty,
      };
}

class PromotionCreateRequest {
  final String name;
  final PromotionType promotionType;
  final int? thresholdUnit;
  final bool? stackWithCoupon;
  final String startsAt;
  final String endsAt;
  final List<PromotionTier>? tiers;
  final List<CouponScopeInput>? scopes;
  final List<PromotionSkuInput>? skus;
  final int? giftCouponTemplateId;
  const PromotionCreateRequest({required this.name, required this.promotionType, this.thresholdUnit, this.stackWithCoupon, required this.startsAt, required this.endsAt, this.tiers, this.scopes, this.skus, this.giftCouponTemplateId});
  factory PromotionCreateRequest.fromJson(Map<String, dynamic> j) => PromotionCreateRequest(
        name: j['name'] as String,
        promotionType: (j['promotion_type'] as num).toInt(),
        thresholdUnit: (j['threshold_unit'] as num?)?.toInt(),
        stackWithCoupon: j['stack_with_coupon'] as bool?,
        startsAt: j['starts_at'] as String,
        endsAt: j['ends_at'] as String,
        tiers: (j['tiers'] as List?)?.map((e) => PromotionTier.fromJson(e as Map<String, dynamic>)).toList(),
        scopes: (j['scopes'] as List?)?.map((e) => CouponScopeInput.fromJson(e as Map<String, dynamic>)).toList(),
        skus: (j['skus'] as List?)?.map((e) => PromotionSkuInput.fromJson(e as Map<String, dynamic>)).toList(),
        giftCouponTemplateId: (j['gift_coupon_template_id'] as num?)?.toInt(),
      );
  Map<String, dynamic> toJson() => {
        'name': name,
        'promotion_type': promotionType,
        if (thresholdUnit != null) 'threshold_unit': thresholdUnit,
        if (stackWithCoupon != null) 'stack_with_coupon': stackWithCoupon,
        'starts_at': startsAt,
        'ends_at': endsAt,
        if (tiers != null) 'tiers': tiers!.map((e) => e.toJson()).toList(),
        if (scopes != null) 'scopes': scopes!.map((e) => e.toJson()).toList(),
        if (skus != null) 'skus': skus!.map((e) => e.toJson()).toList(),
        if (giftCouponTemplateId != null) 'gift_coupon_template_id': giftCouponTemplateId,
      };
}

/// 只改传了的字段；`tiers` / `scopes` / `skus` 传了即整组替换。
class PromotionPatchRequest {
  final String? name;
  final int? thresholdUnit;
  final bool? stackWithCoupon;
  final String? startsAt;
  final String? endsAt;
  final List<PromotionTier>? tiers;
  final List<CouponScopeInput>? scopes;
  final List<PromotionSkuInput>? skus;
  final int? giftCouponTemplateId;
  final int? status;
  const PromotionPatchRequest({this.name, this.thresholdUnit, this.stackWithCoupon, this.startsAt, this.endsAt, this.tiers, this.scopes, this.skus, this.giftCouponTemplateId, this.status});
  factory PromotionPatchRequest.fromJson(Map<String, dynamic> j) => PromotionPatchRequest(
        name: j['name'] as String?,
        thresholdUnit: (j['threshold_unit'] as num?)?.toInt(),
        stackWithCoupon: j['stack_with_coupon'] as bool?,
        startsAt: j['starts_at'] as String?,
        endsAt: j['ends_at'] as String?,
        tiers: (j['tiers'] as List?)?.map((e) => PromotionTier.fromJson(e as Map<String, dynamic>)).toList(),
        scopes: (j['scopes'] as List?)?.map((e) => CouponScopeInput.fromJson(e as Map<String, dynamic>)).toList(),
        skus: (j['skus'] as List?)?.map((e) => PromotionSkuInput.fromJson(e as Map<String, dynamic>)).toList(),
        giftCouponTemplateId: (j['gift_coupon_template_id'] as num?)?.toInt(),
        status: (j['status'] as num?)?.toInt(),
      );
  Map<String, dynamic> toJson() => {
        if (name != null) 'name': name,
        if (thresholdUnit != null) 'threshold_unit': thresholdUnit,
        if (stackWithCoupon != null) 'stack_with_coupon': stackWithCoupon,
        if (startsAt != null) 'starts_at': startsAt,
        if (endsAt != null) 'ends_at': endsAt,
        if (tiers != null) 'tiers': tiers!.map((e) => e.toJson()).toList(),
        if (scopes != null) 'scopes': scopes!.map((e) => e.toJson()).toList(),
        if (skus != null) 'skus': skus!.map((e) => e.toJson()).toList(),
        if (giftCouponTemplateId != null) 'gift_coupon_template_id': giftCouponTemplateId,
        if (status != null) 'status': status,
      };
}

class RefundItemInput {
  final int orderItemId;
  final int quantity;
  const RefundItemInput({required this.orderItemId, required this.quantity});
  factory RefundItemInput.fromJson(Map<String, dynamic> j) => RefundItemInput(
        orderItemId: (j['order_item_id'] as num).toInt(),
        quantity: (j['quantity'] as num).toInt(),
      );
  Map<String, dynamic> toJson() => {
        'order_item_id': orderItemId,
        'quantity': quantity,
      };
}

class RefundCreateRequest {
  final List<RefundItemInput> items;
  final RefundType refundType;
  final RefundReasonCode reasonCode;
  final String? reasonText;
  final List<String>? evidenceUrls;
  const RefundCreateRequest({required this.items, required this.refundType, required this.reasonCode, this.reasonText, this.evidenceUrls});
  factory RefundCreateRequest.fromJson(Map<String, dynamic> j) => RefundCreateRequest(
        items: (j['items'] as List).map((e) => RefundItemInput.fromJson(e as Map<String, dynamic>)).toList(),
        refundType: (j['refund_type'] as num).toInt(),
        reasonCode: (j['reason_code'] as num).toInt(),
        reasonText: j['reason_text'] as String?,
        evidenceUrls: (j['evidence_urls'] as List?)?.map((e) => e as String).toList(),
      );
  Map<String, dynamic> toJson() => {
        'items': items.map((e) => e.toJson()).toList(),
        'refund_type': refundType,
        'reason_code': reasonCode,
        if (reasonText != null) 'reason_text': reasonText,
        if (evidenceUrls != null) 'evidence_urls': evidenceUrls,
      };
}

class RegionCreateRequest {
  final String code;
  final String name;
  const RegionCreateRequest({required this.code, required this.name});
  factory RegionCreateRequest.fromJson(Map<String, dynamic> j) => RegionCreateRequest(
        code: j['code'] as String,
        name: j['name'] as String,
      );
  Map<String, dynamic> toJson() => {
        'code': code,
        'name': name,
      };
}

/// 只给要改的字段。`code` 改了要重查唯一性。
class RegionUpdateRequest {
  final String? code;
  final String? name;
  final int? status;
  const RegionUpdateRequest({this.code, this.name, this.status});
  factory RegionUpdateRequest.fromJson(Map<String, dynamic> j) => RegionUpdateRequest(
        code: j['code'] as String?,
        name: j['name'] as String?,
        status: (j['status'] as num?)?.toInt(),
      );
  Map<String, dynamic> toJson() => {
        if (code != null) 'code': code,
        if (name != null) 'name': name,
        if (status != null) 'status': status,
      };
}

class ReportInventoryAlert {
  final int storeId;
  final String storeName;
  final int regionId;
  final int skuId;
  final String skuCode;
  final Map<String, String> specValues;
  final int productId;
  final String productTitle;
  final int availableQty;
  final int warningQty;
  const ReportInventoryAlert({required this.storeId, required this.storeName, required this.regionId, required this.skuId, required this.skuCode, required this.specValues, required this.productId, required this.productTitle, required this.availableQty, required this.warningQty});
  factory ReportInventoryAlert.fromJson(Map<String, dynamic> j) => ReportInventoryAlert(
        storeId: (j['store_id'] as num).toInt(),
        storeName: j['store_name'] as String,
        regionId: (j['region_id'] as num).toInt(),
        skuId: (j['sku_id'] as num).toInt(),
        skuCode: j['sku_code'] as String,
        specValues: (j['spec_values'] as Map).map((k, e) => MapEntry(k as String, e as String)),
        productId: (j['product_id'] as num).toInt(),
        productTitle: j['product_title'] as String,
        availableQty: (j['available_qty'] as num).toInt(),
        warningQty: (j['warning_qty'] as num).toInt(),
      );
  Map<String, dynamic> toJson() => {
        'store_id': storeId,
        'store_name': storeName,
        'region_id': regionId,
        'sku_id': skuId,
        'sku_code': skuCode,
        'spec_values': specValues,
        'product_id': productId,
        'product_title': productTitle,
        'available_qty': availableQty,
        'warning_qty': warningQty,
      };
}

class ReportInventoryAlerts {
  final int total;
  final List<ReportInventoryAlert> items;
  const ReportInventoryAlerts({required this.total, required this.items});
  factory ReportInventoryAlerts.fromJson(Map<String, dynamic> j) => ReportInventoryAlerts(
        total: (j['total'] as num).toInt(),
        items: (j['items'] as List).map((e) => ReportInventoryAlert.fromJson(e as Map<String, dynamic>)).toList(),
      );
  Map<String, dynamic> toJson() => {
        'total': total,
        'items': items.map((e) => e.toJson()).toList(),
      };
}

/// 一段窗口里的核心指标。**订单口径**：已支付的五种状态
class ReportMetrics {
  final Money paidAmountCents;
  final Money refundAmountCents;
  final Money netSalesCents;
  final int orderCount;
  final int buyerCount;
  final Money avgOrderValueCents;
  final int refundCount;
  final double? refundRate;
  const ReportMetrics({required this.paidAmountCents, required this.refundAmountCents, required this.netSalesCents, required this.orderCount, required this.buyerCount, required this.avgOrderValueCents, required this.refundCount, this.refundRate});
  factory ReportMetrics.fromJson(Map<String, dynamic> j) => ReportMetrics(
        paidAmountCents: (j['paid_amount_cents'] as num).toInt(),
        refundAmountCents: (j['refund_amount_cents'] as num).toInt(),
        netSalesCents: (j['net_sales_cents'] as num).toInt(),
        orderCount: (j['order_count'] as num).toInt(),
        buyerCount: (j['buyer_count'] as num).toInt(),
        avgOrderValueCents: (j['avg_order_value_cents'] as num).toInt(),
        refundCount: (j['refund_count'] as num).toInt(),
        refundRate: (j['refund_rate'] as num?)?.toDouble(),
      );
  Map<String, dynamic> toJson() => {
        'paid_amount_cents': paidAmountCents,
        'refund_amount_cents': refundAmountCents,
        'net_sales_cents': netSalesCents,
        'order_count': orderCount,
        'buyer_count': buyerCount,
        'avg_order_value_cents': avgOrderValueCents,
        'refund_count': refundCount,
        'refund_rate': refundRate,
      };
}

/// 一段半开区间 `[start_at, end_at)`，同时给出它在店铺时区里的起止日期（都含）。
class ReportRange {
  final String startAt;
  final String endAt;
  final String startDate;
  final String endDate;
  const ReportRange({required this.startAt, required this.endAt, required this.startDate, required this.endDate});
  factory ReportRange.fromJson(Map<String, dynamic> j) => ReportRange(
        startAt: j['start_at'] as String,
        endAt: j['end_at'] as String,
        startDate: j['start_date'] as String,
        endDate: j['end_date'] as String,
      );
  Map<String, dynamic> toJson() => {
        'start_at': startAt,
        'end_at': endAt,
        'start_date': startDate,
        'end_date': endDate,
      };
}

/// 一次报表查询的时间窗口。**六条报表共用这一份口径**：
class ReportWindow {
  final String period;
  final String timezone;
  final ReportRange current;
  final ReportRange previous;
  const ReportWindow({required this.period, required this.timezone, required this.current, required this.previous});
  factory ReportWindow.fromJson(Map<String, dynamic> j) => ReportWindow(
        period: j['period'] as String,
        timezone: j['timezone'] as String,
        current: ReportRange.fromJson(j['current'] as Map<String, dynamic>),
        previous: ReportRange.fromJson(j['previous'] as Map<String, dynamic>),
      );
  Map<String, dynamic> toJson() => {
        'period': period,
        'timezone': timezone,
        'current': current.toJson(),
        'previous': previous.toJson(),
      };
}

class ReportOverview {
  final ReportWindow window;
  final ReportMetrics current;
  final ReportMetrics previous;
  const ReportOverview({required this.window, required this.current, required this.previous});
  factory ReportOverview.fromJson(Map<String, dynamic> j) => ReportOverview(
        window: ReportWindow.fromJson(j['window'] as Map<String, dynamic>),
        current: ReportMetrics.fromJson(j['current'] as Map<String, dynamic>),
        previous: ReportMetrics.fromJson(j['previous'] as Map<String, dynamic>),
      );
  Map<String, dynamic> toJson() => {
        'window': window.toJson(),
        'current': current.toJson(),
        'previous': previous.toJson(),
      };
}

class ReportProductRankItem {
  final int rank;
  final int productId;
  final String title;
  final int categoryId;
  final int quantity;
  final Money amountCents;
  final int orderCount;
  final int refundedQuantity;
  final Money refundedAmountCents;
  const ReportProductRankItem({required this.rank, required this.productId, required this.title, required this.categoryId, required this.quantity, required this.amountCents, required this.orderCount, required this.refundedQuantity, required this.refundedAmountCents});
  factory ReportProductRankItem.fromJson(Map<String, dynamic> j) => ReportProductRankItem(
        rank: (j['rank'] as num).toInt(),
        productId: (j['product_id'] as num).toInt(),
        title: j['title'] as String,
        categoryId: (j['category_id'] as num).toInt(),
        quantity: (j['quantity'] as num).toInt(),
        amountCents: (j['amount_cents'] as num).toInt(),
        orderCount: (j['order_count'] as num).toInt(),
        refundedQuantity: (j['refunded_quantity'] as num).toInt(),
        refundedAmountCents: (j['refunded_amount_cents'] as num).toInt(),
      );
  Map<String, dynamic> toJson() => {
        'rank': rank,
        'product_id': productId,
        'title': title,
        'category_id': categoryId,
        'quantity': quantity,
        'amount_cents': amountCents,
        'order_count': orderCount,
        'refunded_quantity': refundedQuantity,
        'refunded_amount_cents': refundedAmountCents,
      };
}

class ReportProductRanking {
  final ReportWindow window;
  final String sortBy;
  final List<ReportProductRankItem> items;
  const ReportProductRanking({required this.window, required this.sortBy, required this.items});
  factory ReportProductRanking.fromJson(Map<String, dynamic> j) => ReportProductRanking(
        window: ReportWindow.fromJson(j['window'] as Map<String, dynamic>),
        sortBy: j['sort_by'] as String,
        items: (j['items'] as List).map((e) => ReportProductRankItem.fromJson(e as Map<String, dynamic>)).toList(),
      );
  Map<String, dynamic> toJson() => {
        'window': window.toJson(),
        'sort_by': sortBy,
        'items': items.map((e) => e.toJson()).toList(),
      };
}

class ReportRegionRow {
  final int regionId;
  final String regionName;
  final int storeCount;
  final Money paidAmountCents;
  final Money refundAmountCents;
  final Money netSalesCents;
  final int orderCount;
  const ReportRegionRow({required this.regionId, required this.regionName, required this.storeCount, required this.paidAmountCents, required this.refundAmountCents, required this.netSalesCents, required this.orderCount});
  factory ReportRegionRow.fromJson(Map<String, dynamic> j) => ReportRegionRow(
        regionId: (j['region_id'] as num).toInt(),
        regionName: j['region_name'] as String,
        storeCount: (j['store_count'] as num).toInt(),
        paidAmountCents: (j['paid_amount_cents'] as num).toInt(),
        refundAmountCents: (j['refund_amount_cents'] as num).toInt(),
        netSalesCents: (j['net_sales_cents'] as num).toInt(),
        orderCount: (j['order_count'] as num).toInt(),
      );
  Map<String, dynamic> toJson() => {
        'region_id': regionId,
        'region_name': regionName,
        'store_count': storeCount,
        'paid_amount_cents': paidAmountCents,
        'refund_amount_cents': refundAmountCents,
        'net_sales_cents': netSalesCents,
        'order_count': orderCount,
      };
}

class ReportSearchTerm {
  final String query;
  final int searchCount;
  final int zeroResultCount;
  const ReportSearchTerm({required this.query, required this.searchCount, required this.zeroResultCount});
  factory ReportSearchTerm.fromJson(Map<String, dynamic> j) => ReportSearchTerm(
        query: j['query'] as String,
        searchCount: (j['search_count'] as num).toInt(),
        zeroResultCount: (j['zero_result_count'] as num).toInt(),
      );
  Map<String, dynamic> toJson() => {
        'query': query,
        'search_count': searchCount,
        'zero_result_count': zeroResultCount,
      };
}

class ReportSearchOverview {
  final ReportWindow window;
  final int searchCount;
  final int zeroResultCount;
  final double? zeroResultRate;
  final int clickCount;
  final List<ReportSearchTerm> topQueries;
  final List<ReportSearchTerm> zeroResultQueries;
  const ReportSearchOverview({required this.window, required this.searchCount, required this.zeroResultCount, this.zeroResultRate, required this.clickCount, required this.topQueries, required this.zeroResultQueries});
  factory ReportSearchOverview.fromJson(Map<String, dynamic> j) => ReportSearchOverview(
        window: ReportWindow.fromJson(j['window'] as Map<String, dynamic>),
        searchCount: (j['search_count'] as num).toInt(),
        zeroResultCount: (j['zero_result_count'] as num).toInt(),
        zeroResultRate: (j['zero_result_rate'] as num?)?.toDouble(),
        clickCount: (j['click_count'] as num).toInt(),
        topQueries: (j['top_queries'] as List).map((e) => ReportSearchTerm.fromJson(e as Map<String, dynamic>)).toList(),
        zeroResultQueries: (j['zero_result_queries'] as List).map((e) => ReportSearchTerm.fromJson(e as Map<String, dynamic>)).toList(),
      );
  Map<String, dynamic> toJson() => {
        'window': window.toJson(),
        'search_count': searchCount,
        'zero_result_count': zeroResultCount,
        'zero_result_rate': zeroResultRate,
        'click_count': clickCount,
        'top_queries': topQueries.map((e) => e.toJson()).toList(),
        'zero_result_queries': zeroResultQueries.map((e) => e.toJson()).toList(),
      };
}

class ReportStoreRow {
  final int storeId;
  final String storeName;
  final String storeCode;
  final int regionId;
  final String regionName;
  final bool deleted;
  final Money paidAmountCents;
  final Money refundAmountCents;
  final Money netSalesCents;
  final int orderCount;
  const ReportStoreRow({required this.storeId, required this.storeName, required this.storeCode, required this.regionId, required this.regionName, required this.deleted, required this.paidAmountCents, required this.refundAmountCents, required this.netSalesCents, required this.orderCount});
  factory ReportStoreRow.fromJson(Map<String, dynamic> j) => ReportStoreRow(
        storeId: (j['store_id'] as num).toInt(),
        storeName: j['store_name'] as String,
        storeCode: j['store_code'] as String,
        regionId: (j['region_id'] as num).toInt(),
        regionName: j['region_name'] as String,
        deleted: j['deleted'] as bool,
        paidAmountCents: (j['paid_amount_cents'] as num).toInt(),
        refundAmountCents: (j['refund_amount_cents'] as num).toInt(),
        netSalesCents: (j['net_sales_cents'] as num).toInt(),
        orderCount: (j['order_count'] as num).toInt(),
      );
  Map<String, dynamic> toJson() => {
        'store_id': storeId,
        'store_name': storeName,
        'store_code': storeCode,
        'region_id': regionId,
        'region_name': regionName,
        'deleted': deleted,
        'paid_amount_cents': paidAmountCents,
        'refund_amount_cents': refundAmountCents,
        'net_sales_cents': netSalesCents,
        'order_count': orderCount,
      };
}

class ReportStoreComparison {
  final ReportWindow window;
  final List<ReportStoreRow> stores;
  final List<ReportRegionRow> regions;
  const ReportStoreComparison({required this.window, required this.stores, required this.regions});
  factory ReportStoreComparison.fromJson(Map<String, dynamic> j) => ReportStoreComparison(
        window: ReportWindow.fromJson(j['window'] as Map<String, dynamic>),
        stores: (j['stores'] as List).map((e) => ReportStoreRow.fromJson(e as Map<String, dynamic>)).toList(),
        regions: (j['regions'] as List).map((e) => ReportRegionRow.fromJson(e as Map<String, dynamic>)).toList(),
      );
  Map<String, dynamic> toJson() => {
        'window': window.toJson(),
        'stores': stores.map((e) => e.toJson()).toList(),
        'regions': regions.map((e) => e.toJson()).toList(),
      };
}

class ReportTrendPoint {
  final String bucketStartAt;
  final String label;
  final Money paidAmountCents;
  final Money refundAmountCents;
  final Money netSalesCents;
  final int orderCount;
  const ReportTrendPoint({required this.bucketStartAt, required this.label, required this.paidAmountCents, required this.refundAmountCents, required this.netSalesCents, required this.orderCount});
  factory ReportTrendPoint.fromJson(Map<String, dynamic> j) => ReportTrendPoint(
        bucketStartAt: j['bucket_start_at'] as String,
        label: j['label'] as String,
        paidAmountCents: (j['paid_amount_cents'] as num).toInt(),
        refundAmountCents: (j['refund_amount_cents'] as num).toInt(),
        netSalesCents: (j['net_sales_cents'] as num).toInt(),
        orderCount: (j['order_count'] as num).toInt(),
      );
  Map<String, dynamic> toJson() => {
        'bucket_start_at': bucketStartAt,
        'label': label,
        'paid_amount_cents': paidAmountCents,
        'refund_amount_cents': refundAmountCents,
        'net_sales_cents': netSalesCents,
        'order_count': orderCount,
      };
}

class ReportTrend {
  final ReportWindow window;
  final String granularity;
  final List<ReportTrendPoint> points;
  const ReportTrend({required this.window, required this.granularity, required this.points});
  factory ReportTrend.fromJson(Map<String, dynamic> j) => ReportTrend(
        window: ReportWindow.fromJson(j['window'] as Map<String, dynamic>),
        granularity: j['granularity'] as String,
        points: (j['points'] as List).map((e) => ReportTrendPoint.fromJson(e as Map<String, dynamic>)).toList(),
      );
  Map<String, dynamic> toJson() => {
        'window': window.toJson(),
        'granularity': granularity,
        'points': points.map((e) => e.toJson()).toList(),
      };
}

class ReturnShipmentRequest {
  final String carrierCode;
  final String trackingNo;
  const ReturnShipmentRequest({required this.carrierCode, required this.trackingNo});
  factory ReturnShipmentRequest.fromJson(Map<String, dynamic> j) => ReturnShipmentRequest(
        carrierCode: j['carrier_code'] as String,
        trackingNo: j['tracking_no'] as String,
      );
  Map<String, dynamic> toJson() => {
        'carrier_code': carrierCode,
        'tracking_no': trackingNo,
      };
}

/// 一件商品在某个作用域（大区或门店）下的可见性与生效价。
class ScopedProductListing {
  final int productId;
  final String title;
  final String? imageUrl;
  final int? status;
  final bool listed;
  final bool effectiveListed;
  final Money? minPriceCents;
  final Money? maxPriceCents;
  final int priceSource;
  const ScopedProductListing({required this.productId, required this.title, this.imageUrl, this.status, required this.listed, required this.effectiveListed, this.minPriceCents, this.maxPriceCents, required this.priceSource});
  factory ScopedProductListing.fromJson(Map<String, dynamic> j) => ScopedProductListing(
        productId: (j['product_id'] as num).toInt(),
        title: j['title'] as String,
        imageUrl: j['image_url'] as String?,
        status: (j['status'] as num?)?.toInt(),
        listed: j['listed'] as bool,
        effectiveListed: j['effective_listed'] as bool,
        minPriceCents: (j['min_price_cents'] as num?)?.toInt(),
        maxPriceCents: (j['max_price_cents'] as num?)?.toInt(),
        priceSource: (j['price_source'] as num).toInt(),
      );
  Map<String, dynamic> toJson() => {
        'product_id': productId,
        'title': title,
        if (imageUrl != null) 'image_url': imageUrl,
        if (status != null) 'status': status,
        'listed': listed,
        'effective_listed': effectiveListed,
        if (minPriceCents != null) 'min_price_cents': minPriceCents,
        if (maxPriceCents != null) 'max_price_cents': maxPriceCents,
        'price_source': priceSource,
      };
}

/// 一个 SKU 在某个作用域（大区或门店）下的价格覆盖与生效价。
class ScopedSkuPrice {
  final int skuId;
  final String? skuCode;
  final Money? basePriceCents;
  final int? overridePriceCents;
  final Money effectivePriceCents;
  final int priceSource;
  const ScopedSkuPrice({required this.skuId, this.skuCode, this.basePriceCents, this.overridePriceCents, required this.effectivePriceCents, required this.priceSource});
  factory ScopedSkuPrice.fromJson(Map<String, dynamic> j) => ScopedSkuPrice(
        skuId: (j['sku_id'] as num).toInt(),
        skuCode: j['sku_code'] as String?,
        basePriceCents: (j['base_price_cents'] as num?)?.toInt(),
        overridePriceCents: (j['override_price_cents'] as num?)?.toInt(),
        effectivePriceCents: (j['effective_price_cents'] as num).toInt(),
        priceSource: (j['price_source'] as num).toInt(),
      );
  Map<String, dynamic> toJson() => {
        'sku_id': skuId,
        if (skuCode != null) 'sku_code': skuCode,
        if (basePriceCents != null) 'base_price_cents': basePriceCents,
        if (overridePriceCents != null) 'override_price_cents': overridePriceCents,
        'effective_price_cents': effectivePriceCents,
        'price_source': priceSource,
      };
}

class SearchFilters {
  final int? categoryId;
  final Money? minPriceCents;
  final Money? maxPriceCents;
  final bool? inStockOnly;
  const SearchFilters({this.categoryId, this.minPriceCents, this.maxPriceCents, this.inStockOnly});
  factory SearchFilters.fromJson(Map<String, dynamic> j) => SearchFilters(
        categoryId: (j['category_id'] as num?)?.toInt(),
        minPriceCents: (j['min_price_cents'] as num?)?.toInt(),
        maxPriceCents: (j['max_price_cents'] as num?)?.toInt(),
        inStockOnly: j['in_stock_only'] as bool?,
      );
  Map<String, dynamic> toJson() => {
        if (categoryId != null) 'category_id': categoryId,
        if (minPriceCents != null) 'min_price_cents': minPriceCents,
        if (maxPriceCents != null) 'max_price_cents': maxPriceCents,
        if (inStockOnly != null) 'in_stock_only': inStockOnly,
      };
}

class SearchHitScores {
  final double? vector;
  final double? keyword;
  final double? rrf;
  final double? rerank;
  final double? business;
  final double? final_;
  const SearchHitScores({this.vector, this.keyword, this.rrf, this.rerank, this.business, this.final_});
  factory SearchHitScores.fromJson(Map<String, dynamic> j) => SearchHitScores(
        vector: (j['vector'] as num?)?.toDouble(),
        keyword: (j['keyword'] as num?)?.toDouble(),
        rrf: (j['rrf'] as num?)?.toDouble(),
        rerank: (j['rerank'] as num?)?.toDouble(),
        business: (j['business'] as num?)?.toDouble(),
        final_: (j['final'] as num?)?.toDouble(),
      );
  Map<String, dynamic> toJson() => {
        if (vector != null) 'vector': vector,
        if (keyword != null) 'keyword': keyword,
        if (rrf != null) 'rrf': rrf,
        if (rerank != null) 'rerank': rerank,
        if (business != null) 'business': business,
        if (final_ != null) 'final': final_,
      };
}

class SearchHit {
  final int id;
  final String title;
  final String? subtitle;
  final String? imageUrl;
  final Money minPriceCents;
  final Money? maxPriceCents;
  final bool? inStock;
  final int? salesCount;
  final int status;
  final List<PromotionTag>? promotionTags;
  final SearchHitScores? scores;
  final String? recallSource;
  const SearchHit({required this.id, required this.title, this.subtitle, this.imageUrl, required this.minPriceCents, this.maxPriceCents, this.inStock, this.salesCount, required this.status, this.promotionTags, this.scores, this.recallSource});
  factory SearchHit.fromJson(Map<String, dynamic> j) => SearchHit(
        id: (j['id'] as num).toInt(),
        title: j['title'] as String,
        subtitle: j['subtitle'] as String?,
        imageUrl: j['image_url'] as String?,
        minPriceCents: (j['min_price_cents'] as num).toInt(),
        maxPriceCents: (j['max_price_cents'] as num?)?.toInt(),
        inStock: j['in_stock'] as bool?,
        salesCount: (j['sales_count'] as num?)?.toInt(),
        status: (j['status'] as num).toInt(),
        promotionTags: (j['promotion_tags'] as List?)?.map((e) => PromotionTag.fromJson(e as Map<String, dynamic>)).toList(),
        scores: j['scores'] == null ? null : SearchHitScores.fromJson(j['scores'] as Map<String, dynamic>),
        recallSource: j['recall_source'] as String?,
      );
  Map<String, dynamic> toJson() => {
        'id': id,
        'title': title,
        if (subtitle != null) 'subtitle': subtitle,
        if (imageUrl != null) 'image_url': imageUrl,
        'min_price_cents': minPriceCents,
        if (maxPriceCents != null) 'max_price_cents': maxPriceCents,
        if (inStock != null) 'in_stock': inStock,
        if (salesCount != null) 'sales_count': salesCount,
        'status': status,
        if (promotionTags != null) 'promotion_tags': promotionTags!.map((e) => e.toJson()).toList(),
        if (scores != null) 'scores': scores!.toJson(),
        if (recallSource != null) 'recall_source': recallSource,
      };
}

/// `POST /search` 的请求体。**本轮从内联 schema 提成具名的**——
class SearchRequest {
  final String query;
  final int? storeId;
  final SearchFilters? filters;
  final int? size;
  final String? strategy;
  final bool? explain;
  const SearchRequest({required this.query, this.storeId, this.filters, this.size, this.strategy, this.explain});
  factory SearchRequest.fromJson(Map<String, dynamic> j) => SearchRequest(
        query: j['query'] as String,
        storeId: (j['store_id'] as num?)?.toInt(),
        filters: j['filters'] == null ? null : SearchFilters.fromJson(j['filters'] as Map<String, dynamic>),
        size: (j['size'] as num?)?.toInt(),
        strategy: j['strategy'] as String?,
        explain: j['explain'] as bool?,
      );
  Map<String, dynamic> toJson() => {
        'query': query,
        if (storeId != null) 'store_id': storeId,
        if (filters != null) 'filters': filters!.toJson(),
        if (size != null) 'size': size,
        if (strategy != null) 'strategy': strategy,
        if (explain != null) 'explain': explain,
      };
}

/// 一期不接受发货明细——整单发货，包裹内容即订单全部商品。
class ShipmentCreateRequest {
  final String carrierCode;
  final String trackingNo;
  const ShipmentCreateRequest({required this.carrierCode, required this.trackingNo});
  factory ShipmentCreateRequest.fromJson(Map<String, dynamic> j) => ShipmentCreateRequest(
        carrierCode: j['carrier_code'] as String,
        trackingNo: j['tracking_no'] as String,
      );
  Map<String, dynamic> toJson() => {
        'carrier_code': carrierCode,
        'tracking_no': trackingNo,
      };
}

class ShopSettings {
  final String shopName;
  final String? servicePhone;
  final String timezone;
  final int autoConfirmDays;
  final int returnShipDays;
  final String? updatedAt;
  const ShopSettings({required this.shopName, this.servicePhone, required this.timezone, required this.autoConfirmDays, required this.returnShipDays, this.updatedAt});
  factory ShopSettings.fromJson(Map<String, dynamic> j) => ShopSettings(
        shopName: j['shop_name'] as String,
        servicePhone: j['service_phone'] as String?,
        timezone: j['timezone'] as String,
        autoConfirmDays: (j['auto_confirm_days'] as num).toInt(),
        returnShipDays: (j['return_ship_days'] as num).toInt(),
        updatedAt: j['updated_at'] as String?,
      );
  Map<String, dynamic> toJson() => {
        'shop_name': shopName,
        'service_phone': servicePhone,
        'timezone': timezone,
        'auto_confirm_days': autoConfirmDays,
        'return_ship_days': returnShipDays,
        'updated_at': updatedAt,
      };
}

class ShopSettingsInput {
  final String? servicePhone;
  final String timezone;
  final int autoConfirmDays;
  final int returnShipDays;
  const ShopSettingsInput({this.servicePhone, required this.timezone, required this.autoConfirmDays, required this.returnShipDays});
  factory ShopSettingsInput.fromJson(Map<String, dynamic> j) => ShopSettingsInput(
        servicePhone: j['service_phone'] as String?,
        timezone: j['timezone'] as String,
        autoConfirmDays: (j['auto_confirm_days'] as num).toInt(),
        returnShipDays: (j['return_ship_days'] as num).toInt(),
      );
  Map<String, dynamic> toJson() => {
        if (servicePhone != null) 'service_phone': servicePhone,
        'timezone': timezone,
        'auto_confirm_days': autoConfirmDays,
        'return_ship_days': returnShipDays,
      };
}

/// `available_qty` 在这里是**允许的**，而在 `SkuUpdateRequest` 里不允许：
class SkuCreateRequest {
  final String skuCode;
  final Map<String, String>? specValues;
  final Money priceCents;
  final Money? costCents;
  final int? weightGram;
  final int? imageUploadId;
  final int? availableQty;
  final int? warningQty;
  const SkuCreateRequest({required this.skuCode, this.specValues, required this.priceCents, this.costCents, this.weightGram, this.imageUploadId, this.availableQty, this.warningQty});
  factory SkuCreateRequest.fromJson(Map<String, dynamic> j) => SkuCreateRequest(
        skuCode: j['sku_code'] as String,
        specValues: (j['spec_values'] as Map?)?.map((k, e) => MapEntry(k as String, e as String)),
        priceCents: (j['price_cents'] as num).toInt(),
        costCents: (j['cost_cents'] as num?)?.toInt(),
        weightGram: (j['weight_gram'] as num?)?.toInt(),
        imageUploadId: (j['image_upload_id'] as num?)?.toInt(),
        availableQty: (j['available_qty'] as num?)?.toInt(),
        warningQty: (j['warning_qty'] as num?)?.toInt(),
      );
  Map<String, dynamic> toJson() => {
        'sku_code': skuCode,
        if (specValues != null) 'spec_values': specValues,
        'price_cents': priceCents,
        if (costCents != null) 'cost_cents': costCents,
        if (weightGram != null) 'weight_gram': weightGram,
        if (imageUploadId != null) 'image_upload_id': imageUploadId,
        if (availableQty != null) 'available_qty': availableQty,
        if (warningQty != null) 'warning_qty': warningQty,
      };
}

/// 设置本作用域的价格覆盖（upsert）。
class SkuPriceSetRequest {
  final Money priceCents;
  const SkuPriceSetRequest({required this.priceCents});
  factory SkuPriceSetRequest.fromJson(Map<String, dynamic> j) => SkuPriceSetRequest(
        priceCents: (j['price_cents'] as num).toInt(),
      );
  Map<String, dynamic> toJson() => {
        'price_cents': priceCents,
      };
}

/// **刻意没有 `available_qty`**：库存有自己的端点，因为它要表达乐观并发。
class SkuUpdateRequest {
  final String? skuCode;
  final Map<String, String>? specValues;
  final Money? priceCents;
  final Money? costCents;
  final int? weightGram;
  final int? imageUploadId;
  final int? status;
  const SkuUpdateRequest({this.skuCode, this.specValues, this.priceCents, this.costCents, this.weightGram, this.imageUploadId, this.status});
  factory SkuUpdateRequest.fromJson(Map<String, dynamic> j) => SkuUpdateRequest(
        skuCode: j['sku_code'] as String?,
        specValues: (j['spec_values'] as Map?)?.map((k, e) => MapEntry(k as String, e as String)),
        priceCents: (j['price_cents'] as num?)?.toInt(),
        costCents: (j['cost_cents'] as num?)?.toInt(),
        weightGram: (j['weight_gram'] as num?)?.toInt(),
        imageUploadId: (j['image_upload_id'] as num?)?.toInt(),
        status: (j['status'] as num?)?.toInt(),
      );
  Map<String, dynamic> toJson() => {
        if (skuCode != null) 'sku_code': skuCode,
        if (specValues != null) 'spec_values': specValues,
        if (priceCents != null) 'price_cents': priceCents,
        if (costCents != null) 'cost_cents': costCents,
        if (weightGram != null) 'weight_gram': weightGram,
        if (imageUploadId != null) 'image_upload_id': imageUploadId,
        if (status != null) 'status': status,
      };
}

/// 1 管理员 · 2 操作员 · 3 大区管理员 · 4 门店管理员
typedef StaffRole = int;

class Staff {
  final int id;
  final String email;
  final String? name;
  final StaffRole role;
  final int status;
  final int? merchantId;
  final List<int> regionIds;
  final List<int> storeIds;
  final String? lastLoginAt;
  final String createdAt;
  const Staff({required this.id, required this.email, this.name, required this.role, required this.status, this.merchantId, required this.regionIds, required this.storeIds, this.lastLoginAt, required this.createdAt});
  factory Staff.fromJson(Map<String, dynamic> j) => Staff(
        id: (j['id'] as num).toInt(),
        email: j['email'] as String,
        name: j['name'] as String?,
        role: (j['role'] as num).toInt(),
        status: (j['status'] as num).toInt(),
        merchantId: (j['merchant_id'] as num?)?.toInt(),
        regionIds: (j['region_ids'] as List).map((e) => (e as num).toInt()).toList(),
        storeIds: (j['store_ids'] as List).map((e) => (e as num).toInt()).toList(),
        lastLoginAt: j['last_login_at'] as String?,
        createdAt: j['created_at'] as String,
      );
  Map<String, dynamic> toJson() => {
        'id': id,
        'email': email,
        if (name != null) 'name': name,
        'role': role,
        'status': status,
        'merchant_id': merchantId,
        'region_ids': regionIds,
        'store_ids': storeIds,
        if (lastLoginAt != null) 'last_login_at': lastLoginAt,
        'created_at': createdAt,
      };
}

/// **没有 `merchant_id` 字段，这是刻意的。** 租户归属从调用者的会话继承。
class StaffCreateRequest {
  final String email;
  final String? name;
  final StaffRole role;
  final List<int>? regionIds;
  final List<int>? storeIds;
  const StaffCreateRequest({required this.email, this.name, required this.role, this.regionIds, this.storeIds});
  factory StaffCreateRequest.fromJson(Map<String, dynamic> j) => StaffCreateRequest(
        email: j['email'] as String,
        name: j['name'] as String?,
        role: (j['role'] as num).toInt(),
        regionIds: (j['region_ids'] as List?)?.map((e) => (e as num).toInt()).toList(),
        storeIds: (j['store_ids'] as List?)?.map((e) => (e as num).toInt()).toList(),
      );
  Map<String, dynamic> toJson() => {
        'email': email,
        if (name != null) 'name': name,
        'role': role,
        if (regionIds != null) 'region_ids': regionIds,
        if (storeIds != null) 'store_ids': storeIds,
      };
}

/// 一串一次性登录 token，与新建员工时服务端签的那一串同一种（15 分钟、用掉即失效），
class StaffLoginToken {
  final int staffId;
  final String token;
  final String expireAt;
  const StaffLoginToken({required this.staffId, required this.token, required this.expireAt});
  factory StaffLoginToken.fromJson(Map<String, dynamic> j) => StaffLoginToken(
        staffId: (j['staff_id'] as num).toInt(),
        token: j['token'] as String,
        expireAt: j['expire_at'] as String,
      );
  Map<String, dynamic> toJson() => {
        'staff_id': staffId,
        'token': token,
        'expire_at': expireAt,
      };
}

class StaffSession {
  final String token;
  final String expireAt;
  final Staff staff;
  const StaffSession({required this.token, required this.expireAt, required this.staff});
  factory StaffSession.fromJson(Map<String, dynamic> j) => StaffSession(
        token: j['token'] as String,
        expireAt: j['expire_at'] as String,
        staff: Staff.fromJson(j['staff'] as Map<String, dynamic>),
      );
  Map<String, dynamic> toJson() => {
        'token': token,
        'expire_at': expireAt,
        'staff': staff.toJson(),
      };
}

/// 买家视角的门店。不含围栏——围栏是运营数据，不该发给客户端。
class Store {
  final int id;
  final String name;
  final String? phone;
  final String? address;
  final double? lat;
  final double? lng;
  final bool isDefault;
  const Store({required this.id, required this.name, this.phone, this.address, this.lat, this.lng, required this.isDefault});
  factory Store.fromJson(Map<String, dynamic> j) => Store(
        id: (j['id'] as num).toInt(),
        name: j['name'] as String,
        phone: j['phone'] as String?,
        address: j['address'] as String?,
        lat: (j['lat'] as num?)?.toDouble(),
        lng: (j['lng'] as num?)?.toDouble(),
        isDefault: j['is_default'] as bool,
      );
  Map<String, dynamic> toJson() => {
        'id': id,
        'name': name,
        if (phone != null) 'phone': phone,
        if (address != null) 'address': address,
        if (lat != null) 'lat': lat,
        if (lng != null) 'lng': lng,
        'is_default': isDefault,
      };
}

/// **围栏不在这里传**，走 `PUT /admin/stores/{store_id}/fence`。
class StoreCreateRequest {
  final int regionId;
  final String code;
  final String name;
  final String? phone;
  final String? province;
  final String? city;
  final String? district;
  final String? address;
  final double? lat;
  final double? lng;
  final bool? isDefault;
  const StoreCreateRequest({required this.regionId, required this.code, required this.name, this.phone, this.province, this.city, this.district, this.address, this.lat, this.lng, this.isDefault});
  factory StoreCreateRequest.fromJson(Map<String, dynamic> j) => StoreCreateRequest(
        regionId: (j['region_id'] as num).toInt(),
        code: j['code'] as String,
        name: j['name'] as String,
        phone: j['phone'] as String?,
        province: j['province'] as String?,
        city: j['city'] as String?,
        district: j['district'] as String?,
        address: j['address'] as String?,
        lat: (j['lat'] as num?)?.toDouble(),
        lng: (j['lng'] as num?)?.toDouble(),
        isDefault: j['is_default'] as bool?,
      );
  Map<String, dynamic> toJson() => {
        'region_id': regionId,
        'code': code,
        'name': name,
        if (phone != null) 'phone': phone,
        if (province != null) 'province': province,
        if (city != null) 'city': city,
        if (district != null) 'district': district,
        if (address != null) 'address': address,
        if (lat != null) 'lat': lat,
        if (lng != null) 'lng': lng,
        if (isDefault != null) 'is_default': isDefault,
      };
}

/// 整体替换这家门店的围栏。
class StoreFenceRequest {
  final dynamic fence;
  const StoreFenceRequest({required this.fence});
  factory StoreFenceRequest.fromJson(Map<String, dynamic> j) => StoreFenceRequest(
        fence: j['fence'],
      );
  Map<String, dynamic> toJson() => {
        'fence': fence,
      };
}

/// 二选一：给 `template_id` 即引用那个模板；不给就必须给齐 `min_order_cents` / `free_over_cents` / `fee_tiers`
class StoreLocalDeliveryRequest {
  final int? templateId;
  final Money? minOrderCents;
  final Money? freeOverCents;
  final List<DeliveryTier>? feeTiers;
  const StoreLocalDeliveryRequest({this.templateId, this.minOrderCents, this.freeOverCents, this.feeTiers});
  factory StoreLocalDeliveryRequest.fromJson(Map<String, dynamic> j) => StoreLocalDeliveryRequest(
        templateId: (j['template_id'] as num?)?.toInt(),
        minOrderCents: (j['min_order_cents'] as num?)?.toInt(),
        freeOverCents: (j['free_over_cents'] as num?)?.toInt(),
        feeTiers: (j['fee_tiers'] as List?)?.map((e) => DeliveryTier.fromJson(e as Map<String, dynamic>)).toList(),
      );
  Map<String, dynamic> toJson() => {
        if (templateId != null) 'template_id': templateId,
        if (minOrderCents != null) 'min_order_cents': minOrderCents,
        if (freeOverCents != null) 'free_over_cents': freeOverCents,
        if (feeTiers != null) 'fee_tiers': feeTiers!.map((e) => e.toJson()).toList(),
      };
}

class StoreMatch {
  final int id;
  final String name;
  final String? phone;
  final String? address;
  final double? lat;
  final double? lng;
  final bool isDefault;
  final double? distanceM;
  const StoreMatch({required this.id, required this.name, this.phone, this.address, this.lat, this.lng, required this.isDefault, this.distanceM});
  factory StoreMatch.fromJson(Map<String, dynamic> j) => StoreMatch(
        id: (j['id'] as num).toInt(),
        name: j['name'] as String,
        phone: j['phone'] as String?,
        address: j['address'] as String?,
        lat: (j['lat'] as num?)?.toDouble(),
        lng: (j['lng'] as num?)?.toDouble(),
        isDefault: j['is_default'] as bool,
        distanceM: (j['distance_m'] as num?)?.toDouble(),
      );
  Map<String, dynamic> toJson() => {
        'id': id,
        'name': name,
        if (phone != null) 'phone': phone,
        if (address != null) 'address': address,
        if (lat != null) 'lat': lat,
        if (lng != null) 'lng': lng,
        'is_default': isDefault,
        'distance_m': distanceM,
      };
}

class StoreResolveResult {
  final StoreMatchType matchType;
  final List<StoreMatch> stores;
  const StoreResolveResult({required this.matchType, required this.stores});
  factory StoreResolveResult.fromJson(Map<String, dynamic> j) => StoreResolveResult(
        matchType: j['match_type'] as String,
        stores: (j['stores'] as List).map((e) => StoreMatch.fromJson(e as Map<String, dynamic>)).toList(),
      );
  Map<String, dynamic> toJson() => {
        'match_type': matchType,
        'stores': stores.map((e) => e.toJson()).toList(),
      };
}

/// 只给要改的字段。**改不了两样**：`fence` 走 `PUT .../fence`（要过
class StoreUpdateRequest {
  final int? regionId;
  final String? code;
  final String? name;
  final String? phone;
  final String? province;
  final String? city;
  final String? district;
  final String? address;
  final double? lat;
  final double? lng;
  final int? status;
  const StoreUpdateRequest({this.regionId, this.code, this.name, this.phone, this.province, this.city, this.district, this.address, this.lat, this.lng, this.status});
  factory StoreUpdateRequest.fromJson(Map<String, dynamic> j) => StoreUpdateRequest(
        regionId: (j['region_id'] as num?)?.toInt(),
        code: j['code'] as String?,
        name: j['name'] as String?,
        phone: j['phone'] as String?,
        province: j['province'] as String?,
        city: j['city'] as String?,
        district: j['district'] as String?,
        address: j['address'] as String?,
        lat: (j['lat'] as num?)?.toDouble(),
        lng: (j['lng'] as num?)?.toDouble(),
        status: (j['status'] as num?)?.toInt(),
      );
  Map<String, dynamic> toJson() => {
        if (regionId != null) 'region_id': regionId,
        if (code != null) 'code': code,
        if (name != null) 'name': name,
        if (phone != null) 'phone': phone,
        if (province != null) 'province': province,
        if (city != null) 'city': city,
        if (district != null) 'district': district,
        if (address != null) 'address': address,
        if (lat != null) 'lat': lat,
        if (lng != null) 'lng': lng,
        if (status != null) 'status': status,
      };
}

class Upload {
  final int id;
  final String url;
  final String contentType;
  final int sizeBytes;
  final String createdAt;
  const Upload({required this.id, required this.url, required this.contentType, required this.sizeBytes, required this.createdAt});
  factory Upload.fromJson(Map<String, dynamic> j) => Upload(
        id: (j['id'] as num).toInt(),
        url: j['url'] as String,
        contentType: j['content_type'] as String,
        sizeBytes: (j['size_bytes'] as num).toInt(),
        createdAt: j['created_at'] as String,
      );
  Map<String, dynamic> toJson() => {
        'id': id,
        'url': url,
        'content_type': contentType,
        'size_bytes': sizeBytes,
        'created_at': createdAt,
      };
}

/// 文件用途，取值与 `uploads.purpose` 逐值一致：
typedef UploadTarget = int;

class UserCoupon {
  final int id;
  final String couponCode;
  final int templateId;
  final String name;
  final CouponType couponType;
  final Money thresholdCents;
  final Money discountCents;
  final int discountRate;
  final Money maxDiscountCents;
  final int status;
  final int source;
  final String validStartAt;
  final String validEndAt;
  final String? usedAt;
  final List<CouponScope> scopes;
  const UserCoupon({required this.id, required this.couponCode, required this.templateId, required this.name, required this.couponType, required this.thresholdCents, required this.discountCents, required this.discountRate, required this.maxDiscountCents, required this.status, required this.source, required this.validStartAt, required this.validEndAt, this.usedAt, required this.scopes});
  factory UserCoupon.fromJson(Map<String, dynamic> j) => UserCoupon(
        id: (j['id'] as num).toInt(),
        couponCode: j['coupon_code'] as String,
        templateId: (j['template_id'] as num).toInt(),
        name: j['name'] as String,
        couponType: (j['coupon_type'] as num).toInt(),
        thresholdCents: (j['threshold_cents'] as num).toInt(),
        discountCents: (j['discount_cents'] as num).toInt(),
        discountRate: (j['discount_rate'] as num).toInt(),
        maxDiscountCents: (j['max_discount_cents'] as num).toInt(),
        status: (j['status'] as num).toInt(),
        source: (j['source'] as num).toInt(),
        validStartAt: j['valid_start_at'] as String,
        validEndAt: j['valid_end_at'] as String,
        usedAt: j['used_at'] as String?,
        scopes: (j['scopes'] as List).map((e) => CouponScope.fromJson(e as Map<String, dynamic>)).toList(),
      );
  Map<String, dynamic> toJson() => {
        'id': id,
        'coupon_code': couponCode,
        'template_id': templateId,
        'name': name,
        'coupon_type': couponType,
        'threshold_cents': thresholdCents,
        'discount_cents': discountCents,
        'discount_rate': discountRate,
        'max_discount_cents': maxDiscountCents,
        'status': status,
        'source': source,
        'valid_start_at': validStartAt,
        'valid_end_at': validEndAt,
        if (usedAt != null) 'used_at': usedAt,
        'scopes': scopes.map((e) => e.toJson()).toList(),
      };
}

class UserIdentity {
  final int id;
  final IdentityProvider provider;
  final bool? hasUnionId;
  final String createdAt;
  const UserIdentity({required this.id, required this.provider, this.hasUnionId, required this.createdAt});
  factory UserIdentity.fromJson(Map<String, dynamic> j) => UserIdentity(
        id: (j['id'] as num).toInt(),
        provider: (j['provider'] as num).toInt(),
        hasUnionId: j['has_union_id'] as bool?,
        createdAt: j['created_at'] as String,
      );
  Map<String, dynamic> toJson() => {
        'id': id,
        'provider': provider,
        if (hasUnionId != null) 'has_union_id': hasUnionId,
        'created_at': createdAt,
      };
}

/// GET /products 的 query 参数
class ListProductsQuery {
  final int? page;
  final int? pageSize;
  final int? storeId;
  final bool? inStockOnly;
  final int? categoryId;
  final String? sort;
  final int? minPriceCents;
  final int? maxPriceCents;
  const ListProductsQuery({this.page, this.pageSize, this.storeId, this.inStockOnly, this.categoryId, this.sort, this.minPriceCents, this.maxPriceCents});
  factory ListProductsQuery.fromJson(Map<String, dynamic> j) => ListProductsQuery(
        page: (j['page'] as num?)?.toInt(),
        pageSize: (j['page_size'] as num?)?.toInt(),
        storeId: (j['store_id'] as num?)?.toInt(),
        inStockOnly: j['in_stock_only'] as bool?,
        categoryId: (j['category_id'] as num?)?.toInt(),
        sort: j['sort'] as String?,
        minPriceCents: (j['min_price_cents'] as num?)?.toInt(),
        maxPriceCents: (j['max_price_cents'] as num?)?.toInt(),
      );
  Map<String, dynamic> toJson() => {
        if (page != null) 'page': page,
        if (pageSize != null) 'page_size': pageSize,
        if (storeId != null) 'store_id': storeId,
        if (inStockOnly != null) 'in_stock_only': inStockOnly,
        if (categoryId != null) 'category_id': categoryId,
        if (sort != null) 'sort': sort,
        if (minPriceCents != null) 'min_price_cents': minPriceCents,
        if (maxPriceCents != null) 'max_price_cents': maxPriceCents,
      };
}

/// GET /products 的 200 响应体
class ListProductsResponse {
  final int page;
  final int pageSize;
  final int total;
  final List<ProductSummary> items;
  final StoreContext store;
  const ListProductsResponse({required this.page, required this.pageSize, required this.total, required this.items, required this.store});
  factory ListProductsResponse.fromJson(Map<String, dynamic> j) => ListProductsResponse(
        page: (j['page'] as num).toInt(),
        pageSize: (j['page_size'] as num).toInt(),
        total: (j['total'] as num).toInt(),
        items: (j['items'] as List).map((e) => ProductSummary.fromJson(e as Map<String, dynamic>)).toList(),
        store: StoreContext.fromJson(j['store'] as Map<String, dynamic>),
      );
  Map<String, dynamic> toJson() => {
        'page': page,
        'page_size': pageSize,
        'total': total,
        'items': items.map((e) => e.toJson()).toList(),
        'store': store.toJson(),
      };
}

/// GET /products/{product_id} 的 query 参数
class GetProductQuery {
  final int? storeId;
  const GetProductQuery({this.storeId});
  factory GetProductQuery.fromJson(Map<String, dynamic> j) => GetProductQuery(
        storeId: (j['store_id'] as num?)?.toInt(),
      );
  Map<String, dynamic> toJson() => {
        if (storeId != null) 'store_id': storeId,
      };
}

/// POST /auth/login 的请求体
class LoginRequest {
  final String phone;
  final String? code;
  final String? password;
  const LoginRequest({required this.phone, this.code, this.password});
  factory LoginRequest.fromJson(Map<String, dynamic> j) => LoginRequest(
        phone: j['phone'] as String,
        code: j['code'] as String?,
        password: j['password'] as String?,
      );
  Map<String, dynamic> toJson() => {
        'phone': phone,
        if (code != null) 'code': code,
        if (password != null) 'password': password,
      };
}

/// POST /auth/refresh 的请求体
class RefreshTokenRequest {
  final String refreshToken;
  const RefreshTokenRequest({required this.refreshToken});
  factory RefreshTokenRequest.fromJson(Map<String, dynamic> j) => RefreshTokenRequest(
        refreshToken: j['refresh_token'] as String,
      );
  Map<String, dynamic> toJson() => {
        'refresh_token': refreshToken,
      };
}

/// GET /orders 的 query 参数
class ListOrdersQuery {
  final int? page;
  final int? pageSize;
  final OrderStatus? status;
  final OrderRefundStatus? refundStatus;
  const ListOrdersQuery({this.page, this.pageSize, this.status, this.refundStatus});
  factory ListOrdersQuery.fromJson(Map<String, dynamic> j) => ListOrdersQuery(
        page: (j['page'] as num?)?.toInt(),
        pageSize: (j['page_size'] as num?)?.toInt(),
        status: (j['status'] as num?)?.toInt(),
        refundStatus: (j['refund_status'] as num?)?.toInt(),
      );
  Map<String, dynamic> toJson() => {
        if (page != null) 'page': page,
        if (pageSize != null) 'page_size': pageSize,
        if (status != null) 'status': status,
        if (refundStatus != null) 'refund_status': refundStatus,
      };
}

/// GET /orders 的 200 响应体
class ListOrdersResponse {
  final int page;
  final int pageSize;
  final int total;
  final List<Order> items;
  const ListOrdersResponse({required this.page, required this.pageSize, required this.total, required this.items});
  factory ListOrdersResponse.fromJson(Map<String, dynamic> j) => ListOrdersResponse(
        page: (j['page'] as num).toInt(),
        pageSize: (j['page_size'] as num).toInt(),
        total: (j['total'] as num).toInt(),
        items: (j['items'] as List).map((e) => Order.fromJson(e as Map<String, dynamic>)).toList(),
      );
  Map<String, dynamic> toJson() => {
        'page': page,
        'page_size': pageSize,
        'total': total,
        'items': items.map((e) => e.toJson()).toList(),
      };
}

/// POST /search 的 200 响应体
class SearchResponse {
  final List<SearchHit> items;
  final StoreContext store;
  final int latencyMs;
  final int? total;
  final String strategy;
  final String? traceId;
  const SearchResponse({required this.items, required this.store, required this.latencyMs, this.total, required this.strategy, this.traceId});
  factory SearchResponse.fromJson(Map<String, dynamic> j) => SearchResponse(
        items: (j['items'] as List).map((e) => SearchHit.fromJson(e as Map<String, dynamic>)).toList(),
        store: StoreContext.fromJson(j['store'] as Map<String, dynamic>),
        latencyMs: (j['latency_ms'] as num).toInt(),
        total: (j['total'] as num?)?.toInt(),
        strategy: j['strategy'] as String,
        traceId: j['trace_id'] as String?,
      );
  Map<String, dynamic> toJson() => {
        'items': items.map((e) => e.toJson()).toList(),
        'store': store.toJson(),
        'latency_ms': latencyMs,
        if (total != null) 'total': total,
        'strategy': strategy,
        if (traceId != null) 'trace_id': traceId,
      };
}

/// POST /search/events 的请求体
class ReportSearchEventRequest {
  final String traceId;
  final String event;
  final int productId;
  const ReportSearchEventRequest({required this.traceId, required this.event, required this.productId});
  factory ReportSearchEventRequest.fromJson(Map<String, dynamic> j) => ReportSearchEventRequest(
        traceId: j['trace_id'] as String,
        event: j['event'] as String,
        productId: (j['product_id'] as num).toInt(),
      );
  Map<String, dynamic> toJson() => {
        'trace_id': traceId,
        'event': event,
        'product_id': productId,
      };
}

/// GET /coupon-templates 的 query 参数
class ListCouponTemplatesQuery {
  final int? page;
  final int? pageSize;
  const ListCouponTemplatesQuery({this.page, this.pageSize});
  factory ListCouponTemplatesQuery.fromJson(Map<String, dynamic> j) => ListCouponTemplatesQuery(
        page: (j['page'] as num?)?.toInt(),
        pageSize: (j['page_size'] as num?)?.toInt(),
      );
  Map<String, dynamic> toJson() => {
        if (page != null) 'page': page,
        if (pageSize != null) 'page_size': pageSize,
      };
}

/// GET /coupon-templates 的 200 响应体
class ListCouponTemplatesResponse {
  final int page;
  final int pageSize;
  final int total;
  final List<ClaimableCouponTemplate> items;
  const ListCouponTemplatesResponse({required this.page, required this.pageSize, required this.total, required this.items});
  factory ListCouponTemplatesResponse.fromJson(Map<String, dynamic> j) => ListCouponTemplatesResponse(
        page: (j['page'] as num).toInt(),
        pageSize: (j['page_size'] as num).toInt(),
        total: (j['total'] as num).toInt(),
        items: (j['items'] as List).map((e) => ClaimableCouponTemplate.fromJson(e as Map<String, dynamic>)).toList(),
      );
  Map<String, dynamic> toJson() => {
        'page': page,
        'page_size': pageSize,
        'total': total,
        'items': items.map((e) => e.toJson()).toList(),
      };
}

/// GET /coupons 的 query 参数
class ListCouponsQuery {
  final int? page;
  final int? pageSize;
  final String? status;
  const ListCouponsQuery({this.page, this.pageSize, this.status});
  factory ListCouponsQuery.fromJson(Map<String, dynamic> j) => ListCouponsQuery(
        page: (j['page'] as num?)?.toInt(),
        pageSize: (j['page_size'] as num?)?.toInt(),
        status: j['status'] as String?,
      );
  Map<String, dynamic> toJson() => {
        if (page != null) 'page': page,
        if (pageSize != null) 'page_size': pageSize,
        if (status != null) 'status': status,
      };
}

/// GET /coupons 的 200 响应体
class ListCouponsResponse {
  final int page;
  final int pageSize;
  final int total;
  final List<UserCoupon> items;
  const ListCouponsResponse({required this.page, required this.pageSize, required this.total, required this.items});
  factory ListCouponsResponse.fromJson(Map<String, dynamic> j) => ListCouponsResponse(
        page: (j['page'] as num).toInt(),
        pageSize: (j['page_size'] as num).toInt(),
        total: (j['total'] as num).toInt(),
        items: (j['items'] as List).map((e) => UserCoupon.fromJson(e as Map<String, dynamic>)).toList(),
      );
  Map<String, dynamic> toJson() => {
        'page': page,
        'page_size': pageSize,
        'total': total,
        'items': items.map((e) => e.toJson()).toList(),
      };
}

/// PATCH /me 的请求体
class UpdateMeRequest {
  final String? nickname;
  final String? avatarUrl;
  final int? gender;
  const UpdateMeRequest({this.nickname, this.avatarUrl, this.gender});
  factory UpdateMeRequest.fromJson(Map<String, dynamic> j) => UpdateMeRequest(
        nickname: j['nickname'] as String?,
        avatarUrl: j['avatar_url'] as String?,
        gender: (j['gender'] as num?)?.toInt(),
      );
  Map<String, dynamic> toJson() => {
        if (nickname != null) 'nickname': nickname,
        if (avatarUrl != null) 'avatar_url': avatarUrl,
        if (gender != null) 'gender': gender,
      };
}

/// GET /cart 的 query 参数
class GetCartQuery {
  final int? storeId;
  final int? addressId;
  const GetCartQuery({this.storeId, this.addressId});
  factory GetCartQuery.fromJson(Map<String, dynamic> j) => GetCartQuery(
        storeId: (j['store_id'] as num?)?.toInt(),
        addressId: (j['address_id'] as num?)?.toInt(),
      );
  Map<String, dynamic> toJson() => {
        if (storeId != null) 'store_id': storeId,
        if (addressId != null) 'address_id': addressId,
      };
}

/// POST /cart/items 的 query 参数
class AddCartItemQuery {
  final int? storeId;
  final int? addressId;
  const AddCartItemQuery({this.storeId, this.addressId});
  factory AddCartItemQuery.fromJson(Map<String, dynamic> j) => AddCartItemQuery(
        storeId: (j['store_id'] as num?)?.toInt(),
        addressId: (j['address_id'] as num?)?.toInt(),
      );
  Map<String, dynamic> toJson() => {
        if (storeId != null) 'store_id': storeId,
        if (addressId != null) 'address_id': addressId,
      };
}

/// POST /cart/items 的请求体
class AddCartItemRequest {
  final int skuId;
  final int quantity;
  const AddCartItemRequest({required this.skuId, required this.quantity});
  factory AddCartItemRequest.fromJson(Map<String, dynamic> j) => AddCartItemRequest(
        skuId: (j['sku_id'] as num).toInt(),
        quantity: (j['quantity'] as num).toInt(),
      );
  Map<String, dynamic> toJson() => {
        'sku_id': skuId,
        'quantity': quantity,
      };
}

/// PATCH /cart/items/{item_id} 的 query 参数
class UpdateCartItemQuery {
  final int? storeId;
  final int? addressId;
  const UpdateCartItemQuery({this.storeId, this.addressId});
  factory UpdateCartItemQuery.fromJson(Map<String, dynamic> j) => UpdateCartItemQuery(
        storeId: (j['store_id'] as num?)?.toInt(),
        addressId: (j['address_id'] as num?)?.toInt(),
      );
  Map<String, dynamic> toJson() => {
        if (storeId != null) 'store_id': storeId,
        if (addressId != null) 'address_id': addressId,
      };
}

/// PATCH /cart/items/{item_id} 的请求体
class UpdateCartItemRequest {
  final int? quantity;
  final bool? selected;
  const UpdateCartItemRequest({this.quantity, this.selected});
  factory UpdateCartItemRequest.fromJson(Map<String, dynamic> j) => UpdateCartItemRequest(
        quantity: (j['quantity'] as num?)?.toInt(),
        selected: j['selected'] as bool?,
      );
  Map<String, dynamic> toJson() => {
        if (quantity != null) 'quantity': quantity,
        if (selected != null) 'selected': selected,
      };
}

/// PUT /cart/selection 的 query 参数
class SelectCartItemsQuery {
  final int? storeId;
  final int? addressId;
  const SelectCartItemsQuery({this.storeId, this.addressId});
  factory SelectCartItemsQuery.fromJson(Map<String, dynamic> j) => SelectCartItemsQuery(
        storeId: (j['store_id'] as num?)?.toInt(),
        addressId: (j['address_id'] as num?)?.toInt(),
      );
  Map<String, dynamic> toJson() => {
        if (storeId != null) 'store_id': storeId,
        if (addressId != null) 'address_id': addressId,
      };
}

/// PUT /cart/selection 的请求体
class SelectCartItemsRequest {
  final bool selected;
  final List<int>? itemIds;
  const SelectCartItemsRequest({required this.selected, this.itemIds});
  factory SelectCartItemsRequest.fromJson(Map<String, dynamic> j) => SelectCartItemsRequest(
        selected: j['selected'] as bool,
        itemIds: (j['item_ids'] as List?)?.map((e) => (e as num).toInt()).toList(),
      );
  Map<String, dynamic> toJson() => {
        'selected': selected,
        if (itemIds != null) 'item_ids': itemIds,
      };
}

/// POST /cart/items/batch-delete 的 query 参数
class BatchDeleteCartItemsQuery {
  final int? storeId;
  final int? addressId;
  const BatchDeleteCartItemsQuery({this.storeId, this.addressId});
  factory BatchDeleteCartItemsQuery.fromJson(Map<String, dynamic> j) => BatchDeleteCartItemsQuery(
        storeId: (j['store_id'] as num?)?.toInt(),
        addressId: (j['address_id'] as num?)?.toInt(),
      );
  Map<String, dynamic> toJson() => {
        if (storeId != null) 'store_id': storeId,
        if (addressId != null) 'address_id': addressId,
      };
}

/// POST /cart/items/batch-delete 的请求体
class BatchDeleteCartItemsRequest {
  final List<int>? itemIds;
  final bool? selected;
  const BatchDeleteCartItemsRequest({this.itemIds, this.selected});
  factory BatchDeleteCartItemsRequest.fromJson(Map<String, dynamic> j) => BatchDeleteCartItemsRequest(
        itemIds: (j['item_ids'] as List?)?.map((e) => (e as num).toInt()).toList(),
        selected: j['selected'] as bool?,
      );
  Map<String, dynamic> toJson() => {
        if (itemIds != null) 'item_ids': itemIds,
        if (selected != null) 'selected': selected,
      };
}

/// GET /refunds 的 query 参数
class ListRefundsQuery {
  final int? page;
  final int? pageSize;
  final RefundStatus? status;
  const ListRefundsQuery({this.page, this.pageSize, this.status});
  factory ListRefundsQuery.fromJson(Map<String, dynamic> j) => ListRefundsQuery(
        page: (j['page'] as num?)?.toInt(),
        pageSize: (j['page_size'] as num?)?.toInt(),
        status: (j['status'] as num?)?.toInt(),
      );
  Map<String, dynamic> toJson() => {
        if (page != null) 'page': page,
        if (pageSize != null) 'page_size': pageSize,
        if (status != null) 'status': status,
      };
}

/// GET /refunds 的 200 响应体
class ListRefundsResponse {
  final int page;
  final int pageSize;
  final int total;
  final List<Refund> items;
  const ListRefundsResponse({required this.page, required this.pageSize, required this.total, required this.items});
  factory ListRefundsResponse.fromJson(Map<String, dynamic> j) => ListRefundsResponse(
        page: (j['page'] as num).toInt(),
        pageSize: (j['page_size'] as num).toInt(),
        total: (j['total'] as num).toInt(),
        items: (j['items'] as List).map((e) => Refund.fromJson(e as Map<String, dynamic>)).toList(),
      );
  Map<String, dynamic> toJson() => {
        'page': page,
        'page_size': pageSize,
        'total': total,
        'items': items.map((e) => e.toJson()).toList(),
      };
}

/// GET /me/notifications 的 query 参数
class ListNotificationsQuery {
  final int? page;
  final int? pageSize;
  final bool? unreadOnly;
  const ListNotificationsQuery({this.page, this.pageSize, this.unreadOnly});
  factory ListNotificationsQuery.fromJson(Map<String, dynamic> j) => ListNotificationsQuery(
        page: (j['page'] as num?)?.toInt(),
        pageSize: (j['page_size'] as num?)?.toInt(),
        unreadOnly: j['unread_only'] as bool?,
      );
  Map<String, dynamic> toJson() => {
        if (page != null) 'page': page,
        if (pageSize != null) 'page_size': pageSize,
        if (unreadOnly != null) 'unread_only': unreadOnly,
      };
}
