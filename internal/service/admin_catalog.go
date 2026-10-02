package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/keel/keel/internal/auth"
	"github.com/keel/keel/internal/catalogimport"
	"github.com/keel/keel/internal/inventory"
	"github.com/keel/keel/internal/repository"
	"github.com/keel/keel/internal/tenant"
	"github.com/keel/keel/internal/understanding"
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

// uploadIDFromURL 把一个 Upload.url 解回 upload id。只认 UploadURL 拼出来的那一个形状，逐字：
// 带 query、带尾斜杠、前导零、外链一律不认（ok = false）。退款凭证与头像共用这一个判据 ——
// 后台按地址精确匹配引用它的退款单，变体会让一张真凭证在后台读不出来；头像换掉时按地址
// 找回旧的那一个取消引用，变体会让它永远停在已引用。
func uploadIDFromURL(u string) (int64, bool) {
	id, err := strconv.ParseInt(strings.TrimPrefix(u, uploadURLPrefix), 10, 64)
	if err != nil || id <= 0 || u != UploadURL(id) {
		return 0, false
	}
	return id, true
}

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

	// channels 是渠道层（KEEL_CHANNELS 关着时为 nil）：改基准价、SKU 停售、商品上下架或删除之后让它重算。
	channels *ChannelService

	// inv 是库存服务（微服务拆分阶段 1a）：后台商品 / SKU 页的库存数、单店捷径的两条
	// 改库存、建 SKU 的初始库存都经它。
	inv inventory.Service

	// Compliance 是快路径的广告法违禁词检查器，完整论证在 compliance.go 的文件头。
	//
	// **导出，而且可以被改掉** —— 这是一个刻意的形状，代价与收益都说清楚：
	// 收益是「检查卡住会怎样」这件事可以被测试造出来（真实的检查器是纯计算，
	// 快得没法超时，而「超时要拒绝」是 §7 里全系统唯一一处宁可误拒的规矩，
	// 它必须有测试盯着）。代价是生产代码里有一个能被赋值的字段。
	//
	// 代价被这一条抵掉：**把它置成 nil 不是「关掉检查」，是「一律拒绝」**
	// （checkCompliance 的第一段）。也就是说这个字段唯一能造成的破坏方向是
	// 发不出商品，不是发出违规商品。
	Compliance ComplianceChecker

	// ComplianceBudget <= 0 时用 complianceBudget（200 ms，§2 给的数）。
	// 同样只给测试调。
	ComplianceBudget time.Duration
}

// NewAdminCatalogService 建一个。store 为 nil 时 POST /admin/uploads 会报错 ——
// 不静默退化成「只登记元数据」，理由见 upload_store.go 的文件头。
//
// 合规检查器由这里填上真的那一个。它不是参数：生产路径上它只有一个取值，
// 而多一个参数意味着多一处可以传 nil 的地方 —— 虽然传了 nil 的后果是
// 一律拒绝（fail-closed），但那是一次谁都不想要的线上故障。
func NewAdminCatalogService(r AdminCatalogRepository, store UploadStore, inv inventory.Service) *AdminCatalogService {
	return &AdminCatalogService{
		repo:       r,
		store:      store,
		inv:        inv,
		Compliance: understanding.ComplianceCheck{},
	}
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
		if err := s.fillManagedBy(ctx, tx, items); err != nil {
			return err
		}
		out.Total, out.Items = total, items
		return nil
	})
	if err != nil {
		return AdminProductPage{}, err
	}
	// total_stock 由库存服务的合计加总（阶段 1a），在事务之外问 —— 理由见
	// inventory_admin.go 的文件头。库存服务不可用时整页 503，不编一个 0。
	if err := fillProductStock(ctx, s.repo, s.inv, out.Items); err != nil {
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
		ps := []repository.AdminProduct{p}
		if err := s.fillManagedBy(ctx, tx, ps); err != nil {
			return err
		}
		out = AdminProductDetail{Product: ps[0], SKUs: skus, Images: imgs}
		return nil
	})
	if err != nil {
		return AdminProductDetail{}, err
	}
	// 商品的 total_stock 与每个 SKU 的水位都来自库存服务，而且要对得上：
	// total_stock 就是下面这些 SKU 的 available_qty 之和（未软删的那些）。
	// 所以只问一次 SKU 合计，商品的数在这里加出来，不再单独问一次 —— 两次问
	// 之间的一次下单就会让两个数对不上。
	if err := fillSKUStock(ctx, s.inv, out.SKUs); err != nil {
		return AdminProductDetail{}, err
	}
	var total int32
	for _, sk := range out.SKUs {
		total += sk.AvailableQty
	}
	out.Product.TotalStock = total
	return out, nil
}

// CreateProduct 实现 POST /admin/products。
//
// 返回的第二个值为 true 表示这是一次幂等重放（契约要求响应带
// Idempotency-Replayed: true）。下面另外四条写接口同此。
func (s *AdminCatalogService) CreateProduct(ctx context.Context, n repository.NewProduct,
	idemKey string) (repository.AdminProduct, bool, error) {

	if _, err := requireMerchantWide(ctx); err != nil {
		return repository.AdminProduct{}, false, err
	}
	// 校验排在抢占幂等键**之前**：一个注定被拒的请求不该占掉客户端的那把
	// 钥匙（同 OrderService.Create 里券那一支）。占掉的后果很具体 ——
	// 客户端改对了请求体再用同一把钥匙重试，会撞上 422 键被复用。
	if err := checkTitle(n.Title); err != nil {
		return repository.AdminProduct{}, false, err
	}
	if err := checkOptText("subtitle", n.Subtitle, 200); err != nil {
		return repository.AdminProduct{}, false, err
	}
	if n.CategoryID <= 0 {
		return repository.AdminProduct{}, false, fmt.Errorf("%w: category_id 必须是正整数", ErrCatalogBadRequest)
	}
	hash, err := adminRequestHash(nil, n)
	if err != nil {
		return repository.AdminProduct{}, false, err
	}
	return idempotentWrite(ctx, s, scopeAdminProductCreate, idemKey, hash, archivedCreated,
		func(tx repository.Tx) (repository.AdminProduct, error) {
			if err := lockFreightTemplateForLink(ctx, tx, n.FreightTemplateID); err != nil {
				return repository.AdminProduct{}, err
			}
			return tx.CreateProduct(ctx, n)
		})
}

// UpdateProduct 实现 PATCH /admin/products/{product_id}。
func (s *AdminCatalogService) UpdateProduct(ctx context.Context, id int64,
	p repository.ProductPatch) (repository.AdminProduct, error) {

	if _, err := requireMerchantWide(ctx); err != nil {
		return repository.AdminProduct{}, err
	}
	// 契约：minProperties: 1。一个字段都没传是 422，不是「什么也不改的 200」——
	// 后者会让客户端以为自己那次编辑保存成功了。
	if p.Title == nil && p.Subtitle == nil && p.Description == nil &&
		p.CategoryID == nil && !p.SetBrandID && !p.SetFreightTemplateID {
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
		// 由渠道管的字段（标题、详情）先于写入拒掉：同一个事务里读映射与 binding 状态。
		managed, e := s.channels.ManagedBy(ctx, tx, []int64{id})
		if e != nil {
			return e
		}
		if ch, ok := managed[id]; ok && (p.Title != nil || p.Description != nil) {
			// 值真变了才拒：原样回传当前值（AI 员工、MCP 工具常发整份请求体）照常放行。
			cur, e := tx.AdminFindProduct(ctx, id)
			if e != nil {
				return e
			}
			curDesc := ""
			if cur.Description != nil {
				curDesc = *cur.Description
			}
			if (p.Title != nil && *p.Title != cur.Title) || (p.Description != nil && *p.Description != curDesc) {
				return ManagedFieldError(ch, "标题与详情")
			}
		}
		if p.SetFreightTemplateID {
			if e := lockFreightTemplateForLink(ctx, tx, p.FreightTemplateID); e != nil {
				return e
			}
		}
		updated, e := tx.UpdateProduct(ctx, id, p)
		if e != nil {
			return e
		}
		if ch, ok := managed[id]; ok {
			updated.ManagedBy = &ch
		}
		// **在架商品的文案改动也要过合规检查**，草稿与已下架的不过。
		//
		// 这一条是补漏，不是加严：只拦上架的话，「先用干净标题上架，
		// 再 PATCH 成违禁标题」是一条两步都返回 200 的完整绕过路径。
		// 要守的不变量是「对外可见的文案没有违禁词」，
		// 而在架状态下的 PATCH 是改变那段可见文案的第二个入口。
		//
		// 判断用的是**改完之后**那一行的 status，不是改之前的：
		// 这个 PATCH 按契约不改状态，两者必然相同 —— 用改完之后的那个，
		// 是因为它和被检查的文案来自同一行、同一个快照。
		if updated.Status == productStatusPublished {
			if e := s.checkCompliance(ctx, updated); e != nil {
				return e
			}
		}
		out = updated
		return nil
	})
	if err != nil {
		return repository.AdminProduct{}, err
	}
	return s.withProductStock(ctx, "PATCH /admin/products/{id}", out), nil
}

// fillManagedBy 给这批商品填 ManagedBy（一次批量查询，不逐件）。渠道层关着时什么都不做、不查库。
func (s *AdminCatalogService) fillManagedBy(ctx context.Context, tx repository.Tx, items []repository.AdminProduct) error {
	if s.channels == nil || len(items) == 0 {
		return nil
	}
	ids := make([]int64, len(items))
	for i, p := range items {
		ids[i] = p.ID
	}
	managed, err := s.channels.ManagedBy(ctx, tx, ids)
	if err != nil {
		return err
	}
	for i := range items {
		if ch, ok := managed[items[i].ID]; ok {
			items[i].ManagedBy = &ch
		}
	}
	return nil
}

// lockFreightTemplateForLink 是「商品挂运费模板」的那道校验（00055）：只能挂一个
// **未删除的全店模板**，并在这个事务里以共享锁钉住它，直到商品这一行写完 ——
// 并发的「把它改成门店模板」或「删除它」要先拿同一行的排他锁再数挂着它的商品，
// 于是两边串行，数完之后溜进来一件新挂上的商品这件事不会发生。
//
// id 为 nil（不挂 / 解除）时什么都不做。不成立时 422（请求体里指名的东西不可用）。
func lockFreightTemplateForLink(ctx context.Context, tx repository.Tx, id *int64) error {
	if id == nil {
		return nil
	}
	err := tx.LockFreightTemplateForLink(ctx, *id)
	if errors.Is(err, repository.ErrFreightTemplateNotFound) {
		return fmt.Errorf("%w: freight_template_id=%d 不是本店一个未删除的全店运费模板"+
			"（门店模板不能单独挂在商品上）", ErrCatalogBadRequest, *id)
	}
	return err
}

// productStatusPublished 是 products.status 的 1（上架）。
//
// 契约把这个枚举写在三处（ProductSummary.status、AdminProduct.status、
// publication 端点的描述），数据模型 §3 也写了一遍。这里给它一个名字，
// 是因为上面那个判断读作「== 1」时，没有人看得出它在问「买家看得见吗」。
const productStatusPublished int16 = 1

// DeleteProduct 实现 DELETE /admin/products/{product_id}。
func (s *AdminCatalogService) DeleteProduct(ctx context.Context, id int64) error {
	if _, err := requireMerchantWide(ctx); err != nil {
		return err
	}
	err := s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		return tx.SoftDeleteProduct(ctx, id)
	})
	if err == nil {
		s.channels.ProductChanged(ctx, id)
	}
	return err
}

// WithChannels 接上渠道层（nil 即不接）。
func (s *AdminCatalogService) WithChannels(c *ChannelService) *AdminCatalogService {
	s.channels = c
	return s
}

// SetPublication 实现 POST /admin/products/{product_id}/publication。
//
// 「没有 SKU 不能上架」那条闸门**不在这里**：它在 repository 的
// PublishProduct 里，写进 UPDATE 的 WHERE 与同一个快照里算出来的 sku_count。
// 在这一层再查一遍是两次快照 —— 中间那一瞬间最后一个 SKU 可能刚被删掉，
// 而且两份实现一定会分叉。
func (s *AdminCatalogService) SetPublication(ctx context.Context, id int64, publish bool,
	idemKey string) (repository.AdminProduct, bool, error) {

	if _, err := requireMerchantWide(ctx); err != nil {
		return repository.AdminProduct{}, false, err
	}
	// 这一条**本来就天生幂等**（状态机终点相同），接上幂等键仍然有意义：
	// 契约把它定成必填，而「服务端连看都没看」与「服务端看了但这次恰好
	// 不需要它」是两件事 —— 前者会让「同一把钥匙配不同请求体」那条 422
	// 在这条接口上不存在，于是一次 publish 与一次 unpublish 用同一把钥匙
	// 会被当成两次独立调用。
	hash, err := adminRequestHash([]int64{id}, publish)
	if err != nil {
		return repository.AdminProduct{}, false, err
	}
	p, replayed, err := idempotentWrite(ctx, s, scopeAdminProductPublish, idemKey, hash, archivedOK,
		func(tx repository.Tx) (repository.AdminProduct, error) {
			p, e := tx.SetProductPublication(ctx, id, publish)
			if e != nil {
				return repository.AdminProduct{}, e
			}
			// 广告法违禁词检查（商品理解服务设计 §2 的快路径）。
			//
			// **只查 publish，不查 unpublish**：下架只会让文案从买家面前消失。
			// 检查拿上架之后那一行去做，命中就让整个事务回滚 —— 完整论证
			// （为什么不是先查再写、为什么草稿不查、超时为什么是拒绝）
			// 写在 compliance.go 的文件头。
			//
			// **它跑在 idempotentWrite 的回调里，也就是抢占幂等键的那同一个
			// 事务里**，这一点是合并两条线时的一个真实取舍，不是顺手：
			// 合规拒绝会把这次抢占一起回滚掉，于是商家改完标题**用同一把
			// 钥匙重试**就能发出去。反过来（先提交抢占、再查合规）会把那把
			// 钥匙钉在一个失败的存档上，改完标题再来是「同一把钥匙配不同
			// 请求体」，回 422 —— 让人用一把新钥匙才能修好自己的错字。
			if publish {
				if e := s.checkCompliance(ctx, p); e != nil {
					return repository.AdminProduct{}, e
				}
			}
			return p, nil
		})
	if err != nil {
		return repository.AdminProduct{}, false, err
	}
	if !replayed {
		s.channels.ProductChanged(ctx, id)
	}
	// total_stock 在存档**之外**现取：存档里的是写那一刻的商品，库存数每次重放都按
	// 当前值回（存档里本来也没有它 —— 它不在写事务里算）。
	return s.withProductStock(ctx, "POST /admin/products/{id}/publication", p), replayed, nil
}

// withProductStock 给写接口回显的商品填 total_stock，库存服务不可用时按 0 回显并喊 WARN
// （写已经提交，不能回 503 让客户端以为没改成）。见 inventory_admin.go 的 echoStockBestEffort。
func (s *AdminCatalogService) withProductStock(ctx context.Context, what string, p repository.AdminProduct) repository.AdminProduct {
	items := []repository.AdminProduct{p}
	echoStockBestEffort(ctx, what, fillProductStock(ctx, s.repo, s.inv, items))
	return items[0]
}

// ReplaceImages 实现 PUT /admin/products/{product_id}/images。
func (s *AdminCatalogService) ReplaceImages(ctx context.Context, productID int64,
	uploadIDs []int64) ([]repository.ProductImage, error) {

	if _, err := requireMerchantWide(ctx); err != nil {
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
		managed, e := s.channels.ManagedBy(ctx, tx, []int64{productID})
		if e != nil {
			return e
		}
		if ch, ok := managed[productID]; ok {
			return ManagedFieldError(ch, "商品图")
		}
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
	in NewSKUInput, idemKey string) (repository.AdminSKU, bool, error) {

	if _, err := requireMerchantWide(ctx); err != nil {
		return repository.AdminSKU{}, false, err
	}
	if err := checkSKUCode(in.SKUCode); err != nil {
		return repository.AdminSKU{}, false, err
	}
	if err := checkPrice("price_cents", in.PriceCents); err != nil {
		return repository.AdminSKU{}, false, err
	}
	if err := checkPrice("cost_cents", in.CostCents); err != nil {
		return repository.AdminSKU{}, false, err
	}
	if err := checkNonNeg("weight_gram", int64(in.WeightGram)); err != nil {
		return repository.AdminSKU{}, false, err
	}
	if err := checkNonNeg("available_qty", int64(in.AvailableQty)); err != nil {
		return repository.AdminSKU{}, false, err
	}
	if err := checkNonNeg("warning_qty", int64(in.WarningQty)); err != nil {
		return repository.AdminSKU{}, false, err
	}
	spec, err := encodeSpecValues(in.SpecValues)
	if err != nil {
		return repository.AdminSKU{}, false, err
	}
	// product_id 进哈希：「给商品 7 建这个 SKU」与「给商品 9 建同样的 SKU」
	// 请求体逐字相同，scope 也相同（adminRequestHash 的注释写了这条）。
	hash, err := adminRequestHash([]int64{productID}, in)
	if err != nil {
		return repository.AdminSKU{}, false, err
	}

	sku, replayed, err := idempotentWrite(ctx, s, scopeAdminSKUCreate, idemKey, hash, archivedCreated,
		func(tx repository.Tx) (repository.AdminSKU, error) {
			imageURL, err := s.resolveSKUImage(ctx, tx, in.ImageUploadID)
			if err != nil {
				return repository.AdminSKU{}, err
			}
			return tx.CreateSKU(ctx, repository.NewSKU{
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
		})
	if err != nil {
		return repository.AdminSKU{}, false, err
	}
	// 初始库存行：SKU 提交之后建，重放那一支也建（幂等），理由见 initSKUStock。
	// 失败时回错误（拆分形态下是 503）：SKU 已经建好并存了档，同一把钥匙重试就会补上。
	if err := initSKUStock(ctx, s.repo, s.inv, sku); err != nil {
		return repository.AdminSKU{}, false, err
	}
	// 新 SKU 在每家店的有货排序标记当场重算（stock_flags.go seedProductStockFlags）。
	seedProductStockFlags(ctx, s.repo, s.inv, []int64{productID})
	return sku, replayed, nil
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

	if _, err := requireMerchantWide(ctx); err != nil {
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
			if err := checkPrice(name, *v); err != nil {
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
		// 规格（spec_values）由渠道管：先于写入拒掉。只在改规格、且开着渠道层时才多查。
		if spec != nil && s.channels != nil {
			pid, e := tx.ProductOfSKU(ctx, skuID)
			if e != nil {
				return e
			}
			managed, e := s.channels.ManagedBy(ctx, tx, []int64{pid})
			if e != nil {
				return e
			}
			if ch, ok := managed[pid]; ok {
				cur, e := tx.AdminFindSKU(ctx, skuID)
				if e != nil {
					return e
				}
				var want map[string]string
				_ = json.Unmarshal(spec, &want)
				if !sameSpec(cur.SpecValues, want) { // 值真变了才拒，同标题那一条
					return ManagedFieldError(ch, "规格")
				}
			}
		}
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
	if err != nil {
		return repository.AdminSKU{}, err
	}
	if in.PriceCents != nil || in.Status != nil {
		s.channels.SKUsChanged(ctx, []int64{skuID})
	}
	// 回显里的库存数（跨门店合计）由库存服务给；写已提交，取不到按 0 回显并喊 WARN。
	skus := []repository.AdminSKU{out}
	echoStockBestEffort(ctx, "PATCH /admin/skus/{id}", fillSKUStock(ctx, s.inv, skus))
	return skus[0], nil
}

// DeleteSKU 实现 DELETE /admin/skus/{sku_id}。
func (s *AdminCatalogService) DeleteSKU(ctx context.Context, skuID int64) error {
	if _, err := requireMerchantWide(ctx); err != nil {
		return err
	}
	err := s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		_, e := tx.SoftDeleteSKU(ctx, skuID)
		return e
	})
	if err == nil {
		s.channels.SKUsChanged(ctx, []int64{skuID})
	}
	return err
}

// SetInventory 实现 PUT /admin/skus/{sku_id}/inventory。
//
// 两种 rows_affected = 0 的分辨**发生在 SQL 里**（SetInventoryByCAS 的两个
// CTE、同一个 MVCC 快照），这一层只是把参数递过去。在这里先查一遍再写，
// 那就是两次快照，而中间那个窗口正是这条接口存在的全部理由。
func (s *AdminCatalogService) SetInventory(ctx context.Context, skuID int64,
	expected, want int32, warning *int32) (repository.Inventory, error) {

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
	bizID, err := inventorySetBizID(ctx)
	if err != nil {
		return repository.Inventory{}, err
	}
	// 00020 之后「这个 SKU 的库存」不再是一个有定义的东西：主键是 (sku_id, store_id)。
	// 契约把这条路径的语义写死成「**本租户恰好有一家未软删的门店时它就是那一家，否则 409
	// store-ambiguous**」，而不是「落到默认门店」—— 库存是唯一真相，猜错一家店的后果是
	// 把另一家店的水位覆盖掉，而且没有任何东西会响。解析、判权、判 SKU 可见在同一个
	// core 事务里；写在库存服务（阶段 1a），见 inventory_admin.go 的 setStockSole。
	return setStockSole(ctx, s.repo, s.inv, skuID, expected, want, warning, bizID)
}

// AdjustInventory 实现 POST /admin/skus/{sku_id}/inventory/adjustments
// （相对调整的单店捷径）。门店在事务里用 SoleStore 解析，其余与按门店那条
// 逐字一致 —— 两条共用 adjustInventory（admin_store.go），不各写一份：
// 幂等、判权、流水任何一处只在一边改了，两条路径就会对同一个请求给出两种结果。
func (s *AdminCatalogService) AdjustInventory(ctx context.Context, skuID int64,
	in InventoryAdjustInput, idemKey string) (repository.StoreInventory, bool, error) {
	return adjustInventory(ctx, s.repo, s.inv, 0, skuID, in, idemKey)
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
func (s *AdminCatalogService) CreateCategory(ctx context.Context, n repository.NewCategory,
	idemKey string) (repository.AdminCategory, bool, error) {

	if _, err := requireMerchantWide(ctx); err != nil {
		return repository.AdminCategory{}, false, err
	}
	if err := checkName(n.Name); err != nil {
		return repository.AdminCategory{}, false, err
	}
	hash, err := adminRequestHash(nil, n)
	if err != nil {
		return repository.AdminCategory{}, false, err
	}
	return idempotentWrite(ctx, s, scopeAdminCategoryCreate, idemKey, hash, archivedCreated,
		func(tx repository.Tx) (repository.AdminCategory, error) {
			return tx.CreateCategory(ctx, n)
		})
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

	if _, err := requireMerchantWide(ctx); err != nil {
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
	if _, err := requireMerchantWide(ctx); err != nil {
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
	body io.Reader, idemKey string) (repository.Upload, bool, error) {

	id, err := requireMerchantWide(ctx)
	if err != nil {
		return repository.Upload{}, false, err
	}
	if s.store == nil {
		return repository.Upload{}, false, errors.New("没有配置文件存储 driver，POST /admin/uploads 不可用")
	}
	// 缺钥匙在**读请求体之前**就拒。这条接口的请求体最大 10 MB，
	// 而一个注定被拒的请求不该先把它落一遍盘。
	if idemKey == "" {
		return repository.Upload{}, false, ErrIdempotencyKeyMissing
	}
	mime, ext, ok := uploadExtensionFor(contentType)
	if !ok {
		return repository.Upload{}, false, fmt.Errorf("%w: %q（只接受 image/jpeg、image/png、image/webp）",
			ErrUploadMediaType, contentType)
	}
	merchantID, err := tenant.FromContext(ctx)
	if err != nil {
		return repository.Upload{}, false, err
	}

	// ===================================================================
	// 这一条与另外四条的差别：字节必须先落盘，幂等键才抢得了
	// ===================================================================
	//
	// 因为 request_hash 要认的就是**这次传的是不是同一个文件**，而那要读完
	// 整个请求体才知道（sha256 由 Put 一边写一边算）。反过来先抢键的话，
	// 「同一把钥匙配了另一张图」这条 422 就判不出来 —— 而那正是 §12 说的
	// 「最容易被省掉、也最不能省」的那一列。
	//
	// 代价是重放那一路会多出一个刚落盘的文件，而它**不会**被孤儿回收看见
	// （回收扫的是 uploads 表，而这个文件没有对应的行）。所以下面重放分支里
	// 那句 Remove 不是打扫，是这条路径正确性的一部分：少了它，客户端每重试
	// 一次，磁盘上就多一份 10 MB。
	key, size, sum, err := s.store.Put(merchantID, ext, body, MaxUploadBytes)
	if err != nil {
		return repository.Upload{}, false, err
	}
	// 哈希认三样：内容、声明的类型、大小。只认 sha256 的话，同一张图换一个
	// content_type 重传会被当成重放 —— 而那两次登记出来的行是不同的
	// （content_type 进库、也决定扩展名）。
	hash, err := adminRequestHash(nil, uploadFingerprint{Mime: mime, SHA256: sum, SizeBytes: size})
	if err != nil {
		return repository.Upload{}, false, err
	}

	out, replayed, err := idempotentWrite(ctx, s, scopeAdminUploadCreate, idemKey, hash, archivedCreated,
		func(tx repository.Tx) (repository.Upload, error) {
			return tx.CreateStaffUpload(ctx, repository.NewUpload{
				StaffID:     id.StaffID,
				Driver:      s.store.Driver(),
				StorageKey:  key,
				ContentType: mime,
				SizeBytes:   size,
				SHA256:      sum,
			})
		})
	// 两条路都要把刚落盘的那个文件删掉（重放 / 失败），理由写在 discardStoredUpload 上。
	err = discardStoredUpload(ctx, s.store, key, err, replayed)
	return out, replayed, err
}

// uploadFingerprint 是上传那条路的 request_hash 素材。
//
// 一个具名结构体而不是一句字符串拼接：字段名进 JSON，于是「加一个维度」
// 是一次显式的改动，而不是在某个 Sprintf 里多一个 %s。
type uploadFingerprint struct {
	Mime      string `json:"mime"`
	SHA256    string `json:"sha256"`
	SizeBytes int64  `json:"size_bytes"`
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

// checkPrice 是单价的校验：[0, catalogimport.MaxPriceCents]。上限挡的是录错单位 / 多敲 0，也保证金额求和不溢出。
func checkPrice(field string, v int64) error {
	if err := checkNonNeg(field, v); err != nil {
		return err
	}
	if v > catalogimport.MaxPriceCents {
		return fmt.Errorf("%w: %s 是 %d 分，超过单价上限一亿元（%d 分），请核对单位", ErrCatalogBadRequest, field, v,
			catalogimport.MaxPriceCents)
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
