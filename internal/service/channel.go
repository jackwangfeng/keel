package service

// 渠道适配层的 core 编排：binding 的生命周期、规则与映射的增删改、「这家商家开没开渠道」的二阶段消息。
// 设计见 docs/superpowers/specs/2026-10-02-channel-adapter-design.md；对外可售数的计算与推送在
// channel_listing.go，回调入口在 channel_inbound.go，任务循环在 channel_worker.go。
//
// KEEL_CHANNELS 关闭时 app 根本不建这个服务（internal/app/channels.go），没有任何路由、分支、任务指向它。
//
// # 开关渠道的消息
//
// 库存服务只给「开了渠道的商家」发 stock.changed（inventory/channel_gate.go）。哪些商家开了，由这里在
// binding 的「启用中的销售渠道」状态翻转的**同一个本地事务里**登记一条二阶段消息告诉库存服务
// （接收方 inventory/channel_msg.go）。载荷是这个事务看到的「这家店还有没有启用中的销售渠道」加一个版本
// （数据库时钟，ChannelSyncRev）；库存库只接受更新的版本。两个事务并发停用最后两个 binding 时，两边都可能
// 看到「还剩一个」而发出「开」——方向是多发 stock.changed（core 收到后找不到 binding，什么都不做），不是漏发。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync/atomic"

	"github.com/keel/keel/internal/channel"
	"github.com/keel/keel/internal/dtm"
	"github.com/keel/keel/internal/inventory"
	"github.com/keel/keel/internal/repository"
	"github.com/keel/keel/internal/tenant"
)

// BranchChannelMerchantQuery 是开关渠道消息的回查分支（永远在 core 这一侧：屏障记在业务库）。
const BranchChannelMerchantQuery = "channel_merchant_msg_query"

const channelMsgGraceSecs = 30

// 渠道层的任务队列（jobs.queue）。
const (
	QueueChannelListingPush      = "channel.listing.push"
	QueueChannelListingRecompute = "channel.listing.recompute"
	QueueChannelInbound          = "channel.inbound"
)

var (
	// ErrChannelUnknownKind：binding 指向一个这个进程没编进来的渠道。
	ErrChannelUnknownKind = errors.New("没有这个渠道的适配器")
	// ErrChannelRoleUnsupported：binding 要的角色这个适配器当不了。
	ErrChannelRoleUnsupported = errors.New("这个渠道当不了所选的角色")
)

// ChannelService 是渠道层的 core 编排。零值不可用。
type ChannelService struct {
	repo *repository.Repo
	inv  inventory.Service
	reg  *channel.Registry
	log  *slog.Logger

	msgAction string // 库存服务的接收分支（单体 local://，拆分 http://）
	msgQuery  string // 本服务的回查分支
	tc        atomic.Value

	workerID string
}

type channelCoord struct{ c dtm.Coordinator }

// NewChannelService 建服务。res 解析库存服务的分支地址，self 解析本服务自己的（同 NewQuotaSync）。
func NewChannelService(repo *repository.Repo, inv inventory.Service, reg *channel.Registry, res, self dtm.BranchResolver) *ChannelService {
	return &ChannelService{repo: repo, inv: inv, reg: reg, log: slog.Default().With("component", "channel"),
		msgAction: res.BranchURL(inventory.BranchChannelMerchantSync),
		msgQuery:  self.BranchURL(BranchChannelMerchantQuery),
		workerID:  channelWorkerID()}
}

// Attach 接上已经启动的协调器（回查分支要在 Start 之前注册，同 QuotaSync）。
func (s *ChannelService) Attach(tc dtm.Coordinator) { s.tc.Store(channelCoord{tc}) }

func (s *ChannelService) coord() dtm.Coordinator {
	b, _ := s.tc.Load().(channelCoord)
	return b.c
}

// Registry 是这个服务用的适配器注册表。
func (s *ChannelService) Registry() *channel.Registry { return s.reg }

// MerchantQueryBranch 是开关渠道消息的回查分支：启停 binding 的本地事务提交了没有。
func (s *ChannelService) MerchantQueryBranch() dtm.BranchFunc {
	return func(gid, branchID, op string) int {
		ctx, _, err := dtm.TenantContextFromTenantGID(context.Background(), inventory.ChannelMerchantMsgGIDPrefix, gid)
		if err != nil {
			s.log.Error("开关渠道消息的回查拿到的 gid 不成立，按未提交作废", "gid", gid, "err", err)
			return dtm.Failure
		}
		committed, err := s.repo.QueryPreparedMsg(ctx, gid)
		if err != nil {
			return dtm.Unknown
		}
		if !committed {
			return dtm.Failure
		}
		return dtm.Success
	}
}

// ChannelBindingCreate 是建 binding 的入参。Status 为 0 时建成停用（先配凭据、门店映射，再启用）。
type ChannelBindingCreate struct {
	Channel, ExternalAccount, Name string
	Roles                          channel.Role
	Status                         int16
	Config                         json.RawMessage
}

func (s *ChannelService) checkKind(kind string, roles channel.Role) error {
	a, ok := s.reg.Lookup(kind)
	if !ok {
		return fmt.Errorf("%w：%q", ErrChannelUnknownKind, kind)
	}
	if roles&^a.Caps().Roles != 0 {
		return fmt.Errorf("%w：%s 只能当 %d，要的是 %d", ErrChannelRoleUnsupported, kind, a.Caps().Roles, roles)
	}
	return nil
}

// CreateBinding 建一个 binding。建成即启用的销售渠道会登记开关渠道消息，并给已映射的门店（新建时没有）排重算。
func (s *ChannelService) CreateBinding(ctx context.Context, in ChannelBindingCreate) (repository.ChannelBinding, error) {
	if err := s.checkKind(in.Channel, in.Roles); err != nil {
		return repository.ChannelBinding{}, err
	}
	status := in.Status
	if status == 0 {
		status = repository.ChannelBindingDisabled
	}
	var b repository.ChannelBinding
	err := s.withMerchantSync(ctx, func(tx repository.Tx) (bool, error) {
		var e error
		b, e = tx.CreateChannelBinding(ctx, repository.ChannelBindingInput{Channel: in.Channel,
			ExternalAccount: in.ExternalAccount, Name: in.Name, Roles: int16(in.Roles), Status: status, Config: in.Config})
		return b.IsActiveOutlet(), e
	})
	return b, err
}

// ChannelBindingUpdate 里为 nil 的字段不改。
type ChannelBindingUpdate struct {
	Name   *string
	Roles  *channel.Role
	Status *int16
	Config json.RawMessage
}

// UpdateBinding 改 binding。「启用中的销售渠道」状态翻转时登记开关渠道消息；变成启用时给它映射的每家门店排一次整店重算。
func (s *ChannelService) UpdateBinding(ctx context.Context, id int64, p ChannelBindingUpdate) (repository.ChannelBinding, error) {
	var b repository.ChannelBinding
	err := s.withMerchantSync(ctx, func(tx repository.Tx) (bool, error) {
		before, err := tx.GetChannelBinding(ctx, id)
		if err != nil {
			return false, err
		}
		roles := channel.Role(before.Roles)
		if p.Roles != nil {
			roles = *p.Roles
		}
		if err := s.checkKind(before.Channel, roles); err != nil {
			return false, err
		}
		patch := repository.ChannelBindingPatch{Name: p.Name, Status: p.Status, Config: p.Config}
		if p.Roles != nil {
			r := int16(*p.Roles)
			patch.Roles = &r
		}
		if b, err = tx.UpdateChannelBinding(ctx, id, patch); err != nil {
			return false, err
		}
		flipped := before.IsActiveOutlet() != b.IsActiveOutlet()
		if flipped && b.IsActiveOutlet() {
			if err := s.enqueueRecomputeBinding(ctx, tx, b.ID); err != nil {
				return false, err
			}
		}
		return flipped, nil
	})
	return b, err
}

// SetSecrets 整体替换 binding 的凭据。
func (s *ChannelService) SetSecrets(ctx context.Context, id int64, secrets json.RawMessage) error {
	if !json.Valid(secrets) || len(secrets) == 0 || secrets[0] != '{' {
		return fmt.Errorf("凭据必须是 JSON 对象")
	}
	return s.repo.WithTenant(ctx, func(tx repository.Tx) error { return tx.SetChannelBindingSecrets(ctx, id, secrets) })
}

func (s *ChannelService) GetBinding(ctx context.Context, id int64) (b repository.ChannelBinding, err error) {
	err = s.repo.WithTenant(ctx, func(tx repository.Tx) error { b, err = tx.GetChannelBinding(ctx, id); return err })
	return b, err
}

func (s *ChannelService) ListBindings(ctx context.Context) (out []repository.ChannelBinding, err error) {
	err = s.repo.WithTenant(ctx, func(tx repository.Tx) error { out, err = tx.ListChannelBindings(ctx); return err })
	return out, err
}

// withMerchantSync 跑一个改 binding 的事务；fn 返回 true 表示「启用中的销售渠道」状态翻转了，
// 于是在同一个事务里登记开关渠道消息，提交之后 submit。
func (s *ChannelService) withMerchantSync(ctx context.Context, fn func(tx repository.Tx) (bool, error)) error {
	merchantID, err := tenant.FromContext(ctx)
	if err != nil {
		return err
	}
	var gid string
	err = s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		gid = ""
		flipped, err := fn(tx)
		if err != nil || !flipped {
			return err
		}
		gid, err = s.prepareMerchantMsg(ctx, tx, merchantID)
		return err
	})
	s.finishMsg(ctx, gid, err == nil)
	return err
}

func (s *ChannelService) prepareMerchantMsg(ctx context.Context, tx repository.Tx, merchantID int64) (string, error) {
	tc := s.coord()
	if tc == nil {
		// 只有部分测试的装配会走到：app 里协调器总是先接上再开始服务请求。
		s.log.WarnContext(ctx, "协调器还没接上，这次不通知库存服务开关渠道")
		return "", nil
	}
	n, err := tx.CountActiveOutletBindings(ctx)
	if err != nil {
		return "", err
	}
	rev, err := tx.ChannelSyncRev(ctx)
	if err != nil {
		return "", err
	}
	gid, err := inventory.ChannelMerchantMsgGID(merchantID)
	if err != nil {
		return "", err
	}
	payload, _ := json.Marshal(inventory.ChannelMerchantPayload{Enabled: n > 0, Rev: rev})
	if err := tc.PrepareMsgEx(gid, []string{s.msgAction}, []string{string(payload)}, s.msgQuery, channelMsgGraceSecs, false); err != nil {
		return "", err
	}
	ok, err := tx.MarkMsgPrepared(ctx, gid)
	if err != nil {
		_ = tc.AbortMsg(gid)
		return "", err
	}
	if !ok {
		// 回查抢先判了「没提交」：这一条作废。状态以库为准，下一次启停会带着完整状态再发；
		// 在那之前库存服务可能少发 / 多发 stock.changed，对账会报出差异。
		s.log.ErrorContext(ctx, "开关渠道消息被回查抢先作废", "gid", gid)
		return "", nil
	}
	return gid, nil
}

func (s *ChannelService) finishMsg(ctx context.Context, gid string, committed bool) {
	tc := s.coord()
	if gid == "" || tc == nil {
		return
	}
	if !committed {
		_ = tc.AbortMsg(gid)
		return
	}
	if err := tc.SubmitMsg(gid); err != nil {
		s.log.WarnContext(ctx, "开关渠道消息提交失败，回查会接着投递", "gid", gid, "err", err)
	}
}

// —— 门店映射、规则（后台接口直接用）

func (s *ChannelService) UpsertStoreLink(ctx context.Context, l repository.ChannelStoreLink) error {
	return s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		if err := tx.UpsertChannelStoreLink(ctx, l); err != nil {
			return err
		}
		return s.enqueueRecompute(ctx, tx, l.BindingID, l.StoreID)
	})
}

func (s *ChannelService) DeleteStoreLink(ctx context.Context, bindingID, storeID int64) error {
	return s.repo.WithTenant(ctx, func(tx repository.Tx) error { return tx.DeleteChannelStoreLink(ctx, bindingID, storeID) })
}

func (s *ChannelService) ListStoreLinks(ctx context.Context, bindingID int64) (out []repository.ChannelStoreLink, err error) {
	err = s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		if _, err = tx.GetChannelBinding(ctx, bindingID); err != nil {
			return err
		}
		out, err = tx.ListChannelStoreLinks(ctx, bindingID)
		return err
	})
	return out, err
}

func (s *ChannelService) UpsertStockRule(ctx context.Context, r repository.ChannelStockRule) (out repository.ChannelStockRule, err error) {
	err = s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		if out, err = tx.UpsertChannelStockRule(ctx, r); err != nil {
			return err
		}
		return s.enqueueRecomputeBinding(ctx, tx, r.BindingID)
	})
	return out, err
}

func (s *ChannelService) DeleteStockRule(ctx context.Context, bindingID, id int64) error {
	return s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		if err := tx.DeleteChannelStockRule(ctx, bindingID, id); err != nil {
			return err
		}
		return s.enqueueRecomputeBinding(ctx, tx, bindingID)
	})
}

func (s *ChannelService) ListStockRules(ctx context.Context, bindingID int64) (out []repository.ChannelStockRule, err error) {
	err = s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		if _, err = tx.GetChannelBinding(ctx, bindingID); err != nil {
			return err
		}
		out, err = tx.ListChannelStockRules(ctx, bindingID)
		return err
	})
	return out, err
}

func (s *ChannelService) UpsertPriceRule(ctx context.Context, r repository.ChannelPriceRule) (out repository.ChannelPriceRule, err error) {
	err = s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		if out, err = tx.UpsertChannelPriceRule(ctx, r); err != nil {
			return err
		}
		return s.enqueueRecomputeBinding(ctx, tx, r.BindingID)
	})
	return out, err
}

func (s *ChannelService) DeletePriceRule(ctx context.Context, bindingID, id int64) error {
	return s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		if err := tx.DeleteChannelPriceRule(ctx, bindingID, id); err != nil {
			return err
		}
		return s.enqueueRecomputeBinding(ctx, tx, bindingID)
	})
}

func (s *ChannelService) ListPriceRules(ctx context.Context, bindingID int64) (out []repository.ChannelPriceRule, err error) {
	err = s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		if _, err = tx.GetChannelBinding(ctx, bindingID); err != nil {
			return err
		}
		out, err = tx.ListChannelPriceRules(ctx, bindingID)
		return err
	})
	return out, err
}

func (s *ChannelService) ListListings(ctx context.Context, bindingID int64, storeID *int64, limit, offset int32) (out []repository.ChannelListing, err error) {
	err = s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		if _, err = tx.GetChannelBinding(ctx, bindingID); err != nil {
			return err
		}
		out, err = tx.ListChannelListingsPage(ctx, bindingID, storeID, limit, offset)
		return err
	})
	return out, err
}

// LinkSKU 登记一个 SKU 在渠道上的外部 ID（第二期由商品同步写；后台与测试也可以直接写）。
func (s *ChannelService) LinkSKU(ctx context.Context, l repository.ChannelItemLink) error {
	l.Kind = repository.ChannelItemSKU
	return s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		if err := tx.UpsertChannelItemLink(ctx, l); err != nil {
			return err
		}
		links, err := tx.ListChannelStoreLinks(ctx, l.BindingID)
		if err != nil {
			return err
		}
		for _, sl := range links {
			if err := s.enqueueListingPush(ctx, tx, l.BindingID, sl.StoreID, l.KeelID); err != nil {
				return err
			}
		}
		return nil
	})
}

// adapterBinding 读出适配器要的 binding（含 secrets）。
func adapterBinding(ctx context.Context, tx repository.Tx, merchantID int64, b repository.ChannelBinding) (channel.Binding, error) {
	sec, err := tx.ChannelBindingSecrets(ctx, b.ID)
	if err != nil {
		return channel.Binding{}, err
	}
	return channel.Binding{ID: b.ID, MerchantID: merchantID, Kind: b.Channel, ExternalAccount: b.ExternalAccount,
		Roles: channel.Role(b.Roles), Config: b.Config, Secrets: sec}, nil
}

func channelWorkerID() string {
	host, _ := os.Hostname()
	return fmt.Sprintf("channel@%s:%d", host, os.Getpid())
}
