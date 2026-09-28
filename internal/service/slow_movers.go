package service

import (
	"context"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/keel/keel/internal/inventory"
	"github.com/keel/keel/internal/repository"
)

// 滞销清仓计算：MCP 工具 slow_movers（AI 经营 M10 计算工具，docs/AI经营-M10M11设计.md §2）。
//
// 与 restock_plan（service/restock.go）同一套口径——有效天数、日均、断货天去分母——只是反过来看：
// 补货问「还能卖多少天」，这里问「已经压了多少天的货没动」：
//
//	库存周转天数 = 可售 ÷ 日均（分母同样去掉断货天，理由见 restock.go 文件头）
//
// 日均为 0（回看期内一件没卖出去）记 null：这是「从没卖出去过」，比任何一个有限的周转天数都更
// 该清，排序时排在最前。不算建议动作（打几折、清多少件）——那是「滞销清仓」手册与
// propose_flash_price 的事，这里只给一份「哪些该清」的名单。
const (
	slowMoversDefaultLookback = 30
	slowMoversDefaultMinAvail = 10
	slowMoversDefaultLimit    = 50
	slowMoversMaxLimit        = 200
)

// SlowMoverLine 是一个（门店，SKU）的滞销计算结果。
type SlowMoverLine struct {
	StoreID       int64    `json:"store_id"`
	StoreName     string   `json:"store_name"`
	SKUID         int64    `json:"sku_id"`
	ProductID     int64    `json:"product_id"`
	ProductTitle  string   `json:"product_title"`
	SKULabel      string   `json:"sku_label"`
	Available     int32    `json:"available"`
	Sold          int64    `json:"sold"`
	EffectiveDays int      `json:"effective_days"`
	DailyAvg      float64  `json:"daily_avg"`
	DaysOfStock   *float64 `json:"days_of_stock,omitempty"` // null：回看期内一件没卖出去，排序排最前
}

// SlowMoversResult 是一次计算的全部结果。
type SlowMoversResult struct {
	GeneratedAt  time.Time       `json:"generated_at"`
	Timezone     string          `json:"timezone"`
	LookbackDays int             `json:"lookback_days"`
	MinAvailable int32           `json:"min_available"`
	Lines        []SlowMoverLine `json:"lines"`
	Truncated    bool            `json:"truncated"`
}

// SlowMoversRepository 是滞销计算要的仓储能力，与 RestockRepository 同一个形状。
type SlowMoversRepository interface {
	WithTenant(ctx context.Context, fn func(repository.Tx) error) error
	ShopTimezone(ctx context.Context) (string, error)
}

// SlowMoversService 算滞销清单。
type SlowMoversService struct {
	repo   SlowMoversRepository
	inv    inventory.Service
	stores *AdminStoreService
	now    func() time.Time
}

func NewSlowMoversService(r SlowMoversRepository, inv inventory.Service, stores *AdminStoreService) *SlowMoversService {
	return &SlowMoversService{repo: r, inv: inv, stores: stores, now: time.Now}
}

// SlowMoversQuery 是计算参数。StoreID 为空即调用者管辖范围内全部营业中的门店。
type SlowMoversQuery struct {
	StoreID      *int64
	LookbackDays int
	MinAvailable int32
	Limit        int
}

// Plan 实现 slow_movers。判权与 restock_plan 逐字相同（storeOperate）：门店管理员只算得到自己的店。
func (s *SlowMoversService) Plan(ctx context.Context, q SlowMoversQuery) (SlowMoversResult, error) {
	if _, err := requireStaff(ctx); err != nil {
		return SlowMoversResult{}, err
	}
	lookback := q.LookbackDays
	if lookback == 0 {
		lookback = slowMoversDefaultLookback
	}
	if lookback < 1 || lookback > 90 {
		return SlowMoversResult{}, fmt.Errorf("%w: lookback_days 取 1–90", ErrAdminListBadRequest)
	}
	minAvail := q.MinAvailable
	if minAvail == 0 {
		minAvail = slowMoversDefaultMinAvail
	}
	if minAvail < 0 {
		return SlowMoversResult{}, fmt.Errorf("%w: min_available 不能是负数", ErrAdminListBadRequest)
	}
	limit := q.Limit
	if limit == 0 {
		limit = slowMoversDefaultLimit
	}
	if limit < 1 || limit > slowMoversMaxLimit {
		return SlowMoversResult{}, fmt.Errorf("%w: limit 取 1–%d", ErrAdminListBadRequest, slowMoversMaxLimit)
	}

	tzName, err := s.repo.ShopTimezone(ctx)
	if err != nil {
		return SlowMoversResult{}, err
	}
	tzName, loc := reportLocation(tzName)
	now := s.now().In(loc)
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	since := today.AddDate(0, 0, -(lookback - 1))

	type storeRef struct {
		ID   int64
		Name string
	}
	var stores []storeRef
	var cands []repository.RestockCandidate
	err = s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		if q.StoreID != nil {
			if _, err := authorizeStore(ctx, tx, *q.StoreID, storeOperate); err != nil {
				return err
			}
			st, err := tx.FindStore(ctx, *q.StoreID)
			if err != nil {
				return err
			}
			stores = []storeRef{{st.ID, st.Name}}
		}
		var err error
		cands, err = tx.RestockCandidates(ctx)
		return err
	})
	if err != nil {
		return SlowMoversResult{}, err
	}
	if q.StoreID == nil {
		// 管辖范围内的门店：与 restock_plan 同一个收窄（门店管理员只有自己的店）。停业的不算。
		for page := 1; ; page++ {
			pg, err := s.stores.ListStores(ctx, nil, false, page, 100)
			if err != nil {
				return SlowMoversResult{}, err
			}
			for _, st := range pg.Items {
				if st.Status == 1 {
					stores = append(stores, storeRef{st.ID, st.Name})
				}
			}
			if int64(page*pg.PageSize) >= pg.Total || len(pg.Items) == 0 {
				break
			}
		}
	}

	skuIDs := make([]int64, 0, len(cands))
	// 断货天数只能数 SKU 上架之后的那几天，理由与算法见 restock.go 的同一段注释。
	windowOf := make(map[int64]int, len(cands))
	byWindow := map[int][]int64{}
	for _, c := range cands {
		skuIDs = append(skuIDs, c.SKUID)
		w := lookback
		if ex := int(math.Ceil(now.Sub(c.CreatedAt).Hours() / 24)); ex > 0 && ex < w {
			w = ex
		}
		windowOf[c.SKUID] = w
		byWindow[w] = append(byWindow[w], c.SKUID)
	}

	result := SlowMoversResult{GeneratedAt: s.now().UTC(), Timezone: tzName, LookbackDays: lookback,
		MinAvailable: minAvail, Lines: []SlowMoverLine{}}
	for _, st := range stores {
		var sales map[int64]int64
		if err := s.repo.WithTenant(ctx, func(tx repository.Tx) error {
			var e error
			sales, e = tx.StoreSKUSales(ctx, st.ID, since)
			return e
		}); err != nil {
			return SlowMoversResult{}, err
		}
		levels := map[int64]inventory.Level{}
		stockout := map[int64]int{}
		for i := 0; i < len(skuIDs); i += restockBatch {
			part := skuIDs[i:min(i+restockBatch, len(skuIDs))]
			lv, err := s.inv.StoreStock(ctx, st.ID, part)
			if err != nil {
				return SlowMoversResult{}, err
			}
			for k, v := range lv {
				levels[k] = v
			}
		}
		for w, ids := range byWindow {
			for i := 0; i < len(ids); i += restockBatch {
				so, err := s.inv.StockoutDays(ctx, st.ID, ids[i:min(i+restockBatch, len(ids))], w, tzName)
				if err != nil {
					return SlowMoversResult{}, err
				}
				for k, v := range so {
					stockout[k] = v
				}
			}
		}
		for _, c := range cands {
			line, ok := computeSlowMoverLine(levels[c.SKUID].Available, sales[c.SKUID], lookback, windowOf[c.SKUID],
				stockout[c.SKUID], minAvail)
			if !ok {
				continue
			}
			line.StoreID, line.StoreName = st.ID, st.Name
			line.SKUID, line.ProductID, line.ProductTitle = c.SKUID, c.ProductID, c.ProductTitle
			line.SKULabel = skuLabel(c.SKUCode, c.SpecValues)
			result.Lines = append(result.Lines, line)
		}
	}
	sortSlowMoverLines(result.Lines)
	if len(result.Lines) > limit {
		result.Lines, result.Truncated = result.Lines[:limit], true
	}
	return result, nil
}

// computeSlowMoverLine 是一个（门店，SKU）的计算，纯函数（单测覆盖边界）。
// available < minAvailable 的不出结果（ok = false）——滞销只看压了货的，可售数不够的不必操心。
func computeSlowMoverLine(available int32, sold int64, lookback, existingDays, stockoutDays int,
	minAvailable int32) (SlowMoverLine, bool) {
	if available < minAvailable {
		return SlowMoverLine{}, false
	}
	window := lookback
	if existingDays > 0 && existingDays < window {
		window = existingDays
	}
	effective := window - stockoutDays
	if effective < 0 {
		effective = 0
	}
	avg := float64(sold) / float64(max(effective, 1))
	l := SlowMoverLine{Available: available, Sold: sold, EffectiveDays: effective,
		DailyAvg: math.Round(avg*100) / 100}
	if avg > 0 {
		days := float64(available) / avg
		rounded := math.Round(days*10) / 10
		l.DaysOfStock = &rounded
	}
	return l, true
}

// sortSlowMoverLines 排序，纯函数（单测覆盖）：从没卖出去过的（days_of_stock 为 null）排最前——
// 比任何一个有限的周转天数都更该清；其余按周转天数降序（压得越久越靠前）。
func sortSlowMoverLines(lines []SlowMoverLine) {
	sort.SliceStable(lines, func(i, j int) bool {
		a, b := lines[i], lines[j]
		an, bn := a.DaysOfStock == nil, b.DaysOfStock == nil
		if an != bn {
			return an
		}
		if an {
			return false
		}
		return *a.DaysOfStock > *b.DaysOfStock
	})
}
