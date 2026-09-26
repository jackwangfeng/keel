package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/keel/keel/internal/dtm"
	"github.com/keel/keel/internal/repository"
	"github.com/keel/keel/internal/tenant"
)

// 下单 SAGA 的三个正向分支与它们的补偿：建单、锁券、扣库存。
//
// # 这个文件里每一行都受制于同一句话：分支只拿到三个字符串
//
//	BranchFunc func(gid, branchID, op string) int
//
// 没有业务载荷，没有请求上下文，没有当前用户。分支跑在任何 HTTP 请求之外，
// 崩溃重启后由协调器重放，进程对原请求毫无记忆。所以：
//
//   - **租户**从 gid 来（dtm.TenantContextFromGID），不从别处来；
//   - **订单号**从 gid 来；
//   - **扣哪些 SKU、各扣几件**从库里读（order_items），因为它们推不出来；
//   - **锁哪张券**也从库里读（orders.user_coupon_id，00026），理由同上。
//
// 第三条正是「订单必须先落库再提交 SAGA」的全部理由，写在 order.go 的文件头。

// 分支名。它们同时出现在两处：注册表（Branches）与编排 JSON（sagaSteps）。
//
// 做成常量而不是两处各写一遍字面量：写错一个字母不会有编译错误，症状是提交时
// 被拒（「未注册的 local:// 名字」），而那个错误指向的是提交它的业务代码。
const (
	BranchOrderCreate     = "order_create"
	BranchOrderCreateUndo = "order_create_undo"
	BranchOrderStock      = "order_stock"
	BranchOrderStockUndo  = "order_stock_undo"
	BranchOrderCoupon     = "order_coupon"
	BranchOrderCouponUndo = "order_coupon_undo"
)

// dtmrs 的 BranchOp 里我们只用到的两个。见 repository/saga.go 的白名单。
const (
	opAction     = "action"
	opCompensate = "compensate"
)

// sagaSteps 是编排。**建单在前，库存在后**，理由写在 order.go 的文件头：
// 补偿只对真的执行过的分支生效，库存排在前面时，一次库存失败会让建单分支的
// 补偿变成一次空回滚，那笔 status = 0 的订单就永远没人关。
//
// **券夹在中间（00026）**，同一条理由推出来的：
//   - 券在库存之前：库存失败时券分支已经执行过，它的补偿是一次**真**补偿，
//     券回到「未使用」。券排在库存之后的话，库存失败时券分支从没跑过，倒也不用解锁 ——
//     但券自己锁不上时，库存已经扣了，要多补偿一个分支，而且扣库存这一步白拿了行锁。
//   - 券在建单之后：券锁不上（被别的单占了、刚好过期）时建单分支已经执行过，
//     订单被补偿关到 90，而不是留下一行没人关的 status = 0。
//
// 没带券的订单同样经过券分支，两个方向都是空操作（lockCoupon / unlockCoupon
// 看到 user_coupon_id 为空直接返回）。不按「带没带券」拼两份编排：一份常量编排
// 意味着分支号（01/02/03）对每一单都一样，排障时不必先问「这单带券了吗」。
//
// 它是一个常量字符串而不是每次 Marshal 一个结构体：这段 JSON 里没有任何随请求
// 变化的东西（steps 里只有地址，不带业务载荷），而一个每次都重新拼的常量
// 只是多了一处可以拼错的地方。
const sagaSteps = `[` +
	`{"action":"local://order_create","compensate":"local://order_create_undo"},` +
	`{"action":"local://order_coupon","compensate":"local://order_coupon_undo"},` +
	`{"action":"local://order_stock","compensate":"local://order_stock_undo"}]`

// 订单状态（数据模型 §5 + 00013）。
const (
	orderStatusDraft   int16 = 0  // 创建中：已落库、SAGA 还没跑完，用户看不见
	orderStatusPending int16 = 10 // 待支付
	orderStatusClosed  int16 = 90 // 已关闭
)

// errOrderNotDraft：建单分支想把订单从 0 推到 10，却发现它已经不是 0 了。
//
// 有了屏障的重复判定之后，走到这里只剩两种可能：这一单在本租户不可见
// （gid 的租户与订单的租户对不上），或者有人在 SAGA 之外改了它的状态。
// 两种都不是业务分支，但都必须让这次 SAGA 失败而不是无限重试 ——
// 所以它是一个 sentinel 而不是一句普通的 fmt.Errorf。
var errOrderNotDraft = errors.New("订单不在「创建中」状态")

// Branches 返回要注册到协调器上的全部进程内分支，键就是编排里 "local://"
// 后面那个名字。
//
// **注册必须发生在 Start 之前**（dtm.Start 把这个顺序封在里面了）。
func (s *OrderService) Branches() map[string]dtm.BranchFunc {
	return map[string]dtm.BranchFunc{
		BranchOrderCreate:     s.branch(BranchOrderCreate, opAction, promoteOrder),
		BranchOrderCreateUndo: s.branch(BranchOrderCreateUndo, opCompensate, closeOrder),
		BranchOrderStock:      s.branch(BranchOrderStock, opAction, deductStock),
		BranchOrderStockUndo:  s.branch(BranchOrderStockUndo, opCompensate, restoreStock),
		BranchOrderCoupon:     s.branch(BranchOrderCoupon, opAction, lockCoupon),
		BranchOrderCouponUndo: s.branch(BranchOrderCouponUndo, opCompensate, unlockCoupon),
	}
}

// branchBody 是一个分支真正要干的事，跑在一个「设好租户 + 过了屏障」的事务里。
type branchBody func(ctx context.Context, tx repository.Tx, order repository.Order) error

// branch 把一个 branchBody 包成 dtmrs 要的 BranchFunc。
//
// 这层包装做四件事，每一件都对应一条前面定下的硬约束：
//
//	① 租户**只**经 dtm.TenantContextFromGID 产生。repository 那一层不解析 gid
//	  （让它 import internal/dtm 会把 cgo 拖进数据访问层），所以「ctx 里的租户」
//	  与「gid 里的租户」一致性由这唯一的产生者按构造保证。手搓一个带着别家租户
//	  的 ctx，屏障与业务都会老老实实跑在那个错租户下 —— 没有任何东西会复核。
//	  order_saga_test.go 的 TestServiceNeverBuildsATenantContextByHand 扫源码
//	  钉住这一点。
//
//	② op 必须是这个分支预期的那个。协调器把正向地址与补偿地址分别登记在
//	  steps 里，写反了（compensate 指向正向分支）不会有编译错误，症状是：
//	  屏障按补偿语义判定（空回滚保护），而分支体在扣库存。这里当场拦下来。
//
//	③ 三种屏障判定**都是正常路径**。空回滚与重复什么都不做、返回成功 ——
//	  返回失败会让协调器误判并无限重试一件本来就不该做的事（架构 §5 明写）。
//	  WithSagaBranch 已经把这件事做完了，这里只是不要再把判定翻成失败。
//
//	④ 失败要分类。业务失败（库存不足）返回 Failure 触发全局补偿；说不清楚的
//	  失败（连接断了、死锁）返回 Unknown 让协调器重试，重复执行由屏障挡住。
//	  **绝不用 Failure 表示「我不知道业务做没做」** —— 那是在断言「肯定没做」。
func (s *OrderService) branch(name, wantOp string, body branchBody) dtm.BranchFunc {
	return func(gid, branchID, op string) int {
		log := s.log.With("gid", gid, "branch_id", branchID, "op", op, "branch", name)

		if op != wantOp {
			// ②。返回 Unknown 而不是 Failure：这是一个编排配错的 bug，
			// 症状是事务卡住（协调器一直重试），而不是一次跑在错误语义下的写入。
			// 卡住看得见，写反了看不见。
			log.Error("分支收到的 op 与它的角色不符，编排里的 action/compensate 写反了？",
				"want_op", wantOp)
			return dtm.Unknown
		}

		// ①。context.Background() 是对的：分支跑在任何 HTTP 请求之外，
		// 本来也没有别的 ctx 可用，而租户只能来自 gid。
		ctx, merchantID, orderNo, err := dtm.TenantContextFromGID(context.Background(), gid)
		if err != nil {
			log.Error("分支拿到的 gid 解析不出租户，拒绝执行", "err", err)
			return dtm.Failure
		}
		log = log.With("merchant_id", merchantID, "order_no", orderNo)

		decision, err := s.repo.WithSagaBranch(ctx, gid, branchID, op, func(tx repository.Tx) error {
			order, err := tx.FindOrderByNo(ctx, orderNo)
			if err != nil {
				return err
			}
			return body(ctx, tx, order)
		})
		if err != nil {
			return s.reportBranchFailure(log, gid, err)
		}
		// ③。Execute / NullCompensation / Duplicated 三种判定都是正常路径。
		log.Debug("分支完成", "decision", decision.String())
		return dtm.Success
	}
}

// reportBranchFailure 把分支错误分类，并把真正的原因留给还在等着的 HTTP 请求。
func (s *OrderService) reportBranchFailure(log *slog.Logger, gid string, err error) int {
	switch {
	case errors.Is(err, repository.ErrInsufficientStock):
		// 正常业务分支：触发全局补偿，用户看到 409「库存不足」。
		log.Info("库存不足，触发全局补偿", "err", err)
		s.notes.put(gid, fmt.Errorf("%w: %v", ErrInsufficientStock, err))
		return dtm.Failure

	case errors.Is(err, repository.ErrSKUNotInTenant):
		// **不是业务分支。** 翻译成「库存不足」会把一次跨租户访问伪装成一次
		// 正常的缺货：补偿是幂等的、日志是正常的，于是它在监控上只表现为
		// 库存波动。这里留一条 Error 级日志，并把一个**不同的** sentinel
		// 交给 HTTP 那一侧，它会变成 500 而不是 409。
		log.Error("扣减时发现 SKU 在本租户不可见 —— 这是 bug 或攻击，不是缺货", "err", err)
		s.notes.put(gid, fmt.Errorf("%w: %v", ErrCrossTenantSKU, err))
		return dtm.Failure

	case errors.Is(err, ErrCouponNotApplicable):
		// 正常业务分支：券在试算之后、锁券之前被别的单占了或刚好过期。
		// 触发全局补偿（建单被关掉），用户看到 409「券不可用」。
		log.Info("券锁不上，触发全局补偿", "err", err)
		s.notes.put(gid, err)
		return dtm.Failure

	case errors.Is(err, repository.ErrOrderNotFound), errors.Is(err, errOrderNotDraft):
		log.Error("分支找不到它要处理的订单，或订单状态不对", "err", err)
		s.notes.put(gid, fmt.Errorf("%w: %v", ErrOrderSagaFailed, err))
		return dtm.Failure

	default:
		// ④。说不清楚的失败（连接断了、死锁、屏障被拒）返回 Unknown：
		// 协调器会重试，而重复执行由屏障挡住。返回 Failure 是在断言
		// 「业务肯定没做」，而这里恰恰不知道。
		log.Error("分支失败，按 Unknown 上报以便协调器重试", "err", err)
		s.notes.put(gid, fmt.Errorf("%w: %v", ErrOrderSagaFailed, err))
		return dtm.Unknown
	}
}

// promoteOrder 是建单分支的正向：0 创建中 → 10 待支付。
func promoteOrder(ctx context.Context, tx repository.Tx, order repository.Order) error {
	n, err := tx.PromoteOrderDraft(ctx, order.OrderNo)
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("%w: %s 当前是 %d，受影响 %d 行",
			errOrderNotDraft, order.OrderNo, order.Status, n)
	}
	return nil
}

// closeOrder 是建单分支的补偿：把订单关到 90。
//
// # 受影响 0 行有两种成因，本轮把它们分开了
//
// 原来这里是 `_, err := tx.CloseOrder(...); return err` —— 0 行一律当成功，
// 理由是「补偿必须幂等，报错会让协调器一直重试一件已经做完的事」。那句话没错，
// 但它把两件事合在了一起：
//
//	① 这一单已经是 90 了     幂等重放。正常，静默。
//	② 这一单已经是 20 或更远  **它在补偿跑到之前被付掉了。**
//
// ② 是接上支付回调（任务 7）之后才第一次变得可能的。它的后果很具体：
// 用户付了钱、订单显示已支付，而库存分支失败了 —— 那批货根本没扣，
// 而 SAGA 会向 HTTP 那一侧报「库存不足」。钱在里面，货没有。
//
// **它今天不可达**，而且不是靠运气：order_no 是 72 bit 随机不可枚举，
// 且只在 SAGA 到达终态之后才对外返回（order.go 的 Create 第三段），
// 所以窗口里没有任何人知道该付哪一单。完整论证写在架构 §5 第 8 条。
//
// 那为什么还要写这一段：**不可达不等于不存在**。它靠的是「单号在终态前不外泄」
// 这条不变量，而那条不变量住在另一个函数里。哪天有人让 Create 提前返回单号
// （比如为了「让前端早点开始轮询」），这个洞就开了，而在此之前没有任何东西
// 会提醒他。这里花五行，让它一旦发生就有声音。
//
// 仍然返回 nil：报错会让协调器无限重试一件它改不了的事（订单已经付掉了，
// 重试一百次也关不掉）。声音留在日志与库里 —— 那一单的状态是 20，
// 而它一件库存都没有，对账会撞上它。
func closeOrder(ctx context.Context, tx repository.Tx, order repository.Order) error {
	n, err := tx.CloseOrder(ctx, order.OrderNo)
	if err != nil {
		return err
	}
	if n == 0 && order.Status != orderStatusClosed {
		// order.Status 是**这个屏障事务里刚读出来的**那一个（branch() 里的
		// FindOrderByNo），不是某个缓存下来的快照 —— 所以它和上面那条 UPDATE
		// 看到的是同一行。
		slog.ErrorContext(ctx, "建单补偿关不掉这一单，它已经被支付了 —— "+
			"「建单在前、库存在后」的窗口真的被踩到了。这一单显示已支付，"+
			"而它的库存从没扣过：需要人工退款。"+
			"是不是有人让 POST /orders 在 SAGA 到终态之前就把订单号返回了？",
			"order_no", order.OrderNo, "status", order.Status)
	}
	return nil
}

// lockCoupon 是券分支的正向：1 未使用 → 2 锁定，回填 order_id。
//
// **锁哪张券从订单行上读**（order.UserCouponID，00026），不从调用参数里拿 ——
// 分支只拿到 (gid, branch_id, op) 三个字符串。order 是 branch() 在**这个屏障事务里**
// 刚按 gid 里的订单号读回来的那一行。
//
// 锁不上（受影响 0 行）是一种正常的业务结果：这张券在试算之后被另一单占了、
// 刚好过了有效期。返回 ErrCouponNotApplicable → reportBranchFailure 报 Failure →
// 建单分支被补偿关单，而库存分支排在后面、从没跑过。
func lockCoupon(ctx context.Context, tx repository.Tx, order repository.Order) error {
	if order.UserCouponID == nil {
		return nil
	}
	n, err := tx.LockUserCoupon(ctx, *order.UserCouponID, order.UserID, order.ID)
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("%w: 订单 %s 要用的券 %d 锁不上（已被占用、已使用或已过期）",
			ErrCouponNotApplicable, order.OrderNo, *order.UserCouponID)
	}
	return nil
}

// unlockCoupon 是券分支的补偿：2 锁定 → 1 未使用，清空 order_id。
//
// 受影响 0 行与 closeOrder 同样有两种成因，也同样分开：
//
//	① 这张券本来就没被这一单锁住（空回滚已由屏障挡掉；走到这里多半是重放） —— 静默。
//	② 这张券已经是 3 已使用：这一单在补偿跑到之前被付掉了。
//
// ② 与 closeOrder 那个窗口是同一个（「单号在终态前不外泄」这条不变量守着它），
// 所以同样只出声、不报错：报错会让协调器无限重试一件改不了的事。
func unlockCoupon(ctx context.Context, tx repository.Tx, order repository.Order) error {
	if order.UserCouponID == nil {
		return nil
	}
	n, err := tx.UnlockCouponForOrder(ctx, order.ID)
	if err != nil {
		return err
	}
	if n == 0 {
		if _, status, err := tx.CouponStatusForOrder(ctx, order.ID); err == nil &&
			status == repository.UserCouponUsed {
			slog.ErrorContext(ctx, "券分支补偿解不开这张券，它已经被核销了 —— "+
				"这一单在补偿跑到之前被支付了。需要人工处理。",
				"order_no", order.OrderNo, "user_coupon_id", *order.UserCouponID)
		}
	}
	return nil
}

// deductStock 是库存分支的正向：逐行扣减，并记流水。
//
// 扣减意图从 order_items 读回来 —— 这是硬约束二的落点，分支手里没有任何载荷。
//
// 流水不是装饰：正向扣减与补偿回补跑完之后，available_qty 回到原值，和
// 「从来没扣过」一模一样。只有这两行流水能把两者分开，而「补偿到底跑没跑」
// 正是 SAGA 最需要能被证伪的那件事。
func deductStock(ctx context.Context, tx repository.Tx, order repository.Order) error {
	lines, err := tx.ListOrderLines(ctx, order.ID)
	if err != nil {
		return err
	}
	if len(lines) == 0 {
		// 一笔没有行的订单扣不了任何东西。返回成功会让这单白拿货，
		// 所以它必须是一个错误 —— 而且是确定性的，不该被重试。
		return fmt.Errorf("%w: 订单 %s 一行都没有", errOrderNotDraft, order.OrderNo)
	}
	for _, ln := range lines {
		// order.StoreID 来自**这个屏障事务里刚读回来的那一行订单**
		// （branch() 里的 FindOrderByNo），不是某个缓存下来的快照 ——
		// 数据模型 §4：分支从 orders 读 store_id，不从 gid 解析，
		// 因为 gid 是屏障幂等的键，改它的文法等于改那把钥匙的形状。
		after, err := tx.DeductInventory(ctx, ln.SKUID, order.StoreID, ln.Quantity)
		if err != nil {
			return err
		}
		if err := tx.AppendInventoryLog(ctx, ln.SKUID, order.StoreID, -ln.Quantity,
			repository.InventoryLogOrderDeduct, order.OrderNo, after+ln.Quantity, after); err != nil {
			return err
		}
		// 跌破预警线的那一次给门店发库存预警，与扣减同一个屏障事务（数据模型 §16）。
		if err := notifyLowStockIfCrossed(ctx, tx, order, ln.SKUID, ln.Quantity, after); err != nil {
			return err
		}
	}
	return nil
}

// restoreStock 是库存分支的补偿：逐行回补，并记流水。
//
// 它与 deductStock 读的是同一份 order_items、同样按 sku_id 排序 ——
// 两个方向按同一个顺序拿行锁，否则两笔互相交叉的订单在高并发下能互相死锁。
func restoreStock(ctx context.Context, tx repository.Tx, order repository.Order) error {
	lines, err := tx.ListOrderLines(ctx, order.ID)
	if err != nil {
		return err
	}
	for _, ln := range lines {
		// 回补一定回补到**当初扣减的那一家店**：补偿分支与正向分支读的是
		// 同一行订单，所以 order.StoreID 逐字相同。
		after, err := tx.RestoreInventory(ctx, ln.SKUID, order.StoreID, ln.Quantity)
		if err != nil {
			return err
		}
		if err := tx.AppendInventoryLog(ctx, ln.SKUID, order.StoreID, ln.Quantity,
			repository.InventoryLogSagaCompense, order.OrderNo, after-ln.Quantity, after); err != nil {
			return err
		}
	}
	return nil
}

// orderGID 是**唯一**造下单 gid 的地方。
//
// 经 dtm.OrderGID 而不是自己拼字符串：它会拒绝拼出一个 dtmrs 会截断的超长 gid，
// 也会拒绝订单号里带空白（那会让「看起来一样的两个 gid」是两笔不同的事务）。
//
// 租户从**请求上下文**取，不从 repository.Order 上取 —— 那个类型刻意没有
// MerchantID（见 repository/user.go 里 User 的同一段注释）：这一层每一次读写都
// 发生在一个设好 app.merchant_id 的事务里，把租户带上来只会制造第二个可能与
// ctx 对不上的真相。而 gid 里那个租户，正是 ctx 里这一个。
func orderGID(ctx context.Context, orderNo string) (string, error) {
	merchantID, err := tenant.FromContext(ctx)
	if err != nil {
		return "", err
	}
	return dtm.OrderGID(merchantID, orderNo)
}

// branchNotes 把分支失败的**真正原因**递给还在等着的那个 HTTP 请求。
//
// 为什么需要它：WaitFinal 只回一个终态字符串（"succeed" / "failed"），
// 里面没有「为什么」。而这条链路上「为什么」不是文案问题 —— 库存不足要回 409，
// 跨租户 SKU 要回 500，两者绝不能合并（那正是硬约束三要防的事）。
//
// **它是进程内的、尽力而为的**，这一点要说清楚：
//   - 分支与 HTTP 请求在同一个进程里（嵌入式协调器），所以正常路径上它总是在的；
//   - 崩溃重启后的重放没有任何请求在等，那时写进来的条目没人读。
//     所以有一个容量上限，满了就丢最新的 —— 丢的是一条诊断信息，不是数据。
//
// 不把原因写进数据库：那需要一张表（或者往 orders 上加一列），而它存的是
// 「上一次为什么失败」这种只在几秒钟内有意义的东西。真要持久化，该做的是
// 让协调器把分支的错误带回来，那是 dtmrs 的接口面问题，不是这里的。
type branchNotes struct {
	mu sync.Mutex
	m  map[string]error
}

// maxBranchNotes 是上限。超过它说明有大量没人等的重放在失败，
// 那时该看的是日志（每一条失败都有 Error 级日志），不是这个 map。
const maxBranchNotes = 1024

func newBranchNotes() *branchNotes { return &branchNotes{m: map[string]error{}} }

// put 只记第一条：一次 SAGA 里第一个失败的分支才是根因，后面的补偿失败是果。
func (n *branchNotes) put(gid string, err error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if _, ok := n.m[gid]; ok {
		return
	}
	if len(n.m) >= maxBranchNotes {
		return
	}
	n.m[gid] = err
}

func (n *branchNotes) get(gid string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.m[gid]
}

func (n *branchNotes) drop(gid string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	delete(n.m, gid)
}

// archivedFailure 是失败存档的形状（idempotency_keys.response_body）。
//
// 存的是**哪一个 sentinel**，不是一个 HTTP 状态码或一句文案。
// 回放时把 sentinel 还原出来，由 handler 用与首次完全相同的那段代码去映射 ——
// 于是「回放的响应与首次一致」不靠人去保持两处同步，它按构造就成立。
type archivedFailure struct {
	Failure string `json:"failure"`
	Detail  string `json:"detail,omitempty"`
}

// replayableFailures 是可回放的失败与它们的存档名。
//
// 只有**业务**失败进这张表。基础设施失败（连不上库、屏障被拒）不该被存档回放：
// 它们下一秒可能就好了，而回放会让客户端在 24 小时里一直看到同一个错误。
var replayableFailures = map[string]error{
	"insufficient_stock":    ErrInsufficientStock,
	"cross_tenant_sku":      ErrCrossTenantSKU,
	"saga_failed":           ErrOrderSagaFailed,
	"coupon_not_applicable": ErrCouponNotApplicable,
}

func encodeArchivedFailure(err error) archivedFailure {
	for name, sentinel := range replayableFailures {
		if errors.Is(err, sentinel) {
			return archivedFailure{Failure: name, Detail: err.Error()}
		}
	}
	return archivedFailure{Failure: "saga_failed", Detail: err.Error()}
}

func decodeArchivedFailure(raw []byte) error {
	var a archivedFailure
	if err := json.Unmarshal(raw, &a); err != nil {
		return fmt.Errorf("%w: 失败存档解不开: %v", ErrOrderSagaFailed, err)
	}
	sentinel, ok := replayableFailures[a.Failure]
	if !ok {
		return fmt.Errorf("%w: 存档里的失败名 %q 不认识", ErrOrderSagaFailed, a.Failure)
	}
	return fmt.Errorf("%w（幂等回放）: %s", sentinel, a.Detail)
}
