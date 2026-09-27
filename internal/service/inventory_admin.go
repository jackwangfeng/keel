package service

// 后台库存的读写在 core 这一侧的全部业务（微服务拆分阶段 1a）。
//
// ===========================================================================
// 拆分之后每一条后台库存写的形状：core 判完，再调库存服务
// ===========================================================================
//
// 拆分前，后台改库存是**一条 SQL** 同时回答四个问题：SKU 可见吗、这家店卖它吗、
// 前提（expected / 不得为负）成立吗、写成功了吗 —— 同一个 MVCC 快照。库存搬走之后，
// 前两个问题要 JOIN skus / stores / 两张覆盖表，只有 core 答得出；后两个只有库存服务答得出。
// 于是每一条都变成两段：
//
//  1. core 事务：解析门店（单店捷径的 SoleStore）、判权（authorizeStore）、判可售
//     （SKUSellableInStore / AdminFindSKU）。不成立就 404，库存服务一次都不调。
//  2. **提交之后**调库存服务写（inventory.Service.Set / Adjust），写与流水在库存服务的
//     一个本地事务里。
//
// 调用刻意放在 core 事务**之外**：单体形态下库存池就是业务池，在持有一条业务连接的
// 事务里再向同一个池要第二条，并发一高就是整池互等（每个请求都攥着一条、等着另一条）；
// 拆分形态下则是攥着一条业务连接等一次网络往返。两种都不该有。
//
// 代价是判定与写之间隔着一次调用，而不是同一个快照：窗口里商品被下架、门店被删，
// 结果是给一件此刻卖不了的商品多录了一次库存 —— 没有任何地方会用到它（下单那一侧自己
// 判「这家店卖不卖」），方向是无害的。
//
// ===========================================================================
// 结果未知
// ===========================================================================
//
// 拆分形态下库存服务可能「没回答」（inventory.ErrOutcomeUnknown）。一律回 503，写可能已经
// 生效也可能没有，客户端原样重试：
//
//	PUT（比较并设置）  天然安全 —— 第一次若已生效，重试时水位已不等于 expected，只会 409，
//	                   current 就是它想设的那个值。
//	POST 相对调整      靠 biz_id（「adj:员工:幂等键」）在库存服务那一侧幂等，同一把
//	                   Idempotency-Key 重试不会加两遍，见 adjustInventory。
//	建 SKU 的初始库存  靠 InitSKUs 的两道守卫可以放心重放，见 initSKUStock。
//
// 单体形态下这些错误不会出现。

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/keel/keel/internal/inventory"
	"github.com/keel/keel/internal/repository"
)

// ---------------------------------------------------------------------------
// 错误翻译：库存服务的错误 → handler 已经认得的那几个
// ---------------------------------------------------------------------------
//
// handler 的错误映射表（admin_catalog.go / admin_store.go）按 repository 的 sentinel 与
// 带数据的错误类型分支，契约的状态码与 Problem 形状都挂在那上面。这里把库存服务的错误
// 翻成同一批类型，于是拆分对 handler 与契约**不可见**：同一个请求在单体与拆分形态下
// 回的是同一个状态码、同一个 type、同一个 current。

func storeInventoryOf(s inventory.Stock) repository.StoreInventory {
	return repository.StoreInventory{SKUID: s.SKUID, StoreID: s.StoreID,
		AvailableQty: s.Available, WarningQty: s.Warning, UpdatedAt: s.UpdatedAt}
}

// ---------------------------------------------------------------------------
// 后台商品 / SKU 页上的库存数（跨门店合计）
// ---------------------------------------------------------------------------

// productTotalStock 算一批商品的 total_stock：未软删 SKU（core 的判断）的跨门店可售之和
// （库存服务的合计）。与拆分前 admin_products.sql 的 LATERAL 同一个公式，缺行记 0。
func productTotalStock(ctx context.Context, repo tenantRunner, inv inventory.Service,
	productIDs []int64) (map[int64]int32, error) {
	out := make(map[int64]int32, len(productIDs))
	if len(productIDs) == 0 {
		return out, nil
	}
	var skus map[int64][]int64
	if err := repo.WithTenant(ctx, func(tx repository.Tx) error {
		var err error
		skus, err = tx.AdminLiveSKUsOfProducts(ctx, productIDs)
		return err
	}); err != nil {
		return nil, err
	}
	var ids []int64
	for _, s := range skus {
		ids = append(ids, s...)
	}
	if len(ids) == 0 {
		// 一个未软删 SKU 都没有（新建的商品）：合计就是 0，不必问库存服务。
		return out, nil
	}
	totals, err := inv.SKUTotals(ctx, ids)
	if err != nil {
		return nil, err
	}
	for pid, s := range skus {
		var sum int32
		for _, id := range s {
			sum += totals[id].Available
		}
		out[pid] = sum
	}
	return out, nil
}

// fillProductStock 给一批后台商品填 total_stock。库存服务没回答时返回错误（读页面回 503：
// 后台那个数是商家用来决定补不补货的，给一个编出来的 0 比不给更糟）。
func fillProductStock(ctx context.Context, repo tenantRunner, inv inventory.Service,
	items []repository.AdminProduct) error {
	ids := make([]int64, 0, len(items))
	for _, p := range items {
		ids = append(ids, p.ID)
	}
	totals, err := productTotalStock(ctx, repo, inv, ids)
	if err != nil {
		return err
	}
	for i := range items {
		items[i].TotalStock = totals[items[i].ID]
	}
	return nil
}

// fillSKUStock 给一批后台 SKU 填 available_qty（跨门店 sum）与 warning_qty（max）。
func fillSKUStock(ctx context.Context, inv inventory.Service, skus []repository.AdminSKU) error {
	ids := make([]int64, 0, len(skus))
	for _, s := range skus {
		ids = append(ids, s.ID)
	}
	totals, err := inv.SKUTotals(ctx, ids)
	if err != nil {
		return err
	}
	for i := range skus {
		t := totals[skus[i].ID]
		skus[i].AvailableQty, skus[i].WarningQty = t.Available, t.Warning
	}
	return nil
}

// echoStockBestEffort 是**写接口回显**上的库存数：写已经提交了，库存服务没回答时
// 不能回 503（客户端会以为没改成），只能照常回 200、库存数留 0，并喊一条 WARN。
//
// 这是拆分形态下唯一一处「必填字段可能不是真值」的地方，契约在 AdminProduct.total_stock
// 与 AdminSku.available_qty 上写明了：写接口回显里的库存数以随后的 GET 为准。
// 读接口（列表、详情）不走这里 —— 它们在库存服务不可用时回 503。
func echoStockBestEffort(ctx context.Context, what string, err error) {
	if err == nil {
		return
	}
	if inventory.IsUnavailable(err) {
		slog.WarnContext(ctx, what+"：写已提交，但回显里的库存数没取到（库存服务不可用），按 0 回显；以随后的 GET 为准",
			"err", err)
		return
	}
	// 不是「对面不在」而是别的错（数据库、代码）：写同样已经提交，同样只能按 0 回显，
	// 但这是要人看的，所以是 ERROR。
	slog.ErrorContext(ctx, what+"：写已提交，但回显里的库存数取失败，按 0 回显", "err", err)
}

// ---------------------------------------------------------------------------
// 建 SKU 的初始库存
// ---------------------------------------------------------------------------

// initSKUStock 给刚建好（已提交）的 SKU 建第一行库存：默认门店，初始量取建 SKU 的入参。
//
// ===========================================================================
// 为什么是「先 SKU、后库存行」，以及中间那个窗口为什么安全
// ===========================================================================
//
// 拆分前两条 INSERT 在同一个事务里。拆开之后必须挑一个先后：
//
//   - 先 SKU 后库存行（现在的做法）：两步之间，SKU 已经可见而库存行还没有 ——
//     缺行 ≡ 可售 0，这个 SKU 在窗口里表现为「在售、0 件」，下单扣减判「库存不足」。
//     **方向是少卖**，而且新建的 SKU 本来就还没上架给任何人买过。
//   - 先库存行后 SKU：SKU 那一步失败（货号撞车、商品不存在）就留下一行指向不存在 SKU 的
//     库存 —— 拆分之后没有外键替我们挡，它会一直留在库存库里。
//
// 第二步失败（拆分形态下库存服务不可用）时回 503，SKU **已经建好并存了档**。客户端拿
// 同一把 Idempotency-Key 重试会走幂等重放，重放那一支也会调这里 —— 于是「重试」就是
// 「补建」，不需要任何新的接口。重复调用是安全的：库存服务的 InvInitSKU 在这家店已有
// 这一行、或这个 SKU 在任何一家店已有行时什么都不做（不覆盖已经卖掉的量，也不在换了
// 默认门店之后再多建一份）。
func initSKUStock(ctx context.Context, repo tenantRunner, inv inventory.Service, sku repository.AdminSKU) error {
	var (
		storeID int64
		ok      bool
	)
	if err := repo.WithTenant(ctx, func(tx repository.Tx) error {
		var err error
		storeID, ok, err = tx.DefaultStoreForNewSKU(ctx)
		return err
	}); err != nil {
		return err
	}
	if !ok {
		// 没有默认门店：一行都不建，这不是失败（缺行 ≡ 可售 0，「开店即营业」）。
		return nil
	}
	err := inv.InitSKUs(ctx, []inventory.InitRow{{
		SKUID: sku.ID, StoreID: storeID, Available: sku.AvailableQty, Warning: sku.WarningQty,
	}})
	if err != nil {
		return fmt.Errorf("sku %d 已建好，初始库存还没写进库存服务（用同一个 Idempotency-Key 重试即可补上）: %w",
			sku.ID, err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// 比较并设置
// ---------------------------------------------------------------------------

// setStockSole 是单店捷径那条 PUT /admin/skus/{sku_id}/inventory 的写：
// 门店由 SoleStore 解析，缺行是 404（AllowInsert = false，与拆分前的纯 UPDATE 同一个语义）。
func setStockSole(ctx context.Context, repo tenantRunner, inv inventory.Service, skuID int64,
	expected, want int32, warning *int32, bizID string) (repository.Inventory, error) {

	var storeID int64
	err := repo.WithTenant(ctx, func(tx repository.Tx) error {
		// 「恰好一家未软删门店才可用，否则 409 store-ambiguous」—— 不猜一家
		// （AdminCatalogService.SetInventory 上那段注释）。
		var e error
		if storeID, e = tx.SoleStore(ctx); e != nil {
			return e
		}
		if _, e := authorizeStore(ctx, tx, storeID, storeOperate); e != nil {
			return e
		}
		// SKU 可见且未软删：拆分前 SetInventoryByCAS 里 JOIN skus 的那一半。
		// 软删的 SKU 契约说一律 404 —— 不判的话后台能给一个「已经不存在」的规格改库存。
		if _, e := tx.AdminFindSKU(ctx, skuID); errors.Is(e, repository.ErrCatalogNotFound) {
			return fmt.Errorf("sku %d: %w", skuID, repository.ErrSKUNotInTenant)
		} else if e != nil {
			return e
		}
		return nil
	})
	if err != nil {
		return repository.Inventory{}, err
	}

	s, err := inv.Set(ctx, inventory.SetRequest{
		SKUID: skuID, StoreID: storeID, Available: want, Expected: expected,
		Warning: warning, AllowInsert: false, BizID: bizID,
	})
	var conflict *inventory.ConflictError
	switch {
	case err == nil:
		refreshSKUStockFlag(ctx, repo, inv, storeID, skuID) // 商品列表的有货排序（stock_flags.go）
		return repository.Inventory{SKUID: skuID, StoreID: storeID,
			AvailableQty: s.Available, WarningQty: s.Warning, UpdatedAt: s.UpdatedAt}, nil
	case errors.As(err, &conflict):
		c := conflict.Current
		return repository.Inventory{}, &repository.InventoryConflict{SKUID: skuID, Expected: expected,
			Current: repository.Inventory{SKUID: skuID, StoreID: storeID,
				AvailableQty: c.Available, WarningQty: c.Warning, UpdatedAt: c.UpdatedAt}}
	case errors.Is(err, inventory.ErrNotFound):
		// 这家店没有这一行：拆分前 visible_rows = 0 的那一支，契约 404。
		return repository.Inventory{}, fmt.Errorf("sku %d 在门店 %d 没有库存行: %w",
			skuID, storeID, repository.ErrSKUNotInTenant)
	default:
		return repository.Inventory{}, err
	}
}

// setStockInStore 是按门店那条 PUT /admin/stores/{store_id}/skus/{sku_id}/inventory 的写：
// 缺行且 expected = 0 时首次录入（AllowInsert = true）。
func setStockInStore(ctx context.Context, repo tenantRunner, inv inventory.Service, storeID, skuID int64,
	in repository.InventorySet) (repository.StoreInventory, error) {

	err := repo.WithTenant(ctx, func(tx repository.Tx) error {
		if _, e := authorizeStore(ctx, tx, storeID, storeOperate); e != nil {
			return e
		}
		return requireSellable(ctx, tx, storeID, skuID)
	})
	if err != nil {
		return repository.StoreInventory{}, err
	}
	s, err := inv.Set(ctx, inventory.SetRequest{
		SKUID: skuID, StoreID: storeID, Available: in.AvailableQty, Expected: in.ExpectedAvailableQty,
		Warning: in.WarningQty, AllowInsert: true, BizID: in.BizID,
	})
	var conflict *inventory.ConflictError
	switch {
	case err == nil:
		refreshSKUStockFlag(ctx, repo, inv, storeID, skuID) // 商品列表的有货排序（stock_flags.go）
		return storeInventoryOf(s), nil
	case errors.As(err, &conflict):
		// CAS 对不上。缺行（而 expected 不是 0）时当前值就是「可售 0」。
		return repository.StoreInventory{}, &repository.StoreInventoryConflict{Current: storeInventoryOf(conflict.Current)}
	case errors.Is(err, inventory.ErrNotFound):
		// AllowInsert 为真时走不到；真走到了是两边版本不一致，按 404 回而不是 500。
		return repository.StoreInventory{}, fmt.Errorf("sku %d 在门店 %d: %w", skuID, storeID, repository.ErrCatalogNotFound)
	default:
		return repository.StoreInventory{}, err
	}
}

// requireSellable 是改库存之前的可售判定（拆分前两条写语句里的 sellable CTE）。
// 三种 404 合成一个：门店 / SKU 不可见或已软删、这家店或它所在大区下架了这件商品 ——
// 两个 id 都在路径里，「这个 URI 下没有这个资源」是 404 的本义。
func requireSellable(ctx context.Context, tx repository.Tx, storeID, skuID int64) error {
	ok, err := tx.SKUSellableInStore(ctx, storeID, skuID)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("sku %d 在门店 %d 不可见或已下架: %w", skuID, storeID, repository.ErrCatalogNotFound)
	}
	return nil
}

// ---------------------------------------------------------------------------
// 相对调整
// ---------------------------------------------------------------------------

// adjustInventory 是相对调整的全部业务，两条路径共用（按门店那条与单店捷径，
// 后者 storeID 传 0，用 SoleStore 解析）。
//
// ===========================================================================
// 为什么它要幂等键，而 PUT 那条不要
// ===========================================================================
//
// PUT 是绝对值：同一个请求发两次，结果与发一次相同。相对调整不是 ——「+100」因为网络
// 超时被客户端重发一次就是 +200，而且没有任何东西会响。
//
// ===========================================================================
// 拆分之后的三段（原先是一个事务）
// ===========================================================================
//
// 拆分前：抢占幂等键、调整、写流水、存档，全在 core 的**同一个事务**里。库存搬走之后
// 调整与流水在库存库，幂等键与存档在业务库，没有一个事务装得下四件事。现在是三段：
//
//  1. core 事务：已经有这把钥匙的存档 → 按存档重放（请求哈希照旧比对，不同即 422）；
//     没有 → 解析门店、判权、判可售。
//  2. 调库存服务 Adjust（事务之外，理由见文件头）。**它自己按 biz_id 幂等**：
//     biz_id =「adj:员工:幂等键」，同一个 biz_id 只生效一次，第二次按第一次的流水回原结果。
//  3. core 事务：抢占这把钥匙并存档（idempotentTx，回调里只返回第 2 步的结果）。
//     抢占失败（并发的同一个请求先存了档）→ 按存档重放，与拆分前同一段判定。
//
// 于是任何一段之后断掉，客户端用同一把钥匙重试都收敛到同一个结果：第 2 步之后断掉，
// 重试的第 2 步是一次重放（不会加两遍）；第 3 步之后断掉，重试在第 1 步就按存档回了。
// 409（扣完会变负）在第 2 步就回了，什么都没写、什么都没存 —— 补完货之后拿同一把钥匙
// 重试可以成功，失败的请求等于没有发生过，与拆分前一致。
//
// 同一把钥匙配了不同的请求体、而第一个还没存档时，第 2 步里库存服务会发现同一个 biz_id
// 上一次是别的 SKU / 门店 / 数量（ErrBizIDReused），翻成 422 idempotency-key-reused。
//
// ===========================================================================
// 请求哈希里的门店位
// ===========================================================================
//
// 路径参数进哈希（adminRequestHash 的规矩）。捷径的门店不在路径上，写 0 —— 门店 id 是
// 自增主键，不会是 0。于是同一把钥匙先打捷径、再打按门店那条，哪怕落到同一家店，
// 也是两个不同的请求（422），不会被当成重放。
//
// 通知：不发。动作是商家自己做的，改到预警线以下时他正看着那个数（notification_policy.go）。
func adjustInventory(ctx context.Context, repo tenantRunner, inv inventory.Service, storeID, skuID int64,
	in InventoryAdjustInput, idemKey string) (repository.StoreInventory, bool, error) {

	staff, err := requireStaff(ctx)
	if err != nil {
		return repository.StoreInventory{}, false, err
	}
	// 校验排在一切之前：一个注定被拒的请求不该占掉客户端那把钥匙，也不该惊动库存服务。
	if in.Delta == 0 {
		// 一次什么都不改的调整只会在流水里留下一行 before = after 的噪声。
		return repository.StoreInventory{}, false,
			fmt.Errorf("%w: delta 不能为 0", ErrCatalogBadRequest)
	}
	if in.Delta > maxInventoryAdjustDelta || in.Delta < -maxInventoryAdjustDelta {
		// available_qty 是 INT：没有上限的话几次重复提交就能把它推到溢出，
		// 而溢出在 PG 里是 22003，会以 500 的样子出现。
		return repository.StoreInventory{}, false, fmt.Errorf(
			"%w: delta 是 %d，绝对值上限是 %d", ErrCatalogBadRequest, in.Delta, maxInventoryAdjustDelta)
	}
	if err := checkOptText("reason", in.Reason, maxInventoryAdjustReason); err != nil {
		return repository.StoreInventory{}, false, err
	}
	if idemKey == "" {
		return repository.StoreInventory{}, false, ErrIdempotencyKeyMissing
	}
	hash, err := adminRequestHash([]int64{storeID, skuID}, in)
	if err != nil {
		return repository.StoreInventory{}, false, err
	}
	subj := repository.StaffSubject(staff.StaffID)
	// biz_id：谁、哪一次请求。幂等存档 24 小时后会被清掉，员工 id 不会 ——
	// 过了存档期，流水里仍然说得出是谁调的。它同时是库存服务那一侧的幂等键。
	bizID := fmt.Sprintf("adj:%d:%s", staff.StaffID, idemKey)

	// 第 1 段：存档重放，或者解析 + 判权 + 判可售。
	var (
		target   = storeID
		archived *repository.StoreInventory
	)
	err = repo.WithTenant(ctx, func(tx repository.Tx) error {
		_, e := tx.FindIdempotencyKey(ctx, scopeAdminInventoryAdjust, subj, idemKey)
		switch {
		case e == nil:
			v, e := replayArchived[repository.StoreInventory](ctx, tx, scopeAdminInventoryAdjust, subj, idemKey, hash)
			if e != nil {
				return e
			}
			archived = &v
			return nil
		case errors.Is(e, repository.ErrIdempotencyKeyNotFound):
		default:
			return e
		}
		if target == 0 {
			// 单店捷径。解析与判权在同一个事务里：分开的话，中间的一次开店会让
			// 「解析到 A 店」与「调进 A 店」之间出现窗口，而那时正确答案已经是 409 了。
			sole, e := tx.SoleStore(ctx)
			if e != nil {
				return e
			}
			target = sole
		}
		if _, e := authorizeStore(ctx, tx, target, storeOperate); e != nil {
			return e
		}
		return requireSellable(ctx, tx, target, skuID)
	})
	if err != nil {
		return repository.StoreInventory{}, false, err
	}
	if archived != nil {
		return *archived, true, nil
	}

	// 第 2 段：库存服务，按 biz_id 幂等。
	res, err := inv.Adjust(ctx, inventory.AdjustRequest{
		SKUID: skuID, StoreID: target, Delta: in.Delta, Reason: in.Reason, BizID: bizID,
	})
	var short *inventory.InsufficientError
	switch {
	case err == nil:
		refreshSKUStockFlag(ctx, repo, inv, target, skuID) // 商品列表的有货排序（stock_flags.go）
	case errors.As(err, &short):
		return repository.StoreInventory{}, false, &repository.InventoryInsufficient{
			Delta: in.Delta, Current: storeInventoryOf(short.Current)}
	case errors.Is(err, inventory.ErrBizIDReused):
		return repository.StoreInventory{}, false, fmt.Errorf("%w: %v", ErrIdempotencyKeyReused, err)
	default:
		return repository.StoreInventory{}, false, err
	}
	out := storeInventoryOf(res.Stock)

	// 第 3 段：抢占 + 存档。回调里不做任何事，只把第 2 段的结果交出去存档。
	return idempotentTx(ctx, repo, subj, scopeAdminInventoryAdjust, idemKey, hash, archivedOK,
		func(repository.Tx) (repository.StoreInventory, error) { return out, nil })
}

// ---------------------------------------------------------------------------
// 门店库存清单
// ---------------------------------------------------------------------------

// listStoreInventories 是 GET /admin/stores/{store_id}/inventories 的全部读：
//
//  1. low_stock_only 时先向库存服务要这家店「水位高于预警线」的 SKU（补集就是低库存，
//     缺行的 SKU 算低库存 —— 与拆分前 LEFT JOIN 的判据逐点相同）；
//  2. core 事务：判权、确认门店存在、按 SKU 分页（排除第 1 步那批），total 精确；
//  3. 这一页的 SKU 向库存服务批量要水位，合并。缺行记 0 / 0，updated_at 回落到 SKU 自己的。
//
// 第 1 步排在判权之前：它只是一次读，判权不过时它的结果不会离开这个函数。放在后面的话
// 要么再开一个事务，要么在持有业务连接时调库存服务（文件头说了为什么不行）。
//
// 与拆分前的差别：第 1 步与第 3 步是两次读，中间有并发扣减时，一行可能按第 1 步的水位
// 进了「低库存」这一页、却按第 3 步的水位显示得比预警线高。这是后台清单，下一次刷新就对了。
func listStoreInventories(ctx context.Context, repo tenantRunner, inv inventory.Service, storeID int64,
	lowStockOnly bool, page, pageSize int) (StoreInventoryPage, error) {

	page, pageSize = clampPaging(page, pageSize)
	out := StoreInventoryPage{Items: []repository.StoreInventory{}, Page: page, PageSize: pageSize}

	exclude := []int64{}
	if lowStockOnly {
		healthy, err := inv.HealthySKUs(ctx, storeID)
		if err != nil {
			return StoreInventoryPage{}, err
		}
		exclude = healthy
	}

	var skus []repository.StoreInventorySKU
	err := repo.WithTenant(ctx, func(tx repository.Tx) error {
		if _, e := authorizeStore(ctx, tx, storeID, storeOperate); e != nil {
			return e
		}
		if _, _, e := tx.StoreScope(ctx, storeID); e != nil {
			return e
		}
		items, total, e := tx.ListStoreInventorySKUs(ctx, exclude,
			int32(pageSize), int32(offsetOf(page, pageSize)))
		if e != nil {
			return e
		}
		skus, out.Total = items, total
		return nil
	})
	if err != nil {
		return StoreInventoryPage{}, err
	}

	ids := make([]int64, 0, len(skus))
	for _, s := range skus {
		ids = append(ids, s.SKUID)
	}
	levels, err := inv.StoreStock(ctx, storeID, ids)
	if err != nil {
		return StoreInventoryPage{}, err
	}
	for _, s := range skus {
		l := levels[s.SKUID]
		updated := s.UpdatedAt
		if l.Exists {
			updated = l.UpdatedAt
		}
		out.Items = append(out.Items, repository.StoreInventory{
			SKUID: s.SKUID, StoreID: storeID, SKUCode: s.SKUCode,
			AvailableQty: l.Available, WarningQty: l.Warning, UpdatedAt: updated,
		})
	}
	return out, nil
}
