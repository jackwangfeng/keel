package handler_test

// 活动配额同步改走二阶段消息（service/promotion_quota_msg.go 发、inventory 包 activity_msg.go 收）。
//
// 单体与两库各跑一遍同一组断言：
//
//   - 新建 / 修改活动商品：活动与消息同一个事务，写接口返回时库存服务里的配额已经是定义的样子；
//   - 库存服务（单体里是接收方回源那一步）暂时不可用：活动照样保存，回来之后配额最终一致；
//   - 同一条消息投递两次：第二次被屏障挡住，结果不变；
//   - 两条消息乱序到达：结果以处理那一刻 core 的定义为准；
//   - 投递时才发现违反已售规则：钳到不变量上（配额抬到已售、卖出过的 SKU 留着）。
//
// DTMRS_RETRY_INTERVAL 调到 1 秒：dtmrs 的首次重试默认 10 秒，「回来之后最终一致」要等它。

import (
	"context"
	"fmt"
	"net/http"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/keel/keel/internal/app"
	"github.com/keel/keel/internal/dtm"
	"github.com/keel/keel/internal/inventory"
	"github.com/keel/keel/internal/repository"
	"github.com/keel/keel/internal/service"
	"github.com/keel/keel/internal/tenant"
)

type quotaMsgRig struct {
	engine  *gin.Engine
	setDown func(bool)
	deliver func(gid string) int
	invExec func(t *testing.T, sql string, args ...any)
	invInt  func(t *testing.T, sql string, args ...any) int64
}

// flakySource 是单体里「接收方回源那一步失败」的开关（单体没有一个能单独停掉的库存服务）。
type flakySource struct {
	inner inventory.QuotaSource
	down  atomic.Bool
}

func (f *flakySource) QuotaDefinition(ctx context.Context, id int64) (inventory.QuotaDefinition, error) {
	if f.down.Load() {
		return inventory.QuotaDefinition{}, fmt.Errorf("%w: 测试注入：回源不可用", inventory.ErrUnavailable)
	}
	return f.inner.QuotaDefinition(ctx, id)
}

func TestQuotaMsgMonolith(t *testing.T) {
	t.Setenv("DTMRS_RETRY_INTERVAL", "1")
	cs := newCouponShop(t)
	store := repository.NewInventoryStore(testPool)
	local := inventory.NewLocal(store)
	orders := service.NewOrderService(repository.New(testPool), local, nil, nil)
	src := &flakySource{inner: service.NewPromotionQuotaSource(repository.New(testPool))}
	q := service.NewQuotaSync(repository.New(testPool), dtm.BranchResolver{})
	branches := app.Branches(orders)
	for name, fn := range app.QuotaSyncBranches(q, local, src) {
		branches[name] = fn
	}
	tc, err := dtm.StartEx("sqlite:"+filepath.Join(t.TempDir(), "dtm.db"), 0, branches, app.InventoryBranches(local))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(tc.Close)
	orders.AttachCoordinator(tc)
	q.Attach(tc)
	engine := app.Router(testPool, tenant.NewResolver(testPool, tenant.Config{BaseDomain: baseDomain}), testSigner,
		orders, service.PaymentConfig{Sandbox: true}, conceptEmbedder{}, app.WithInventory(local), app.WithQuotaSync(q))
	useEngine(t, engine)
	recv := local.ActivitySyncBranch(src)
	exerciseQuotaMsg(t, cs, quotaMsgRig{
		engine:  engine,
		setDown: func(d bool) { src.down.Store(d) },
		deliver: func(gid string) int { return recv(gid, "01", "action") },
		invExec: func(t *testing.T, sql string, args ...any) { t.Helper(); adminExec(t, sql, args...) },
		invInt:  func(t *testing.T, sql string, args ...any) int64 { t.Helper(); return adminQueryInt64(t, sql, args...) },
	})
}

func TestQuotaMsgTwoDatabases(t *testing.T) {
	t.Setenv("DTMRS_RETRY_INTERVAL", "1")
	cs := newCouponShop(t)
	e := newTwoDB(t)
	t.Cleanup(func() {
		ctx := context.Background()
		for _, tbl := range []string{"inventory_logs", "activity_stocks", "inventories"} {
			e.invAdmin.Exec(ctx, `DELETE FROM `+tbl+` WHERE merchant_id = $1`, cs.MerchantID)
		}
	})
	e.moveStock(t, cs.MerchantID)
	e.use(t)
	defer coreUntouched(t, cs.MerchantID)
	recv := e.branchHTTP(inventory.BranchActivitySync)
	exerciseQuotaMsg(t, cs, quotaMsgRig{
		engine:  e.engine,
		setDown: func(d bool) { e.down.Store(d) },
		deliver: func(gid string) int { return recv(gid, "01", "action", "") },
		invExec: func(t *testing.T, sql string, args ...any) {
			t.Helper()
			if _, err := e.invAdmin.Exec(context.Background(), sql, args...); err != nil {
				t.Fatal(err)
			}
		},
		invInt: e.invInt,
	})
}

func exerciseQuotaMsg(t *testing.T, cs couponShop, rig quotaMsgRig) {
	sku := cs.DressSKU
	var pid int64
	quota := func(t *testing.T) int64 {
		t.Helper()
		return rig.invInt(t, `SELECT COALESCE((SELECT quota FROM activity_stocks WHERE promotion_id = $1 AND sku_id = $2), -1)`, pid, sku)
	}
	definition := func(t *testing.T) int64 {
		t.Helper()
		return adminQueryInt64(t, `SELECT COALESCE((SELECT quota_qty FROM promotion_skus WHERE promotion_id = $1 AND sku_id = $2), -1)`, pid, sku)
	}
	waitQuota := func(t *testing.T, want int64, what string) {
		t.Helper()
		deadline := time.Now().Add(30 * time.Second)
		for quota(t) != want {
			if time.Now().After(deadline) {
				t.Fatalf("%s：等了 30 秒库存服务里的配额仍是 %d，期望 %d", what, quota(t), want)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	skusJSON := func(q int) string {
		return fmt.Sprintf(`"skus":[{"sku_id":%d,"promo_price_cents":990,"stock_qty":%d}]`, sku, q)
	}
	deliver := func(t *testing.T, gid string) {
		t.Helper()
		if got := rig.deliver(gid); got != dtm.Success {
			t.Fatalf("接收分支对 %s 返回 %d", gid, got)
		}
	}
	gidOf := func(tag string) string {
		return fmt.Sprintf("%s%d-%d-%s", inventory.ActivityMsgGIDPrefix, cs.MerchantID, pid, tag)
	}

	t.Run("新建_配额随消息同步", func(t *testing.T) {
		p := cs.createPromotion(t, "消息秒杀", `"promotion_type":4,`+skusJSON(5))
		pid = p.Id
		if d := definition(t); d != 5 {
			t.Fatalf("core 里的配额定义是 %d，期望 5", d)
		}
		// 写接口等过投递（至多 2 秒），返回时库存服务里已经是 5。
		waitQuota(t, 5, "新建之后")
	})
	if pid == 0 {
		t.FailNow()
	}

	t.Run("修改活动商品_同步", func(t *testing.T) {
		wantStatus(t, cs.patchPromotion(t, pid, "{"+skusJSON(8)+"}"), http.StatusOK, "改配额")
		waitQuota(t, 8, "改成 8 之后")
	})

	t.Run("库存服务不在时照样保存_回来后最终一致", func(t *testing.T) {
		rig.setDown(true)
		w := cs.patchPromotion(t, pid, "{"+skusJSON(3)+"}")
		rig.setDown(false)
		wantStatus(t, w, http.StatusOK, "库存服务不在时改配额")
		if d := definition(t); d != 3 {
			t.Fatalf("core 里的定义是 %d，期望 3（活动应当照样保存）", d)
		}
		waitQuota(t, 3, "库存服务回来之后")
	})

	t.Run("重复投递_结果不变", func(t *testing.T) {
		gid := gidOf("00000000000000d1")
		deliver(t, gid)
		deliver(t, gid)
		if q := quota(t); q != definition(t) {
			t.Fatalf("配额 %d 与定义 %d 不一致", q, definition(t))
		}
		if n := rig.invInt(t, `SELECT count(*) FROM barrier WHERE gid = $1 AND branch_id = '01'`, gid); n != 1 {
			t.Fatalf("接收分支的屏障 %d 行，期望 1（第二次被判成重复）", n)
		}
	})

	t.Run("乱序投递_以当前定义为准", func(t *testing.T) {
		// 两次改定义（直接改库，不经写接口 —— 消息由测试手投，好控制顺序）：先 11（消息 A），再 12（消息 B）。
		adminExec(t, `UPDATE promotion_skus SET quota_qty = 11 WHERE promotion_id = $1 AND sku_id = $2`, pid, sku)
		a := gidOf("00000000000000a1")
		adminExec(t, `UPDATE promotion_skus SET quota_qty = 12 WHERE promotion_id = $1 AND sku_id = $2`, pid, sku)
		b := gidOf("00000000000000b1")
		deliver(t, b)
		if q := quota(t); q != 12 {
			t.Fatalf("B 先到之后配额 %d，期望 12", q)
		}
		deliver(t, a) // A 迟到：接收方不信它，按当前定义（12）设
		if q := quota(t); q != 12 {
			t.Fatalf("迟到的 A 把配额改成了 %d", q)
		}
	})

	t.Run("投递时违反已售规则_钳到不变量上", func(t *testing.T) {
		rig.invExec(t, `UPDATE activity_stocks SET sold = 4 WHERE promotion_id = $1 AND sku_id = $2`, pid, sku)
		defer rig.invExec(t, `UPDATE activity_stocks SET sold = 0 WHERE promotion_id = $1 AND sku_id = $2`, pid, sku)
		// 定义低于已售：抬到已售。
		adminExec(t, `UPDATE promotion_skus SET quota_qty = 2 WHERE promotion_id = $1 AND sku_id = $2`, pid, sku)
		deliver(t, gidOf("00000000000000c1"))
		if q := quota(t); q != 4 {
			t.Fatalf("定义 2 < 已售 4，配额应当抬到 4，实得 %d", q)
		}
		// 定义里去掉了卖出过的 SKU：那一行留着。
		adminExec(t, `DELETE FROM promotion_skus WHERE promotion_id = $1 AND sku_id = $2`, pid, sku)
		deliver(t, gidOf("00000000000000c2"))
		if q := quota(t); q != 4 {
			t.Fatalf("卖出过的 SKU 被移出了活动（配额 %d）", q)
		}
		// 预检：同样的改法经写接口会被 422 挡住（库存服务在，按它的已售判）。
		w := cs.patchPromotion(t, pid, fmt.Sprintf(`{"skus":[{"sku_id":%d,"promo_price_cents":990,"stock_qty":3}]}`, cs.ShirtSKU))
		wantStatus(t, w, http.StatusUnprocessableEntity, "移除卖出过的 SKU")
	})
}
