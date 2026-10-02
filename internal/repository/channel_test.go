package repository_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/keel/keel/internal/db"
	"github.com/keel/keel/internal/repository"
	"github.com/keel/keel/internal/tenant"
)

func defaultStoreID(t *testing.T, merchantID int64) int64 {
	t.Helper()
	admin, err := pgx.Connect(context.Background(), db.AdminDSN())
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(context.Background())
	var id int64
	if err := admin.QueryRow(context.Background(),
		`SELECT id FROM stores WHERE merchant_id = $1 AND is_default`, merchantID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestChannelBindingNeverCarriesSecrets(t *testing.T) {
	idA, _ := seedTwoTenants(t)
	r := repository.New(pool(t))
	ctx := tenant.NewContext(context.Background(), idA)
	var b repository.ChannelBinding
	err := r.WithTenant(ctx, func(tx repository.Tx) error {
		var e error
		b, e = tx.CreateChannelBinding(ctx, repository.ChannelBindingInput{
			Channel: "fake", ExternalAccount: "acct-1", Name: "假渠道",
			Roles: repository.ChannelRoleOutlet, Status: repository.ChannelBindingActive,
			Config: json.RawMessage(`{"auto_accept":true}`),
		})
		if e != nil {
			return e
		}
		if e = tx.SetChannelBindingSecrets(ctx, b.ID, json.RawMessage(`{"token":"s3cr3t"}`)); e != nil {
			return e
		}
		// 只改 config，secrets 不能被带掉
		if _, e = tx.UpdateChannelBinding(ctx, b.ID, repository.ChannelBindingPatch{Config: json.RawMessage(`{"auto_accept":false}`)}); e != nil {
			return e
		}
		list, e := tx.ListChannelBindings(ctx)
		if e != nil {
			return e
		}
		raw, _ := json.Marshal(list)
		if strings.Contains(string(raw), "s3cr3t") {
			t.Errorf("列表序列化出了 secrets：%s", raw)
		}
		sec, e := tx.ChannelBindingSecrets(ctx, b.ID)
		if e != nil {
			return e
		}
		if !strings.Contains(string(sec), "s3cr3t") {
			t.Errorf("只改 config 之后 secrets 变了：%s", sec)
		}
		n, e := tx.CountActiveOutletBindings(ctx)
		if e != nil {
			return e
		}
		if n != 1 {
			t.Errorf("启用中的销售渠道 binding 数 = %d，期望 1", n)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// 违反约束的语句会让事务作废，所以单独一个事务
	err = r.WithTenant(ctx, func(tx repository.Tx) error {
		_, e := tx.CreateChannelBinding(ctx, repository.ChannelBindingInput{
			Channel: "fake", ExternalAccount: "acct-1", Name: "重复", Roles: 4, Status: 2})
		return e
	})
	if !errors.Is(err, repository.ErrChannelDuplicate) {
		t.Errorf("同渠道同账号建第二个 binding：err = %v，期望 ErrChannelDuplicate", err)
	}
}

func TestChannelBindingsAreTenantScoped(t *testing.T) {
	idA, idB := seedTwoTenants(t)
	r := repository.New(pool(t))
	ctxA := tenant.NewContext(context.Background(), idA)
	var id int64
	if err := r.WithTenant(ctxA, func(tx repository.Tx) error {
		b, e := tx.CreateChannelBinding(ctxA, repository.ChannelBindingInput{
			Channel: "fake", ExternalAccount: "a", Name: "A", Roles: 4, Status: 1})
		id = b.ID
		return e
	}); err != nil {
		t.Fatal(err)
	}
	ctxB := tenant.NewContext(context.Background(), idB)
	err := r.WithTenant(ctxB, func(tx repository.Tx) error {
		_, e := tx.GetChannelBinding(ctxB, id)
		if !errors.Is(e, repository.ErrChannelNotFound) {
			t.Errorf("B 读 A 的 binding：err = %v，期望 ErrChannelNotFound", e)
		}
		if _, e := tx.ChannelBindingSecrets(ctxB, id); !errors.Is(e, repository.ErrChannelNotFound) {
			t.Errorf("B 读 A 的 secrets：err = %v，期望 ErrChannelNotFound", e)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestChannelRulesListingsAndStoreLinks(t *testing.T) {
	idA, _ := seedTwoTenants(t)
	store := defaultStoreID(t, idA)
	r := repository.New(pool(t))
	ctx := tenant.NewContext(context.Background(), idA)
	var bindingID int64
	err := r.WithTenant(ctx, func(tx repository.Tx) error {
		b, e := tx.CreateChannelBinding(ctx, repository.ChannelBindingInput{
			Channel: "fake", ExternalAccount: "a", Name: "A", Roles: 4, Status: 1})
		if e != nil {
			return e
		}
		if e = tx.UpsertChannelStoreLink(ctx, repository.ChannelStoreLink{BindingID: b.ID, StoreID: store, ExternalStoreID: "poi-1"}); e != nil {
			return e
		}
		outs, e := tx.ListActiveOutletBindingsForStore(ctx, store)
		if e != nil {
			return e
		}
		if len(outs) != 1 || outs[0].ExternalStoreID != "poi-1" {
			t.Errorf("门店的销售渠道 = %+v，期望一条 poi-1", outs)
		}
		// 渠道级规则写两次是同一行（NULLS NOT DISTINCT）
		r1, e := tx.UpsertChannelStockRule(ctx, repository.ChannelStockRule{BindingID: b.ID, RatioBP: 8000})
		if e != nil {
			return e
		}
		r2, e := tx.UpsertChannelStockRule(ctx, repository.ChannelStockRule{BindingID: b.ID, RatioBP: 7000, SafetyQty: 2})
		if e != nil {
			return e
		}
		if r1.ID != r2.ID || r2.RatioBP != 7000 {
			t.Errorf("渠道级规则写两次：%+v / %+v，期望同一行被覆盖", r1, r2)
		}
		if _, e = tx.UpsertChannelStockRule(ctx, repository.ChannelStockRule{BindingID: b.ID, StoreID: &store, RatioBP: 5000}); e != nil {
			return e
		}
		rules, e := tx.ChannelStockRulesForStore(ctx, b.ID, store)
		if e != nil {
			return e
		}
		if len(rules) != 2 {
			t.Errorf("门店可见的规则 %d 条，期望 2（渠道级 + 门店级）", len(rules))
		}
		bindingID = b.ID
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	err = r.WithTenant(ctx, func(tx repository.Tx) error {
		return tx.UpsertChannelStoreLink(ctx, repository.ChannelStoreLink{BindingID: bindingID, StoreID: 999999999, ExternalStoreID: "x"})
	})
	if !errors.Is(err, repository.ErrChannelRefInvalid) {
		t.Errorf("映射到不存在的门店：err = %v，期望 ErrChannelRefInvalid", err)
	}
}

func TestChannelInboundEventDedupesConcurrentDeliveries(t *testing.T) {
	idA, _ := seedTwoTenants(t)
	r := repository.New(pool(t))
	ctx := tenant.NewContext(context.Background(), idA)
	var bindingID int64
	if err := r.WithTenant(ctx, func(tx repository.Tx) error {
		b, e := tx.CreateChannelBinding(ctx, repository.ChannelBindingInput{
			Channel: "fake", ExternalAccount: "a", Name: "A", Roles: 4, Status: 1})
		bindingID = b.ID
		return e
	}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	inserted := 0
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := r.WithTenant(ctx, func(tx repository.Tx) error {
				_, ok, e := tx.InsertChannelInboundEvent(ctx, repository.ChannelInboundEventInput{
					BindingID: bindingID, ExternalEventID: "evt-1", Topic: "orders/create", Payload: json.RawMessage(`{}`)})
				if ok {
					mu.Lock()
					inserted++
					mu.Unlock()
				}
				return e
			})
			if err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if inserted != 1 {
		t.Errorf("同一外部事件并发投递两次，插入了 %d 行，期望 1", inserted)
	}
}

func TestChannelMerchantFlagInInventoryStore(t *testing.T) {
	idA, _ := seedTwoTenants(t)
	s := repository.NewInventoryStore(pool(t))
	ctx := tenant.NewContext(context.Background(), idA)
	set := func(on bool) {
		if err := s.WithTenant(ctx, func(tx repository.InventoryStoreTx) error { return tx.SetChannelMerchant(ctx, on) }); err != nil {
			t.Fatal(err)
		}
	}
	get := func() bool {
		var on bool
		if err := s.WithTenant(ctx, func(tx repository.InventoryStoreTx) error {
			var e error
			on, e = tx.ChannelMerchantEnabled(ctx)
			return e
		}); err != nil {
			t.Fatal(err)
		}
		return on
	}
	if get() {
		t.Fatal("新商家默认就开了渠道")
	}
	set(true)
	set(true) // 幂等
	if !get() {
		t.Error("登记之后仍是未开")
	}
	set(false)
	if get() {
		t.Error("撤销之后仍是开")
	}
}
