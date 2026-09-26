package handler_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/keel/keel/internal/api"
)

// 买家侧类目：GET /categories 的树形，以及 GET /products?category_id= 的子树筛选。
//
// 两条接口放在一条测试里，因为买家端的类目浏览就是这两跳连着走：先拿树画导航，
// 再拿某个节点的 id 去筛商品。树里出现了一个筛不出东西的节点，或者筛出来的东西
// 在树里找不到入口，单测哪一边都是绿的。

type categoryShop struct {
	sh adminShop
	// 服装 > 女装 > 连衣裙；食品 > 零食（食品被停用）。
	clothes, women, dress, food, snack int64
	// 挂在连衣裙、服装（根）、零食下的三件在架商品。
	pDress, pClothes, pSnack int64
}

func newCategoryShop(t *testing.T) categoryShop {
	t.Helper()
	cs := categoryShop{sh: newAdminShop(t)}
	cs.clothes = createCategory(t, cs.sh, "服装")
	cs.women = createChildCategory(t, cs.sh, cs.clothes, "女装")
	cs.dress = createChildCategory(t, cs.sh, cs.women, "连衣裙")
	cs.food = createCategory(t, cs.sh, "食品")
	cs.snack = createChildCategory(t, cs.sh, cs.food, "零食")

	cs.pDress, _ = createPublishedSKU(t, cs.sh, cs.dress, "雪纺连衣裙", 23900)
	cs.pClothes, _ = createPublishedSKU(t, cs.sh, cs.clothes, "基础款T恤", 5900)
	cs.pSnack, _ = createPublishedSKU(t, cs.sh, cs.snack, "坚果礼盒", 12800)

	// 停用「食品」。它的子类目「零食」本身是启用的 —— 这正是要测的那个形状。
	wantStatus(t, patchAs(t, cs.sh.Host, fmt.Sprintf("/api/v1/admin/categories/%d", cs.food),
		`{"status":0}`, cs.sh.Token), http.StatusOK, "停用食品")
	return cs
}

func createChildCategory(t *testing.T, sh adminShop, parent int64, name string) int64 {
	t.Helper()
	var cat api.AdminCategory
	decodeInto(t, postIdem(t, sh.Host, "/api/v1/admin/categories",
		fmt.Sprintf(`{"name":%q,"parent_id":%d}`, name+" "+sh.Suffix, parent), sh.Token),
		http.StatusCreated, "建子类目 "+name, &cat)
	return cat.Id
}

func buyerCategories(t *testing.T, host string) []api.Category {
	t.Helper()
	w := do(t, host, "/api/v1/categories")
	if w.Code != http.StatusOK {
		t.Fatalf("GET /categories 回了 %d：%s", w.Code, w.Body.String())
	}
	var tree []api.Category
	if err := json.Unmarshal(w.Body.Bytes(), &tree); err != nil {
		t.Fatalf("GET /categories 的响应解不开：%v\n%s", err, w.Body.String())
	}
	return tree
}

func findCategory(tree []api.Category, id int64) *api.Category {
	for i := range tree {
		if tree[i].Id == id {
			return &tree[i]
		}
		if tree[i].Children != nil {
			if c := findCategory(*tree[i].Children, id); c != nil {
				return c
			}
		}
	}
	return nil
}

func productIDs(t *testing.T, host, query string) (ids map[int64]bool, total int64) {
	t.Helper()
	w := do(t, host, "/api/v1/products"+query)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /products%s 回了 %d：%s", query, w.Code, w.Body.String())
	}
	// 列表响应在契约里是内联 schema（PageMeta + items），Go 这边没有具名类型；
	// 用 product_test.go 里那个 listResp，和别的列表测试解的是同一个形状。
	var page listResp
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatalf("GET /products%s 的响应解不开：%v", query, err)
	}
	ids = map[int64]bool{}
	for _, it := range page.Items {
		ids[it.ID] = true
	}
	return ids, page.Total
}

func TestCategoryTreeShowsEnabledCategoriesNested(t *testing.T) {
	cs := newCategoryShop(t)
	tree := buyerCategories(t, cs.sh.Host)

	clothes := findCategory(tree, cs.clothes)
	if clothes == nil {
		t.Fatalf("树里没有「服装」（id=%d）", cs.clothes)
	}
	if clothes.Level != 1 {
		t.Errorf("服装的 level 是 %d，期望 1", clothes.Level)
	}
	women := findCategory(*clothes.Children, cs.women)
	if women == nil {
		t.Fatal("「女装」没挂在「服装」下面")
	}
	dress := findCategory(*women.Children, cs.dress)
	if dress == nil {
		t.Fatal("「连衣裙」没挂在「女装」下面")
	}
	if dress.Level != 3 {
		t.Errorf("连衣裙的 level 是 %d，期望 3", dress.Level)
	}
	// 叶子节点也给 children: []（handler 的约定），客户端不必判 null。
	if dress.Children == nil {
		t.Error("叶子节点的 children 是 null，约定是 []")
	}
}

// 停用「食品」之后，它**和它的整棵子树**都不在导航里 —— 包括本身是启用的「零食」。
func TestCategoryTreeHidesTheSubtreeOfADisabledCategory(t *testing.T) {
	cs := newCategoryShop(t)
	tree := buyerCategories(t, cs.sh.Host)

	if findCategory(tree, cs.food) != nil {
		t.Error("停用的「食品」出现在了买家的类目树里 —— 契约：只返回 status = 1 的")
	}
	if findCategory(tree, cs.snack) != nil {
		t.Error("「零食」本身是启用的，但它的父类目「食品」停用了，它不该出现在导航里 —— " +
			"出现了说明孤儿子类目被提到了别处，停用只停了一半")
	}
}

// 按类目筛**含子孙**：筛「服装」要看得到挂在孙子类目「连衣裙」下的商品。
func TestProductsFilterByCategoryIncludesDescendants(t *testing.T) {
	cs := newCategoryShop(t)

	ids, total := productIDs(t, cs.sh.Host, fmt.Sprintf("?category_id=%d", cs.clothes))
	if !ids[cs.pDress] {
		t.Error("筛「服装」时看不到挂在孙子类目「连衣裙」下的商品 —— 筛选没有带上子孙")
	}
	if !ids[cs.pClothes] {
		t.Error("筛「服装」时看不到直接挂在「服装」下的商品")
	}
	if ids[cs.pSnack] {
		t.Error("筛「服装」时出现了「零食」下的商品 —— 筛选没生效，或者子树取错了")
	}
	if total != 2 {
		t.Errorf("筛「服装」的 total 是 %d，期望 2 —— total 的条件与列表不一致时，"+
			"客户端算出来的页数会不对", total)
	}

	// 筛中间那一层，只剩它自己那一支。
	ids, total = productIDs(t, cs.sh.Host, fmt.Sprintf("?category_id=%d", cs.women))
	if !ids[cs.pDress] || ids[cs.pClothes] || total != 1 {
		t.Errorf("筛「女装」应当只有连衣裙那一件，实际 %v（total=%d）", ids, total)
	}
}

// 不存在的类目给**空列表**，不是全部商品，也不是错误。
//
// 「全部商品」那种写法（筛选条件为 NULL 时整个条件被短路掉）在一个过期的类目
// 链接上会让买家看到「这个类目里什么都有」。
func TestProductsFilterByUnknownCategoryIsEmpty(t *testing.T) {
	cs := newCategoryShop(t)

	ids, total := productIDs(t, cs.sh.Host, "?category_id=999999999")
	if len(ids) != 0 || total != 0 {
		t.Errorf("筛一个不存在的类目，期望空列表，实际 %d 件（total=%d）—— "+
			"多半是筛选条件在类目不存在时整个被短路了", len(ids), total)
	}
	// 阳性对照：不带 category_id 时这三件都在。没有这一句，一个「永远返回空」
	// 的实现会让上面那条也是绿的。
	all, _ := productIDs(t, cs.sh.Host, "")
	if !all[cs.pDress] || !all[cs.pClothes] || !all[cs.pSnack] {
		t.Errorf("不筛选时应当三件都在，实际 %v", all)
	}
}

// 类目的启停**只管导航，不管商品能不能被看到**（products.sql 文件头那一段）。
// 停用的「食品」不在树里，但按它的 id 筛，它子树下的在架商品照样出来 ——
// 与「不筛选时这件商品本来就看得见」保持一致。
func TestDisabledCategoryStillFiltersItsProducts(t *testing.T) {
	cs := newCategoryShop(t)

	ids, _ := productIDs(t, cs.sh.Host, fmt.Sprintf("?category_id=%d", cs.food))
	if !ids[cs.pSnack] {
		t.Error("按停用的「食品」筛，它子树下的在架商品不见了 —— 启停应当只影响导航")
	}
}

// 另一家店看不到这家店的类目。这是 RLS 在这条新查询上的执行者：
// db/queries/categories.sql 里一个 merchant_id 都没有，挡住跨店读取的只有策略。
func TestCategoryTreeIsTenantScoped(t *testing.T) {
	cs := newCategoryShop(t)
	other := newAdminShop(t)

	tree := buyerCategories(t, other.Host)
	for _, id := range []int64{cs.clothes, cs.women, cs.dress} {
		if findCategory(tree, id) != nil {
			t.Errorf("另一家店的类目树里出现了本店的类目 %d —— RLS 没挡住", id)
		}
	}
	// 阳性对照：本店自己看得到。
	if findCategory(buyerCategories(t, cs.sh.Host), cs.clothes) == nil {
		t.Error("本店自己的类目树里看不到「服装」，上面那条就证明不了什么")
	}
}
