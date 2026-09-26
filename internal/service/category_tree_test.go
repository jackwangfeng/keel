package service

import (
	"testing"

	"github.com/keel/keel/internal/repository"
)

func node(id int64, parent int64, name string, level int16, sort int32) repository.CategoryNode {
	n := repository.CategoryNode{ID: id, Name: name, Level: level, SortOrder: sort}
	if parent != 0 {
		p := parent
		n.ParentID = &p
	}
	return n
}

// 三层树，而且每一层都有**不止一个**孩子。
//
// 「不止一个」是这条测试的全部意义：buildCategoryTree 用下标路径挂子节点，而不是
// 存 *Category 指针，理由写在那个函数上 —— Children 是值切片，append 搬家之后
// 先拿到的指针指向旧底层数组，后挂上去的孙子节点就丢了。只有一条链的树
// （每层一个孩子）在指针写法下也是绿的，所以这里刻意让「服装」下面挂两个、
// 两个下面各再挂一个，并且先挂完第一个孩子的孙子再挂第二个孩子。
func TestCategoryTreeKeepsEveryNodeAcrossSiblingAppends(t *testing.T) {
	nodes := []repository.CategoryNode{
		// 输入按 level 升序（SQL 的 ORDER BY 保证），同层按 sort_order。
		node(1, 0, "服装", 1, 0),
		node(2, 0, "食品", 1, 1),
		node(10, 1, "女装", 2, 0),
		node(11, 1, "男装", 2, 1),
		node(100, 10, "连衣裙", 3, 0),
		node(101, 11, "衬衫", 3, 0),
		node(102, 10, "半身裙", 3, 1),
	}
	tree := buildCategoryTree(nodes)

	if len(tree) != 2 || tree[0].Name != "服装" || tree[1].Name != "食品" {
		t.Fatalf("顶层应当是 [服装 食品]，实际 %+v", names(tree))
	}
	clothes := tree[0]
	if len(clothes.Children) != 2 || clothes.Children[0].Name != "女装" || clothes.Children[1].Name != "男装" {
		t.Fatalf("服装下应当是 [女装 男装]，实际 %v", names(clothes.Children))
	}
	women := clothes.Children[0]
	if got := names(women.Children); len(got) != 2 || got[0] != "连衣裙" || got[1] != "半身裙" {
		t.Fatalf("女装下应当是 [连衣裙 半身裙]，实际 %v —— 丢了哪个，多半就是"+
			"指针指到了 append 之前的底层数组", got)
	}
	if got := names(clothes.Children[1].Children); len(got) != 1 || got[0] != "衬衫" {
		t.Fatalf("男装下应当是 [衬衫]，实际 %v", got)
	}
	if women.Level != 2 || women.Children[0].Level != 3 {
		t.Fatalf("level 没透传：女装 %d，连衣裙 %d", women.Level, women.Children[0].Level)
	}
}

// 父节点停用了（所以不在查询结果里），它的整棵子树都不显示 —— 而且是**传递**的：
// 孙子节点的父亲在场，但祖父不在，孙子也不该冒出来。
//
// 这条规则不是在代码里写出来的，是「只挂到在场的父节点上」自然推出来的，
// 所以才要一条测试钉住它：哪天有人把「找不到父节点」改成「那就挂到顶层」，
// 停用就只停了一半，而且买家首页会冒出一个莫名其妙的二级类目。
func TestDisabledParentHidesItsWholeSubtree(t *testing.T) {
	nodes := []repository.CategoryNode{
		node(1, 0, "服装", 1, 0),
		// 7 号「数码」被停用了，所以它本身不在输入里 —— 只有它的后代在。
		node(70, 7, "手机壳", 2, 0),
		node(700, 70, "硅胶款", 3, 0),
		node(10, 1, "女装", 2, 0),
	}
	tree := buildCategoryTree(nodes)

	if got := names(tree); len(got) != 1 || got[0] != "服装" {
		t.Fatalf("顶层应当只有 [服装]，实际 %v —— 停用类目的子孙被提到了顶层", got)
	}
	if got := names(tree[0].Children); len(got) != 1 || got[0] != "女装" {
		t.Fatalf("服装下应当是 [女装]，实际 %v", got)
	}
}

// 空输入给空数组，不是 nil：handler 把它序列化成 []，客户端不必判 null。
func TestEmptyCategoryTreeIsAnEmptySliceNotNil(t *testing.T) {
	tree := buildCategoryTree(nil)
	if tree == nil {
		t.Fatal("没有类目时返回了 nil，序列化出来是 null 而不是 []")
	}
	leaf := buildCategoryTree([]repository.CategoryNode{node(1, 0, "服装", 1, 0)})
	if leaf[0].Children == nil {
		t.Fatal("叶子节点的 Children 是 nil，序列化出来是 null 而不是 []")
	}
}

func names(cs []Category) []string {
	out := make([]string, 0, len(cs))
	for _, c := range cs {
		out = append(out, c.Name)
	}
	return out
}
