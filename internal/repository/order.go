package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/keel/keel/internal/repository/internal/db"
)

// 下单主链路在 repository 边界上的那一面。
//
// 单独一个文件、单独一个接口（OrderTx），不往 product.go 的 Tx 里平铺加方法 ——
// 理由见那里的注释：Tx 那份组合定义同时被好几条并发任务碰到，平铺等于所有人
// 改同一批行。这里只让 Tx 多一行嵌入。

var (
	// ErrAddressNotFound：这个 address_id 在本租户、本买家名下查不到（或已软删）。
	//
	// 它刻意不区分「地址不存在」与「地址是别人的」：契约里 address_id 是自增 id，
	// 而防越权靠的正是服务端按 user_id 强制过滤（数据模型 §9 的约定 4）。
	// 分开报的话，这个接口就成了一个「猜 id 探测别人有几个地址」的口子。
	ErrAddressNotFound = errors.New("收货地址不存在")

	// ErrOrderNotFound：按 order_no 查不到订单。
	//
	// 在 SAGA 分支里，它同时覆盖两件事：这一单真的不存在，或者**这个 gid 的租户
	// 不是这一单的租户**（RLS 把行挡在视野外）。两者在这一层无法也不必分开 ——
	// 分支的租户由 dtm.TenantContextFromGID 从 gid 解出来，按构造就该对得上，
	// 对不上就是 bug，而不是一种业务状态。
	ErrOrderNotFound = errors.New("订单不存在")

	// ErrIdempotencyKeyNotFound：抢占插入说「已存在」，回头读却读不到那一行。
	//
	// 它几乎只可能是 expire_at 到了、清理任务刚好把行删掉。给它一个 sentinel，
	// 是为了让这种罕见竞态在日志里有名字，而不是伪装成一次「幂等键被复用」。
	ErrIdempotencyKeyNotFound = errors.New("幂等键记录不存在")
)

// PriceableSKU 是定价与快照需要的一行。
//
// 它只有可售 SKU 才会出现（查询里过滤了 sku.status / product.status /
// product.deleted_at），所以「请求了 N 个 SKU 却只回来 M 行」本身就是
// 「有 M - N 个不可售」这条信息，服务层据此报错而不是少算一行钱。
type PriceableSKU struct {
	ID         int64
	ProductID  int64
	Title      string
	SpecValues []byte // JSONB 原样带上来，快照要一字不差地拷进 order_items
	ImageURL   *string
	PriceCents int64

	// BrandID 与 CategoryPath 是券挑行的素材（数据模型 §7）：适用范围按品牌、
	// 分类（含子孙，按 path 前缀判）挑出参与计算的行。与价格从同一条查询取，
	// 试算与下单才不可能在「这一行算不算适用」上分叉。
	// CategoryPath 为 nil 表示商品所在分类已软删 —— 分类规则命中不了它。
	BrandID      *int64
	CategoryPath *string
}

// Address 是收货地址里会被拍进 orders.receiver_snapshot 的那几列。
//
// 刻意不带 is_default / tag：契约的 ReceiverSnapshot 说得很清楚，快照不该带
// 属于地址簿管理的字段，也不该随地址簿 schema 演进而变形。
type Address struct {
	ID           int64
	ReceiverName string
	Phone        string
	Province     string
	City         string
	District     string
	Street       string
	Detail       string
	RegionCode   *string
	PostalCode   *string
}

// Order 是订单在 repository 边界上的形状。
//
// 后面三个时间戳是可空的（*time.Time），而 ExpireAt / CreatedAt 不是 —— 那不是
// 风格的不一致，是列本身的语义：DDL 上前两列 NOT NULL，后三列可空。用零值
// time.Time 表示「还没发生」会让 handler 分不清「没付款」与「在 0001-01-01
// 付的款」，而契约里 paid_at 这类字段的缺席正是「这件事还没发生」的唯一表达。
type Order struct {
	ID      int64
	OrderNo string
	UserID  int64
	// StoreID / RegionID 是**履约门店**与下单时它所属的大区。两列都 NOT NULL
	// （数据模型 §5）：SAGA 分支读回订单行拿到 NULL 时无路可走 —— 既不能猜
	// 默认店（那会把单扣到另一家店去），也不能失败（订单已经落库了）。
	// 库存分支的扣减与回补都按这个 StoreID 走。
	StoreID          int64
	RegionID         int64
	Status           int16
	GoodsAmountCents int64
	FreightCents     int64
	DiscountCents    int64
	PayableCents     int64
	PaidCents        int64
	RefundedCents    int64
	RefundStatus     int16
	ExpireAt         time.Time
	CreatedAt        time.Time
	PaidAt           *time.Time
	ShippedAt        *time.Time
	FinishedAt       *time.Time

	// UserCouponID 是这一单用的券（00026）。SAGA 的券分支从这里知道锁哪一张 ——
	// 分支只拿到三个字符串，这件事推不出来，只能落在订单行上。
	UserCouponID *int64
}

// optTime 把 pgtype.Timestamptz 收成 *time.Time：NULL → nil。
//
// 一个函数而不是在五处各写一遍 `if r.PaidAt.Valid { ... }`：写岔一处的症状是
// 某一条路径上「未发货的订单带着一个 0001 年的发货时间」，而那看上去像数据脏了。
func optTime(ts pgtype.Timestamptz) *time.Time {
	if !ts.Valid {
		return nil
	}
	t := ts.Time
	return &t
}

// OrderLine 是库存分支重建「扣减意图」所需的全部信息。
type OrderLine struct {
	SKUID    int64
	Quantity int32
}

// NewOrderDraft 是落一笔「创建中」订单要写的列。
//
// 没有 MerchantID：那一列的默认值是 current_merchant()（00013），
// 调用方没有那个参数可以传错。
type NewOrderDraft struct {
	OrderNo string
	UserID  int64
	// StoreID 是本次请求解析到的那家门店。region_id 与 store_snapshot 都由
	// 那条 INSERT ... SELECT FROM stores 从**同一行**取，不从这里传：
	// 应用先查一次门店再把字段拼进 INSERT，两步之间那家店可以改名，
	// 于是外键指着 A、快照写着 A 的旧名字，而两者都「看起来正常」。
	StoreID          int64
	GoodsAmountCents int64
	FreightCents     int64
	DiscountCents    int64
	PayableCents     int64
	ReceiverSnapshot []byte
	Remark           *string
	ExpireAt         time.Time
	UserCouponID     *int64
}

// NewOrderItem 是一行订单项快照。
type NewOrderItem struct {
	OrderID       int64
	SKUID         int64
	ProductID     int64
	TitleSnapshot string
	SpecSnapshot  []byte
	ImageSnapshot *string
	PriceCents    int64
	Quantity      int32
	AmountCents   int64
	DiscountCents int64
}

// 库存流水的 biz_type（数据模型 §4）。
//
// 做成常量而不是让调用方传 1 / 2：这两个值会决定对账时一行流水算「扣」还是
// 算「补」，而一个写反了的字面量在任何测试里都长得像一次正常的库存波动。
const (
	InventoryLogOrderDeduct    int16 = 1 // 下单扣减
	InventoryLogSagaCompense   int16 = 2 // SAGA 补偿回补
	InventoryLogTimeoutRelease int16 = 3 // 超时关单释放
)

// 2 与 3 的差别值得单说一句，因为两者在库里长得一模一样（同一个 sku、同一个
// 正数 change_qty、同一个订单号）：
//
//   - 2 是**下单没成功**，SAGA 在正向阶段自己把刚扣的货放了回去；
//   - 3 是**下单成功了但用户没付钱**，几十分钟之后由定时任务放回去。
//
// 合并成一个值的话，「这批货被锁了多久」与「转化率在哪一步掉的」两个问题
// 都答不出来 —— 而它们正是这条链路上运营最先会问的两个。

// IdempotencyRecord 是幂等键那一行里服务层要用的部分（数据模型 §12）。
type IdempotencyRecord struct {
	RequestHash  string
	Status       int16 // 0 处理中 / 1 成功 / 2 失败
	ResponseCode *int32
	ResponseBody []byte
}

// 幂等记录的三态。同上：常量，不是散落的字面量。
const (
	IdempotencyInFlight  int16 = 0
	IdempotencySucceeded int16 = 1
	IdempotencyFailed    int16 = 2
)

// idempotency_keys.subject_kind 的取值，与 00023 里那条 CHECK 逐值一致。
const (
	idempotencySubjectUser  int16 = 1 // 买家，subject_id 是 users.id
	idempotencySubjectStaff int16 = 2 // 后台操作员，subject_id 是 staff.id
)

// IdempotencySubject 是「这把钥匙属于谁」：一个身份域加一个 id。
//
// ===========================================================================
// 为什么它是一个类型，而不是两个参数
// ===========================================================================
//
// 因为两个参数里的那个 int16 可以传错，而传错的症状是**没有症状**：
// 一次把 staff_id 记成买家的调用会正常返回、正常存档、正常回放，
// 只是它占的是买家 7 号的键空间。staff.id 与 users.id 来自同一种自增序列
// （数据模型 §14），所以这不是理论上的碰撞。
//
// 做成一个只能由下面两个构造函数造出来的结构体之后，调用点上必须写出
// BuyerSubject 或 StaffSubject 这个词 —— 也就是说「这把钥匙是谁的」
// 变成一件在代码里读得出来、而且拼错就编译不过的事。
//
// 零值是无效的（Kind = 0 不在 CHECK 里），所以忘了构造会在数据库上当场
// 23514 失败，而不是安静地落进某个键空间。
type IdempotencySubject struct {
	kind int16
	id   int64
}

// BuyerSubject 是买家那一侧：subject_id 放 users.id。
func BuyerSubject(userID int64) IdempotencySubject {
	return IdempotencySubject{kind: idempotencySubjectUser, id: userID}
}

// StaffSubject 是后台那一侧：subject_id 放 staff.id。
//
// **这就是那个曾经做不到的东西。** 00023 之前这张表的主键是
// (scope, user_id, idem_key)，后台要用它只能把 staff_id 塞进 user_id ——
// 而那正是 auth/staff_middleware.go 与 §14 反复点名的那件事。
func StaffSubject(staffID int64) IdempotencySubject {
	return IdempotencySubject{kind: idempotencySubjectStaff, id: staffID}
}

// OrderTx 是下单主链路这一面。
type OrderTx interface {
	// ListSKUsForPricing 按一批 sku_id 取定价与快照素材。
	// **试算与真下单共用它** —— 两条路算出不同的钱是这条链路最严重的一类 bug。
	ListSKUsForPricing(ctx context.Context, sc StoreScope, skuIDs []int64) ([]PriceableSKU, error)

	// FindAddress 取当前买家名下的一条收货地址。查不到返回 ErrAddressNotFound。
	FindAddress(ctx context.Context, addressID, userID int64) (Address, error)

	// CreateOrderDraft 落一笔 status = 0 创建中的订单，返回它。
	CreateOrderDraft(ctx context.Context, d NewOrderDraft) (Order, error)

	// CreateOrderItem 落一行订单项快照。
	CreateOrderItem(ctx context.Context, it NewOrderItem) error

	// FindOrderByNo 按对外编号取订单。查不到返回 ErrOrderNotFound。
	FindOrderByNo(ctx context.Context, orderNo string) (Order, error)

	// ListOrderLines 取一笔订单的全部行（sku_id + 数量），按 sku_id 排序。
	// 这是 SAGA 库存分支重建扣减意图的唯一来源。
	ListOrderLines(ctx context.Context, orderID int64) ([]OrderLine, error)

	// PromoteOrderDraft 把订单从 0 创建中推到 10 待支付，返回受影响行数。
	PromoteOrderDraft(ctx context.Context, orderNo string) (int64, error)

	// CloseOrder 把订单关到 90（只从 0 或 10 进来），返回受影响行数。
	CloseOrder(ctx context.Context, orderNo string) (int64, error)

	// AppendInventoryLog 记一行库存流水。
	//
	// storeID 本轮（00020）加进签名：对账口径从「这个商家这个 SKU 扣了多少」
	// 变成「这家店这个 SKU 扣了多少」，而 before/after 记的正是某一家门店的
	// 水位 —— 不写下是哪一家，同一个 SKU 在五家店的流水会交织成一条谁也
	// 对不平的序列。
	AppendInventoryLog(ctx context.Context, skuID, storeID int64, changeQty int32,
		bizType int16, bizID string, before, after int32) error

	IdempotencyTx

	// ReleaseIdempotencyKey 撤销一次抢占，返回是否真的撤掉了一行。
	//
	// 它只对**处理中**的记录生效（status = 0）。返回 false 表示这把钥匙上的
	// 记录已经不是「处理中」了 —— 那时撤销不该发生，也没有发生。
	ReleaseIdempotencyKey(ctx context.Context, scope string, subj IdempotencySubject, key string) (bool, error)
}

// IdempotencyTx 是幂等存档的「抢占 → 读 → 存档」三步（数据模型 §12）。
//
// 从 OrderTx 里拆出来，是因为 00028 之后它有了**两种作用域**的持有者：
// 租户作用域的 Tx（买家下单、后台那几条商家写接口）与平台作用域的 PlatformTx
// （开店、平台管理员加平台操作员）。同一份实现、同一张表、同三条 SQL ——
// 差别全在事务里那句 set_config 与 RLS 上，这里一个字都不知道自己跑在哪一边。
// 那正是想要的：作用域没有任何一处可以被调用方传错。
//
// ReleaseIdempotencyKey 不在这里：它只服务下单那条跨事务的 SAGA，
// 单事务的写接口不需要「把钥匙还回去」（admin_idempotency.go 的文件头）。
type IdempotencyTx interface {
	// ClaimIdempotencyKey 抢占式插入。抢到返回 true；已存在返回 false。
	ClaimIdempotencyKey(ctx context.Context, scope string, subj IdempotencySubject,
		key, requestHash string) (bool, error)

	// FindIdempotencyKey 读出已存在的那一行。查不到返回 ErrIdempotencyKeyNotFound。
	FindIdempotencyKey(ctx context.Context, scope string, subj IdempotencySubject,
		key string) (IdempotencyRecord, error)

	// FinishIdempotencyKey 把存档写回去（成功或失败都要写）。
	//
	// responseCode 可空：成功那一路存的是契约写明的 201；失败那一路存 nil ——
	// 失败响应的状态码由 handler 按 sentinel 映射，在这里再存一份数字，
	// 同一件事就有了两个可能对不上的真相（存档回放时按哪一个？）。
	FinishIdempotencyKey(ctx context.Context, scope string, subj IdempotencySubject, key string,
		status int16, responseCode *int32, responseBody []byte) error
}

func (t tenantTx) ListSKUsForPricing(ctx context.Context, sc StoreScope, skuIDs []int64) ([]PriceableSKU, error) {
	if len(skuIDs) == 0 {
		// 空数组交给 Postgres 是合法的（回 0 行），但让它走到这里意味着上游的
		// 「至少一行」校验没生效。报错而不是回空：空结果会一路变成一笔 0 元订单。
		return nil, errors.New("定价查询收到了空的 sku 列表")
	}
	if sc.StoreID <= 0 {
		// 没有门店就没有价：视图的键是 (store_id, sku_id)，store_id = 0 会让
		// 这条查询安静地返回 0 行，而调用方会把它读成「这些 SKU 都不可售」。
		return nil, errors.New("定价查询没有门店上下文")
	}
	rows, err := t.q.ListSKUsForPricing(ctx, db.ListSKUsForPricingParams{
		StoreID: sc.StoreID, RegionID: sc.RegionID, SkuIds: skuIDs,
	})
	if err != nil {
		return nil, err
	}
	out := make([]PriceableSKU, 0, len(rows))
	for _, r := range rows {
		out = append(out, PriceableSKU{
			ID:         r.ID,
			ProductID:  r.ProductID,
			Title:      r.Title,
			SpecValues: r.SpecValues,
			ImageURL:   r.ImageUrl,
			PriceCents: r.PriceCents,

			BrandID:      r.BrandID,
			CategoryPath: r.CategoryPath,
		})
	}
	return out, nil
}

func (t tenantTx) FindAddress(ctx context.Context, addressID, userID int64) (Address, error) {
	r, err := t.q.GetUserAddress(ctx, db.GetUserAddressParams{ID: addressID, UserID: userID})
	if errors.Is(err, pgx.ErrNoRows) {
		return Address{}, fmt.Errorf("address %d: %w", addressID, ErrAddressNotFound)
	}
	if err != nil {
		return Address{}, err
	}
	return Address{
		ID:           r.ID,
		ReceiverName: r.ReceiverName,
		Phone:        r.Phone,
		Province:     r.Province,
		City:         r.City,
		District:     r.District,
		Street:       r.Street,
		Detail:       r.Detail,
		RegionCode:   r.RegionCode,
		PostalCode:   r.PostalCode,
	}, nil
}

func (t tenantTx) CreateOrderDraft(ctx context.Context, d NewOrderDraft) (Order, error) {
	r, err := t.q.CreateOrderDraft(ctx, db.CreateOrderDraftParams{
		OrderNo:          d.OrderNo,
		UserID:           d.UserID,
		StoreID:          d.StoreID,
		GoodsAmountCents: d.GoodsAmountCents,
		FreightCents:     d.FreightCents,
		DiscountCents:    d.DiscountCents,
		PayableCents:     d.PayableCents,
		ReceiverSnapshot: d.ReceiverSnapshot,
		Remark:           d.Remark,
		ExpireAt:         pgtype.Timestamptz{Time: d.ExpireAt, Valid: true},
		UserCouponID:     d.UserCouponID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		// 那条 INSERT ... SELECT FROM stores 插了 0 行：门店不存在、
		// 不属于本租户、或已软删。契约的 422（store_id 在请求体里，
		// 不在路径里 —— 数据模型 §4 那条分界线）。
		return Order{}, fmt.Errorf("store %d: %w", d.StoreID, ErrCatalogBadReference)
	}
	if err != nil {
		return Order{}, err
	}
	return Order{
		ID:               r.ID,
		OrderNo:          r.OrderNo,
		UserID:           d.UserID,
		StoreID:          r.StoreID,
		RegionID:         r.RegionID,
		Status:           r.Status,
		GoodsAmountCents: r.GoodsAmountCents,
		FreightCents:     r.FreightCents,
		DiscountCents:    r.DiscountCents,
		PayableCents:     r.PayableCents,
		PaidCents:        r.PaidCents,
		RefundedCents:    r.RefundedCents,
		RefundStatus:     r.RefundStatus,
		ExpireAt:         r.ExpireAt.Time,
		CreatedAt:        r.CreatedAt.Time,
		UserCouponID:     r.UserCouponID,
	}, nil
}

func (t tenantTx) CreateOrderItem(ctx context.Context, it NewOrderItem) error {
	return t.q.CreateOrderItem(ctx, db.CreateOrderItemParams{
		OrderID:       it.OrderID,
		SkuID:         it.SKUID,
		ProductID:     it.ProductID,
		TitleSnapshot: it.TitleSnapshot,
		SpecSnapshot:  it.SpecSnapshot,
		ImageSnapshot: it.ImageSnapshot,
		PriceCents:    it.PriceCents,
		Quantity:      it.Quantity,
		AmountCents:   it.AmountCents,
		DiscountCents: it.DiscountCents,
	})
}

func (t tenantTx) FindOrderByNo(ctx context.Context, orderNo string) (Order, error) {
	r, err := t.q.GetOrderByNo(ctx, orderNo)
	if errors.Is(err, pgx.ErrNoRows) {
		return Order{}, fmt.Errorf("order %s: %w", orderNo, ErrOrderNotFound)
	}
	if err != nil {
		return Order{}, err
	}
	return Order{
		ID:               r.ID,
		OrderNo:          r.OrderNo,
		UserID:           r.UserID,
		StoreID:          r.StoreID,
		RegionID:         r.RegionID,
		Status:           r.Status,
		GoodsAmountCents: r.GoodsAmountCents,
		FreightCents:     r.FreightCents,
		DiscountCents:    r.DiscountCents,
		PayableCents:     r.PayableCents,
		PaidCents:        r.PaidCents,
		RefundedCents:    r.RefundedCents,
		RefundStatus:     r.RefundStatus,
		ExpireAt:         r.ExpireAt.Time,
		CreatedAt:        r.CreatedAt.Time,
		PaidAt:           optTime(r.PaidAt),
		ShippedAt:        optTime(r.ShippedAt),
		FinishedAt:       optTime(r.FinishedAt),
		UserCouponID:     r.UserCouponID,
	}, nil
}

func (t tenantTx) ListOrderLines(ctx context.Context, orderID int64) ([]OrderLine, error) {
	rows, err := t.q.ListOrderItemsForBranch(ctx, orderID)
	if err != nil {
		return nil, err
	}
	out := make([]OrderLine, 0, len(rows))
	for _, r := range rows {
		out = append(out, OrderLine{SKUID: r.SkuID, Quantity: r.Quantity})
	}
	return out, nil
}

func (t tenantTx) PromoteOrderDraft(ctx context.Context, orderNo string) (int64, error) {
	return t.q.PromoteOrderDraft(ctx, orderNo)
}

func (t tenantTx) CloseOrder(ctx context.Context, orderNo string) (int64, error) {
	return t.q.CloseOrder(ctx, orderNo)
}

func (t tenantTx) AppendInventoryLog(ctx context.Context, skuID, storeID int64, changeQty int32,
	bizType int16, bizID string, before, after int32) error {
	if changeQty == 0 {
		// 0 的流水不是流水，是噪声。挡在这里，因为它只可能来自一次算错的差值。
		return fmt.Errorf("sku %d 的库存流水 change_qty 是 0", skuID)
	}
	if storeID <= 0 {
		// 与 DeductInventory 同一条：一行不写明门店的流水对不了账，
		// 而它不会报错，只会在半年后的一次盘点里对不平。
		return fmt.Errorf("sku %d 的库存流水没有门店", skuID)
	}
	return t.q.AppendInventoryLog(ctx, db.AppendInventoryLogParams{
		SkuID:           skuID,
		StoreID:         storeID,
		ChangeQty:       changeQty,
		BizType:         bizType,
		BizID:           bizID,
		BeforeAvailable: before,
		AfterAvailable:  after,
	})
}

func (t tenantTx) ClaimIdempotencyKey(ctx context.Context, scope string, subj IdempotencySubject,
	key, requestHash string) (bool, error) {
	n, err := t.q.ClaimIdempotencyKey(ctx, db.ClaimIdempotencyKeyParams{
		Scope:       scope,
		SubjectKind: subj.kind,
		SubjectID:   subj.id,
		IdemKey:     key,
		RequestHash: requestHash,
	})
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

func (t tenantTx) FindIdempotencyKey(ctx context.Context, scope string, subj IdempotencySubject,
	key string) (IdempotencyRecord, error) {
	r, err := t.q.GetIdempotencyKey(ctx, db.GetIdempotencyKeyParams{
		Scope: scope, SubjectKind: subj.kind, SubjectID: subj.id, IdemKey: key,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return IdempotencyRecord{}, ErrIdempotencyKeyNotFound
	}
	if err != nil {
		return IdempotencyRecord{}, err
	}
	return IdempotencyRecord{
		RequestHash:  r.RequestHash,
		Status:       r.Status,
		ResponseCode: r.ResponseCode,
		ResponseBody: r.ResponseBody,
	}, nil
}

func (t tenantTx) FinishIdempotencyKey(ctx context.Context, scope string, subj IdempotencySubject,
	key string, status int16, responseCode *int32, responseBody []byte) error {
	return t.q.FinishIdempotencyKey(ctx, db.FinishIdempotencyKeyParams{
		Scope:        scope,
		SubjectKind:  subj.kind,
		SubjectID:    subj.id,
		IdemKey:      key,
		Status:       status,
		ResponseCode: responseCode,
		ResponseBody: responseBody,
	})
}

func (t tenantTx) ReleaseIdempotencyKey(ctx context.Context, scope string, subj IdempotencySubject,
	key string) (bool, error) {
	n, err := t.q.ReleaseIdempotencyKey(ctx, db.ReleaseIdempotencyKeyParams{
		Scope: scope, SubjectKind: subj.kind, SubjectID: subj.id, IdemKey: key,
	})
	if err != nil {
		return false, err
	}
	return n == 1, nil
}
