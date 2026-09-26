package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/keel/keel/internal/repository"
)

// 后台那 5 条 POST 的幂等（契约给它们都声明了必填的 Idempotency-Key 与
// 422 IdempotencyKeyReused）。M4 收尾。
//
// ===========================================================================
// 它此前缺的不是代码，是一列
// ===========================================================================
//
// idempotency_keys 的主键原先是 (scope, user_id, idem_key)，而后台这条路上
// 要往 user_id 里放 staff_id —— 那正是 auth/staff_middleware.go 与数据模型
// §14 反复点名的那件事（两张表的 id 来自同一种自增序列）。
// 00023 把主体列换成了 (subject_kind, subject_id)，repository 那一层因此有了
// StaffSubject。这个文件是它上面那一层。
//
// ===========================================================================
// 抢占、业务、存档在**同一个事务**里
// ===========================================================================
//
// 与 payment_intent.go 的 CreateIntent 同一个形状，理由也一字不差：失败路径上
// 不需要任何「把钥匙还回去」的善后代码。类目不存在、SKU 货号撞车、上传的
// content_type 不对 —— 任何一个错误都让整个事务回滚，抢占那一行随之消失，
// 客户端拿同一把钥匙原样重试即可，因为确实什么都没发生。
//
// 下单那条链路做不到这一点（它的 SAGA 跨了好几个事务，所以 order.go 里有
// ReleaseIdempotencyKey 与 archiveFailure 两段善后）。这 5 条全是**本地**
// 写操作，一个事务装得下 —— 那是它们比下单简单的地方，不该被抄成一样复杂。
//
// 一个直接后果：**「409 处理中」在这条路上按构造不可达。** 第二个并发请求的
// 抢占 INSERT 会在唯一索引上阻塞，等到的一定是已提交的存档（status = 1）
// 或者一次回滚（那一行不存在，于是它自己抢到）。分支仍然实现出来，
// 因为「不可达」依赖的前提是「这个 scope 上没有别的写入者」，
// 而那是一个会变的前提。
//
// ===========================================================================
// response_body 里存的是**领域对象**，不是渲染后的响应体
// ===========================================================================
//
// 这是与 §12 字面描述的一处偏离，说清楚。
//
// 渲染归 handler：契约类型在 internal/api，映射函数（apiAdminProduct 之类）
// 在 internal/handler。要存「渲染后的字节」，就得把渲染器当参数传进这一层
// —— 也就是让业务层认得对外表示，而那是这个仓库一直在避免的方向
// （repository.ProductImage 上刻意没有 URL 字段，注释写着同一条）。
//
// 存领域对象、回放时由**同一个渲染器**渲染同一份数据，客户端拿到的字节与首次
// 完全相同（渲染是确定性的）。顺带还多一个好处：响应体的形状哪天变了，
// 重放拿到的是新形状，而不是一份过期的存档。
//
// 代价也说清楚：**回放的是数据，不是那一次响应**。如果将来某条接口的响应里
// 出现了一个不由领域对象决定的东西（一个随机 nonce、一个签名），
// 那一样东西在重放时会变。今天这 5 条里没有这种字段。

// adminIdempotencyScope 是这 5 条接口各自在 idempotency_keys 里的作用域。
//
// 每条一个，不能共用：同一个客户端在同一秒里建商品又建 SKU，用的很可能是同一
// 个请求 id，共用 scope 的话第二次会命中第一次的存档 —— 于是「建 SKU」返回
// 一件商品。这条推理与 payment_intent.go 里那段一字不差。
//
// 串以 admin. 打头，与买家那两个（orders.create / payments.create）分开。
// 那个前缀今天不承担任何逻辑（主体域由 subject_kind 那一列说了算），
// 它是给读日志的人看的。
const (
	scopeAdminUploadCreate   = "admin.uploads.create"
	scopeAdminProductCreate  = "admin.products.create"
	scopeAdminProductPublish = "admin.products.publication"
	scopeAdminSKUCreate      = "admin.skus.create"
	scopeAdminCategoryCreate = "admin.categories.create"
)

// 存档里的 response_code。取值就是契约在各自 201 / 200 上写的那个。
const (
	archivedCreated int32 = 201
	archivedOK      int32 = 200
)

// ErrIdempotencyKeyMissing：契约把 Idempotency-Key 定成必填，而这次请求没带。
//
// 422 而不是 400：契约在这 5 条上的错误集合里有 422，没有 400，
// 而「必填的东西没给」正是 422 说的那件事。
var ErrIdempotencyKeyMissing = errors.New("缺少 Idempotency-Key 请求头")

// idempotentWrite 把一次后台写操作包进「抢占 → 业务 → 存档」。
//
// 它是自由函数而不是 *AdminCatalogService 的方法，只因为 Go 的方法不能带类型
// 参数。泛型在这里不是炫技：没有它，存档与回放要走 any，而回放那一步
// （把 JSON 解回领域对象）就会变成一次调用方各写一遍的类型断言 ——
// 五个调用点，五次写错的机会，而写错的症状是重放返回一个零值对象。
//
// 返回的第二个值为 true 表示这是一次**重放**：本次调用没有执行任何业务动作，
// handler 要给响应加上 Idempotency-Replayed: true。
func idempotentWrite[T any](ctx context.Context, s *AdminCatalogService,
	scope, idemKey, hash string, code int32,
	fn func(tx repository.Tx) (T, error)) (T, bool, error) {

	var zero T
	id, err := requireStaff(ctx)
	if err != nil {
		return zero, false, err
	}
	// 这一句是整个改动的落点：主体是 staff，而且**它在类型上就说得出来**。
	// 00023 之前这里只能写 id.StaffID，落进一个叫 user_id 的列。
	return idempotentTx(ctx, s.repo, repository.StaffSubject(id.StaffID), scope, idemKey, hash, code, fn)
}

// tenantRunner 是 idempotentTx 需要的全部仓储能力：开一个租户事务。
type tenantRunner interface {
	WithTenant(ctx context.Context, fn func(repository.Tx) error) error
}

// idempotentTx 是 idempotentWrite 的主体无关版本：「抢占 → 业务 → 存档」在
// **同一个事务**里，主体由调用方给（买家 BuyerSubject / 后台 StaffSubject）。
//
// 订单后半程那几条写接口（取消、确认收货、发货、退款申请 / 撤回 / 审核）
// 全是本地事务，一个事务装得下，所以它们都走这里 —— 与后台那 5 条同一个形状，
// 失败路径上不需要任何「把钥匙还回去」的善后：业务报错，整个事务回滚，
// 抢占那一行随之消失，客户端拿同一把钥匙原样重试即可。
//
// 抽出来而不是再抄一份：两份「抢占 → 回放判定 → 存档」各自演化的话，
// 迟早有一份漏掉 request_hash 的比对 —— 而那正是 §12 说「最不能省」的那一列。
func idempotentTx[T any](ctx context.Context, repo tenantRunner, subj repository.IdempotencySubject,
	scope, idemKey, hash string, code int32,
	fn func(tx repository.Tx) (T, error)) (T, bool, error) {

	var zero T
	if idemKey == "" {
		return zero, false, ErrIdempotencyKeyMissing
	}

	var out T
	replayed := false
	err := repo.WithTenant(ctx, func(tx repository.Tx) error {
		claimed, err := tx.ClaimIdempotencyKey(ctx, scope, subj, idemKey, hash)
		if err != nil {
			return err
		}
		if !claimed {
			v, err := replayArchived[T](ctx, tx, scope, subj, idemKey, hash)
			if err != nil {
				return err
			}
			out, replayed = v, true
			return nil
		}

		v, err := fn(tx)
		if err != nil {
			return err
		}
		body, err := json.Marshal(v)
		if err != nil {
			return err
		}
		if err := tx.FinishIdempotencyKey(ctx, scope, subj, idemKey,
			repository.IdempotencySucceeded, &code, body); err != nil {
			return err
		}
		out = v
		return nil
	})
	if err != nil {
		return zero, false, err
	}
	return out, replayed, nil
}

// replayArchived 读出已存在的那一行，按三态处置（§12 那张表）。
func replayArchived[T any](ctx context.Context, tx repository.Tx,
	scope string, subj repository.IdempotencySubject, idemKey, hash string) (T, error) {

	var zero T
	rec, err := tx.FindIdempotencyKey(ctx, scope, subj, idemKey)
	if errors.Is(err, repository.ErrIdempotencyKeyNotFound) {
		// 抢占说「已存在」，回头读却读不到：expire_at 到了，清理任务刚好把行
		// 删掉。让它以「处理中」的形式回 409 —— 客户端退避重试时那把钥匙已经
		// 彻底没了，会正常地抢到。同 replayIntent 里那一支。
		return zero, fmt.Errorf("%w: 幂等记录刚好过期了", ErrIdempotencyInFlight)
	}
	if err != nil {
		return zero, err
	}
	if rec.RequestHash != hash {
		// 契约那个 422。**绝不能当成重放静默吞掉**（§12 原话）：
		// 那会让用户以为第二个请求生效了，而实际什么都没发生。
		return zero, fmt.Errorf("%w: 同一把 Idempotency-Key 配了不同的请求体",
			ErrIdempotencyKeyReused)
	}
	if rec.Status != repository.IdempotencySucceeded {
		// 0 处理中：见文件头 —— 单事务形态下按构造不可达，但「不可达」依赖的
		// 是一个会变的前提。
		// 2 失败：这条路不存档失败（整个事务回滚了，抢占那一行都不在），
		// 出现它说明这一行是别的东西写的。
		return zero, fmt.Errorf("%w: 幂等记录状态是 %d", ErrIdempotencyInFlight, rec.Status)
	}
	if err := json.Unmarshal(rec.ResponseBody, &zero); err != nil {
		return zero, fmt.Errorf("幂等存档解不开（scope=%s）: %w", scope, err)
	}
	return zero, nil
}

// adminRequestHash 是 §12 那一列 request_hash。
//
// **路径参数也进哈希**，这是与买家那两条的唯一差别，而它是必须的：
// 「给商品 7 建一个 SKU」与「给商品 9 建一个同样的 SKU」请求体逐字相同，
// 而 scope 也相同。不带路径的话，同一把钥匙第二次会拿到第一次那个 SKU 的
// 存档 —— 客户端以为给商品 9 建好了，实际什么都没发生。
//
// 规范化靠 encoding/json 对结构体的确定性序列化（字段按声明顺序），
// 不拿原始请求体去哈希 —— 理由与 order.go 的 requestHash 一字不差：
// 多一个空格、字段换个顺序就成了另一个请求，而客户端重试时序列化出来的
// 字节本来就不保证一模一样。
func adminRequestHash(pathIDs []int64, payload any) (string, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	for _, id := range pathIDs {
		fmt.Fprintf(h, "%d\x00", id)
	}
	h.Write(raw)
	return hex.EncodeToString(h.Sum(nil)), nil
}
