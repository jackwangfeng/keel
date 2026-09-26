package service

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// ===========================================================================
// 从源头枚举状态迁移，逐一核对「发了通知，或写明了为什么不发」
// ===========================================================================
//
// 通知漏接的形状是安静的：某条状态迁移没调 notifyXxx，订单照样走、测试照样绿，
// 只是买家永远收不到那一条。所以这里不信任何手写的「迁移清单」，从三个源头往回推：
//
//  1. db/queries 里每一条改 orders.status、改 refunds、建 refunds、改库存水位的语句
//     （正则识别，见 stateChangingQuery）—— 这是状态变化真正落库的地方；
//  2. internal/repository 里调这条语句的方法（t.q.<Query>(…) 所在的方法）；
//  3. internal/service 里每一处 tx.<方法>(…) 的调用，按所在的函数归类。
//
// 第 3 步的每一处都必须在 notificationCallSites 里登记：要么点名发哪个 notifyXxx ——
// 而且**所在的函数里真的调了它**；要么写明为什么刻意不发。新加一条状态迁移、新加一个调用点、
// 或者把 notifyXxx 那一行删掉，这条测试都会红。
//
// 另有一张以状态机为源头的表：order_status_transitions / refund_status_transitions 的每一条边
// （从迁移文件里解析）都必须在 stateEdges 里写明由哪条语句走 —— 状态机长出一条新边而没人
// 决定它发不发通知，同样会红。

// stateChangingQuery 判一条 sqlc 语句是不是「状态变化」：改订单履约状态、改退款单（任何列）、
// 建退款单、改库存水位。
var stateChangingQuery = regexp.MustCompile(
	`(?i)\bUPDATE\s+orders\s+(?:\w+\s+)?SET\s+status\b` +
		`|\bUPDATE\s+refunds\s+(?:\w+\s+)?SET\b` +
		`|\bINSERT\s+INTO\s+refunds\s*\(` +
		`|\bUPDATE\s+inventories\b` +
		`|\bINSERT\s+INTO\s+inventories\b`)

var queryNameRE = regexp.MustCompile(`^-- name: (\w+) :\w+`)

// stateQueries 读 db/queries，返回状态变化语句的名字。
func stateQueries(t *testing.T) map[string]bool {
	t.Helper()
	files, err := filepath.Glob(filepath.Join("..", "..", "db", "queries", "*.sql"))
	if err != nil || len(files) == 0 {
		t.Fatalf("读不到 db/queries：%v", err)
	}
	out := map[string]bool{}
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		name := ""
		var body strings.Builder
		flush := func() {
			if name != "" && stateChangingQuery.MatchString(body.String()) {
				out[name] = true
			}
		}
		for _, line := range strings.Split(string(raw), "\n") {
			if m := queryNameRE.FindStringSubmatch(line); m != nil {
				flush()
				name, body = m[1], strings.Builder{}
				continue
			}
			if strings.HasPrefix(strings.TrimSpace(line), "--") {
				continue
			}
			body.WriteString(line)
			body.WriteString(" ")
		}
		flush()
	}
	return out
}

func parseGoDir(t *testing.T, dir string) []*ast.File {
	t.Helper()
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, dir, func(fi os.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	var out []*ast.File
	for _, p := range pkgs {
		for _, f := range p.Files {
			out = append(out, f)
		}
	}
	return out
}

// repoMethodsCalling 把 sqlc 语句名映射到调它的 repository 方法名（t.q.<Query>(…) 所在的方法）。
func repoMethodsCalling(t *testing.T, queries map[string]bool) map[string]string {
	t.Helper()
	out := map[string]string{} // 方法名 → 语句名
	for _, f := range parseGoDir(t, filepath.Join("..", "repository")) {
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				sel, ok := n.(*ast.SelectorExpr)
				if !ok || !queries[sel.Sel.Name] {
					return true
				}
				// t.q.<Query>
				if inner, ok := sel.X.(*ast.SelectorExpr); ok && inner.Sel.Name == "q" {
					out[fn.Name.Name] = sel.Sel.Name
				}
				return true
			})
		}
	}
	return out
}

func funcKey(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return fn.Name.Name
	}
	typ := fn.Recv.List[0].Type
	if star, ok := typ.(*ast.StarExpr); ok {
		typ = star.X
	}
	if id, ok := typ.(*ast.Ident); ok {
		return id.Name + "." + fn.Name.Name
	}
	return fn.Name.Name
}

// callsIdent 判函数体里有没有调某个自由函数。
func callsIdent(fn *ast.FuncDecl, name string) bool {
	found := false
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok {
			if id, ok := call.Fun.(*ast.Ident); ok && id.Name == name {
				found = true
			}
		}
		return !found
	})
	return found
}

func TestEveryStateTransitionNotifiesOrSaysWhyNot(t *testing.T) {
	queries := stateQueries(t)
	if len(queries) < 15 {
		t.Fatalf("只从 db/queries 里识别出 %d 条状态变化语句 —— 正则失效了，这条测试没在检查它该检查的东西：%v",
			len(queries), queries)
	}
	methods := repoMethodsCalling(t, queries)

	// 每条状态语句都要有一个 repository 方法在调 —— 否则第 3 步对它是瞎的。
	covered := map[string]bool{}
	for _, q := range methods {
		covered[q] = true
	}
	for q := range queries {
		if !covered[q] {
			t.Errorf("状态变化语句 %s 没有任何 repository 方法以 t.q.%s(…) 调它 —— "+
				"这条测试看不见它从 service 的哪里被调，请把调用形状改回 t.q.<语句>(…)", q, q)
		}
	}

	seen := map[string]bool{}
	for _, f := range parseGoDir(t, ".") {
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				q, ok := methods[sel.Sel.Name]
				if !ok {
					return true
				}
				if id, ok := sel.X.(*ast.Ident); !ok || id.Name != "tx" {
					return true
				}
				key := funcKey(fn) + "/" + sel.Sel.Name
				seen[key] = true
				p, ok := notificationCallSites[key]
				switch {
				case !ok:
					t.Errorf("%s 调了 tx.%s（落库语句 %s，是一次状态变化），但 notificationCallSites 里没有 %q —— "+
						"请决定这次状态变化发哪条通知（Notify: \"notifyXxx\"，并在这个函数里调它），"+
						"或者写明为什么刻意不发（Silent: \"理由\"）", funcKey(fn), sel.Sel.Name, q, key)
				case p.Notify != "" && p.Silent != "":
					t.Errorf("%s 同时写了 Notify 与 Silent，只能二选一", key)
				case p.Notify == "" && strings.TrimSpace(p.Silent) == "":
					t.Errorf("%s 既没点名发哪条通知，也没写不发的理由", key)
				case p.Notify != "" && !callsIdent(fn, p.Notify):
					t.Errorf("%s 登记为发 %s，但 %s 里没有调 %s —— 状态改了，通知没写",
						key, p.Notify, funcKey(fn), p.Notify)
				}
				return true
			})
		}
	}
	if len(seen) < 15 {
		t.Fatalf("只在 service 里找到 %d 处状态变化调用 —— 这条测试没在检查它该检查的东西", len(seen))
	}
	var stale []string
	for key := range notificationCallSites {
		if !seen[key] {
			stale = append(stale, key)
		}
	}
	sort.Strings(stale)
	for _, key := range stale {
		t.Errorf("notificationCallSites 里登记了 %s，但 service 里已经没有这处调用 —— 请删掉这一行", key)
	}

	// 每个点名的 notifyXxx 都真的存在（callsIdent 只比名字，一个拼错的名字会恒不命中而报错，
	// 但一个被删掉定义、却还被调用的名字编译不过 —— 两头都有人管）。
	t.Logf("状态变化语句 %d 条，service 里的调用点 %d 处", len(queries), len(seen))
}

var transitionInsertRE = regexp.MustCompile(`(?s)INSERT INTO (order|refund)_status_transitions[^;]*?VALUES(.*?);`)
var edgeRE = regexp.MustCompile(`\((\d+)\s*,\s*(\d+)\)`)

// 状态机的每一条边都在 stateEdges 里写明由哪条语句走，而那条语句在上面的登记里。
func TestEveryStateMachineEdgeIsAccountedFor(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "..", "db", "migrations", "*.sql"))
	if err != nil {
		t.Fatal(err)
	}
	fromMigrations := map[string]bool{}
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		up := string(raw)
		if i := strings.Index(up, "-- +goose Down"); i >= 0 {
			up = up[:i]
		}
		for _, m := range transitionInsertRE.FindAllStringSubmatch(up, -1) {
			for _, e := range edgeRE.FindAllStringSubmatch(m[2], -1) {
				fromMigrations[m[1]+":"+e[1]+"->"+e[2]] = true
			}
		}
	}
	if len(fromMigrations) < 10 {
		t.Fatalf("只从迁移里解析出 %d 条状态机的边 —— 解析失效了：%v", len(fromMigrations), fromMigrations)
	}
	queries := stateQueries(t)
	for edge := range fromMigrations {
		qs, ok := stateEdges[edge]
		if !ok {
			t.Errorf("状态机里有一条边 %s，但 stateEdges 没写它由哪条语句走 —— "+
				"新边要先决定它发不发通知（notificationCallSites），再登记在这里", edge)
			continue
		}
		for _, q := range qs {
			if q != "" && !queries[q] {
				t.Errorf("stateEdges[%s] 点名的语句 %s 不是 db/queries 里的一条状态变化语句", edge, q)
			}
		}
	}
	for edge := range stateEdges {
		if !fromMigrations[edge] {
			t.Errorf("stateEdges 里写了 %s，但状态机里没有这条边 —— 请删掉这一行", edge)
		}
	}
}

// 模板表与契约的枚举在 internal/handler 那边核对（那里才 import 得到 api）；
// 这里核对每一种都渲染得出来、没有漏掉的占位符。
func TestEveryNotificationTemplateRenders(t *testing.T) {
	p := notifyParams{OrderNo: "O1", RefundNo: "R1", AmountCents: 1234, CarrierCode: "sf",
		TrackingNo: "SF1", Reason: "<不符>", RefundType: 2, Days: 7, ProductTitle: "连衣裙",
		SpecLabel: "（黑）", StoreName: "北京门店", Left: 0, Warning: 5}
	for kind := range notificationTemplates {
		_, title, body, err := renderNotification(kind, p)
		if err != nil {
			t.Errorf("%s 渲染失败: %v", kind, err)
			continue
		}
		if title == "" || body == "" || strings.Contains(title+body, "<no value>") {
			t.Errorf("%s 渲染出来是 %q / %q", kind, title, body)
		}
	}
	if _, _, body, _ := renderNotification(KindRefundRejected, p); !strings.Contains(body, "<不符>") {
		t.Errorf("驳回理由被转义了：%q —— 通知是纯文本，不该做 HTML 转义", body)
	}
	if _, _, body, _ := renderNotification(KindOrderPaid, p); !strings.Contains(body, "¥12.34") {
		t.Errorf("金额渲染成了 %q，期望 ¥12.34", body)
	}
	if got := strconv.Itoa(len(NotificationKinds())); got != strconv.Itoa(len(notificationTemplates)) {
		t.Errorf("NotificationKinds() 有 %s 种，模板有 %d 种", got, len(notificationTemplates))
	}
}
