package repository_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/keel/keel/internal/dtm"
	"github.com/keel/keel/internal/inventory"
	"github.com/keel/keel/internal/repository"
	"github.com/keel/keel/internal/tenant"
)

// deductViaSaga 用下单 SAGA 真正的库存分支（inventory 包 saga.go）扣一次，返回扣减后的水位。
//
// 微服务拆分阶段 1b 之后扣减不在 core 的 Tx 上了；「新建的 SKU 扣得动」这类断言要守的正是
// 「下单那条路扣得动」，所以直接调注册在协调器上的那个分支函数，而不是另写一条扣减语句。
// 被拒（缺行、不够）时 Fatal，并把拒绝码带出来。
func deductViaSaga(t *testing.T, pool *pgxpool.Pool, merchantID, storeID, skuID int64, qty int32) int32 {
	t.Helper()
	local := inventory.NewLocal(repository.NewInventoryStore(pool))
	orderNo := fmt.Sprintf("tdeduct%d", time.Now().UnixNano())
	gid, err := dtm.OrderGID(merchantID, orderNo)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := inventory.EncodeDeductPayload(inventory.DeductPayload{
		OrderNo: orderNo, StoreID: storeID, Lines: []inventory.OrderLine{{SKUID: skuID, Qty: qty}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := local.SagaBranches()[inventory.BranchDeduct](gid, "03", "action", payload); got != dtm.Success {
		t.Fatalf("库存分支返回 %d，期望 Success", got)
	}
	trail, err := local.OrderTrail(tenant.NewContext(context.Background(), merchantID), orderNo)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range trail {
		switch e.BizType {
		case inventory.BizOrderDeduct:
			return e.After
		case inventory.BizOrderRejected:
			t.Fatalf("库存分支拒绝了扣减（%s）：sku %d 在门店 %d 可售 %d", e.Reason, skuID, storeID, e.Before)
		}
	}
	t.Fatalf("库存分支成功了，流水里却没有扣减也没有拒绝：%+v", trail)
	return 0
}
