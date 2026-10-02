package repository

import (
	"errors"
	"fmt"
	"time"
)

// 商家写路径（契约 /admin/ 下的 16 条写接口）在 repository 边界上的领域类型与
// 失败取值。M4 Task 2。
//
// ===========================================================================
// 为什么这一层要有这么多 sentinel，而不是一个 error
// ===========================================================================
//
// 因为契约在这些路径上把失败分成了**响应码不同、调用方行为不同**的几类，而
// 数据库给出的信号在其中好几处**完全一样**：一次 UPDATE 影响 0 行。
//
//	改库存：rows = 0 既可能是 CAS 对不上（409，重读后重试会成功），
//	        也可能是这个 SKU 根本不在本租户（404，重试永远不会成功）。
//	改商品：rows = 0 既可能是这条商品不存在（404），也可能是它已被软删（409）。
//	删商品：rows = 0 既可能是已经删过了（404），也可能是它还在架（409）。
//
// 每一处「混为一谈」的代价都不是「错误信息不够漂亮」：把 404 报成 409，
// 调用方会对着一个永远不会成功的请求无限重试；把 409 报成 404，后台页面会
// 显示「这个商品不见了」，而它就在那里。
//
// 做成 sentinel 而不是 (ok bool, err error)：布尔只有两个取值，第三种情形一定
// 会被挤进其中一个，而挤进去的那一刻没有任何东西会报警。这条理由与
// inventory.go 里 ErrInsufficientStock / ErrSKUNotInTenant 那一对是同一条。
var (
	// ErrCatalogNotFound：这个 id 在当前租户下查不到 —— 不存在，或者属于别家店。
	// 契约在每一条 /admin/ 路径上都把这两者合并成 404 而不是 403，理由写在
	// OpenAPI 那一段的段头：403 会让自增 id 空间变成一个跨租户的存在性探针。
	ErrCatalogNotFound = errors.New("目标在当前租户下不存在")

	// ErrProductDeleted：商品已软删，不接受修改（契约 409 product-deleted）。
	ErrProductDeleted = errors.New("商品已软删")

	// ErrProductStillPublished：在架商品不许删，先下架
	// （契约 409 product-still-published）。理由不是洁癖：删一个仍在前台列表与
	// 检索结果里的商品，会让一个正在下单的买家在 SAGA 中途撞上一个消失的商品，
	// 而那条路上的补偿分支处理的是「库存不足」，不是「商品没了」。
	ErrProductStillPublished = errors.New("商品仍在架，不能删")

	// ErrProductHasNoSKU：一个 SKU 都没有的商品不能上架
	// （契约 409 product-has-no-sku）。上架意味着它会进前台列表与检索结果，
	// 而 min_price_cents 此时是 0，前台会把它渲染成免费。
	ErrProductHasNoSKU = errors.New("商品没有任何 SKU，不能上架")

	// ErrSKUCodeDuplicated：该租户内已有同一个货号
	// （契约 409 sku-code-duplicated）。判据是 uk_skus_code 这条**部分**唯一
	// 索引，它只管未软删的行 —— 软删掉的规格不占货号。
	ErrSKUCodeDuplicated = errors.New("货号在本租户内已存在")

	// ErrSKULastOfPublishedProduct：这是某个在架商品的最后一个 SKU
	// （契约 409 sku-last-of-published-product）。放行的话，一个前台可见的商品
	// 会变成没有任何可买规格 —— 与「没有 SKU 不能上架」是同一条闸门的另一侧。
	ErrSKULastOfPublishedProduct = errors.New("这是在架商品的最后一个 SKU")

	// ErrInventoryPrecondition：CAS 对不上（契约 409
	// inventory-precondition-failed）。**它和 ErrSKUNotInTenant 是两件事**，
	// 见 InventoryConflict 的注释。
	ErrInventoryPrecondition = errors.New("库存的 expected_available_qty 与当前值不符")

	// ErrUploadNotFound：这个 upload_id 不存在或不属于当前租户
	// （契约 422 upload-not-found）。
	ErrUploadNotFound = errors.New("upload 不存在或不属于当前租户")

	// ErrUploadWrongPurpose：这个 upload 的 purpose 不是 1 商品图
	// （契约 422 upload-wrong-purpose）—— 比如拿一张退款凭证当商品图挂上去。
	// **数据库挡不住这一件事**：复合外键挡的是跨租户，不是用途。
	ErrUploadWrongPurpose = errors.New("upload 的用途不是商品图")

	// ErrProductImageDuplicated：同一个 upload_id 在一次整组替换里出现了两次
	// （契约 422 product-image-duplicated）。
	//
	// 不靠 UNIQUE (merchant_id, product_id, upload_id) 去撞：那条唯一约束确实
	// 会拒绝，但它给出的是 23505，与「货号撞车」在 Go 侧是同一种错误，
	// 要靠约束名再分一次；而更要紧的是，撞上它的时候 ClearProductImages 已经
	// 执行过了 —— 事务会回滚，所以数据是对的，但「先毁掉再发现输入不合法」
	// 这个形状不该留在代码里。
	ErrProductImageDuplicated = errors.New("同一个 upload_id 出现了两次")

	// ErrCategoryHasChildren：分类下还有未软删的子分类（契约 409
	// category-has-children）。复合外键不会拦 —— 软删不删行，父子引用在数据库
	// 看来一直是完整的，所以这一条只能在这里挡。
	ErrCategoryHasChildren = errors.New("分类下还有子分类")

	// ErrCategoryHasProducts：分类下还有未软删的商品（契约 409
	// category-has-products）。放行的话，前台目录树里找不到这些商品，
	// 而它们仍在架、仍能被搜到、仍能下单。
	ErrCategoryHasProducts = errors.New("分类下还有商品")

	// ErrCategoryCycle：目标父节点是自己或自己的后代，移动会形成环
	// （契约 409 category-cycle）。没有这条，一次误操作就能把一棵子树从树上
	// 摘下来变成一个独立的环，而 path 的重写会在那个环上跑不完。
	ErrCategoryCycle = errors.New("移动分类会形成环")
)

// InventoryConflict 是 CAS 失败时**带着当前真实值**的错误。
//
// 契约的 InventoryConflict schema 要求 Problem 里带一个 current 字段，理由写在
// 那条端点上：省掉调用方「先重查再重试」的那一跳，而那一跳本身又是一次会过期
// 的读。所以这里不是一个裸 sentinel，而是一个带数据的错误类型。
//
// Unwrap 让 errors.Is(err, ErrInventoryPrecondition) 成立，errors.As 取到值。
// 两种用法都要有：服务层用 Is 决定响应码，用 As 填 current。
type InventoryConflict struct {
	SKUID    int64
	Expected int32
	Current  Inventory
}

func (e *InventoryConflict) Error() string {
	return fmt.Sprintf("sku %d: 期望 available_qty = %d，当前是 %d",
		e.SKUID, e.Expected, e.Current.AvailableQty)
}

func (e *InventoryConflict) Unwrap() error { return ErrInventoryPrecondition }

// AdminProduct 是后台视角的商品（契约 AdminProduct）。
//
// 与前台的 Product 分成两个类型而不是加几个字段：前台那个是**对买家**的视图，
// 里面没有草稿状态、没有软删标记；把它们加进去，等于让生成出来的买家客户端
// 带着一组它永远收不到、也不该收到的字段 —— 而客户端会照着类型去渲染。
type AdminProduct struct {
	ID            int64
	CategoryID    int64
	BrandID       *int64
	Title         string
	Subtitle      *string
	Description   *string
	MinPriceCents int64
	MaxPriceCents int64
	TotalStock    int32
	SalesCount    int32
	Status        int16
	PublishedAt   *time.Time
	DeletedAt     *time.Time
	CreatedAt     time.Time
	UpdatedAt     time.Time

	// FreightTemplateID 是商品单独挂的运费模板（00055）；nil = 不单独挂。
	FreightTemplateID *int64

	// ManagedBy 是管这件商品的渠道（启用中的商品源，契约 AdminProduct.managed_by）。仓储不填：
	// service 在列表 / 详情 / PATCH 里经 ChannelService.ManagedBy 填上；nil = keel 自己管。
	ManagedBy *string
}

// AdminSKU 是后台视角的规格（契约 AdminSku）。
//
// CostCents 是成本，**只在后台接口里出现** —— 它出现在任何一个前台响应里
// 都是一次商业信息泄露。SpecValues 是 JSONB 原样的字节：这一层不解释它。
type AdminSKU struct {
	ID           int64
	ProductID    int64
	SKUCode      string
	SpecValues   []byte
	PriceCents   int64
	CostCents    int64
	WeightGram   int32
	ImageURL     *string
	Status       int16
	AvailableQty int32
	WarningQty   int32
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// AdminCategory 是后台视角的分类（契约 AdminCategory），**扁平**，不嵌 children。
// Path 与 Level 由服务端维护，不接受写入：它们是 idx_categories_path 的内容。
type AdminCategory struct {
	ID        int64
	ParentID  *int64
	Name      string
	Path      string
	Level     int16
	SortOrder int32
	Status    int16
	DeletedAt *time.Time
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Inventory 是一行库存（契约 AdminInventory）。
type Inventory struct {
	SKUID int64

	// StoreID 这行水位属于哪家门店。00020 之后 inventories 的主键是
	// (sku_id, store_id)，同一个 SKU 会有好几行 —— 契约把 AdminInventory.store_id
	// 定成**必返**，正是因为一个不写明属于谁的水位在多门店之后没有意义。
	//
	// 这条路径（PUT /admin/skus/{sku_id}/inventory）自己不带 store_id，
	// 它由「本租户恰好一家门店」推出来（SoleStore）—— 但推出来之后必须
	// **原样回给调用方**：调用方据此知道自己刚改的是哪一家，
	// 而它哪天开第二家店时，同一个请求会 409 store-ambiguous 而不是猜一家。
	StoreID      int64
	AvailableQty int32
	WarningQty   int32
	UpdatedAt    time.Time
}

// ProductImage 是商品图与商品的一条关联（契约 ProductImage 的库内那一半）。
//
// **没有 IsPrimary**：主图就是 SortOrder 最小的那一张。一个布尔允许「零张主图」
// 和「两张主图」这两种业务上不存在的状态被表达出来，而顺序本来就要维护。
//
// URL 不在这里：它是 /api/v1/uploads/{upload_id} 这个形状的东西，由服务层从
// UploadID 拼出来 —— repository 认得的是列，不是对外路由。
type ProductImage struct {
	ID        int64
	ProductID int64
	UploadID  int64
	SortOrder int32
}

// Upload 是一条文件元数据（数据模型 §13）。
//
// 没有 MerchantID 字段，这不是漏了：db/queries 里连这个词都不许出现
// （scripts/check_query_tenancy.py），所以它根本没被 SELECT 出来。
// 一行能被读出来，本身就已经证明它属于当前租户 —— 那正是 RLS 的意思。
type Upload struct {
	ID          int64
	UserID      *int64
	StaffID     *int64
	Purpose     int16
	Driver      int16
	StorageKey  string
	ContentType string
	SizeBytes   int64
	SHA256      string
	Referenced  bool
	CreatedAt   time.Time
}

// uploads.purpose 的三个取值，与数据模型 §13 和契约的 UploadTarget 逐值一致。
//
// 写入面只用得上第一个：头像（2）与退款凭证（3）走 C 端那条 POST /uploads，
// 而那条还没有实现。另外两个仍然定在这里，因为**读**那条路用得上它们 ——
// GET /uploads/{upload_id} 要按 purpose 判谁可读（service/upload.go 的
// publicPurposes），而一张用魔数 1/2/3 写的准入表，谁也没法在 review 里
// 一眼看出它放行了什么。
const (
	UploadPurposeProductImage int16 = 1 // 商品图
	UploadPurposeAvatar       int16 = 2 // 头像
	UploadPurposeRefundProof  int16 = 3 // 退款凭证
)

// AdminCatalogTx 把商家写路径的四个面合成一个，嵌进 Tx（见 product.go）。
//
// 分成四个子接口而不是一张平铺的方法表，理由与 Tx 自己一样：同一时期有好几条
// 任务在往这一层加能力，平铺意味着他们改的是同一处声明的同一批行。
type AdminCatalogTx interface {
	AdminProductTx
	AdminSKUTx
	AdminCategoryTx
	UploadTx
}
