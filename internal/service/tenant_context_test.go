package service_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"strings"
	"testing"
)

// SAGA 分支的租户上下文**只许**由 dtm.TenantContextFromGID 产生。
//
// 这条是 M2 计划里写死的硬约束：
//
//	WithSagaBranch 不解析 gid，租户从 ctx 取（与 WithTenant 同规矩）。于是
//	「ctx 里的租户」与「gid 里的租户」一致性，由唯一的产生者
//	dtm.TenantContextFromGID 按构造保证，repository 那一层**没有复核**。
//
// 为什么 repository 不复核：让它 import internal/dtm 会把 cgo 拖进数据访问层，
// 还要让它认得 gid 的文法（多一份会漂移的真相）。这是刻意不做的。
//
// 代价就是这条约束没有编译期的落点 —— 一个 tenant.NewContext(ctx, 42) 能编译、
// 能跑，而且**屏障与业务都会老老实实跑在那个错租户下**，一声不响。
// 所以它需要一条机械检查。
//
// 行为侧的证明在 internal/handler 的 TestBranchTakesItsTenantFromTheGIDOnly：
// 拿别家的 gid 调分支，业务跑不起来、库存一分没动。两条一起才完整：
// 那条证明「现在这条路是对的」，这条证明「没有第二条路」。
func TestServiceNeverBuildsATenantContextByHand(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi fs.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	files := 0
	handmade := 0
	fromGID := 0

	for _, pkg := range pkgs {
		for name, f := range pkg.Files {
			files++
			ast.Inspect(f, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				ident, ok := sel.X.(*ast.Ident)
				if !ok {
					return true
				}
				switch {
				case ident.Name == "tenant" && sel.Sel.Name == "NewContext":
					handmade++
					t.Errorf("%s:%d 手搓了一个租户上下文（tenant.NewContext）—— "+
						"SAGA 分支的租户只许由 dtm.TenantContextFromGID 从 gid 解出来。"+
						"repository 那一层不会复核，所以一个错租户会被原样执行："+
						"屏障与业务都跑在别家店下，而且不报错",
						name, fset.Position(call.Pos()).Line)
				case ident.Name == "dtm" && sel.Sel.Name == "TenantContextFromGID":
					fromGID++
				}
				return true
			})
		}
	}

	// 阳性对照。少了它，这条测试在「包被改名了」「解析失败返回空集」
	// 「分支代码整个搬走了」这三种情况下都会恒绿。
	if files == 0 {
		t.Fatal("一个源文件都没解析到 —— 这条测试没在检查任何东西")
	}
	if fromGID == 0 {
		t.Fatal("本包里一次 dtm.TenantContextFromGID 都没有 —— " +
			"要么分支代码搬走了（那这条测试该跟着搬），要么租户改成从别处来了")
	}
	t.Logf("扫了 %d 个源文件：手搓租户上下文 %d 处，经 gid 解析 %d 处",
		files, handmade, fromGID)
}
