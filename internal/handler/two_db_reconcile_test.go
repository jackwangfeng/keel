package handler_test

// 库存对账在两库形态上（微服务拆分阶段 2，service/inventory_reconcile.go）：core 在包级测试库上，
// 库存在只跑过 db/migrations-inventory 的第二个库上，对账经 inventory.Remote 走 HTTP 问库存那一半。
//
// 对账扫的是全部活跃商家，而包级测试库里还有别的测试建的店（它们的库存与配额不在这个库存库里，
// 会被报成差异）—— 所以这里的断言一律只看这家店的明细。

import (
	"context"
	"fmt"
	"testing"

	"github.com/keel/keel/internal/repository"
	"github.com/keel/keel/internal/service"
)

func TestTwoDatabasesReconcile(t *testing.T) {
	e := newTwoDB(t)
	cs := newCouponShop(t)
	ctx := context.Background()
	t.Cleanup(func() {
		for _, tbl := range []string{"inventory_logs", "activity_stocks", "inventories"} {
			e.invAdmin.Exec(ctx, `DELETE FROM `+tbl+` WHERE merchant_id = $1`, cs.MerchantID)
		}
		adminExec(t, `DELETE FROM jobs WHERE merchant_id = $1`, cs.MerchantID)
	})
	e.moveStock(t, cs.MerchantID)
	e.use(t)
	// 上线一个秒杀：配额整组设进库存库（与 TestTwoDatabases 同一条路）。
	p := cs.livePromotion(t, "对账秒杀", fmt.Sprintf(`"promotion_type":4,`+
		`"skus":[{"sku_id":%d,"promo_price_cents":990,"stock_qty":3}]`, cs.ShirtSKU))

	// PageSize 1：库存键逐行翻页，分页的边界（键集的「严格之后」）被每一行都走一遍。
	rec := service.NewInventoryReconcileService(repository.New(testPool), e.remote,
		service.InventoryReconcileConfig{PageSize: 1, SampleSize: 100000}, nil)
	mine := func(rep service.InventoryReconcileReport) map[string][]service.ReconcileFinding {
		out := map[string][]service.ReconcileFinding{}
		for _, f := range rep.Findings {
			if f.MerchantID == cs.MerchantID {
				out[f.Kind] = append(out[f.Kind], f)
			}
		}
		return out
	}
	stockRows := e.invInt(t, `SELECT count(*) FROM inventories WHERE merchant_id = $1`, cs.MerchantID)
	if stockRows < 2 {
		t.Fatalf("夹具只有 %d 行库存，分页测不出东西", stockRows)
	}

	t.Run("两边一致时这家店没有差异", func(t *testing.T) {
		rep, err := rec.ReconcileOnce(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if got := mine(rep); len(got) != 0 {
			t.Fatalf("两边一致，却报了差异：%+v", got)
		}
		if int64(rep.StockRows) < stockRows {
			t.Fatalf("过了 %d 行库存键，这家店就有 %d 行 —— 分页漏了", rep.StockRows, stockRows)
		}
		if _, _, runs := rec.Last(); runs != 1 {
			t.Fatalf("跑完一轮 Last 记的轮数是 %d", runs)
		}
	})

	t.Run("孤儿行_软删遗留_配额差集_死信都报出来", func(t *testing.T) {
		// 1. 孤儿：库存库里有一行，core 里没有这个 SKU。
		const ghostSKU = 987654321
		if _, err := e.invAdmin.Exec(ctx, `INSERT INTO inventories (sku_id, store_id, merchant_id, available_qty, warning_qty)
			VALUES ($1, $2, $3, 5, 0)`, ghostSKU, cs.NorthStore, cs.MerchantID); err != nil {
			t.Fatal(err)
		}
		// 软删遗留：连衣裙软删之后它的库存行照旧在（预期内，只计数不告警）。
		adminExec(t, `UPDATE skus SET deleted_at = now() WHERE id = $1`, cs.DressSKU)
		dressRows := e.invInt(t, `SELECT count(*) FROM inventories WHERE merchant_id = $1 AND sku_id = $2`, cs.MerchantID, cs.DressSKU)
		// 2. 配额：衬衫的配额行被删掉（缺），另塞一行活动商品里没有的连衣裙（多）。
		if _, err := e.invAdmin.Exec(ctx, `DELETE FROM activity_stocks WHERE merchant_id = $1 AND promotion_id = $2`, cs.MerchantID, p.Id); err != nil {
			t.Fatal(err)
		}
		if _, err := e.invAdmin.Exec(ctx, `INSERT INTO activity_stocks (merchant_id, promotion_id, sku_id, quota)
			VALUES ($1, $2, $3, 1)`, cs.MerchantID, p.Id, cs.DressSKU); err != nil {
			t.Fatal(err)
		}
		// 3. 死信的库存任务（core 的 jobs）。
		adminExec(t, `INSERT INTO jobs (merchant_id, queue, job_key, status, attempts, max_attempts, last_error)
			VALUES ($1, $2, 'release:RECON-DEAD', 3, 200, 200, '库存服务不在')`, cs.MerchantID, service.QueueInventoryRelease)

		rep, err := rec.ReconcileOnce(ctx)
		if err != nil {
			t.Fatal(err)
		}
		got := mine(rep)
		if o := got[service.ReconcileStockOrphan]; len(o) != 1 || o[0].SKUID != ghostSKU || o[0].StoreID != cs.NorthStore {
			t.Errorf("孤儿库存行：%+v，期望恰好 sku=%d store=%d", o, ghostSKU, cs.NorthStore)
		}
		if rep.StockStale < int(dressRows) {
			t.Errorf("软删遗留计了 %d 行，这家店的连衣裙就有 %d 行", rep.StockStale, dressRows)
		}
		if len(got[service.ReconcileStockStale]) != 0 {
			t.Errorf("软删遗留不该进明细：%+v", got[service.ReconcileStockStale])
		}
		if m := got[service.ReconcileActivityMissing]; len(m) != 1 || m[0].PromotionID != p.Id || m[0].SKUID != cs.ShirtSKU {
			t.Errorf("缺配额行：%+v，期望恰好 promotion=%d sku=%d", m, p.Id, cs.ShirtSKU)
		}
		if x := got[service.ReconcileActivityExtra]; len(x) != 1 || x[0].PromotionID != p.Id || x[0].SKUID != cs.DressSKU {
			t.Errorf("多配额行：%+v，期望恰好 promotion=%d sku=%d", x, p.Id, cs.DressSKU)
		}
		if d := got[service.ReconcileDeadJob]; len(d) != 1 || d[0].JobKey != "release:RECON-DEAD" || d[0].Detail != "库存服务不在" {
			t.Errorf("死信：%+v", d)
		}
		if !rep.Drift() || rep.DeadJobs < 1 {
			t.Errorf("有差异却没报 Drift：%+v", rep)
		}
		// 只读：对账前后两边一行都没被改。
		if n := e.invInt(t, `SELECT count(*) FROM inventories WHERE merchant_id = $1 AND sku_id = $2`, cs.MerchantID, ghostSKU); n != 1 {
			t.Errorf("孤儿行被动过了（剩 %d 行）—— 对账只报不修", n)
		}
		if n := adminQueryInt64(t, `SELECT count(*) FROM jobs WHERE merchant_id = $1 AND status = 3`, cs.MerchantID); n != 1 {
			t.Errorf("死信被动过了（剩 %d 条）", n)
		}
	})

	t.Run("库存服务不在时记失败_不打断", func(t *testing.T) {
		e.down.Store(true)
		defer e.down.Store(false)
		rep, err := rec.ReconcileOnce(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if rep.Failed < 1 {
			t.Fatalf("库存服务不在，这一轮却没有一家记失败：%+v", rep)
		}
		// 死信那一项只读 core，照样报得出来。
		found := false
		for _, f := range mine(rep)[service.ReconcileDeadJob] {
			found = found || f.JobKey == "release:RECON-DEAD"
		}
		if !found {
			t.Fatal("库存服务不在时死信那一项也没了 —— 它只读 core，不该受影响")
		}
	})
}
