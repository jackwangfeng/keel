package service

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/keel/keel/internal/inventory"
	"github.com/keel/keel/internal/repository"
)

// 补货计算：MCP 工具 restock_plan（AI 经营 M9 任务 3，docs/AI经营-M9设计.md §5）。
//
// 「数字由 Keel 算」：agent 不自己估销量、不自己算补多少，它拿这里的结果去判断、取舍、写证据。
// 所以这里宁可简单、可解释，每一步都能从返回的字段复现：
//
//	有效天数   = min(回看天数, SKU 上架至今的天数) − 断货天数（收盘时可售 ≤ 0 的天，库存服务按流水算）
//	日均销量   = 回看期内已付款件数 ÷ max(有效天数, 1)
//	可售天数   = 可售 ÷ 日均
//	建议补货量 = ⌈日均 × 覆盖天数 − 可售⌉，≤ 0 记 0，否则向上取到 5 的倍数（整箱习惯），上限 1000
//	置信       = 有效天数 < 5 时 low（手册要求 low 的只进简报、不提案）
//
// 分母去掉断货天：断过货的 SKU 按日历天平均会低估需求，越断越少补。不做季节性与促销修正 ——
// 那是「销量预测」的事，这里只做一个人能手算核对的版本。

const (
	restockDefaultCover    = 14
	restockDefaultLookback = 14
	restockMaxSuggest      = 1000
	restockLowConfidence   = 5
	restockMaxLines        = 200
	restockBatch           = 1000
)

// RestockLine 是一个（门店，SKU）的补货计算结果。
type RestockLine struct {
	StoreID       int64    `json:"store_id"`
	StoreName     string   `json:"store_name"`
	SKUID         int64    `json:"sku_id"`
	ProductID     int64    `json:"product_id"`
	ProductTitle  string   `json:"product_title"`
	SKULabel      string   `json:"sku_label"`
	Available     int32    `json:"available"`
	Sold          int64    `json:"sold"`
	EffectiveDays int      `json:"effective_days"`
	StockoutDays  int      `json:"stockout_days"`
	DailyAvg      float64  `json:"daily_avg"`
	DaysOfCover   *float64 `json:"days_of_cover,omitempty"`
	StockoutDate  string   `json:"stockout_date,omitempty"` // 按日均预计卖断的日期（店铺时区）
	Suggested     int      `json:"suggested"`
	Confidence    string   `json:"confidence"` // normal / low
}

// RestockPlan 是一次计算的全部结果。
type RestockPlan struct {
	GeneratedAt  time.Time     `json:"generated_at"`
	Timezone     string        `json:"timezone"`
	CoverDays    int           `json:"cover_days"`
	LookbackDays int           `json:"lookback_days"`
	Lines        []RestockLine `json:"lines"`
	Truncated    bool          `json:"truncated"`
}

// RestockRepository 是补货计算要的仓储能力。
type RestockRepository interface {
	WithTenant(ctx context.Context, fn func(repository.Tx) error) error
	ShopTimezone(ctx context.Context) (string, error)
}

// RestockService 算补货建议。
type RestockService struct {
	repo   RestockRepository
	inv    inventory.Service
	stores *AdminStoreService
	now    func() time.Time
}

func NewRestockService(r RestockRepository, inv inventory.Service, stores *AdminStoreService) *RestockService {
	return &RestockService{repo: r, inv: inv, stores: stores, now: time.Now}
}

// RestockQuery 是计算参数。StoreID 为空即调用者管辖范围内全部营业中的门店。
type RestockQuery struct {
	StoreID      *int64
	CoverDays    int
	LookbackDays int
	OnlyNeeded   bool // 只返回建议补货量 > 0 的
}

// Plan 实现 restock_plan。判权与后台门店库存清单相同（storeOperate）：门店管理员只算得到自己的店。
func (s *RestockService) Plan(ctx context.Context, q RestockQuery) (RestockPlan, error) {
	if _, err := requireStaff(ctx); err != nil {
		return RestockPlan{}, err
	}
	cover, lookback := q.CoverDays, q.LookbackDays
	if cover == 0 {
		cover = restockDefaultCover
	}
	if lookback == 0 {
		lookback = restockDefaultLookback
	}
	if cover < 1 || cover > 90 || lookback < 1 || lookback > 90 {
		return RestockPlan{}, fmt.Errorf("%w: cover_days 与 lookback_days 取 1–90", ErrAdminListBadRequest)
	}
	tzName, err := s.repo.ShopTimezone(ctx)
	if err != nil {
		return RestockPlan{}, err
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
		return RestockPlan{}, err
	}
	if q.StoreID == nil {
		// 管辖范围内的门店：与后台门店列表同一个收窄（门店管理员只有自己的店）。停业的不算。
		for page := 1; ; page++ {
			pg, err := s.stores.ListStores(ctx, nil, false, page, 100)
			if err != nil {
				return RestockPlan{}, err
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
	// 断货天数只能数 SKU 上架之后的那几天：之前的天没有流水，库存服务会按第一条流水的「变动前」算成 0，
	// 把一个今天才建的 SKU 算成断货十几天。所以按「窗口 = min(回看, 上架天数)」分组，各组按自己的窗口问。
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

	plan := RestockPlan{GeneratedAt: s.now().UTC(), Timezone: tzName, CoverDays: cover, LookbackDays: lookback,
		Lines: []RestockLine{}}
	for _, st := range stores {
		var sales map[int64]int64
		if err := s.repo.WithTenant(ctx, func(tx repository.Tx) error {
			var e error
			sales, e = tx.StoreSKUSales(ctx, st.ID, since)
			return e
		}); err != nil {
			return RestockPlan{}, err
		}
		levels := map[int64]inventory.Level{}
		stockout := map[int64]int{}
		for i := 0; i < len(skuIDs); i += restockBatch {
			part := skuIDs[i:min(i+restockBatch, len(skuIDs))]
			lv, err := s.inv.StoreStock(ctx, st.ID, part)
			if err != nil {
				return RestockPlan{}, err
			}
			for k, v := range lv {
				levels[k] = v
			}
		}
		for w, ids := range byWindow {
			for i := 0; i < len(ids); i += restockBatch {
				so, err := s.inv.StockoutDays(ctx, st.ID, ids[i:min(i+restockBatch, len(ids))], w, tzName)
				if err != nil {
					return RestockPlan{}, err
				}
				for k, v := range so {
					stockout[k] = v
				}
			}
		}
		for _, c := range cands {
			line, ok := computeRestockLine(levels[c.SKUID].Available, sales[c.SKUID], lookback, windowOf[c.SKUID],
				stockout[c.SKUID], cover, today)
			if !ok || (q.OnlyNeeded && line.Suggested == 0) {
				continue
			}
			line.StoreID, line.StoreName = st.ID, st.Name
			line.SKUID, line.ProductID, line.ProductTitle = c.SKUID, c.ProductID, c.ProductTitle
			line.SKULabel = skuLabel(c.SKUCode, c.SpecValues)
			plan.Lines = append(plan.Lines, line)
		}
	}
	// 最先卖断的在前；同一天按建议量大的在前。
	sort.SliceStable(plan.Lines, func(i, j int) bool {
		a, b := plan.Lines[i], plan.Lines[j]
		ca, cb := coverOrMax(a), coverOrMax(b)
		if ca != cb {
			return ca < cb
		}
		return a.Suggested > b.Suggested
	})
	if len(plan.Lines) > restockMaxLines {
		plan.Lines, plan.Truncated = plan.Lines[:restockMaxLines], true
	}
	return plan, nil
}

func coverOrMax(l RestockLine) float64 {
	if l.DaysOfCover == nil {
		return math.MaxFloat64
	}
	return *l.DaysOfCover
}

// computeRestockLine 是一个（门店，SKU）的计算，纯函数（单测覆盖边界）。回看期内一件没卖出去的不出结果（ok = false）。
// existingDays 是 SKU 上架至今的天数（向上取整）；today 是店铺时区的今天零点。
func computeRestockLine(available int32, sold int64, lookback, existingDays, stockoutDays, cover int,
	today time.Time) (RestockLine, bool) {
	if sold <= 0 {
		return RestockLine{}, false
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
	l := RestockLine{Available: available, Sold: sold, EffectiveDays: effective, StockoutDays: stockoutDays,
		DailyAvg: math.Round(avg*100) / 100, Confidence: "normal"}
	if effective < restockLowConfidence {
		l.Confidence = "low"
	}
	doc := 0.0
	if available > 0 {
		doc = float64(available) / avg
	}
	rounded := math.Round(doc*10) / 10
	l.DaysOfCover = &rounded
	l.StockoutDate = today.AddDate(0, 0, int(math.Floor(doc))).Format("2006-01-02")
	need := int(math.Ceil(avg*float64(cover) - float64(available)))
	if need > 0 {
		need = (need + 4) / 5 * 5
		if need > restockMaxSuggest {
			need = restockMaxSuggest
		}
		l.Suggested = need
	}
	return l, true
}

// skuLabel 是给人读的规格：「颜色：黑 / 尺码：M」；没有规格值时用 sku_code。
func skuLabel(code, specJSON string) string {
	var m map[string]string
	if json.Unmarshal([]byte(specJSON), &m) != nil || len(m) == 0 {
		return code
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"："+m[k])
	}
	return strings.Join(parts, " / ")
}
