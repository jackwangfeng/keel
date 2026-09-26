package repository_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/keel/keel/internal/db"
	"github.com/keel/keel/internal/repository"
	"github.com/keel/keel/internal/tenant"
)

// 00023 那次 schema 决定的**执行者**。
//
// ===========================================================================
// 它守的是哪一句
// ===========================================================================
//
// idempotency_keys 的主键原先是 (scope, user_id, idem_key)。后台那 5 条 POST
// 要往 user_id 里放 staff.id，而 staff.id 与 users.id 来自同一种自增序列
// （数据模型 §14）—— 7 号员工与 7 号买家会落在同一行上，后果是其中一个人
// 拿到另一个人的存档响应，**而它不报错**。00023 因此把主体列换成了
// (subject_kind, subject_id)，Go 侧的对应物是 repository.IdempotencySubject。
//
// 在这条测试之前，那个决定在仓库里**没有任何行为执行者**：
//
//	· internal/db 的迁移测试只对账 db/tenancy.json 里那条 unique_global_ok
//	  登记与库里的索引定义 —— 把 subject_kind 从主键里拿掉，只要顺手改一下
//	  那行登记，它就是绿的；
//	· handler 那两条幂等测试（TestAdminWritesAreIdempotent 与买家那条）
//	  各自只用一个身份域，**两个域永远不在同一条测试里出现**，
//	  所以「两个域共用一个键空间」这件事它们一个字都断言不到。
//
// 变异验证（本轮实跑）：把 repository.StaffSubject 的 kind 改成
// idempotencySubjectUser（也就是退回「把 staff_id 当成 user_id 用」），
// go build 全绿、go vet 全绿、handler 那两条幂等测试全绿，**只有这一条红**。
//
// ===========================================================================
// 为什么写在 repository 这一层，而不是 handler
// ===========================================================================
//
// 因为要撞的是「同一个 scope + 同一个 id + 同一把钥匙，两个身份域」，
// 而 HTTP 那一侧造不出这个局面：scope 串是按接口起的，买家那两条
// （orders.create / payments.create）与后台那 5 条（admin.*）永远不相等，
// 于是走接口打进来永远撞不上 —— 测试会绿，而它绿的原因是**碰不到**，
// 不是**碰得到但被挡住了**。一条靠「构造不出反例」而绿的测试没有区分力。
//
// 而 scope 不相等这件事本身正是一条**没有执行者的约定**（00023 的文件头
// 原话）：它只活在「起 scope 名字的人知道这回事」里。这条测试把最后那道
// 防线 —— 主键本身 —— 直接顶上去测。
func TestIdempotencyKeySpacesDoNotCollideAcrossIdentityDomains(t *testing.T) {
	ctx := context.Background()
	idA, _ := seedTwoTenants(t)
	r := repository.New(pool(t))

	// 同一个 scope、同一个数字 id、同一把钥匙、同一个 request_hash。
	// **四样全都一样**，唯一的差别是身份域。
	scope := fmt.Sprintf("idemtest.%d", time.Now().UnixNano())
	const id int64 = 7
	const key = "same-key-two-domains"
	const hash = "same-request-hash"
	cleanupIdemScope(t, scope)

	buyer := repository.BuyerSubject(id)
	staff := repository.StaffSubject(id)

	// —— 两次抢占都必须成功。第二次失败就说明两个域共用一个键空间。
	for _, c := range []struct {
		name string
		subj repository.IdempotencySubject
	}{{"买家", buyer}, {"后台操作员", staff}} {
		if err := r.WithTenant(tenant.NewContext(ctx, idA), func(tx repository.Tx) error {
			claimed, err := tx.ClaimIdempotencyKey(ctx, scope, c.subj, key, hash)
			if err != nil {
				return err
			}
			if !claimed {
				return fmt.Errorf("%s 没抢到这把钥匙 —— 它被另一个身份域占着，"+
					"也就是说 subject_kind 没有把两个键空间切开："+
					"7 号员工与 7 号买家落在了同一行上", c.name)
			}
			return nil
		}); err != nil {
			t.Fatalf("%s 抢占失败：%v", c.name, err)
		}
	}

	// —— 各自存档，存不一样的东西。
	code := int32(201)
	for _, c := range []struct {
		subj repository.IdempotencySubject
		body string
	}{{buyer, `{"who":"buyer"}`}, {staff, `{"who":"staff"}`}} {
		if err := r.WithTenant(tenant.NewContext(ctx, idA), func(tx repository.Tx) error {
			return tx.FinishIdempotencyKey(ctx, scope, c.subj, key,
				repository.IdempotencySucceeded, &code, []byte(c.body))
		}); err != nil {
			t.Fatalf("存档失败：%v", err)
		}
	}

	// —— 各自读回自己那一份。**这一条才是真正的判据**：上面两次抢占都成功
	// 只证明插进去了两行，证明不了读的时候分得开。共用键空间的实现在这里
	// 会让两边读到同一份存档 —— 而那正是「买家拿到员工的响应」本身。
	//
	// 比的是解开之后的字段，不是字节：response_body 是 jsonb，
	// Postgres 会把它重新排版（实测存进去的 {"who":"buyer"} 读回来是
	// {"who": "buyer"}）。拿字节去比，红的会是排版而不是语义。
	for _, c := range []struct {
		name string
		subj repository.IdempotencySubject
		want string
	}{{"买家", buyer, "buyer"}, {"后台操作员", staff, "staff"}} {
		var got struct {
			Who string `json:"who"`
		}
		if err := r.WithTenant(tenant.NewContext(ctx, idA), func(tx repository.Tx) error {
			rec, err := tx.FindIdempotencyKey(ctx, scope, c.subj, key)
			if err != nil {
				return err
			}
			return json.Unmarshal(rec.ResponseBody, &got)
		}); err != nil {
			t.Fatalf("%s 读存档失败：%v", c.name, err)
		}
		if got.Who != c.want {
			t.Fatalf("%s 读到的存档是 %q 那一份，期望 %q —— 两个身份域共用了同一行，"+
				"其中一个人拿到了另一个人的响应", c.name, got.Who, c.want)
		}
	}

	// —— 反面：同一个身份域里，同一把钥匙第二次抢不到。
	//
	// 没有这一条，上面全部断言都可以被「主键根本不起作用」满足
	// （那时任何两次抢占都会成功，而且各存各的行）。
	if err := r.WithTenant(tenant.NewContext(ctx, idA), func(tx repository.Tx) error {
		claimed, err := tx.ClaimIdempotencyKey(ctx, scope, staff, key, hash)
		if err != nil {
			return err
		}
		if claimed {
			return errors.New("同一个身份域里同一把钥匙抢到了第二次 —— " +
				"主键没有在锁任何东西，幂等是假的")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// 零值 IdempotencySubject 必须在数据库上当场失败，不能安静地落进某个键空间。
//
// 它盯的是 00023 里那条 CHECK (subject_kind IN (1, 2))。Go 这一侧
// IdempotencySubject 的字段是私有的、只能由两个构造函数造出来，
// 所以「忘了构造」在包外编译不过；但 repository 包**内部**写得出零值，
// 而那一路的症状是 subject_kind = 0 —— 一个既不是买家也不是员工的第三个
// 键空间，里面的每一行都不属于任何人。删掉那条 CHECK，这条会红。
func TestZeroIdempotencySubjectIsRejectedByTheDatabase(t *testing.T) {
	ctx := context.Background()
	idA, _ := seedTwoTenants(t)

	scope := fmt.Sprintf("idemtest.zero.%d", time.Now().UnixNano())
	cleanupIdemScope(t, scope)

	// 不经 repository：那一层按构造造不出零值主体，而要测的正是数据库那道闸。
	conn, err := db.Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)

	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx,
		`SELECT set_config('app.merchant_id', $1, true)`,
		strconv.FormatInt(idA, 10)); err != nil {
		t.Fatal(err)
	}
	_, err = tx.Exec(ctx,
		`INSERT INTO idempotency_keys (scope, subject_kind, subject_id, idem_key,
		                               request_hash, expire_at)
		 VALUES ($1, 0, 7, 'zero-kind', 'h', now() + interval '24 hours')`, scope)
	if err == nil {
		t.Fatal("subject_kind = 0 被接受了 —— CHECK 约束不在，" +
			"一个忘了构造主体的调用会安静地开出第三个键空间")
	}
	var pgErr interface{ SQLState() string }
	if !errors.As(err, &pgErr) || pgErr.SQLState() != "23514" {
		t.Fatalf("期望 23514（CHECK 违反），实得 %v", err)
	}
}

// cleanupIdemScope 在测试结束时把这个 scope 下的行删掉。
//
// 走管理员连接：idempotency_keys 上有 RLS，而这些行分属哪个租户由
// current_merchant() 填的默认值决定 —— 清理时再去猜一遍租户是多余的，
// 而猜错的症状是「测试数据留在库里」，下一轮才现形。
func cleanupIdemScope(t *testing.T, scope string) {
	t.Helper()
	t.Cleanup(func() {
		c := context.Background()
		admin, err := pgx.Connect(c, db.AdminDSN())
		if err != nil {
			t.Logf("清理 %s 时连不上库：%v", scope, err)
			return
		}
		defer admin.Close(c)
		if _, err := admin.Exec(c,
			`DELETE FROM idempotency_keys WHERE scope = $1`, scope); err != nil {
			t.Logf("清理 %s 失败：%v", scope, err)
		}
	})
}
