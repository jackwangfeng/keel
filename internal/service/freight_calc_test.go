package service

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/keel/keel/internal/repository"
)

// freightContext.quote 是全仓库唯一一份运费计算（freight_calc.go 文件头）。这里不碰
// 数据库，把每一条口径逐分核对：首件 / 续件的进位、按重量、按地区挑规则、满额 / 满件
// 包邮、不配送、模板怎么挑、多模板求和。
//
// 每条用例断言一个具体的分数：一个把续件进位写成向下取整的实现在「2300 克」那一条上
// 会红，一个把满额比较写成 > 的实现在「刚好 99 元」那一条上会红。

const (
	fStore      int64 = 11
	fOtherStore int64 = 12
)

// 全国首件 8 元续件 2 元、满 99 包邮；新疆西藏首件 20 元续件 10 元、不包邮；港澳台不配送。
func nationwide(id int64) repository.FreightTemplate {
	return repository.FreightTemplate{
		ID: id, Name: "默认运费", ChargeMode: freightChargeByPiece, IsDefault: true,
		UndeliverableRegionCodes: []string{"710000", "810000", "820000"},
		Rules: []repository.FreightRule{
			{RegionCodes: []string{"650000", "540000"}, FirstUnit: 1, FirstFeeCents: 2000,
				AdditionalUnit: 1, AdditionalFeeCents: 1000},
			{RegionCodes: []string{}, FirstUnit: 1, FirstFeeCents: 800, AdditionalUnit: 1,
				AdditionalFeeCents: 200, FreeThresholdCents: 9900},
		},
	}
}

func byWeight(id int64) repository.FreightTemplate {
	return repository.FreightTemplate{
		ID: id, Name: "大件按重量", ChargeMode: freightChargeByWeight,
		Rules: []repository.FreightRule{
			{RegionCodes: []string{}, FirstUnit: 1000, FirstFeeCents: 1000,
				AdditionalUnit: 500, AdditionalFeeCents: 300},
		},
	}
}

func fctx(info map[int64]repository.SKUFreightInfo, tpls ...repository.FreightTemplate) freightContext {
	m := map[int64]repository.FreightTemplate{}
	for _, t := range tpls {
		m[t.ID] = t
	}
	return freightContext{store: repository.StoreScope{StoreID: fStore, RegionID: 21}, info: info, templates: m}
}

func sku(id int64, gram int32, tpl *int64) repository.SKUFreightInfo {
	return repository.SKUFreightInfo{SKUID: id, WeightGram: gram, TemplateID: tpl}
}

func mustQuote(t *testing.T, fc freightContext, province string, items []FreightItem, goods int64) FreightBreakdown {
	t.Helper()
	b, bad, err := fc.quote(province, items, goods)
	if err != nil {
		t.Fatalf("quote: %v", err)
	}
	if len(bad) > 0 {
		t.Fatalf("期望全部送得到，实得送不到 %+v", bad)
	}
	var sum int64
	for _, g := range b.Groups {
		sum += g.FeeCents
	}
	if sum != b.FreightCents {
		t.Fatalf("各组运费之和 %d ≠ freight_cents %d", sum, b.FreightCents)
	}
	return b
}

func TestFreightByPieceFirstPlusAdditional(t *testing.T) {
	fc := fctx(map[int64]repository.SKUFreightInfo{1: sku(1, 0, nil), 2: sku(2, 0, nil)}, nationwide(100))
	// 3 件：首件 800 + 续 2 件 × 200 = 1200。
	b := mustQuote(t, fc, "440000", []FreightItem{{1, 2}, {2, 1}}, 5000)
	if b.FreightCents != 1200 {
		t.Fatalf("广东 3 件应收 1200 分，实得 %d", b.FreightCents)
	}
	g := b.Groups[0]
	if g.Units != 3 || g.TemplateID == nil || *g.TemplateID != 100 || g.Rule == nil || len(g.Rule.RegionCodes) != 0 {
		t.Fatalf("分组明细不对：%+v", g)
	}
	if b.ProvinceCode != "440000" {
		t.Fatalf("province_code 应回显 440000，实得 %q", b.ProvinceCode)
	}
	// 1 件只收首件。
	if b := mustQuote(t, fc, "440000", []FreightItem{{1, 1}}, 5000); b.FreightCents != 800 {
		t.Fatalf("1 件应只收首件 800，实得 %d", b.FreightCents)
	}
}

func TestFreightRegionalRuleBeatsDefault(t *testing.T) {
	fc := fctx(map[int64]repository.SKUFreightInfo{1: sku(1, 0, nil)}, nationwide(100))
	// 新疆 2 件：首件 2000 + 续 1 件 1000 = 3000；而且新疆那条没有满额包邮，
	// 金额再高也照收。
	b := mustQuote(t, fc, "650000", []FreightItem{{1, 2}}, 1_000_000)
	if b.FreightCents != 3000 || b.Groups[0].FreeReason != "" {
		t.Fatalf("新疆 2 件应收 3000 分且不包邮，实得 %d（free_reason=%q）", b.FreightCents, b.Groups[0].FreeReason)
	}
	if !slices.Contains(b.Groups[0].Rule.RegionCodes, "650000") {
		t.Fatalf("命中的应是偏远地区那条规则，实得 %+v", b.Groups[0].Rule)
	}
}

func TestFreightFreeThresholdIsInclusiveOnPayableGoods(t *testing.T) {
	fc := fctx(map[int64]repository.SKUFreightInfo{1: sku(1, 0, nil)}, nationwide(100))
	// 满 99 包邮：刚好 9900 分包邮，9899 分照收。比的是调用方递进来的「优惠后应付」。
	b := mustQuote(t, fc, "110000", []FreightItem{{1, 1}}, 9900)
	if b.FreightCents != 0 || b.Groups[0].FreeReason != freightFreeThreshold {
		t.Fatalf("刚好 9900 分应满额包邮，实得 %d（%q）", b.FreightCents, b.Groups[0].FreeReason)
	}
	b = mustQuote(t, fc, "110000", []FreightItem{{1, 1}}, 9899)
	if b.FreightCents != 800 || b.Groups[0].FreeReason != "" {
		t.Fatalf("9899 分不满 99，应收首件 800，实得 %d（%q）", b.FreightCents, b.Groups[0].FreeReason)
	}
}

func TestFreightFreeQuantityCountsWholeOrder(t *testing.T) {
	tpl := nationwide(100)
	tpl.Rules[1].FreeThresholdCents = 0
	tpl.Rules[1].FreeQuantity = 3
	fc := fctx(map[int64]repository.SKUFreightInfo{1: sku(1, 0, nil), 2: sku(2, 0, nil)}, tpl)
	b := mustQuote(t, fc, "320000", []FreightItem{{1, 2}, {2, 1}}, 100)
	if b.FreightCents != 0 || b.Groups[0].FreeReason != freightFreeQuantity {
		t.Fatalf("整单 3 件应满件包邮，实得 %d（%q）", b.FreightCents, b.Groups[0].FreeReason)
	}
	b = mustQuote(t, fc, "320000", []FreightItem{{1, 2}}, 100)
	if b.FreightCents != 1000 {
		t.Fatalf("2 件不满 3 件，应收 800 + 200 = 1000，实得 %d", b.FreightCents)
	}
}

func TestFreightByWeightRoundsUpPartialSteps(t *testing.T) {
	w := int64(200)
	fc := fctx(map[int64]repository.SKUFreightInfo{
		1: sku(1, 800, &w), 2: sku(2, 700, &w), 3: sku(3, 0, &w),
	}, byWeight(200), nationwide(100))
	// 800×2 + 700×1 = 2300 克：首重 1000 + ⌈1300 / 500⌉ = 3 次续重 → 1000 + 900 = 1900。
	b := mustQuote(t, fc, "440000", []FreightItem{{1, 2}, {2, 1}}, 100)
	if b.FreightCents != 1900 || b.Groups[0].Units != 2300 {
		t.Fatalf("2300 克应收 1900 分，实得 %d（units=%d）", b.FreightCents, b.Groups[0].Units)
	}
	// 恰好 1500 克：超出 500 克 = 1 次续重，不多收。
	fc.info[1] = sku(1, 750, &w)
	if b := mustQuote(t, fc, "440000", []FreightItem{{1, 2}}, 100); b.FreightCents != 1300 {
		t.Fatalf("1500 克应收 1000 + 300 = 1300，实得 %d", b.FreightCents)
	}
	// 没填重量（0 克）：一组只要有货，首重费照收。
	if b := mustQuote(t, fc, "440000", []FreightItem{{3, 5}}, 100); b.FreightCents != 1000 {
		t.Fatalf("0 克的一组应只收首重 1000，实得 %d", b.FreightCents)
	}
}

func TestFreightUndeliverableRegionFlagsEveryLineOfThatTemplate(t *testing.T) {
	w := int64(200)
	fc := fctx(map[int64]repository.SKUFreightInfo{1: sku(1, 0, nil), 2: sku(2, 0, nil), 3: sku(3, 500, &w)},
		nationwide(100), byWeight(200))
	b, bad, err := fc.quote("810000", []FreightItem{{1, 1}, {2, 1}, {3, 1}}, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(bad) != 2 || bad[0].SKUID != 1 || bad[1].SKUID != 2 {
		t.Fatalf("香港不在「默认运费」的配送范围，sku 1、2 应送不到，实得 %+v", bad)
	}
	if bad[0].ReasonCode != undeliverableRegionExcluded || !strings.Contains(bad[0].Reason, "香港特别行政区") {
		t.Fatalf("原因应是 region_excluded 且说出省名，实得 %+v", bad[0])
	}
	// 另一个模板（没设不配送）照常计费，明细只覆盖送得到的那一组。
	if len(b.Groups) != 1 || b.Groups[0].SKUIDs[0] != 3 || b.FreightCents != 1000 {
		t.Fatalf("明细应只剩按重量那一组（1000 分），实得 %+v", b)
	}
}

func TestFreightUnknownProvince(t *testing.T) {
	fc := fctx(map[int64]repository.SKUFreightInfo{1: sku(1, 0, nil)}, nationwide(100))
	// 模板设了不配送地区，而地址归不到省：判不了送不送得到 —— 报出来，不假装送得到。
	_, bad, err := fc.quote("", []FreightItem{{1, 1}}, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(bad) != 1 || bad[0].ReasonCode != undeliverableProvinceUnknown {
		t.Fatalf("应报 province_unknown，实得 %+v", bad)
	}
	// 模板没设不配送地区：归不到省就按默认规则算。
	tpl := nationwide(100)
	tpl.UndeliverableRegionCodes = []string{}
	fc = fctx(map[int64]repository.SKUFreightInfo{1: sku(1, 0, nil)}, tpl)
	b := mustQuote(t, fc, "", []FreightItem{{1, 1}}, 100)
	if b.FreightCents != 800 || b.ProvinceCode != "" {
		t.Fatalf("归不到省应按默认规则收 800，实得 %d（province=%q）", b.FreightCents, b.ProvinceCode)
	}
}

func TestFreightTemplatePrecedence(t *testing.T) {
	product := int64(300)
	storeTpl := repository.FreightTemplate{ID: 301, Name: "本店", StoreID: &[]int64{fStore}[0],
		ChargeMode: freightChargeByPiece,
		Rules:      []repository.FreightRule{{RegionCodes: []string{}, FirstUnit: 1, FirstFeeCents: 500, AdditionalUnit: 1}}}
	otherStoreTpl := repository.FreightTemplate{ID: 302, Name: "别店", StoreID: &[]int64{fOtherStore}[0],
		ChargeMode: freightChargeByPiece,
		Rules:      []repository.FreightRule{{RegionCodes: []string{}, FirstUnit: 1, FirstFeeCents: 9999, AdditionalUnit: 1}}}
	productTpl := repository.FreightTemplate{ID: product, Name: "商品专用", ChargeMode: freightChargeByPiece,
		Rules: []repository.FreightRule{{RegionCodes: []string{}, FirstUnit: 1, FirstFeeCents: 100, AdditionalUnit: 1}}}
	def := nationwide(100)
	info := map[int64]repository.SKUFreightInfo{1: sku(1, 0, &product), 2: sku(2, 0, nil)}

	// 商品单独挂的 → 门店模板（别家门店的模板不算）；两组求和 100 + 500。
	fc := fctx(info, productTpl, storeTpl, otherStoreTpl, def)
	b := mustQuote(t, fc, "110000", []FreightItem{{1, 1}, {2, 1}}, 100)
	if b.FreightCents != 600 || len(b.Groups) != 2 || *b.Groups[0].TemplateID != product || *b.Groups[1].TemplateID != 301 {
		t.Fatalf("应是商品模板 100 + 门店模板 500 = 600，实得 %d %+v", b.FreightCents, b.Groups)
	}
	// 没有门店模板 → 全店默认。
	fc = fctx(info, productTpl, otherStoreTpl, def)
	b = mustQuote(t, fc, "110000", []FreightItem{{2, 1}}, 100)
	if b.FreightCents != 800 || *b.Groups[0].TemplateID != 100 {
		t.Fatalf("没有门店模板时应落到全店默认（800），实得 %d %+v", b.FreightCents, b.Groups)
	}
	// 什么模板都没有 → 这一行不计运费，明细里说清楚是 no_template。
	fc = fctx(info, otherStoreTpl)
	b = mustQuote(t, fc, "110000", []FreightItem{{2, 3}}, 100)
	if b.FreightCents != 0 || b.Groups[0].FreeReason != freightFreeNoTemplate || b.Groups[0].TemplateID != nil {
		t.Fatalf("没有任何模板应是 0 且 free_reason=no_template，实得 %+v", b)
	}
}

func TestFreightTemplateWithoutDefaultRuleIsAnError(t *testing.T) {
	tpl := nationwide(100)
	tpl.Rules = tpl.Rules[:1] // 只剩偏远地区那条
	fc := fctx(map[int64]repository.SKUFreightInfo{1: sku(1, 0, nil)}, tpl)
	if _, _, err := fc.quote("110000", []FreightItem{{1, 1}}, 100); err == nil {
		t.Fatal("没有默认规则的模板是写入校验被绕过，应报错而不是算成 0")
	}
}

func TestProvinceOf(t *testing.T) {
	cases := []struct {
		code *string
		text string
		want string
	}{
		{str("440305"), "广东省", "440000"},
		{str("440305"), "", "440000"},
		{nil, "内蒙古", "150000"},
		{nil, "内蒙古自治区", "150000"},
		{nil, "广西壮族自治区", "450000"},
		{str("990000"), "新疆维吾尔自治区", "650000"}, // 区划码写错：接着看文字
		{nil, "火星", ""},
		{nil, "", ""},
	}
	for _, tc := range cases {
		if got := provinceOf(tc.code, tc.text); got != tc.want {
			t.Errorf("provinceOf(%v, %q) = %q，期望 %q", tc.code, tc.text, got, tc.want)
		}
	}
}

// TestProvinceListMatchesMigration：Go 侧的 34 个省级区划码与 00041 两条 CHECK 里的
// 数组必须是同一份（freight_region.go 文件头）。任何一边多一个少一个都红。
func TestProvinceListMatchesMigration(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "db", "migrations", "00041_freight_templates.sql"))
	if err != nil {
		t.Fatal(err)
	}
	arrays := regexp.MustCompile(`(?s)<@ ARRAY\[(.*?)\]::TEXT\[\]`).FindAllStringSubmatch(string(raw), -1)
	if len(arrays) != 2 {
		t.Fatalf("00041 里应有两条区划码 CHECK，找到 %d 条", len(arrays))
	}
	want := make([]string, 0, len(provinces))
	for _, p := range provinces {
		want = append(want, p.Code)
	}
	slices.Sort(want)
	for i, a := range arrays {
		got := regexp.MustCompile(`'(\d{6})'`).FindAllStringSubmatch(a[1], -1)
		codes := make([]string, 0, len(got))
		for _, g := range got {
			codes = append(codes, g[1])
		}
		slices.Sort(codes)
		if !slices.Equal(codes, want) {
			t.Fatalf("第 %d 条 CHECK 的区划码与 freight_region.go 不一致：\n迁移 %v\n代码 %v", i+1, codes, want)
		}
	}
	if len(want) != 34 {
		t.Fatalf("省级行政区应是 34 个，实得 %d", len(want))
	}
}
