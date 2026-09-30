package inventory

// 下单 SAGA 的库存分支，以及与它共用「按流水判」这一套的关单释放 / 退款回补
// （微服务拆分阶段 1b，docs/电商系统-微服务拆分方案.md「要拆掉的耦合」第 3 节）。
//
// ===========================================================================
// 分支拿到的东西：gid + 载荷，没有订单表
// ===========================================================================
//
// 拆分前库存分支从 order_items 读「扣哪些 SKU、各扣几件」、从 orders 读门店、锁订单行判
// 「这一单还是不是待支付」。库存服务的库里没有那两张表，所以：
//
//   - 扣什么随步骤的载荷带过来（DeductPayload：订单号、门店、行），载荷在提交时与步骤一起
//     落进协调器的存储，崩溃重放时原样再给 —— 与 gid 同样可靠（internal/dtm 的 BranchFuncEx）；
//   - **租户只从 gid 来**（dtm.TenantContextFromGID），载荷里没有租户、有也不采信；
//     载荷里的订单号必须与 gid 里的一致，不一致是编排的 bug，确定性失败；
//   - 「这一单是不是还待支付」库存服务判不了，守卫分成两半（见下「关单守卫」）。
//
// ===========================================================================
// 一个本地事务
// ===========================================================================
//
// 扣门店库存、扣活动配额、写流水、写屏障，全在库存库的**同一个事务**里（InventoryStore.
// WithSagaBranch）：门店库存与活动配额是同一件货的两道闸，必须同生共死；屏障记录必须与业务
// 变更同事务提交，否则重试会被误判成重复（repository/saga.go 的文件头）。单体形态下库存池
// 就是业务池，屏障写的是同一张 barrier 表；拆分形态下是库存库里那一张。
//
// ===========================================================================
// 扣减被拒为什么是「成功」
// ===========================================================================
//
// 库存不足、活动配额不足、这一单已经被关单释放过 —— 这三种「业务上扣不了」，分支**提交**
// 一行扣减被拒的流水（biz_type 7，change 0，reason 是拒绝码）并返回 Success，而不是 Failure。
//
// 原因是 dtmrs 不把分支的响应带回给提交方：WaitFinal 只回「succeed / failed」。拆分前
// 分支与下单请求在同一个进程里，失败原因经一个进程内的 map 递过去（service 的 branchNotes）；
// 拆分之后分支跑在库存进程里，那个 map 够不着。于是原因落进库存库，由排在库存分支之后的
// core 收尾分支来问（OrderTrail）：看到拒绝码就在 core 进程里记下原因、返回 Failure 触发
// 全局补偿 —— 单体与拆分走的是同一条路，HTTP 那一侧拿到的 409 类型逐字相同。
//
// 代价写清楚：被拒的这一单在流水里多一行 change 0 的记录（秒杀抢光时每个没抢到的人一行），
// 以及库存分支的补偿会被真的调一次（屏障看到正向执行过）—— 补偿按流水放回，流水里没有扣减，
// 于是什么都不放。
//
// ===========================================================================
// 关单守卫：拆分前的「锁订单行判待支付」分成两半
// ===========================================================================
//
// 审查发现的窗口（F2）：建单分支先把订单推到 10，库存分支才扣；库存分支一次说不清的失败会被
// 协调器重试，重试之前买家取消、或超时任务关单了，关单那一侧按流水回补（什么都没扣，什么都
// 不补），随后迟到的重试扣下了库存 —— 一笔关掉的单占着货，再也没人放回来。
//
// 拆分前的守卫是在库存分支里锁订单行、看到 90 就拒绝。库存服务看不见订单，这里的做法是
// 让两边**各自在自己的库里、按同一把按单号的锁**判：
//
//  1. 关单释放（ReleaseForOrder）在库存库里按订单号加锁，放回之后**每一行都留一行释放流水**
//     （放回 0 件也写一行 change 0 的核对行）；扣减在同一把锁之下先看流水，**看到释放流水就拒绝**
//     （拒绝码 released）。于是「释放先、扣减后」时扣减不会发生。
//  2. 「扣减先、关单后」时关单那一侧按流水放回的正好是扣掉的那些（1 里的同一把锁让两者串行，
//     释放看得见扣减的流水）。而 SAGA 这一侧，排在库存分支之后的 core 收尾分支锁订单行再看一眼：
//     已经关掉了就确定性失败，全局补偿（本文件的 restore，同样按流水放回）—— 与关单释放谁先谁后
//     都只放回一次，因为两者都只放「流水里还没补回来的部分」，且在同一把锁之下。
//
// 只在库存分支之前加一道 core 的守卫是不够的：守卫提交之后、扣减之前那个窗口里的取消照样漏。
// 守卫必须落在「扣减」与「释放」两件事都经过的那个地方 —— 也就是库存库里、那把按单号的锁。
// internal/handler 的 TestCancelRacingARetriedInventoryBranch（单库与两库各一遍）钉住这件事。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/keel/keel/internal/dtm"
	"github.com/keel/keel/internal/repository"
)

// 分支名：进程内是 local://<名字>，拆分部署是 <KEEL_INVENTORY_URL>/internal/v1/saga/<名字>。
const (
	BranchDeduct  = "inventory_deduct"
	BranchRestore = "inventory_restore"
)

// maxOrderLines 是一个载荷里至多多少行。core 的下单上限远低于它（service.maxOrderLines），
// 这里只挡 bug 与伪造的载荷：一个几万行的载荷会在一个事务里锁几万行库存。
const maxOrderLines = 500

// DeductPayload 是库存分支的载荷（正向与补偿共用同一份）。
type DeductPayload struct {
	OrderNo string      `json:"order_no"`
	StoreID int64       `json:"store_id"`
	Lines   []OrderLine `json:"lines"`
}

// EncodeDeductPayload 把载荷编成 JSON。行按 sku_id 排好（锁的顺序），同一个 SKU 不许出现两次。
func EncodeDeductPayload(p DeductPayload) (string, error) {
	lines, err := normalizeLines(p.Lines)
	if err != nil {
		return "", err
	}
	p.Lines = lines
	if p.OrderNo == "" || p.StoreID <= 0 {
		return "", fmt.Errorf("%w: 扣减载荷缺订单号或门店（order_no=%q store_id=%d）", ErrInvalid, p.OrderNo, p.StoreID)
	}
	b, err := json.Marshal(p)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// normalizeLines 校验并按 sku_id 排序（复制一份，不改调用方的切片）。
func normalizeLines(in []OrderLine) ([]OrderLine, error) {
	if len(in) == 0 {
		return nil, fmt.Errorf("%w: 一行都没有", ErrInvalid)
	}
	if len(in) > maxOrderLines {
		return nil, fmt.Errorf("%w: 一张单至多 %d 行，实得 %d 行", ErrInvalid, maxOrderLines, len(in))
	}
	out := append([]OrderLine(nil), in...)
	sort.Slice(out, func(i, j int) bool { return out[i].SKUID < out[j].SKUID })
	for i, ln := range out {
		if ln.SKUID <= 0 || ln.Qty <= 0 {
			return nil, fmt.Errorf("%w: 行 sku=%d qty=%d 不合法", ErrInvalid, ln.SKUID, ln.Qty)
		}
		if ln.PromotionID != nil && *ln.PromotionID <= 0 {
			return nil, fmt.Errorf("%w: sku %d 的 promotion_id %d 不合法", ErrInvalid, ln.SKUID, *ln.PromotionID)
		}
		if i > 0 && out[i-1].SKUID == ln.SKUID {
			// 同一个 SKU 两行：按行放回时第二行会把第一行的净值当成自己的，账就乱了。
			return nil, fmt.Errorf("%w: sku %d 出现了两次", ErrInvalid, ln.SKUID)
		}
	}
	return out, nil
}

func skuIDsOf(lines []OrderLine) []int64 {
	out := make([]int64, len(lines))
	for i, ln := range lines {
		out[i] = ln.SKUID
	}
	return out
}

// errBadBranchInput：载荷或 gid 不成立。编排的 bug，重试也改变不了 —— 确定性失败。
var errBadBranchInput = errors.New("库存分支的输入不成立")

// SagaBranches 返回库存服务的两个 SAGA 分支（键是分支名）。
//
// 单体（KEEL_ROLE=all）注册到进程内协调器上（local://），拆分部署挂到库存进程的内网端口上
// （dtm.MountBranches(routes.Saga, ...)）—— 同一个函数，只是地址不同。
func (l *Local) SagaBranches() map[string]dtm.BranchFuncEx {
	return map[string]dtm.BranchFuncEx{
		BranchDeduct:  l.branch(BranchDeduct, "action", l.deduct),
		BranchRestore: l.branch(BranchRestore, "compensate", l.restore),
	}
}

type branchBody func(ctx context.Context, tx repository.InventoryStoreTx, rec *stockCrossings, p DeductPayload) error

// branch 把分支体包成 BranchFuncEx：op 核对、租户只从 gid 来、载荷解析与核对、屏障、失败分类。
// 与 core 那一侧的 OrderService.branch 同一套规矩（service/order_saga.go），逐条对应。
func (l *Local) branch(name, wantOp string, body branchBody) dtm.BranchFuncEx {
	return func(gid, branchID, op, payload string) int {
		log := slog.Default().With("gid", gid, "branch_id", branchID, "op", op, "branch", name)
		if op != wantOp {
			// 编排里 action / compensate 写反了。Unknown：事务卡住看得见，跑在错误语义下的写入看不见。
			log.Error("库存分支收到的 op 与它的角色不符，编排里的 action/compensate 写反了？", "want_op", wantOp)
			return dtm.Unknown
		}
		ctx, merchantID, orderNo, err := dtm.TenantContextFromGID(context.Background(), gid)
		if err != nil {
			log.Error("库存分支拿到的 gid 解析不出租户，拒绝执行", "err", err)
			return dtm.Failure
		}
		log = log.With("merchant_id", merchantID, "order_no", orderNo)
		p, err := decodePayload(payload, orderNo)
		if err != nil {
			log.Error("库存分支的载荷不成立，拒绝执行", "err", err)
			return dtm.Failure
		}
		// stockTx：跨 0 通知与扣减 / 补偿同一个事务登记（stock_msg.go）。屏障判成重复或空回滚时 body 不跑，
		// rec 是空的，不发任何通知 —— 那一次调用本来就什么都没改。
		decision := repository.Decision(0)
		err = l.stockTx(ctx, func(fn func(repository.InventoryStoreTx) error) error {
			var e error
			decision, e = l.store.WithSagaBranch(ctx, gid, branchID, op, fn)
			return e
		}, func(tx repository.InventoryStoreTx, rec *stockCrossings) error {
			return body(ctx, tx, rec, p)
		})
		if err != nil {
			// 说不清楚的失败（连接断了、死锁、屏障被拒）一律 Unknown：协调器会重试，
			// 重复执行由屏障挡住。业务上的「扣不了」不走这里（见文件头「扣减被拒为什么是成功」）。
			log.Error("库存分支失败，按 Unknown 上报以便协调器重试", "err", err)
			return dtm.Unknown
		}
		log.Debug("库存分支完成", "decision", decision.String())
		return dtm.Success
	}
}

func decodePayload(raw, orderNo string) (DeductPayload, error) {
	var p DeductPayload
	if strings.TrimSpace(raw) == "" {
		return p, fmt.Errorf("%w: 没有载荷", errBadBranchInput)
	}
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return p, fmt.Errorf("%w: 载荷解不开: %v", errBadBranchInput, err)
	}
	if p.OrderNo != orderNo {
		// 租户与订单号只认 gid；载荷里的订单号对不上说明编排拼错了载荷，绝不能扣到别的单头上。
		return p, fmt.Errorf("%w: 载荷里的订单号 %q 与 gid 里的 %q 不一致", errBadBranchInput, p.OrderNo, orderNo)
	}
	if p.StoreID <= 0 {
		return p, fmt.Errorf("%w: 载荷没有门店", errBadBranchInput)
	}
	lines, err := normalizeLines(p.Lines)
	if err != nil {
		return p, fmt.Errorf("%w: %v", errBadBranchInput, err)
	}
	p.Lines = lines
	return p, nil
}

// deduct 是库存分支的正向：锁 → 判 → 扣（或记一行拒绝），一个事务。
func (l *Local) deduct(ctx context.Context, tx repository.InventoryStoreTx, rec *stockCrossings, p DeductPayload) error {
	if err := tx.LockBizID(ctx, p.OrderNo); err != nil {
		return err
	}
	trail, err := tx.BizTrail(ctx, p.OrderNo)
	if err != nil {
		return err
	}
	first := p.Lines[0]
	for _, e := range trail {
		if e.BizType == BizTimeoutRelease || e.BizType == BizBuyerCancel {
			// 关单守卫第 1 条（文件头）：这一单已经被关单释放过了，再扣就是给一笔关掉的单扣货。
			return reject(ctx, tx, p, first.SKUID, 0, RejectReleased)
		}
	}

	levels, err := tx.LockStoreStock(ctx, p.StoreID, skuIDsOf(p.Lines))
	if err != nil {
		return err
	}
	avail := make(map[int64]int32, len(levels))
	for _, lv := range levels {
		avail[lv.SKUID] = lv.Available
	}
	for _, ln := range p.Lines {
		if avail[ln.SKUID] < ln.Qty {
			// 缺行 ≡ 可售 0（数据模型 §4），也落在这一支。
			return reject(ctx, tx, p, ln.SKUID, avail[ln.SKUID], RejectInsufficient)
		}
	}
	// 活动配额：库存行一律先于配额行加锁（与释放、整组设配额同一个顺序，见 inventory_svc.sql）。
	promo := promoLines(p.Lines)
	for _, ln := range promo {
		a, ok, err := tx.LockActivity(ctx, *ln.PromotionID, ln.SKUID)
		if err != nil {
			return err
		}
		if !ok || (a.Quota > 0 && a.Sold+ln.Qty > a.Quota) {
			// 行不在 = 配额还没同步：宁可少卖，不超卖（00075 文件头）。
			return reject(ctx, tx, p, ln.SKUID, avail[ln.SKUID], RejectSoldOut)
		}
	}

	for _, ln := range p.Lines {
		after, err := tx.DeductLocked(ctx, ln.SKUID, p.StoreID, ln.Qty)
		if err != nil {
			return err
		}
		// 跨 0 的判断就在这把行锁之下、用这条语句本来就回的水位（stock_msg.go）：扣到 0 的那一单发通知，别的不发。
		rec.record(p.StoreID, ln.SKUID, after+ln.Qty, after)
		// 流水不是装饰：正向扣减与补偿回补跑完之后水位回到原值，和「从来没扣过」一模一样，
		// 只有流水能把两者分开 —— 而关单释放、补偿、收尾分支的预警判定全都按它来。
		if err := tx.AppendBizLog(ctx, repository.BizLogEntry{
			SKUID: ln.SKUID, StoreID: p.StoreID, ChangeQty: -ln.Qty, BizType: BizOrderDeduct,
			BizID: p.OrderNo, Before: after + ln.Qty, After: after,
		}); err != nil {
			return err
		}
	}
	for _, ln := range promo {
		if err := tx.AddActivitySold(ctx, *ln.PromotionID, ln.SKUID, ln.Qty); err != nil {
			return err
		}
	}
	return nil
}

// reject 记一行扣减被拒的流水（change 0，before = after = 那一刻读到的水位）。
func reject(ctx context.Context, tx repository.InventoryStoreTx, p DeductPayload,
	skuID int64, avail int32, code string) error {
	slog.InfoContext(ctx, "库存分支拒绝扣减（提交一行拒绝流水，由 core 的收尾分支触发全局补偿）",
		"order_no", p.OrderNo, "sku_id", skuID, "reason", code)
	return tx.AppendBizLog(ctx, repository.BizLogEntry{
		SKUID: skuID, StoreID: p.StoreID, ChangeQty: 0, BizType: BizOrderRejected,
		BizID: p.OrderNo, Before: avail, After: avail, Reason: &code,
	})
}

// promoLines 是按活动价成交的行，按 (活动, SKU) 排好（配额行的加锁顺序）。
func promoLines(lines []OrderLine) []OrderLine {
	var out []OrderLine
	for _, ln := range lines {
		if ln.PromotionID != nil {
			out = append(out, ln)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if *out[i].PromotionID != *out[j].PromotionID {
			return *out[i].PromotionID < *out[j].PromotionID
		}
		return out[i].SKUID < out[j].SKUID
	})
	return out
}

// restore 是库存分支的补偿：按流水放回这一单还没补回来的部分（biz_type 2）。
//
// 屏障只保证「正向执行过才补、补一次」；「补多少」按流水算，而不是按载荷的件数：
// 正向可能是一次拒绝（什么都没扣），也可能在补偿之前关单释放已经放回过了。
func (l *Local) restore(ctx context.Context, tx repository.InventoryStoreTx, rec *stockCrossings, p DeductPayload) error {
	if err := tx.LockBizID(ctx, p.OrderNo); err != nil {
		return err
	}
	_, err := putBack(ctx, tx, rec, p.OrderNo, p.StoreID, BizSagaCompensate, p.Lines, false)
	return err
}

// putBack 按流水把一张订单还没补回来的库存（与对应的活动配额）放回，调用方已经持有按订单号的锁。
//
// 每个 SKU 的「还欠多少」= −(这一单在这家店这个 SKU 上全部流水的净值)：扣减记负、补偿与释放
// 记正、拒绝与核对行记 0。marker 为真（关单释放）时，没有可放回的行也写一行 change 0 的核对行 ——
// 它是关单守卫第 1 条的依据（扣减看到释放流水就拒绝），也是对账时「关单时核对过，净值 0」的记录。
func putBack(ctx context.Context, tx repository.InventoryStoreTx, rec *stockCrossings, orderNo string, storeID int64,
	bizType int16, lines []OrderLine, marker bool) (int32, error) {
	trail, err := tx.BizTrail(ctx, orderNo)
	if err != nil {
		return 0, err
	}
	net := map[int64]int32{}
	for _, e := range trail {
		if e.StoreID == storeID {
			net[e.SKUID] += e.ChangeQty
		}
	}
	levels, err := tx.LockStoreStock(ctx, storeID, skuIDsOf(lines))
	if err != nil {
		return 0, err
	}
	cur := make(map[int64]int32, len(levels))
	for _, lv := range levels {
		cur[lv.SKUID] = lv.Available
	}
	var total int32
	for _, ln := range lines {
		owed := -net[ln.SKUID]
		if owed > ln.Qty {
			// 流水说扣得比下单数量还多：数据已经不对了，不要再按错的数加回去。
			return 0, fmt.Errorf("订单 %s sku %d 的扣减流水净值 %d 超过下单数量 %d", orderNo, ln.SKUID, owed, ln.Qty)
		}
		if owed <= 0 {
			if marker {
				if err := tx.AppendBizLog(ctx, repository.BizLogEntry{
					SKUID: ln.SKUID, StoreID: storeID, ChangeQty: 0, BizType: bizType,
					BizID: orderNo, Before: cur[ln.SKUID], After: cur[ln.SKUID],
				}); err != nil {
					return 0, err
				}
			}
			continue
		}
		after, err := tx.AddStock(ctx, ln.SKUID, storeID, owed)
		if err != nil {
			return 0, err
		}
		rec.record(storeID, ln.SKUID, after-owed, after)
		if err := tx.AppendBizLog(ctx, repository.BizLogEntry{
			SKUID: ln.SKUID, StoreID: storeID, ChangeQty: owed, BizType: bizType,
			BizID: orderNo, Before: after - owed, After: after,
		}); err != nil {
			return 0, err
		}
		if ln.PromotionID != nil {
			ok, err := tx.ReleaseActivitySold(ctx, *ln.PromotionID, ln.SKUID, owed)
			if err != nil {
				return 0, err
			}
			if !ok {
				// 放不回只出声不报错：这个 SKU 已不在活动里（卖出过的不能移除，正常路径上走不到）。
				// 报错会让补偿无限重试、关单任务进死信 —— 为了一个计数把库存也锁在这一单上。
				slog.WarnContext(ctx, "放回活动配额时受影响 0 行：这个 SKU 已不在活动里，或已售不够减",
					"order_no", orderNo, "promotion_id", *ln.PromotionID, "sku_id", ln.SKUID)
			}
		}
		total += owed
	}
	return total, nil
}
