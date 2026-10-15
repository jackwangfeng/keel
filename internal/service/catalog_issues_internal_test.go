package service

import (
	"strings"
	"testing"

	"github.com/keel/keel/internal/inventory"
	"github.com/keel/keel/internal/repository"
	"github.com/keel/keel/internal/understanding"
)

func codes(is []CatalogIssue) string {
	var c []string
	for _, i := range is {
		c = append(c, i.Code)
	}
	return strings.Join(c, ",")
}

func TestCatalogProductTextIssues(t *testing.T) {
	cases := []struct {
		name string
		p    repository.CatalogProduct
		v    []understanding.Violation
		want string
	}{
		{"干净的商品没有问题",
			repository.CatalogProduct{Title: "赣南脐橙", Subtitle: "当季鲜果", CategoryName: "水果", ImageCount: 1}, nil, ""},
		{"营销词与符号、没副标题、默认分类、没图",
			repository.CatalogProduct{Title: "【特价】★赣南脐橙 包邮", CategoryName: "默认分类"}, nil,
			"title_noise,no_subtitle,uncategorized,no_image"},
		{"标题按字数判长，不按字节",
			repository.CatalogProduct{Title: strings.Repeat("橙", 31), Subtitle: "x", CategoryName: "水果", ImageCount: 1}, nil,
			"title_too_long"},
		{"三十个字刚好不算长",
			repository.CatalogProduct{Title: strings.Repeat("橙", 30), Subtitle: "x", CategoryName: "水果", ImageCount: 1}, nil, ""},
		{"违禁词",
			repository.CatalogProduct{Title: "脐橙", Subtitle: "x", CategoryName: "水果", ImageCount: 1},
			[]understanding.Violation{{Field: understanding.FieldTitle, Term: "最好"}}, "forbidden_words"},
		{"类目被删了也算没分类",
			repository.CatalogProduct{Title: "脐橙", Subtitle: "x", CategoryName: "", ImageCount: 1}, nil, "uncategorized"},
		{"商品名里的间隔号与斜杠不是噪音",
			repository.CatalogProduct{Title: "可口可乐·零度 330ml/罐", Subtitle: "x", CategoryName: "饮料", ImageCount: 1}, nil, ""},
	}
	for _, c := range cases {
		if got := codes(productTextIssues(c.p, c.v)); got != c.want {
			t.Errorf("%s：得到 %q，期望 %q", c.name, got, c.want)
		}
	}
}

func TestCatalogTitleKeyIgnoresSpecsAndNoise(t *testing.T) {
	same := [][2]string{
		{"赣南脐橙 2.5kg装", "【特价】赣南脐橙 5kg"},
		{"矿泉水 550ml×24 瓶", "矿泉水 550ML x 12瓶"},
		{"土鸡蛋 30 枚", "土鸡蛋"},
	}
	for _, p := range same {
		if a, b := catalogTitleKey(p[0]), catalogTitleKey(p[1]); a != b {
			t.Errorf("%q 与 %q 应当同名：%q ≠ %q", p[0], p[1], a, b)
		}
	}
	if catalogTitleKey("赣南脐橙") == catalogTitleKey("红富士苹果") {
		t.Error("不同的商品被归成了同名")
	}
}

func TestCatalogCrossProductIssues(t *testing.T) {
	products := []repository.CatalogProduct{
		{ID: 1, Title: "赣南脐橙 2.5kg"}, {ID: 2, Title: "赣南脐橙 5kg"}, {ID: 3, Title: "苹果"},
		{ID: 4, Title: "香蕉"}, {ID: 5, Title: "梨"}, {ID: 6, Title: "桃"},
	}
	// 类目 9：六个 SKU，中位数 1000；#13 多打了一个 0 → 10000（10 倍），#14 是 150（0.15 倍）
	prices := []repository.CatalogSKUPrice{
		{SKUID: 11, ProductID: 1, PriceCents: 1000, CategoryID: 9},
		{SKUID: 12, ProductID: 2, PriceCents: 1200, CategoryID: 9},
		{SKUID: 13, ProductID: 3, PriceCents: 10000, CategoryID: 9},
		{SKUID: 14, ProductID: 4, PriceCents: 150, CategoryID: 9},
		{SKUID: 15, ProductID: 5, PriceCents: 900, CategoryID: 9},
		{SKUID: 16, ProductID: 6, PriceCents: 1100, CategoryID: 9},
	}
	totals := map[int64]inventory.Total{11: {Available: 3}, 12: {Available: 3}, 13: {Available: 3}, 14: {Available: 3}, 15: {Available: 3}}
	items := make([]CatalogIssueItem, len(products))
	for i, p := range products {
		items[i] = CatalogIssueItem{ProductID: p.ID}
	}
	addCrossProductIssues(items, products, prices, totals)
	want := map[int64]string{1: "duplicate_title", 2: "duplicate_title", 3: "price_outlier", 4: "price_outlier", 5: "", 6: "zero_stock"}
	for _, it := range items {
		if got := codes(it.Issues); got != want[it.ProductID] {
			t.Errorf("商品 #%d：得到 %q，期望 %q（%v）", it.ProductID, got, want[it.ProductID], it.Issues)
		}
	}
}

func TestCatalogPriceNeedsEnoughSamples(t *testing.T) {
	products := []repository.CatalogProduct{{ID: 1, Title: "a"}, {ID: 2, Title: "b"}}
	prices := []repository.CatalogSKUPrice{
		{SKUID: 1, ProductID: 1, PriceCents: 100, CategoryID: 1},
		{SKUID: 2, ProductID: 2, PriceCents: 100000, CategoryID: 1},
	}
	items := []CatalogIssueItem{{ProductID: 1}, {ProductID: 2}}
	addCrossProductIssues(items, products, prices, map[int64]inventory.Total{1: {Available: 1}, 2: {Available: 1}})
	for _, it := range items {
		if len(it.Issues) != 0 {
			t.Errorf("同类目只有 2 个 SKU，不该判价格异常：%v", it.Issues)
		}
	}
}
