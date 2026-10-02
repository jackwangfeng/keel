package service

// 后台的渠道管理（/admin/channel-*）：ChannelService 外面那一层权限。
//
// 读要全店范围（管理员、操作员）；写只许管理员 —— 凭据是一把能代表这家店在渠道上下单改价的钥匙，
// 启用一个销售渠道会把全店的库存与价格推出去，与「店铺设置」同一个分量（requireMerchantAdmin 的注释）。
// worker 内部的调用（凭据失效时停推送）不经过这一层：那不是哪个员工做的事。

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/keel/keel/internal/channel"
	"github.com/keel/keel/internal/repository"
	"github.com/keel/keel/internal/tenant"
)

const scopeAdminChannelBindingCreate = "admin.channel-bindings.create"

// ErrChannelBadRequest：后台写的参数不成立（422）。
var ErrChannelBadRequest = errors.New("渠道配置不成立")

// AdminChannelService 是后台渠道管理。
type AdminChannelService struct{ ch *ChannelService }

func NewAdminChannelService(ch *ChannelService) *AdminChannelService {
	return &AdminChannelService{ch: ch}
}

// Kinds 是编进来的渠道与各自的能力。
func (s *AdminChannelService) Kinds(ctx context.Context) ([]channel.Adapter, error) {
	if _, err := requireMerchantWide(ctx); err != nil {
		return nil, err
	}
	var out []channel.Adapter
	for _, k := range s.ch.reg.Kinds() {
		a, _ := s.ch.reg.Lookup(k)
		out = append(out, a)
	}
	return out, nil
}

func (s *AdminChannelService) List(ctx context.Context) ([]repository.ChannelBinding, error) {
	if _, err := requireMerchantWide(ctx); err != nil {
		return nil, err
	}
	return s.ch.ListBindings(ctx)
}

func (s *AdminChannelService) Get(ctx context.Context, id int64) (repository.ChannelBinding, error) {
	if _, err := requireMerchantWide(ctx); err != nil {
		return repository.ChannelBinding{}, err
	}
	return s.ch.GetBinding(ctx, id)
}

// Create 建一个 binding（幂等，Idempotency-Key 必填）。
func (s *AdminChannelService) Create(ctx context.Context, in ChannelBindingCreate, idemKey string) (repository.ChannelBinding, bool, error) {
	id, err := requireMerchantAdmin(ctx)
	if err != nil {
		return repository.ChannelBinding{}, false, err
	}
	if idemKey == "" {
		return repository.ChannelBinding{}, false, ErrIdempotencyKeyMissing
	}
	if err := s.ch.checkKind(in.Channel, in.Roles); err != nil {
		return repository.ChannelBinding{}, false, err
	}
	hash, err := adminRequestHash(nil, in)
	if err != nil {
		return repository.ChannelBinding{}, false, err
	}
	merchantID, err := tenant.FromContext(ctx)
	if err != nil {
		return repository.ChannelBinding{}, false, err
	}
	status := in.Status
	if status == 0 {
		status = repository.ChannelBindingDisabled
	}
	var gid string
	lost := false
	b, replayed, err := idempotentTx(ctx, s.ch.repo, repository.StaffSubject(id.StaffID), scopeAdminChannelBindingCreate, idemKey, hash,
		archivedCreated, func(tx repository.Tx) (repository.ChannelBinding, error) {
			gid, lost = "", false
			b, err := tx.CreateChannelBinding(ctx, repository.ChannelBindingInput{Channel: in.Channel,
				ExternalAccount: in.ExternalAccount, Name: in.Name, Roles: int16(in.Roles), Status: status, Config: in.Config})
			if err != nil || !b.IsActiveOutlet() {
				return b, err
			}
			gid, lost, err = s.ch.prepareMerchantMsg(ctx, tx, merchantID)
			return b, err
		})
	s.ch.finishMsg(ctx, gid, err == nil)
	if err == nil && lost {
		s.ch.resendMerchantMsg(ctx, merchantID)
	}
	return b, replayed, err
}

func (s *AdminChannelService) Update(ctx context.Context, id int64, p ChannelBindingUpdate) (repository.ChannelBinding, error) {
	if _, err := requireMerchantAdmin(ctx); err != nil {
		return repository.ChannelBinding{}, err
	}
	return s.ch.UpdateBinding(ctx, id, p)
}

func (s *AdminChannelService) SetSecrets(ctx context.Context, id int64, secrets json.RawMessage) error {
	if _, err := requireMerchantAdmin(ctx); err != nil {
		return err
	}
	if err := s.ch.SetSecrets(ctx, id, secrets); err != nil {
		if errors.Is(err, repository.ErrChannelNotFound) {
			return err
		}
		return errors.Join(ErrChannelBadRequest, err)
	}
	return nil
}

func (s *AdminChannelService) StoreLinks(ctx context.Context, bindingID int64) ([]repository.ChannelStoreLink, error) {
	if _, err := requireMerchantWide(ctx); err != nil {
		return nil, err
	}
	return s.ch.ListStoreLinks(ctx, bindingID)
}

func (s *AdminChannelService) PutStoreLink(ctx context.Context, l repository.ChannelStoreLink) (repository.ChannelStoreLink, error) {
	if _, err := requireMerchantAdmin(ctx); err != nil {
		return repository.ChannelStoreLink{}, err
	}
	if _, err := s.ch.GetBinding(ctx, l.BindingID); err != nil {
		return repository.ChannelStoreLink{}, err
	}
	if err := s.ch.UpsertStoreLink(ctx, l); err != nil {
		return repository.ChannelStoreLink{}, err
	}
	links, err := s.ch.ListStoreLinks(ctx, l.BindingID)
	if err != nil {
		return repository.ChannelStoreLink{}, err
	}
	for _, x := range links {
		if x.StoreID == l.StoreID {
			return x, nil
		}
	}
	return repository.ChannelStoreLink{}, repository.ErrChannelNotFound
}

func (s *AdminChannelService) DeleteStoreLink(ctx context.Context, bindingID, storeID int64) error {
	if _, err := requireMerchantAdmin(ctx); err != nil {
		return err
	}
	return s.ch.DeleteStoreLink(ctx, bindingID, storeID)
}

func (s *AdminChannelService) PutSKULink(ctx context.Context, l repository.ChannelItemLink) error {
	if _, err := requireMerchantAdmin(ctx); err != nil {
		return err
	}
	if _, err := s.ch.GetBinding(ctx, l.BindingID); err != nil {
		return err
	}
	var exists bool
	if err := s.ch.repo.WithTenant(ctx, func(tx repository.Tx) error {
		var e error
		exists, e = tx.ChannelSKUExists(ctx, l.KeelID)
		return e
	}); err != nil {
		return err
	}
	if !exists {
		return repository.ErrChannelRefInvalid
	}
	return s.ch.LinkSKU(ctx, l)
}

func (s *AdminChannelService) StockRules(ctx context.Context, bindingID int64) ([]repository.ChannelStockRule, error) {
	if _, err := requireMerchantWide(ctx); err != nil {
		return nil, err
	}
	return s.ch.ListStockRules(ctx, bindingID)
}

func (s *AdminChannelService) PutStockRule(ctx context.Context, r repository.ChannelStockRule) (repository.ChannelStockRule, error) {
	if _, err := requireMerchantAdmin(ctx); err != nil {
		return repository.ChannelStockRule{}, err
	}
	if r.SKUID != nil && r.StoreID == nil {
		return repository.ChannelStockRule{}, errors.Join(ErrChannelBadRequest, errors.New("SKU 级规则要同时给门店"))
	}
	if _, err := s.ch.GetBinding(ctx, r.BindingID); err != nil {
		return repository.ChannelStockRule{}, err
	}
	return s.ch.UpsertStockRule(ctx, r)
}

func (s *AdminChannelService) DeleteStockRule(ctx context.Context, bindingID, id int64) error {
	if _, err := requireMerchantAdmin(ctx); err != nil {
		return err
	}
	return s.ch.DeleteStockRule(ctx, bindingID, id)
}

func (s *AdminChannelService) PriceRules(ctx context.Context, bindingID int64) ([]repository.ChannelPriceRule, error) {
	if _, err := requireMerchantWide(ctx); err != nil {
		return nil, err
	}
	return s.ch.ListPriceRules(ctx, bindingID)
}

func (s *AdminChannelService) PutPriceRule(ctx context.Context, r repository.ChannelPriceRule) (repository.ChannelPriceRule, error) {
	if _, err := requireMerchantAdmin(ctx); err != nil {
		return repository.ChannelPriceRule{}, err
	}
	if r.FixedCents != nil && r.SKUID == nil {
		return repository.ChannelPriceRule{}, errors.Join(ErrChannelBadRequest, errors.New("固定价只能设在 SKU 级"))
	}
	if _, err := s.ch.GetBinding(ctx, r.BindingID); err != nil {
		return repository.ChannelPriceRule{}, err
	}
	return s.ch.UpsertPriceRule(ctx, r)
}

func (s *AdminChannelService) DeletePriceRule(ctx context.Context, bindingID, id int64) error {
	if _, err := requireMerchantAdmin(ctx); err != nil {
		return err
	}
	return s.ch.DeletePriceRule(ctx, bindingID, id)
}

func (s *AdminChannelService) Listings(ctx context.Context, bindingID int64, storeID *int64, limit, offset int32) ([]repository.ChannelListing, error) {
	if _, err := requireMerchantWide(ctx); err != nil {
		return nil, err
	}
	return s.ch.ListListings(ctx, bindingID, storeID, limit, offset)
}
