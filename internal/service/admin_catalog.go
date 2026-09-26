package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/keel/keel/internal/auth"
	"github.com/keel/keel/internal/repository"
	"github.com/keel/keel/internal/tenant"
)

// 商家自助发布那 16 条写接口的业务层（契约 Admin + Catalog 两个 tag）。M4 Task 3。
//
// ===========================================================================
// 这一层**刻意不重新定义一套错误**
// ===========================================================================
//
// repository 那一层已经把失败分成了与契约一一对应的十几个 sentinel
// （internal/repository/admin_catalog.go），而且每一个都做过变异验证。
// 在这里再定义一份 service.ErrXxx 再逐条转译，等于造出第二张会与第一张分叉
// 的映射表 —— 而它们分叉的那天，某一条 409 会变成 404，没有任何东西会红。
//
// 所以这一层**原样放行** repository 的 sentinel，由 handler 的
// writeCatalogError 一次性翻成状态码。下面这三个是这一层自己产生的失败，
// repository 看不到它们（它收到的已经是校验过的值）：
var (
	// ErrCatalogBadRequest 是请求体本身不成立：一个字段都没传
	// （契约里三条 PATCH 都是 minProperties: 1）、字段超长、数量为负。
	// handler 翻成 422。
	ErrCatalogBadRequest = errors.New("请求参数不合法")

	// ErrUploadTooLarge 是文件超过 10 MB（契约 413）。
	ErrUploadTooLarge = errors.New("文件超过大小上限")

	// ErrUploadMediaType 是 content_type 不在 image/jpeg | png | webp 里
	// （契约 415）。
	ErrUploadMediaType = errors.New("不支持的文件类型")
)

// MaxUploadBytes 是单个文件的上限，与契约逐字一致（「单文件不超过 10 MB」）。
const MaxUploadBytes int64 = 10 << 20

// maxProductImages 与契约 ProductImagesReplaceRequest.images 的 maxItems 一致。
const maxProductImages = 20

// uploadURLPrefix 是 Upload.url / ProductImage.url / AdminSku.image_url 的形状，
// 契约在三处写着同一句「形如 /api/v1/uploads/{upload_id}」。
//
// 拼在这一层而不是 repository：那一层认得的是列，不是对外路由
// （repository.ProductImage 上刻意没有 URL 字段，注释写着这条）。
//
// 它指向 GET /uploads/{upload_id}（买家侧接口，M4 收尾那一轮实现了，
// 见 service/upload.go）：那一跳判完归属再 302 到一个限时地址。
// 客户端不该解析这个串，原样回传即可 —— 契约在 Upload.url 上是这么写的。
//
// 这一段原先写的是「它今天指向一条还没有实现的路由」。那句话过期了，
// 而留着一句过期的「还没实现」，下一个人会照着它去找一个已经存在的东西。
const uploadURLPrefix = "/api/v1/uploads/"

// UploadURL 按 upload id 拼出对外地址。导出给 handler 用（它要填三种响应）。
func UploadURL(id int64) string { return uploadURLPrefix + strconv.FormatInt(id, 10) }

// AdminCatalogRepository 是本服务需要的仓储能力。
//
// **只有 WithTenant，没有 WithPlatform。** 这 16 条全是商家级路径：
// 商品、SKU、库存、类目、商品图都挂在某一家店名下，平台级操作员来做这些事
// 时做的也是**某一家店的**事 —— 那家店由请求的 Host 定出来（租户中间件），
// 而 auth.StaffBearer 已经核过令牌与它对得上（平台级令牌对任何一家店放行，
// 理由写在 auth.StaffClaims.TenantMatches 上）。
//
// 给它一个 WithPlatform 入口，就等于让「以平台作用域建一件商品」成为一句
// 写得出来的代码，而平台作用域里 current_merchant() 是 NULL ——
// products.merchant_id 的 DEFAULT 会取到 NULL，那一行 INSERT 以 NOT NULL
// 违例失败。失败方向是对的，但那条错误里没有任何东西指向「作用域选错了」。
type AdminCatalogRepository interface {
	WithTenant(ctx context.Context, fn func(repository.Tx) error) error
}

// AdminCatalogService 实现那 16 条。
type AdminCatalogService struct {
	repo  AdminCatalogRepository
	store UploadStore
}

// NewAdminCatalogService 建一个。store 为 nil 时 POST /admin/uploads 会报错 ——
// 不静默退化成「只登记元数据」，理由见 upload_store.go 的文件头。
func NewAdminCatalogService(r AdminCatalogRepository, store UploadStore) *AdminCatalogService {
	return &AdminCatalogService{repo: r, store: store}
}

// requireStaff 确认这个请求真的带着后台身份。
//
// 这 16 条接口一条都不用 StaffIdentity 里的字段（除了上传要 StaffID），
// 那为什么还要取一次？**因为它是「这条路由挂没挂 auth.StaffBearer」的唯一
// 机械检查。** 漏挂的话，租户中间件仍然会把 Host 解出来，WithTenant 照常开
// 事务，于是一个匿名请求能改这家店的商品价格 —— 而它会返回 200。
//
// auth.StaffFromContext 取不到时返回 ErrNoStaff 而不是零值（那一层刻意如此：
// 零值的 MerchantID 是 nil，也就是平台级）。handler 把它归进 500：
// 它是装配 bug，不是客户端的错。
func requireStaff(ctx context.Context) (auth.StaffIdentity, error) {
	return auth.StaffFromContext(ctx)
}

// ---------------------------------------------------------------------------
// 商品
// ---------------------------------------------------------------------------

// AdminProductPage 是后台商品列表的一页。Page / PageSize 是**钳制之后**的值，
// 理由与 ProductList 那一处一字不差。
type AdminProductPage struct {
	Items    []repository.AdminProduct
	Total    int64
	Page     int
	PageSize int
}

// ListProducts 实现 GET /admin/products。
func (s *AdminCatalogService) ListProducts(ctx context.Context, page, pageSize int,
	f repository.ProductFilter) (AdminProductPage, error) {

	if _, err := requireStaff(ctx); err != nil {
		return AdminProductPage{}, err
	}
	page, pageSize = clampPaging(page, pageSize)
	f.Limit, f.Offset = int64(pageSize), offsetOf(page, pageSize)

	out := AdminProductPage{Items: []repository.AdminProduct{}, Page: page, PageSize: pageSize}
	err := s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		// 计数与取页在同一个事务里，所以 total 和 items 看到的是同一个快照 ——
		// 分开两次访问时，中间的一次建 / 删商品会让「total=21 但第二页是空的」
		// 偶发出现。理由与前台 ProductService.List 一字不差。
		total, err := tx.AdminCountProducts(ctx, f)
		if err != nil {
			return err
		}
		items, err := tx.AdminListProducts(ctx, f)
		if err != nil {
			return err
		}
		out.Total, out.Items = total, items
		return nil
	})
	if err != nil {
		return AdminProductPage{}, err
	}
	return out, nil
}

// AdminProductDetail 是后台商品详情：商品 + 全部未软删 SKU + 按顺序的图。
type AdminProductDetail struct {
	Product repository.AdminProduct
	SKUs    []repository.AdminSKU
	Images  []repository.ProductImage
}

// FindProduct 实现 GET /admin/products/{product_id}。
func (s *AdminCatalogService) FindProduct(ctx context.Context, id int64) (AdminProductDetail, error) {
	if _, err := requireStaff(ctx); err != nil {
		return AdminProductDetail{}, err
	}
	var out AdminProductDetail
	err := s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		// 三次读在同一个事务里：分开的话，中间的一次加 SKU 会让详情里
		// 「价格区间」与「规格列表」来自两个快照，而它们正是要对得上的两个数。
		p, err := tx.AdminFindProduct(ctx, id)
		if err != nil {
			return err
		}
		skus, err := tx.AdminListProductSKUs(ctx, id)
		if err != nil {
			return err
		}
		imgs, err := tx.ListProductImages(ctx, id)
		if err != nil {
			return err
		}
		out = AdminProductDetail{Product: p, SKUs: skus, Images: imgs}
		return nil
	})
	if err != nil {
		return AdminProductDetail{}, err
	}
	return out, nil
}

// CreateProduct 实现 POST /admin/products。
func (s *AdminCatalogService) CreateProduct(ctx context.Context, n repository.NewProduct) (repository.AdminProduct, error) {
	if _, err := requireStaff(ctx); err != nil {
		return repository.AdminProduct{}, err
	}
	if err := checkTitle(n.Title); err != nil {
		return repository.AdminProduct{}, err
	}
	if err := checkOptText("subtitle", n.Subtitle, 200); err != nil {
		return repository.AdminProduct{}, err
	}
	if n.CategoryID <= 0 {
		return repository.AdminProduct{}, fmt.Errorf("%w: category_id 必须是正整数", ErrCatalogBadRequest)
	}
	var out repository.AdminProduct
	err := s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		var e error
		out, e = tx.CreateProduct(ctx, n)
		return e
	})
	return out, err
}

// UpdateProduct 实现 PATCH /admin/products/{product_id}。
func (s *AdminCatalogService) UpdateProduct(ctx context.Context, id int64,
	p repository.ProductPatch) (repository.AdminProduct, error) {

	if _, err := requireStaff(ctx); err != nil {
		return repository.AdminProduct{}, err
	}
	// 契约：minProperties: 1。一个字段都没传是 422，不是「什么也不改的 200」——
	// 后者会让客户端以为自己那次编辑保存成功了。
	if p.Title == nil && p.Subtitle == nil && p.Description == nil &&
		p.CategoryID == nil && !p.SetBrandID {
		return repository.AdminProduct{}, fmt.Errorf(
			"%w: 一个字段都没传（契约 ProductUpdateRequest 是 minProperties: 1）", ErrCatalogBadRequest)
	}
	if p.Title != nil {
		if err := checkTitle(*p.Title); err != nil {
			return repository.AdminProduct{}, err
		}
	}
	if err := checkOptText("subtitle", p.Subtitle, 200); err != nil {
		return repository.AdminProduct{}, err
	}
	var out repository.AdminProduct
	err := s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		var e error
		out, e = tx.UpdateProduct(ctx, id, p)
		return e
	})
	return out, err
}

// DeleteProduct 实现 DELETE /admin/products/{product_id}。
func (s *AdminCatalogService) DeleteProduct(ctx context.Context, id int64) error {
	if _, err := requireStaff(ctx); err != nil {
		return err
	}
	return s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		return tx.SoftDeleteProduct(ctx, id)
	})
}

// SetPublication 实现 POST /admin/products/{product_id}/publication。
//
// 「没有 SKU 不能上架」那条闸门**不在这里**：它在 repository 的
// PublishProduct 里，写进 UPDATE 的 WHERE 与同一个快照里算出来的 sku_count。
// 在这一层再查一遍是两次快照 —— 中间那一瞬间最后一个 SKU 可能刚被删掉，
// 而且两份实现一定会分叉。
func (s *AdminCatalogService) SetPublication(ctx context.Context, id int64, publish bool) (repository.AdminProduct, error) {
	if _, err := requireStaff(ctx); err != nil {
		return repository.AdminProduct{}, err
	}
	var out repository.AdminProduct
	err := s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		var e error
		out, e = tx.SetProductPublication(ctx, id, publish)
		return e
	})
	return out, err
}

// ReplaceImages 实现 PUT /admin/products/{product_id}/images。
func (s *AdminCatalogService) ReplaceImages(ctx context.Context, productID int64,
	uploadIDs []int64) ([]repository.ProductImage, error) {

	if _, err := requireStaff(ctx); err != nil {
		return nil, err
	}
	if len(uploadIDs) > maxProductImages {
		return nil, fmt.Errorf("%w: 一次最多 %d 张图（契约 maxItems）",
			ErrCatalogBadRequest, maxProductImages)
	}
	// 「同一个 upload_id 出现两次」与「upload 不是本租户的 / 用途不对」都不在
	// 这里判：repository.ReplaceProductImages 在动任何一行之前就查完了，
	// 而且它对这三条各有一个 sentinel。
	var out []repository.ProductImage
	err := s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		var e error
		out, e = tx.ReplaceProductImages(ctx, productID, uploadIDs)
		return e
	})
	return out, err
}

// ---------------------------------------------------------------------------
// SKU 与库存
// ---------------------------------------------------------------------------

// NewSKUInput 是建 SKU 的入参，比 repository.NewSKU 多一个 ImageUploadID：
// 契约收的是 uploads.id，而库里那一列是 image_url。这一层负责那次翻译
// （顺带把 referenced 在同一个事务里置位）。
type NewSKUInput struct {
	SKUCode       string
	SpecValues    map[string]string
	PriceCents    int64
	CostCents     int64
	WeightGram    int32
	ImageUploadID *int64
	AvailableQty  int32
	WarningQty    int32
}

// CreateSKU 实现 POST /admin/products/{product_id}/skus。
func (s *AdminCatalogService) CreateSKU(ctx context.Context, productID int64,
	in NewSKUInput) (repository.AdminSKU, error) {

	if _, err := requireStaff(ctx); err != nil {
		return repository.AdminSKU{}, err
	}
	if err := checkSKUCode(in.SKUCode); err != nil {
		return repository.AdminSKU{}, err
	}
	if err := checkNonNeg("price_cents", in.PriceCents); err != nil {
		return repository.AdminSKU{}, err
	}
	if err := checkNonNeg("cost_cents", in.CostCents); err != nil {
		return repository.AdminSKU{}, err
	}
	if err := checkNonNeg("weight_gram", int64(in.WeightGram)); err != nil {
		return repository.AdminSKU{}, err
	}
	if err := checkNonNeg("available_qty", int64(in.AvailableQty)); err != nil {
		return repository.AdminSKU{}, err
	}
	if err := checkNonNeg("warning_qty", int64(in.WarningQty)); err != nil {
		return repository.AdminSKU{}, err
	}
	spec, err := encodeSpecValues(in.SpecValues)
	if err != nil {
		return repository.AdminSKU{}, err
	}

	var out repository.AdminSKU
	err = s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		imageURL, err := s.resolveSKUImage(ctx, tx, in.ImageUploadID)
		if err != nil {
			return err
		}
		out, err = tx.CreateSKU(ctx, repository.NewSKU{
			ProductID:  productID,
			SKUCode:    in.SKUCode,
			SpecValues: spec,
			PriceCents: in.PriceCents,
			CostCents:  in.CostCents,
			WeightGram: in.WeightGram,
			ImageURL:   imageURL,
			// 契约的 SkuCreateRequest 里没有 status —— 新建的规格一律在售。
			// 给它一个入参就等于让「建一个已经停售的规格」成为一个能表达的
			// 动作，而那件事没有任何业务含义（停售走 PATCH）。
			Status:       1,
			AvailableQty: in.AvailableQty,
			WarningQty:   in.WarningQty,
		})
		return err
	})
	return out, err
}

// SKUPatchInput 是改 SKU 的入参。ImageUploadID 的三态与 repository.SKUPatch
// 的 SetImageURL 一一对应：SetImageUploadID 为 true 且 ImageUploadID 为 nil
// 就是「清空小图」。
type SKUPatchInput struct {
	SKUCode    *string
	SpecValues *map[string]string
	PriceCents *int64
	CostCents  *int64
	WeightGram *int32
	Status     *int16

	SetImageUploadID bool
	ImageUploadID    *int64
}

// UpdateSKU 实现 PATCH /admin/skus/{sku_id}。
func (s *AdminCatalogService) UpdateSKU(ctx context.Context, skuID int64,
	in SKUPatchInput) (repository.AdminSKU, error) {

	if _, err := requireStaff(ctx); err != nil {
		return repository.AdminSKU{}, err
	}
	if in.SKUCode == nil && in.SpecValues == nil && in.PriceCents == nil &&
		in.CostCents == nil && in.WeightGram == nil && in.Status == nil &&
		!in.SetImageUploadID {
		return repository.AdminSKU{}, fmt.Errorf(
			"%w: 一个字段都没传（契约 SkuUpdateRequest 是 minProperties: 1）", ErrCatalogBadRequest)
	}
	if in.SKUCode != nil {
		if err := checkSKUCode(*in.SKUCode); err != nil {
			return repository.AdminSKU{}, err
		}
	}
	for name, v := range map[string]*int64{"price_cents": in.PriceCents, "cost_cents": in.CostCents} {
		if v != nil {
			if err := checkNonNeg(name, *v); err != nil {
				return repository.AdminSKU{}, err
			}
		}
	}
	if in.WeightGram != nil {
		if err := checkNonNeg("weight_gram", int64(*in.WeightGram)); err != nil {
			return repository.AdminSKU{}, err
		}
	}
	if in.Status != nil && *in.Status != 0 && *in.Status != 1 {
		return repository.AdminSKU{}, fmt.Errorf("%w: status 只能是 0 停售或 1 在售", ErrCatalogBadRequest)
	}
	var spec []byte
	if in.SpecValues != nil {
		var err error
		if spec, err = encodeSpecValues(*in.SpecValues); err != nil {
			return repository.AdminSKU{}, err
		}
	}

	var out repository.AdminSKU
	err := s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		p := repository.SKUPatch{
			SKUCode:     in.SKUCode,
			SpecValues:  spec,
			PriceCents:  in.PriceCents,
			CostCents:   in.CostCents,
			WeightGram:  in.WeightGram,
			Status:      in.Status,
			SetImageURL: in.SetImageUploadID,
		}
		if in.SetImageUploadID {
			url, err := s.resolveSKUImage(ctx, tx, in.ImageUploadID)
			if err != nil {
				return err
			}
			p.ImageURL = url
		}
		var e error
		out, e = tx.UpdateSKU(ctx, skuID, p)
		return e
	})
	return out, err
}

// DeleteSKU 实现 DELETE /admin/skus/{sku_id}。
func (s *AdminCatalogService) DeleteSKU(ctx context.Context, skuID int64) error {
	if _, err := requireStaff(ctx); err != nil {
		return err
	}
	return s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		_, e := tx.SoftDeleteSKU(ctx, skuID)
		return e
	})
}

// SetInventory 实现 PUT /admin/skus/{sku_id}/inventory。
//
// 两种 rows_affected = 0 的分辨**发生在 SQL 里**（SetInventoryByCAS 的两个
// CTE、同一个 MVCC 快照），这一层只是把参数递过去。在这里先查一遍再写，
// 那就是两次快照，而中间那个窗口正是这条接口存在的全部理由。
func (s *AdminCatalogService) SetInventory(ctx context.Context, skuID int64,
	expected, want int32, warning *int32) (repository.Inventory, error) {

	if _, err := requireStaff(ctx); err != nil {
		return repository.Inventory{}, err
	}
	// 契约：两个数量都是 minimum: 0，为负是 422。
	// **不靠 chk_qty_nonneg 兜底**：它兜不住负的 expected，而一个负的 expected
	// 永远匹配不上任何一行，症状是「怎么改都 409」—— 而 409 的含义是
	// 「重读一次再试就能成功」，于是调用方会一直试下去。
	if err := checkNonNeg("available_qty", int64(want)); err != nil {
		return repository.Inventory{}, err
	}
	if err := checkNonNeg("expected_available_qty", int64(expected)); err != nil {
		return repository.Inventory{}, err
	}
	if warning != nil {
		if err := checkNonNeg("warning_qty", int64(*warning)); err != nil {
			return repository.Inventory{}, err
		}
	}
	var out repository.Inventory
	err := s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		var e error
		out, e = tx.SetInventory(ctx, skuID, expected, want, warning)
		return e
	})
	return out, err
}

// resolveSKUImage 把 image_upload_id 翻成 skus.image_url，并在同一个事务里
// 把那个 upload 标成已引用。
//
// 三件事缺一不可：
//
//	① 这个 upload 要在本租户视野内（FindUpload 走 RLS，查不到就是
//	   ErrUploadNotFound → 契约的 422）；
//	② 用途必须是 1 商品图 —— **数据库表达不了这一条**，复合外键挡的是跨租户，
//	   不是用途。拿一张退款凭证当规格小图挂上去，只有这里挡得住；
//	② referenced 要在**同一个事务**里置位（§13）。不置的话，孤儿回收会在
//	   24 小时后把这张刚用上的图删掉，而 skus.image_url 还指着它。
//
// 传 nil（PATCH 里显式传 image_upload_id: null）表示清空，返回 nil。
func (s *AdminCatalogService) resolveSKUImage(ctx context.Context, tx repository.Tx,
	uploadID *int64) (*string, error) {

	if uploadID == nil {
		return nil, nil
	}
	u, err := tx.FindUpload(ctx, *uploadID)
	if err != nil {
		return nil, err
	}
	if u.Purpose != repository.UploadPurposeProductImage {
		return nil, fmt.Errorf("upload %d 的 purpose 是 %d: %w",
			*uploadID, u.Purpose, repository.ErrUploadWrongPurpose)
	}
	if err := tx.MarkUploadReferenced(ctx, *uploadID); err != nil {
		return nil, err
	}
	url := UploadURL(u.ID)
	return &url, nil
}

// ---------------------------------------------------------------------------
// 类目
// ---------------------------------------------------------------------------

// ListCategories 实现 GET /admin/categories。
func (s *AdminCatalogService) ListCategories(ctx context.Context) ([]repository.AdminCategory, error) {
	if _, err := requireStaff(ctx); err != nil {
		return nil, err
	}
	out := []repository.AdminCategory{}
	err := s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		rows, e := tx.AdminListCategories(ctx)
		if e != nil {
			return e
		}
		out = rows
		return nil
	})
	return out, err
}

// CreateCategory 实现 POST /admin/categories。
func (s *AdminCatalogService) CreateCategory(ctx context.Context, n repository.NewCategory) (repository.AdminCategory, error) {
	if _, err := requireStaff(ctx); err != nil {
		return repository.AdminCategory{}, err
	}
	if err := checkName(n.Name); err != nil {
		return repository.AdminCategory{}, err
	}
	var out repository.AdminCategory
	err := s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		var e error
		out, e = tx.CreateCategory(ctx, n)
		return e
	})
	return out, err
}

// CategoryPatchInput 是改类目的入参。
//
// SetParentID 把 parent_id 的三态展开：契约说「显式传 null 表示移到根，
// 不传这个字段则不动层级 —— null 与『没传』在这里是两件事」，
// 而一个 *int64 只有两个状态。
type CategoryPatchInput struct {
	Name      *string
	SortOrder *int32
	Status    *int16

	SetParentID bool
	ParentID    *int64
}

// UpdateCategory 实现 PATCH /admin/categories/{category_id}。
//
// 传了 parent_id 就是**移动子树**，那是 repository.MoveCategory 的事
// （改 parent_id + 重写整棵子树的 path/level + 判环，三步一个事务）。
// 两件事都传时先改属性再移动，顺序不要紧 —— 它们在同一个事务里，
// 而 MoveCategory 最后会回读整行，所以返回的一定是两次都生效之后的样子。
func (s *AdminCatalogService) UpdateCategory(ctx context.Context, id int64,
	in CategoryPatchInput) (repository.AdminCategory, error) {

	if _, err := requireStaff(ctx); err != nil {
		return repository.AdminCategory{}, err
	}
	if in.Name == nil && in.SortOrder == nil && in.Status == nil && !in.SetParentID {
		return repository.AdminCategory{}, fmt.Errorf(
			"%w: 一个字段都没传（契约 CategoryUpdateRequest 是 minProperties: 1）", ErrCatalogBadRequest)
	}
	if in.Name != nil {
		if err := checkName(*in.Name); err != nil {
			return repository.AdminCategory{}, err
		}
	}
	if in.Status != nil && *in.Status != 0 && *in.Status != 1 {
		return repository.AdminCategory{}, fmt.Errorf("%w: status 只能是 0 停用或 1 启用", ErrCatalogBadRequest)
	}

	var out repository.AdminCategory
	err := s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		if in.Name != nil || in.SortOrder != nil || in.Status != nil {
			p, err := tx.UpdateCategory(ctx, id, repository.CategoryPatch{
				Name: in.Name, SortOrder: in.SortOrder, Status: in.Status,
			})
			if err != nil {
				return err
			}
			out = p
		}
		if in.SetParentID {
			p, err := tx.MoveCategory(ctx, id, in.ParentID)
			if err != nil {
				return err
			}
			out = p
			return nil
		}
		if in.Name == nil && in.SortOrder == nil && in.Status == nil {
			// 上面那个 minProperties 检查保证走不到这里。断言而不是静默返回
			// 零值：一个全零的 AdminCategory 会被序列化成一个 id=0 的类目。
			return fmt.Errorf("category %d: 既没有属性要改也没有要移动，"+
				"而 minProperties 检查放它过来了", id)
		}
		return nil
	})
	return out, err
}

// DeleteCategory 实现 DELETE /admin/categories/{category_id}。
//
// 「有子分类」「有商品」两条闸门在 repository.SoftDeleteCategory 里，
// 不在这里 —— 它们各有一个 sentinel，而契约给了两个不同的 Problem type。
func (s *AdminCatalogService) DeleteCategory(ctx context.Context, id int64) error {
	if _, err := requireStaff(ctx); err != nil {
		return err
	}
	return s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		return tx.SoftDeleteCategory(ctx, id)
	})
}

// ---------------------------------------------------------------------------
// 上传
// ---------------------------------------------------------------------------

// CreateUpload 实现 POST /admin/uploads：把字节写进 driver，再登记一条元数据。
//
// 顺序是「先落盘、后入库」，两个方向的失败都要交代：
//
//   - 落盘成功但入库失败（事务回滚）→ 磁盘上多一个谁也不认识的文件。
//     **这是一次真的泄漏**，一期接受它：另一条路是先入库再落盘，
//     那样的失败会留下一条指向不存在文件的元数据 —— 而那正是 §13 建这张表
//     要避免的东西（「归属校验」「迁移能力」都建立在「清单是真的」之上）。
//     两害相权，宁可多一个孤儿文件。
//   - 入库成功但 referenced 一直是 FALSE → 24 小时后被孤儿回收删掉。
//     那是设计好的行为（§13），不是泄漏。
//
// contentType 由调用方从 multipart part 的头上取，**不从文件名猜**。
func (s *AdminCatalogService) CreateUpload(ctx context.Context, contentType string,
	body io.Reader) (repository.Upload, error) {

	id, err := requireStaff(ctx)
	if err != nil {
		return repository.Upload{}, err
	}
	if s.store == nil {
		return repository.Upload{}, errors.New("没有配置文件存储 driver，POST /admin/uploads 不可用")
	}
	mime, ext, ok := uploadExtensionFor(contentType)
	if !ok {
		return repository.Upload{}, fmt.Errorf("%w: %q（只接受 image/jpeg、image/png、image/webp）",
			ErrUploadMediaType, contentType)
	}
	merchantID, err := tenant.FromContext(ctx)
	if err != nil {
		return repository.Upload{}, err
	}

	key, size, sum, err := s.store.Put(merchantID, ext, body, MaxUploadBytes)
	if err != nil {
		return repository.Upload{}, err
	}

	var out repository.Upload
	err = s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		var e error
		out, e = tx.CreateStaffUpload(ctx, repository.NewUpload{
			StaffID:     id.StaffID,
			Driver:      s.store.Driver(),
			StorageKey:  key,
			ContentType: mime,
			SizeBytes:   size,
			SHA256:      sum,
		})
		return e
	})
	return out, err
}

// ---------------------------------------------------------------------------
// 校验小工具
// ---------------------------------------------------------------------------

// 长度一律按**字符**数，不按字节：契约里 title 的 maxLength: 200 说的是
// JSON 字符串的长度，而 200 个汉字在 UTF-8 里是 600 字节。按字节挡的话
// 上限会变成 66 个汉字 —— 契约允许长度的三分之一，而且没有任何东西会红。
// （与 handler 那条 query maxLength 闸门是同一条推理，写在 search.go 上。）

func checkTitle(v string) error {
	n := utf8.RuneCountInString(strings.TrimSpace(v))
	if n == 0 {
		return fmt.Errorf("%w: title 不能为空", ErrCatalogBadRequest)
	}
	if n > 200 {
		return fmt.Errorf("%w: title 有 %d 个字，契约上限是 200", ErrCatalogBadRequest, n)
	}
	return nil
}

func checkName(v string) error {
	n := utf8.RuneCountInString(strings.TrimSpace(v))
	if n == 0 {
		return fmt.Errorf("%w: name 不能为空", ErrCatalogBadRequest)
	}
	if n > 100 {
		return fmt.Errorf("%w: name 有 %d 个字，契约上限是 100", ErrCatalogBadRequest, n)
	}
	return nil
}

func checkSKUCode(v string) error {
	n := utf8.RuneCountInString(strings.TrimSpace(v))
	if n == 0 {
		return fmt.Errorf("%w: sku_code 不能为空", ErrCatalogBadRequest)
	}
	if n > 64 {
		return fmt.Errorf("%w: sku_code 有 %d 个字，契约上限是 64", ErrCatalogBadRequest, n)
	}
	return nil
}

func checkOptText(field string, v *string, max int) error {
	if v == nil {
		return nil
	}
	if n := utf8.RuneCountInString(*v); n > max {
		return fmt.Errorf("%w: %s 有 %d 个字，契约上限是 %d", ErrCatalogBadRequest, field, n, max)
	}
	return nil
}

func checkNonNeg(field string, v int64) error {
	if v < 0 {
		return fmt.Errorf("%w: %s 是 %d，不能为负", ErrCatalogBadRequest, field, v)
	}
	return nil
}

// encodeSpecValues 把契约的 additionalProperties: {type: string} 编成 JSONB 的
// 字节。与 DecodeSpecValues 是一对 —— 两侧各写一套编解码的话，同一块
// spec_values 在写进去和读出来时可以长得不一样。
//
// nil / 空 map 编成 "{}"，而不是让 repository 去兜底：那一层确实也兜了
// （空字节会以 22P02 失败，而那条错误里没有任何东西指向「规格没填」），
// 但一层兜底不该成为另一层可以不管的理由。
func encodeSpecValues(m map[string]string) ([]byte, error) {
	if len(m) == 0 {
		return []byte("{}"), nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("%w: spec_values 编码失败: %v", ErrCatalogBadRequest, err)
	}
	return b, nil
}
