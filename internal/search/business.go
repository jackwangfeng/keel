package search

import "sort"

// 业务重排（语义检索层 §6）。
//
// 接在 RRF 融合之后、截断到 size 之前：候选面是融合出来的**全部**候选，
// 而不是已经截成 size 条的那一页 —— 否则一件缺货商品只会在第一页里往下挪几位，
// 而不是让出位置给第一页之外的有货商品。§11 阶段 4 的验收标准
// 「缺货商品不再出现在首屏」要的是后者。
//
// ===========================================================================
// 一、乘性衰减，不是加性扣分
// ===========================================================================
//
//	final = rrf × business      business = Π 各因子
//
// §6 的原话：硬约束应当接近归零而非扣几分 —— 加性扣分在高相关度下仍可能排到前面。
// 在 RRF 上这句话是可以算出来的：RRF 分的值域极窄，两路都第一名是 2/61 ≈ 0.0328，
// 单路第 300 名（size=100 时每一路最多召回 300 条）是 1/360 ≈ 0.0028。
// 缺货因子 0.05 把最好的那一条压到 0.0016，**低于任何一条有货候选的最低分** ——
// 也就是说缺货的一律排在有货的后面，而缺货商品之间仍保持相关度的先后。
// 加性扣分做不到这一点：扣多少都得对着 RRF 的绝对值去调，而那个值随召回条数变。
//
// ===========================================================================
// 二、本轮只实现了一个因子（库存），其余的为什么没有
// ===========================================================================
//
// §6 的表里有四个因子外加销量，本轮只有 w_stock。不是漏做，每一个都有具体的原因，
// 而且「缺席」在 explain 与检索日志里都看得出来（scores.business 就是这里的乘积，
// 不含任何没算的因子；search_logs.stages 记的是 business，不是一个假装齐全的名字）：
//
//	· w_promo（活动有效性）：数据模型里**没有活动表**。券（00026）不是活动 ——
//	  券挂在买家身上，不挂在商品上，「这件商品有没有进行中的活动」无从回答。
//	· w_quality（口碑）：没有评价数据，好评率无从计算。
//	· 预售（w_stock = 0.85 那一档）：没有预售这个商品状态。
//	· w_freshness（时效 ±5%）与 w_sales（销量对数，0 销量 1.0、999 销量 1.3）：
//	  数据都在（published_at / sales_count），**刻意不做**。§6 的系数是对着
//	  cross-encoder 精排分定的，精排分的动态范围宽；而这里乘的是 RRF 分，
//	  同一路里第 1 名与第 2 名只差 1.6%（61/62），第 1 名与第 20 名只差 31%（80/61）。
//	  一个 ±5% 的时效因子会让相关度相邻的五六名整体按上架时间重排，
//	  一个 999 销量的 1.3 倍会把同一路第 20 名推到 0 销量的第 1 名前面 ——
//	  那已经不是「叠加一点业务偏好」，是把排序主体换成了销量，
//	  恰好是 §6 用对数缩放想避免的「头部商品垄断」。
//	  要不要上、系数多大，得等精排落地（分数有了动态范围）并且有 §9.1 的离线评测集
//	  之后再定：「没有评估体系的检索优化等于盲调」（§9）。
//	· 毛利：§6「关于毛利权重的诚实建议」—— 默认关闭。这里连开关都没有做：
//	  一个默认关闭、没有任何调用方会打开的开关是一段没有执行者的代码。
//	  真要做，照 §6 的建议只作同分 tiebreaker、上限 ±3%、记入 search_logs。
//
// ===========================================================================
// 三、下架 / 删除 / 区域不可售不在这里
// ===========================================================================
//
// §6 把它们归为硬过滤（直接剔除，不参与打分），它们在两条召回查询的 WHERE 里
// （db/queries/search.sql：status = 1、deleted_at IS NULL、大区与门店两条排除），
// 根本到不了这一层。在这里再判一次只会让人以为那几条 WHERE 可以删。

// StockFactorInStock / StockFactorOutOfStock 是 §6 表里 w_stock 的两档。
//
// 0.05 而不是 0：缺货商品仍然出现在结果里（排在所有有货商品之后），
// 只是不占首屏。要彻底不看缺货的，filters.in_stock_only = true 是那个开关 ——
// 契约把它的默认值定成 false，正是为了让「降权」这句话被执行，而不是被过滤抢先。
const (
	StockFactorInStock    = 1.0
	StockFactorOutOfStock = 0.05
)

// BusinessSignals 是一条候选身上业务重排要读的信号。
type BusinessSignals struct {
	InStock bool
}

// BusinessFactor 算一条候选的业务乘子（本轮 = w_stock，见文件头第二节）。
func BusinessFactor(s BusinessSignals) float64 {
	if !s.InStock {
		return StockFactorOutOfStock
	}
	return StockFactorInStock
}

// Ranked 是业务重排之后的一条。
type Ranked struct {
	Fused

	// Business 是业务乘子，Final = Fused.Score × Business。
	Business float64
	Final    float64
}

// RerankByBusiness 对融合结果做业务重排，返回按 Final 降序的新切片（不改入参）。
//
// signals 按 id 给出每条候选的信号；缺了某条时按有货处理并**不**报错 ——
// 那种情况只会是调用方的 bug（融合的 id 全部来自召回行），而为它让整次检索失败
// 不值得；但也不静默吞掉：返回的第二个值是缺信号的条数，调用方据此记日志。
//
// 排序键：Final 降序 → Fused.Score 降序 → id 升序。第二键让乘子相同的候选
// 保持融合时的先后（重排只该在业务上有差别的地方改变顺序），第三键理由同
// FuseRRF：sort.Slice 不稳定，同分时不给次级键，同一个请求两次返回的顺序会不同。
func RerankByBusiness(fused []Fused, signals map[int64]BusinessSignals) ([]Ranked, int) {
	missing := 0
	out := make([]Ranked, 0, len(fused))
	for _, f := range fused {
		sig, ok := signals[f.ID]
		if !ok {
			missing++
			sig = BusinessSignals{InStock: true}
		}
		b := BusinessFactor(sig)
		out = append(out, Ranked{Fused: f, Business: b, Final: f.Score * b})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Final != out[j].Final {
			return out[i].Final > out[j].Final
		}
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].ID < out[j].ID
	})
	return out, missing
}
