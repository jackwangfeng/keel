package service

// 渠道单的接单 SAGA（第三期）：一张已在平台付过款的单，在 keel 建成一张已支付的订单并扣同一份库存。
//
//	01 channel_order_open     core   0 → 10（补偿 channel_order_open_undo：关到 90、渠道单标异常、AcceptRequired 入队拒单）
//	02 inventory_deduct       库存   与自营下单同一个分支（补偿 inventory_restore，按流水放回；被拒时流水里没有扣减，放回是空操作）
//	03 channel_order_finish   core   扣减被拒 → Failure 触发全局补偿；否则 10 → 20（paid = payable、paid_at = 平台下单时间）、
//	                                 渠道单 → 已接单、推送基线扣掉这单的数量、AcceptRequired 入队接单、门店收「新订单待发货」
//
// 与自营下单 SAGA（order_saga.go）同一套规矩：分支只拿到 (gid, branch_id, op)，租户与订单号从 gid 来
// （dtm.TenantContextFromGID），其余从库里读（订单行、orders.channel_order_id → 渠道单 → binding → Caps）；
// 业务失败返回 Failure，说不清楚的失败返回 Unknown 让协调器重试、由屏障挡重复。没有 0 → 20 的边：
// 01 走 0 → 10、03 走 10 → 20，各自过触发器。
//
// # 为什么推送基线在 03 里扣（Review Focus 3）
//
// 平台卖出时自己先减了平台上的数（Shopify：A=7 卖 2 → 5）。keel 扣库存后推送，CAS 的 changeFromQuantity
// 若还是旧基线 7，每一张平台单都会撞一次冲突、记一条差异。接单成功的同一个事务里把这个 binding 在
// （门店, SKU）上的 published_qty 减去这单的数量（下限 0），推送就是一次正常的 CAS。
// 已知的窄窗口：扣减后的 stock.changed 若在 03 提交前就被推送 worker 处理，那一次推送仍会冲突一次
// （keel 是权威，冲突后按 keel 的值重推，自愈）。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/keel/keel/internal/channel"
	"github.com/keel/keel/internal/dtm"
	"github.com/keel/keel/internal/inventory"
	"github.com/keel/keel/internal/repository"
)

const (
	BranchChannelOrderOpen     = "channel_order_open"
	BranchChannelOrderOpenUndo = "channel_order_open_undo"
	BranchChannelOrderFinish   = "channel_order_finish"
	// BranchChannelOrderFinishUndo 是收尾分支的空补偿（dtmrs 要求每一步都有 compensate 地址，同 order_finish_undo）。
	BranchChannelOrderFinishUndo = "channel_order_finish_undo"
)

// errChannelSagaFailed：确定性失败（订单状态不对、渠道单已取消、扣减被拒）。重试改变不了，触发全局补偿。
var errChannelSagaFailed = errors.New("渠道单接单失败")

// OrderBranches 是接单 SAGA 的 core 分支（键是 local:// 后面的名字）。只在 KEEL_CHANNELS 开着时注册（app/channels.go）。
func (s *ChannelService) OrderBranches() map[string]dtm.BranchFunc {
	return map[string]dtm.BranchFunc{
		BranchChannelOrderOpen:       s.channelOrderOpenBranch(),
		BranchChannelOrderOpenUndo:   s.channelOrderOpenUndoBranch(),
		BranchChannelOrderFinish:     s.channelOrderFinishBranch(),
		BranchChannelOrderFinishUndo: func(string, string, string) int { return dtm.Success },
	}
}

// channelSagaSteps 是编排（建单在前、库存在后，理由同 order_saga.go 的 sagaStepsFor）。
func (s *ChannelService) channelSagaSteps(orderNo string, storeID int64, lines []inventory.OrderLine) (string, error) {
	payload, err := inventory.EncodeDeductPayload(inventory.DeductPayload{OrderNo: orderNo, StoreID: storeID, Lines: lines})
	if err != nil {
		return "", err
	}
	return dtm.StepsJSON(
		dtm.Step{Action: s.self.BranchURL(BranchChannelOrderOpen), Compensate: s.self.BranchURL(BranchChannelOrderOpenUndo)},
		dtm.Step{Action: s.res.BranchURL(inventory.BranchDeduct), Compensate: s.res.BranchURL(inventory.BranchRestore),
			Payload: payload},
		dtm.Step{Action: s.self.BranchURL(BranchChannelOrderFinish), Compensate: s.self.BranchURL(BranchChannelOrderFinishUndo)},
	)
}

// sagaResult 把分支错误分成 Failure（确定性）与 Unknown（重试）。
func sagaResult(log *slog.Logger, err error) int {
	if errors.Is(err, errChannelSagaFailed) || errors.Is(err, repository.ErrOrderNotFound) {
		log.Warn("接单 SAGA 分支确定性失败，触发全局补偿", "err", err)
		return dtm.Failure
	}
	log.Error("接单 SAGA 分支失败，按 Unknown 上报以便协调器重试", "err", err)
	return dtm.Unknown
}

// sagaEnv 解析 gid、核对 op。ok 为假时 code 是要返回的结果。
func (s *ChannelService) sagaEnv(name, wantOp, gid, branchID, op string) (ctx context.Context, orderNo string, log *slog.Logger, code int, ok bool) {
	log = s.log.With("gid", gid, "branch_id", branchID, "op", op, "branch", name)
	if op != wantOp {
		log.Error("分支收到的 op 与它的角色不符，编排里的 action/compensate 写反了？", "want_op", wantOp)
		return nil, "", log, dtm.Unknown, false
	}
	// 租户只从 gid 来（tenant_context_test.go 钉着）。
	ctx, merchantID, orderNo, err := dtm.TenantContextFromGID(context.Background(), gid)
	if err != nil {
		log.Error("分支拿到的 gid 解析不出租户，拒绝执行", "err", err)
		return nil, "", log, dtm.Failure, false
	}
	return ctx, orderNo, log.With("merchant_id", merchantID, "order_no", orderNo), 0, true
}

// channelOrderOpenBranch 是 01 的正向：0 → 10。
func (s *ChannelService) channelOrderOpenBranch() dtm.BranchFunc {
	return func(gid, branchID, op string) int {
		ctx, orderNo, log, code, ok := s.sagaEnv(BranchChannelOrderOpen, opAction, gid, branchID, op)
		if !ok {
			return code
		}
		_, err := s.repo.WithSagaBranch(ctx, gid, branchID, op, func(tx repository.Tx) error {
			order, err := tx.FindOrderByNo(ctx, orderNo)
			if err != nil {
				return err
			}
			if order.Source != repository.OrderSourceChannel {
				return fmt.Errorf("%w：订单 %s 不是渠道单", errChannelSagaFailed, orderNo)
			}
			n, err := tx.PromoteOrderDraft(ctx, orderNo)
			if err != nil {
				return err
			}
			if n != 1 {
				return fmt.Errorf("%w：订单 %s 不在创建中（%d）", errChannelSagaFailed, orderNo, order.Status)
			}
			return nil
		})
		if err != nil {
			return sagaResult(log, err)
		}
		return dtm.Success
	}
}

// channelOrderOpenUndoBranch 是 01 的补偿：订单关到 90（不回补：库存分支自己按流水放回，被拒时没扣过）；
// 渠道单清掉 order_no（下次重试建新单号，这张 90 的留痕）；渠道单没被平台取消时标异常、发员工通知、
// AcceptRequired 的渠道入队拒单。
func (s *ChannelService) channelOrderOpenUndoBranch() dtm.BranchFunc {
	return func(gid, branchID, op string) int {
		ctx, orderNo, log, code, ok := s.sagaEnv(BranchChannelOrderOpenUndo, opCompensate, gid, branchID, op)
		if !ok {
			return code
		}
		// 缺货的原因在库存流水里；问库存服务在屏障事务之外（同 order_saga.go 的 finishBranch）。
		trail, err := s.inv.OrderTrail(ctx, orderNo)
		if err != nil {
			log.Warn("补偿分支问不到库存流水，按 Unknown 上报以便协调器重试", "err", err)
			return dtm.Unknown
		}
		_, err = s.repo.WithSagaBranch(ctx, gid, branchID, op, func(tx repository.Tx) error {
			order, err := tx.FindOrderByNo(ctx, orderNo)
			if err != nil {
				return err
			}
			n, err := tx.CloseOrder(ctx, orderNo)
			if err != nil {
				return err
			}
			if n == 0 && order.Status != orderStatusClosed {
				log.Error("接单补偿关不掉这一单，它已经是 20 或更远 —— 收尾分支成功之后不该再有补偿", "status", order.Status)
			}
			if order.ChannelOrderID == nil {
				return nil
			}
			co, err := tx.LockChannelOrder(ctx, *order.ChannelOrderID)
			if err != nil {
				return err
			}
			if co.OrderNo == nil || *co.OrderNo != orderNo {
				return nil // 渠道单已经换了一张 keel 订单（重放）
			}
			st := repository.ChannelOrderState{Status: co.Status, OrderNo: nil, Exception: co.Exception,
				AcceptDeadline: co.AcceptDeadline}
			gone := co.Status == repository.ChannelOrderCancelled || co.Status == repository.ChannelOrderRejected
			var reason string
			if !gone {
				lines, err := tx.ListOrderLines(ctx, order.ID)
				if err != nil {
					return err
				}
				want, ids := map[int64]int32{}, make([]int64, 0, len(lines))
				for _, l := range lines {
					want[l.SKUID] += l.Quantity
					ids = append(ids, l.SKUID)
				}
				skus, err := tx.ChannelOrderSKUs(ctx, ids)
				if err != nil {
					return err
				}
				titles := map[int64]string{}
				for id, sku := range skus {
					titles[id] = sku.Title
				}
				if reason = stockoutReason(trail, want, titles); reason == "" {
					reason = "接单没成功（keel 订单没建成，详见服务端日志）"
				}
				st.Exception = &reason
			}
			if err := tx.SetChannelOrderState(ctx, co.ID, st); err != nil {
				return err
			}
			if gone {
				return nil
			}
			if err := notifyChannelOrderException(ctx, tx, order, reason); err != nil {
				return err
			}
			if s.capsOf(ctx, tx, co.BindingID).AcceptRequired {
				return enqueueChannelAction(ctx, tx, co.ID, channel.Action{Kind: channel.ActReject, Reason: reason})
			}
			return nil
		})
		if err != nil {
			return sagaResult(log, err)
		}
		return dtm.Success
	}
}

// channelOrderFinishBranch 是 03（只有正向，见文件头）。
func (s *ChannelService) channelOrderFinishBranch() dtm.BranchFunc {
	return func(gid, branchID, op string) int {
		ctx, orderNo, log, code, ok := s.sagaEnv(BranchChannelOrderFinish, opAction, gid, branchID, op)
		if !ok {
			return code
		}
		trail, err := s.inv.OrderTrail(ctx, orderNo)
		if err != nil {
			log.Warn("收尾分支问不到库存流水，按 Unknown 上报以便协调器重试", "err", err)
			return dtm.Unknown
		}
		if rej := rejectionOf(trail); rej != nil {
			log.Info("渠道单扣不了库存，触发全局补偿", "err", rej)
			return dtm.Failure
		}
		deducted := false
		for _, e := range trail {
			deducted = deducted || e.BizType == inventory.BizOrderDeduct
		}
		if !deducted {
			log.Error("库存流水里既没有扣减也没有拒绝 —— 不能当成扣过了")
			return dtm.Failure
		}
		_, err = s.repo.WithSagaBranch(ctx, gid, branchID, op, func(tx repository.Tx) error {
			order, err := tx.FindOrderByNo(ctx, orderNo)
			if err != nil {
				return err
			}
			st, err := tx.LockOrderStatus(ctx, order.ID)
			if err != nil {
				return err
			}
			if st != orderStatusPending || order.ChannelOrderID == nil {
				return fmt.Errorf("%w：订单 %s 是 %d，扣下的库存要放回", errChannelSagaFailed, orderNo, st)
			}
			co, err := tx.LockChannelOrder(ctx, *order.ChannelOrderID)
			if err != nil {
				return err
			}
			if co.Status == repository.ChannelOrderCancelled || co.Status == repository.ChannelOrderRejected ||
				co.OrderNo == nil || *co.OrderNo != orderNo {
				return fmt.Errorf("%w：渠道单 %d 在接单途中被取消了，扣下的库存要放回", errChannelSagaFailed, co.ID)
			}
			if err := tx.MarkOrderPlaced(ctx, order.ID); err != nil {
				return err
			}
			if err := tx.SettleOrder(ctx, orderNo, order.PayableCents, placedAtOf(co)); err != nil {
				return err
			}
			if co.Status == repository.ChannelOrderNew {
				co.Status = repository.ChannelOrderAccepted
			}
			if err := tx.SetChannelOrderState(ctx, co.ID, repository.ChannelOrderState{Status: co.Status,
				OrderNo: co.OrderNo, Exception: nil, AcceptDeadline: co.AcceptDeadline}); err != nil {
				return err
			}
			lines, err := tx.ListOrderLines(ctx, order.ID)
			if err != nil {
				return err
			}
			for _, l := range lines {
				if err := tx.DecrementChannelListingBaseline(ctx, co.BindingID, order.StoreID, l.SKUID, l.Quantity); err != nil {
					return err
				}
			}
			if s.capsOf(ctx, tx, co.BindingID).AcceptRequired {
				if err := enqueueChannelAction(ctx, tx, co.ID, channel.Action{Kind: channel.ActAccept}); err != nil {
					return err
				}
			}
			// 买家侧跳过（渠道单无 keel 买家，平台自己通知顾客）；门店收「新订单待发货」。
			order.Status = 20
			if err := notifyOrderPaid(ctx, tx, order); err != nil {
				return err
			}
			for _, e := range trail {
				if e.BizType != inventory.BizOrderDeduct {
					continue
				}
				if err := notifyLowStockIfCrossed(ctx, tx, order, e); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			return sagaResult(log, err)
		}
		return dtm.Success
	}
}

// placedAtOf 是平台下单时间（last_payload 里规整后的 PlacedAt）；读不出时用此刻。
func placedAtOf(co repository.ChannelOrder) time.Time {
	var o struct{ PlacedAt time.Time }
	if json.Unmarshal(co.LastPayload, &o) == nil && !o.PlacedAt.IsZero() {
		return o.PlacedAt
	}
	return time.Now()
}

// capsOf 是 binding 的适配器能力；binding 读不出或适配器没编进来时为零值（不需要接单）。
func (s *ChannelService) capsOf(ctx context.Context, tx repository.Tx, bindingID int64) channel.Caps {
	b, err := tx.GetChannelBinding(ctx, bindingID)
	if err != nil {
		return channel.Caps{}
	}
	a, ok := s.reg.Lookup(b.Channel)
	if !ok {
		return channel.Caps{}
	}
	return a.Caps()
}
