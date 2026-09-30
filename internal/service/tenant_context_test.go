package service_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
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
//
// # M2 任务 6 让这条检查第一次需要一个豁免面，理由与形状
//
// 超时补偿定时任务（sweep.go）**没有 gid**，也没有 Host —— 它跑在任何 HTTP
// 请求之外，而它要处理的订单分属不同的租户。它拿租户的唯一办法就是枚举
// merchants 再一家一家 tenant.NewContext（论证写在 repository/sweep.go 与
// service/sweep.go 的文件头）。
//
// 所以这条检查从「本包里一次都不许出现」变成「只许出现在登记过的文件里」。
// 那是一次真实的放宽，所以它配了三道，缺一道这份清单就会烂掉：
//
//	① 清单是**按文件**的，每一条要写 reason —— 和 db/tenancy.json 的豁免面
//	  同一个规矩：往里加一行是一个需要解释的动作。
//	② 登记过的文件**不许同时出现 WithSagaBranch**。这一条是豁免与它的理由
//	  之间的锁：豁免的依据是「这个文件不是 SAGA 分支」，而不是「这个文件比较
//	  特殊」。哪天有人把一个分支搬进 sweep.go，豁免当场失效。
//	③ 登记了却已经不用 tenant.NewContext 的文件 → 红。清单不能留着过期的行，
//	  否则下一个人会以为那个文件天然享有豁免。
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
	usedAllowance := map[string]bool{}

	for _, pkg := range pkgs {
		for name, f := range pkg.Files {
			files++
			base := filepath.Base(name)
			// ② 豁免的依据是「这个文件不是 SAGA 分支」。把依据本身查一遍，
			// 而不是相信登记的那一刻依据成立。
			if _, allowed := tenantContextAllowed[base]; allowed && mentions(f, "WithSagaBranch") {
				t.Errorf("%s 登记在 tenantContextAllowed 里，但它在调 WithSagaBranch —— "+
					"豁免的依据是「这个文件不是 SAGA 分支」，而它现在是了。"+
					"把分支挪回 order_saga.go，或者删掉这条豁免", base)
			}
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
					if why, allowed := tenantContextAllowed[base]; allowed {
						usedAllowance[base] = true
						t.Logf("%s:%d tenant.NewContext 按登记放行：%s",
							base, fset.Position(call.Pos()).Line, why)
						return true
					}
					handmade++
					t.Errorf("%s:%d 手搓了一个租户上下文（tenant.NewContext）—— "+
						"SAGA 分支的租户只许由 dtm.TenantContextFromGID 从 gid 解出来。"+
						"repository 那一层不会复核，所以一个错租户会被原样执行："+
						"屏障与业务都跑在别家店下，而且不报错。"+
						"如果这段代码真的没有 gid 可用（比如定时任务），"+
						"请往 tenantContextAllowed 里加一行并写明理由",
						base, fset.Position(call.Pos()).Line)
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
	// ③ 清单不能烂掉：登记了却已经不用的行必须被删掉。
	for base, why := range tenantContextAllowed {
		if !usedAllowance[base] {
			t.Errorf("tenantContextAllowed 里挂着 %q（%s），但它已经不再调 "+
				"tenant.NewContext 了 —— 请删掉这一行，别让下一个人以为"+
				"这个文件天然享有豁免", base, why)
		}
	}

	t.Logf("扫了 %d 个源文件：手搓租户上下文 %d 处（登记放行 %d 个文件），经 gid 解析 %d 处",
		files, handmade, len(usedAllowance), fromGID)
}

// tenantContextAllowed 是允许出现 tenant.NewContext 的源文件，键是文件名。
//
// **每一条都要写清楚「为什么这里没有 gid 也没有 Host」**，因为那是豁免的全部
// 依据。默认答案是「租户从 gid 来」——想加一行之前先确认真的不是这种情况。
var tenantContextAllowed = map[string]string{
	"inventory_outbox.go": "库存 outbox 的 worker（关单释放、退款回补，微服务拆分阶段 1b），与 notification_delivery.go 同一处境：" +
		"跑在任何 HTTP 请求之外，没有 Host 也没有 gid。按出队（或按 job_key 点名占下）那一行的 jobs.merchant_id " +
		"进 WithTenant 与调库存服务 —— 那一列由入队时的 current_merchant() 写下，写不出别家的租户（00022）。它不是 SAGA 分支。",
	"notification_delivery.go": "消息通知的外发 worker 与保留期清理，与 index.go 同一处境：" +
		"跑在任何 HTTP 请求之外，没有 Host 也没有 gid。消费侧按出队那一行的 jobs.merchant_id " +
		"进 WithTenant（与 index.go 的消费侧同一个做法）；保留期清理要逐家删过期的通知" +
		"（notifications 有 RLS，没有能跨租户的 DELETE），拿租户的办法照抄 sweep：" +
		"枚举 merchants 再逐家进，公平调度共用 fairRound。它不是 SAGA 分支，上面第 ② 道会把这一点钉住。",
	"auto_confirm.go": "自动确认收货定时任务，与 sweep.go 同一处境：跑在任何 HTTP 请求之外，" +
		"没有 Host 也没有 gid（它推进的是已发货订单，不属于任何一笔正在跑的全局事务）。" +
		"拿租户的办法照抄 sweep：枚举 merchants 再逐家进 WithTenant，公平调度直接共用 fairRound。" +
		"它不是 SAGA 分支，上面第 ② 道会把这一点钉住。",
	"return_timeout.go": "退货超时未寄回自动关闭，与 auto_confirm.go 同一处境：跑在任何 HTTP 请求之外，" +
		"没有 Host 也没有 gid（它关的是停在 20 的售后单，不属于任何一笔正在跑的全局事务）。" +
		"拿租户的办法照抄 sweep：枚举 merchants 再逐家进 WithTenant，公平调度共用 fairRound。" +
		"它不是 SAGA 分支，上面第 ② 道会把这一点钉住。",
	"inventory_reconcile.go": "库存对账（微服务拆分阶段 2），与 auto_confirm.go 同一处境：跑在任何 HTTP 请求之外，" +
		"没有 Host 也没有 gid。只读：枚举 merchants 再逐家进 WithTenant 读 core 的 skus / stores / promotion_skus，" +
		"并带着同一个租户上下文调库存服务（租户头由它给出）。它不是 SAGA 分支，上面第 ② 道会把这一点钉住。",
	"agent_proposal_outcome.go": "AI 员工提案的执行后复盘（00122），与 agent_proposal_expiry.go 同一处境：跑在任何 HTTP 请求之外，" +
		"没有 Host 也没有 gid。枚举 merchants 再逐家进 WithTenant，量到点的提案并写回 outcome；它调库存服务（断货天数）用的也是这个租户上下文。它不是 SAGA 分支。",
	"payment_return.go": "多收款退回的兜底扫描与重试（00150），与 agent_proposal_expiry.go 同一处境：跑在任何 HTTP 请求之外，" +
		"没有 Host 也没有 gid。枚举 merchants 再逐家进 WithTenant，补开订单不认的到账的退回单、提交待提交的。它不是 SAGA 分支。",
	"agent_proposal_expiry.go": "AI 员工提案的过期扫描（00091），与 stock_flags.go 同一处境：跑在任何 HTTP 请求之外，" +
		"没有 Host 也没有 gid。枚举 merchants 再逐家进 WithTenant，把过期的待处理提案置 50。它不是 SAGA 分支。",
	"agent_event_sweep.go": "AI 员工事件扫描（00121，stock_low / search_zero_spike），与 inventory_reconcile.go 同一处境：" +
		"跑在任何 HTTP 请求之外，没有 Host 也没有 gid。枚举 merchants 再逐家进 WithTenant，并带着同一个租户上下文调库存服务" +
		"（LowStock，租户头由它给出）。它不是 SAGA 分支。",
	"agent_webhook_delivery.go": "AI 员工事件 webhook 的投递 worker（00121），与 notification_delivery.go 同一处境：" +
		"跑在任何 HTTP 请求之外，没有 Host 也没有 gid。按出队那一行的 jobs.merchant_id 进 WithTenant —— 那一列由入队时的 " +
		"current_merchant() 写下，写不出别家的租户（00022）。它不是 SAGA 分支。",
	"stock_flags.go": "商品列表有货排序标记的全量刷新（00087），与 inventory_reconcile.go 同一处境：跑在任何 HTTP 请求之外，" +
		"没有 Host 也没有 gid。枚举 merchants 再逐家进 WithTenant 读 skus / stores、写 product_store_stock，" +
		"并带着同一个租户上下文调库存服务（租户头由它给出）。它不是 SAGA 分支，上面第 ② 道会把这一点钉住。",
	"upload_gc.go": "孤儿上传文件回收，与 auto_confirm.go 同一处境：跑在任何 HTTP 请求之外，" +
		"没有 Host 也没有 gid。uploads 有 RLS，没有能跨租户的 DELETE，所以枚举 merchants 再逐家进，" +
		"公平调度共用 fairRound。它不是 SAGA 分支，上面第 ② 道会把这一点钉住。",
	"retention.go": "只增不删的表按保留期分批清理（幂等存档、检索日志、工具调用、库存流水），与 upload_gc.go " +
		"同一处境：跑在任何 HTTP 请求之外，没有 Host 也没有 gid。这几张表都有 RLS，没有能跨租户的 DELETE，" +
		"所以枚举 merchants（含停用，保留期对停用的店同样成立）再逐家进租户事务。它不是 SAGA 分支。",
	"sweep.go": "超时补偿定时任务跑在任何 HTTP 请求之外：没有 Host（tenant.Resolver 用不上）、" +
		"也没有 gid（它处理的订单不属于任何一笔正在跑的全局事务）。它拿租户的唯一办法是" +
		"枚举 merchants（tenant-root 类，没有 RLS）再逐家进 WithTenant —— " +
		"论证写在 repository/sweep.go 与 service/sweep.go 的文件头。" +
		"它不是 SAGA 分支，上面第 ② 道会把这一点钉住。",
	"index.go": "商品理解服务的慢路径（文本向量 + bigram 串），与 sweep.go 同一处境：" +
		"它跑在任何 HTTP 请求之外，没有 Host（tenant.Resolver 用不上），" +
		"也没有 gid（它加工的是商品，不属于任何一笔正在跑的全局事务）。" +
		"拿租户的办法照抄 sweep：枚举 merchants（tenant-root 类，没有 RLS）" +
		"再逐家进 WithTenant，公平调度也照那一套（每租户上限 + 每轮总预算 + 轮转起点 + 兜底）。" +
		"论证写在 repository/sweep.go 与 service/sweep.go 的文件头，" +
		"service/index.go 的文件头第二节只说了与它不同的那两处（其一：上限的量级" +
		"按一次 /v1/embeddings 的批大小定）。" +
		"M4 阶段 1 在中间插进了 jobs 表之后，**消费侧其实不再需要这条豁免**：" +
		"出队拿到的每一行都带着 jobs.merchant_id，worker 按它进 WithTenant。" +
		"生产侧仍然需要 —— 触发点扫描是按租户切的 N 条查询（products 有 RLS，" +
		"没有一条能跨租户的 SELECT），所以「这一轮先扫谁」还得由应用层枚举 merchants 决定。" +
		"它不是 SAGA 分支，上面第 ② 道会把这一点钉住。",
}

// mentions 判断这个文件里有没有出现某个标识符（作为选择器的字段名）。
//
// 只看名字、不做类型解析：这条检查要的是「这个文件里有没有 SAGA 分支的味道」，
// 而一个假阳性（提到了但没调用）的代价只是逼人写清楚，比漏掉便宜得多。
func mentions(f *ast.File, name string) bool {
	found := false
	ast.Inspect(f, func(n ast.Node) bool {
		if sel, ok := n.(*ast.SelectorExpr); ok && sel.Sel.Name == name {
			found = true
		}
		return !found
	})
	return found
}
