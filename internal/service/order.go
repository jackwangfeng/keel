package service

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/keel/keel/internal/auth"
	"github.com/keel/keel/internal/repository"
)

// 下单主链路：POST /orders/preview（无副作用试算）与 POST /orders（SAGA 建单）。
//
// # 顺序：为什么订单先落库，SAGA 后提交
//
// 这是这个文件里最要紧的一条，它由 dtmrs 的接口面推出来，不是偏好：
//
//	BranchFunc func(gid, branchID, op string) int
//
// 分支**只拿到这三个字符串**，而且它跑在任何 HTTP 请求之外 —— 崩溃重启后由
// 协调器重放，进程对原请求毫无记忆。于是分支需要的一切必须能从 gid 推出来，
// 或者已经落在库里。「扣哪些 SKU、各扣几件」推不出来（gid 里只有租户与订单号），
// 只能落库。
//
// 所以顺序只能是：**先把订单与订单项写进库，再提交 SAGA**。
//
// 反过来做不到。「SAGA 先跑、建单分支再把订单写出来」这句话听着更干净，但那个
// 分支手里没有商品、没有数量、没有收货地址、没有金额 —— 它连要写什么都不知道。
// 唯一能把这些交给它的通道是 gid，而 gid 有 128 字节上限（dtmrs 的
// Backend::ID_MAX），塞不下一张购物车，塞得下也会变成一条谁都能伪造的载荷。
//
// 代价是「已落库」与「已成交」被拆成了两件事，于是订单需要一个用户看不见的
// 中间态（status = 0 创建中，见 00013）。这个代价是真的，但它换来的是：
// 分支重放时要的东西一定还在，因为它在数据库里，而不是在某个已经死掉的进程里。
//
// # 两个正向分支的先后：建单在前，库存在后
//
// 架构 §5 的图把库存画在前面。本实现刻意反过来，理由是**补偿只对真的执行过的
// 分支生效**（子事务屏障的空回滚保护，见 repository/saga.go）：
//
//   - 库存在前：库存失败 → 建单分支从没执行过 → 它的补偿是一次空回滚 →
//     那笔 status = 0 的订单**永远没人关**，成为一行谁也扫不到的垃圾。
//   - 建单在前：库存失败 → 建单的补偿是一次真补偿（10 → 90 已关闭）→
//     订单一定被关掉，而且是 SAGA 自己关的，不靠 HTTP 那一侧的善后代码
//     （那段代码在进程崩溃时不会跑）。
//
// 换来的窗口是：建单成功、库存还没扣的那一瞬间，订单已经是 10 待支付。
//
// # 那个窗口，接上支付回调（Task 7）之后重新算过了：结论是维持
//
// 上一轮写的是「本期没有支付回调，窗口里没有任何人能对它做什么；接上支付之后
// 这里要重新算一遍账」。现在算完了。
//
// **窗口仍然不可达**，而且不是靠运气 —— 它靠一条这个函数自己维持的不变量：
// `order_no` 是 72 bit 随机不可枚举（newOrderNo），而 Create **只在 SAGA 到达
// 终态、并且重新读库确认 status = 10 之后**才把单号交出去（下面第三段）。
// 支付回调必须带着订单号才找得到订单，所以窗口里没有任何人知道该付哪一单。
//
// **换成「库存在前」的代价比这个窗口大**，这是本轮真正推翻的那一句：
// 上一轮维持这个顺序的理由是「库存在前的话，那笔 status = 0 的订单永远没人关」，
// 而任务 6 的孤儿草稿清理恰好把那条理由消掉了（service/sweep.go）。所以理由要换
// 一条新的，而新的这条更硬：库存排在前面时，`order_create_undo` 在**每一条**
// 失败路径上都退化成空回滚 —— 建单排在最后，它自己失败时补偿是空回滚，
// 它成功时整笔 SAGA 就成功了。于是下单主链路上「补偿到底跑没跑」这件事
// 再也测不出来，而那正是 SAGA 最需要能被证伪的性质。
//
// 维持的同时补了一道：`closeOrder`（建单分支的补偿）在关不掉这一单时不再静默
// 成功，它要能区分「已经是 90」与「已经被付掉了」。理由写在那个函数上：
// 不可达不等于不存在，而这个洞靠的是「单号在终态前不外泄」这条住在别处的不变量。

// 下单相关的业务错误。handler 按它们映射契约里明写的响应码。
var (
	// ErrAddressNotFound：address_id 在本买家名下查不到。契约里这条接口没有
	// 单独的 404，落到 422（请求里的 address_id 不成立）。
	ErrAddressNotFound = errors.New("收货地址不存在")

	// ErrPriceChanged：expected_payable_cents 与服务端试算对不上。
	// 契约明写这是 409：「防止价格变动导致用户以旧价成交」。
	ErrPriceChanged = errors.New("应付金额已变动")

	// ErrInsufficientStock：库存不足。**正常业务分支**，触发 SAGA 全局补偿，
	// 用户看到 409。
	//
	// 它与下面那个刻意分开，理由见 repository/inventory.go：把「SKU 不属于本
	// 租户」翻译成「库存不足」，等于把一次跨租户访问伪装成一次正常的缺货。
	ErrInsufficientStock = errors.New("库存不足")

	// ErrCrossTenantSKU：扣减时发现这一行在本租户不可见。
	//
	// **不是业务分支**，是 bug 或攻击（或者这个 SKU 根本没有库存行）。
	// handler 把它映射成 500 而不是 409 —— 回 409「库存不足」会让客户端
	// 提示用户「换一件商品」，而真正发生的事情根本不在客户端能处理的范围里。
	ErrCrossTenantSKU = errors.New("SKU 在当前租户不可见")

	// ErrIdempotencyInFlight：同一个 Idempotency-Key 正在处理中。
	// 契约：409 + Retry-After，客户端应退避重试而不是当成业务失败弹窗。
	ErrIdempotencyInFlight = errors.New("同一个幂等键正在处理中")

	// ErrIdempotencyKeyReused：同一个键配了不同的请求体（request_hash 不一致）。
	// 契约：422。数据模型 §12 写得很清楚 —— 宁可显式失败，也不把不同的请求
	// 当成重放静默吞掉，那会让用户以为下单成功了而实际什么都没发生。
	ErrIdempotencyKeyReused = errors.New("幂等键被复用")

	// ErrOrderSagaFailed：SAGA 没有成功，而且没有更具体的原因。
	// 出现它意味着分支把失败原因弄丢了 —— 值得一条 500 和一条日志。
	ErrOrderSagaFailed = errors.New("下单事务未能完成")
)

// idempotencyScope 是这条接口在 idempotency_keys 里的作用域（数据模型 §12）。
const idempotencyScope = "orders.create"

// archivedCreateStatus 是成功存档里的 response_code。
//
// service 一般不该认得 HTTP 状态码，这里是个有理由的例外：§12 定义的存档结构
// 里就有这一列，而「哪一次调用算已经做过了」是业务规则，不是传输细节。
// 201 来自契约。
const archivedCreateStatus int32 = 201

// orderExpireIn 是待支付订单的超时时长。
//
// 30 分钟是电商的惯例值。它写在这里而不是配置里，是因为本轮还没有 shop_settings
// 上对应的列，而一个假装可配置的常量比一个诚实的常量更糟。
const orderExpireIn = 30 * time.Minute

// sagaWaitTimeout 是等 SAGA 到达终态的上限。
//
// 必须有界：WaitFinal 是一个阻塞 cgo 调用，它整段时间都占着 dtm 那个有界
// 信号量的一张通行证和一个 OS 线程（见 internal/dtm/tc.go）。等到天荒地老的
// 调用只要有 32 个，协调器就再也收不到新事务。
//
// 超时不等于失败：那时 SAGA 可能还在跑。此时返回的是 ErrIdempotencyInFlight
// （409 + Retry-After），而不是一个「下单失败」——后者会让用户重新下一单，
// 而第一单可能马上就成功了。
const sagaWaitTimeout = 15 * time.Second

// Coordinator 是 service 需要的协调器能力。*dtm.TC 满足它。
//
// 收接口而不是 *dtm.TC：这个接口上没有 Register、没有 Close，
// 业务层拿不到「再注册一个分支」或者「把协调器关掉」这两个动作。
type Coordinator interface {
	SubmitSaga(gid, stepsJSON string) error
	WaitFinal(gid string, timeoutMS int) (string, error)
}

// OrderRepository 是本服务需要的仓储能力。
//
// 比 AuthRepository 多一个 WithSagaBranch —— 那是 SAGA 分支进入数据库的唯一
// 正门（开事务、设租户、跑屏障判定，再按判定决定跑不跑业务）。
type OrderRepository interface {
	WithTenant(ctx context.Context, fn func(repository.Tx) error) error
	WithSagaBranch(ctx context.Context, gid, branchID, op string,
		fn func(repository.Tx) error) (repository.Decision, error)
}

// OrderService 实现试算与下单。
type OrderService struct {
	repo  OrderRepository
	tc    Coordinator
	log   *slog.Logger
	notes *branchNotes

	// now 可替换，好让测试构造「已过期」这类时间相关的场景。
	now func() time.Time
}

// AttachCoordinator 把协调器接上。
//
// 为什么要分两步：这里有一个真实的环 —— **分支要 repo，协调器要分支**
// （dtmrs 要求注册发生在 Start 之前），**服务要协调器**。三者里必须有一处允许
// 先造后接。
//
// 选了这一处，因为它是唯一一处「没接上就当场报错」的：Create 第一件碰协调器的
// 事就是 SubmitSaga，而那一步会在第一次下单时立刻暴露。把环开在别处
// （比如让分支晚一点注册）踩错了不报错 —— 未注册的 local:// 名字要等到提交时
// 才被拒，而那时错误指向的是业务代码。
func (s *OrderService) AttachCoordinator(tc Coordinator) { s.tc = tc }

func NewOrderService(r OrderRepository, tc Coordinator, log *slog.Logger) *OrderService {
	if log == nil {
		log = slog.Default()
	}
	return &OrderService{
		repo:  r,
		tc:    tc,
		log:   log,
		notes: newBranchNotes(),
		now:   time.Now,
	}
}

// CreateRequest 是 OrderCreateRequest 在 service 边界上的形状。
//
// **试算与下单共用它**，因为契约里这两条接口共用同一个 schema。
// 共用同一个入参类型是「共用同一份定价」这件事在类型上的第一道保证：
// 两边连能传进去的东西都是同一个，剩下的差别只能在副作用上，不能在金额上。
type CreateRequest struct {
	Items     []LineInput
	AddressID int64

	// StoreID 是履约门店。**契约里它是必填的，服务端不替客户端猜。**
	//
	// 读接口（/products、/search）省略 store_id 时会走回落链，这里刻意不走：
	// 买家在 A 店看到的价格与库存，下单时若被服务端静默落到默认门店 B，
	// 结果是「在 A 店看的货从 B 店发出、按 B 店的价成交」——
	// 而那是一个没有任何东西会报出来的错（两家店都有这件商品、两个价都合法）。
	StoreID              int64
	Remark               *string
	ExpectedPayableCents *int64
	UserCouponID         *int64
}

// CreateResult 是一次下单的结果。
type CreateResult struct {
	Order repository.Order

	// Replayed 为真表示这是一次幂等重放：**本次调用没有执行任何业务动作**，
	// 返回的是首次那一单。契约要求响应带 Idempotency-Replayed: true，
	// 客户端据此区分「我真的下了单」与「这是上次那单」。
	Replayed bool
}

// Preview 实现 POST /orders/preview。**无副作用。**
//
// 它和 Create 调同一个 priceOrder，这是本任务的一条硬要求：试算和真下单算出
// 不同的钱是这条链路最严重的一类 bug，而共用一份实现是唯一能让它不可能发生的
// 办法（见 pricing.go 的文件头）。
//
// 带了券就按券算，券用不了就报 409（ErrCouponNotApplicable），**绝不忽略这张券
// 按原价试算**：用户就是照着试算结果决定要不要下单的，一个忽略了券的试算会让他
// 以为这个价格就是用券之后的价格。
//
// 顺带回一份「本单可用券」（Quote.ApplicableCoupons），与 POST /coupons/applicable
// 同一份实现，客户端据此渲染选券，不必再请求一次。
func (s *OrderService) Preview(ctx context.Context, req CreateRequest) (Quote, error) {
	id, err := auth.FromContext(ctx)
	if err != nil {
		// 契约里 /orders/preview 继承全局 bearerAuth，所以到这里一定有身份。
		// 取不到就是装配 bug（中间件没挂），不是客户端的错。
		return Quote{}, err
	}
	now := s.now()

	var q Quote
	err = s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		sc, err := orderScope(ctx, tx, req.StoreID)
		if err != nil {
			return err
		}
		// 运费按收货地址算（00056）：试算与下单读的是同一个地址、归到同一个省。
		// 此前试算不读 address_id，现在它和下单一样查不到就 422。
		addr, err := tx.FindAddress(ctx, req.AddressID, id.UserID)
		if errors.Is(err, repository.ErrAddressNotFound) {
			return fmt.Errorf("%w: address_id=%d", ErrAddressNotFound, req.AddressID)
		}
		if err != nil {
			return err
		}
		dest := destinationOf(addr)
		q, err = priceOrder(ctx, tx, sc, &dest, req.Items, couponOf(id.UserID, req, now))
		if err != nil {
			return err
		}
		// 超出每人限购：试算就说出来（409），而不是等下单时才被库存分支拒掉。
		if err := q.checkPromotionLimits(); err != nil {
			return err
		}
		q.ApplicableCoupons, err = applicableCoupons(ctx, tx, sc, id.UserID, q, now)
		return err
	})
	return q, err
}

// couponOf 是试算与下单构造 couponRequest 的唯一方式：同一个买家、同一张券、
// 同一个时钟来源。
func couponOf(userID int64, req CreateRequest, now time.Time) couponRequest {
	return couponRequest{UserID: userID, ID: req.UserCouponID, Now: now}
}

// orderScope 把请求里那个必填的 store_id 变成一个 StoreScope。
//
// 门店不存在 / 不属于本租户 / 已软删 → ErrStoreNotFound，由 handler 翻成 422
// （store_id 在请求体里，不在路径里 —— 数据模型 §4 那条分界线）。
//
// **不回落**：见 CreateRequest.StoreID 上那段。这个函数存在的意义就是让
// 「下单也顺手回落一下」写不出来 —— 它连那条路径都没有。
func orderScope(ctx context.Context, tx repository.Tx, storeID int64) (repository.StoreScope, error) {
	if storeID <= 0 {
		return repository.StoreScope{}, fmt.Errorf("%w: store_id 必须为正，实得 %d",
			ErrBadRequest, storeID)
	}
	_, regionID, err := tx.StoreScope(ctx, storeID)
	if errors.Is(err, repository.ErrCatalogNotFound) {
		return repository.StoreScope{}, fmt.Errorf("%w: store_id=%d", ErrStoreNotFound, storeID)
	}
	if err != nil {
		return repository.StoreScope{}, err
	}
	return repository.StoreScope{StoreID: storeID, RegionID: regionID}, nil
}

// Create 实现 POST /orders。
func (s *OrderService) Create(ctx context.Context, req CreateRequest, idemKey string) (CreateResult, error) {
	if idemKey == "" {
		return CreateResult{}, fmt.Errorf("%w: 缺少 Idempotency-Key 请求头", ErrBadRequest)
	}
	id, err := auth.FromContext(ctx)
	if err != nil {
		return CreateResult{}, err
	}

	hash, err := requestHash(req)
	if err != nil {
		return CreateResult{}, err
	}

	// ---- 第一段：抢幂等键 + 把订单落库。一个事务。----
	//
	// 合在一个事务里是刻意的：抢到了键却没落下订单，那把键就锁死了 24 小时，
	// 而库里什么都没有。它们同生共死之后，这一段的任何失败（地址不存在、
	// SKU 不可售、金额对不上）都会把抢占一起回滚掉 —— 客户端可以拿同一个键
	// 原样重试，因为**确实什么都没发生**。
	//
	// 与 §12 的「失败也存档回放」不冲突：那条说的是**业务已经执行过**的失败
	// （SAGA 跑完了但没成功），见下面第三段。校验期的失败没有任何副作用可言。
	var (
		draft     repository.Order
		replayed  *CreateResult
		replayErr error
	)
	err = s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		claimed, err := tx.ClaimIdempotencyKey(ctx, idempotencyScope, repository.BuyerSubject(id.UserID), idemKey, hash)
		if err != nil {
			return err
		}
		if !claimed {
			replayed, replayErr, err = s.replay(ctx, tx, id.UserID, idemKey, hash)
			return err
		}
		draft, err = s.placeDraft(ctx, tx, id.UserID, req)
		return err
	})
	if err != nil {
		return CreateResult{}, err
	}
	if replayErr != nil {
		return CreateResult{}, replayErr
	}
	if replayed != nil {
		return *replayed, nil
	}

	// ---- 第二段：提交 SAGA 并等它到终态。----
	if s.tc == nil {
		return CreateResult{}, errors.New("事务协调器没有接上（AttachCoordinator 没被调用）")
	}
	gid, err := orderGID(ctx, draft.OrderNo)
	if err != nil {
		return CreateResult{}, err
	}
	defer s.notes.drop(gid)

	if err := s.tc.SubmitSaga(gid, sagaSteps); err != nil {
		// 提交都没成功：分支一个都没跑过，订单还停在 status = 0。
		//
		// **把抢占记录撤掉**，这是上一轮记下的那笔账。不撤的话，客户端拿同一把
		// 钥匙重试会一直撞 409 处理中，直到 24 小时后 expire_at 过期 ——
		// 而「同一个逻辑请求的重试用同一把钥匙」正是 Idempotency-Key 的语义，
		// 也就是说我们锁死的是客户端**正确的**行为。
		//
		// 竞态想清楚了，结论是「撤是安全的，但不是无代价的」：
		//
		//   · SubmitSaga 返回错误不等于「协调器一定没收下这笔事务」。它有可能
		//     在协调器已经落库之后才断的连接，那时那笔 SAGA 是活的。撤了钥匙、
		//     客户端立刻重试，就会有**第二笔**订单，而第一笔照样会被推完。
		//   · 但第一笔的单号从没返回给任何人（Create 只在终态之后才交出单号），
		//     所以它是一笔谁也付不了的待支付订单 —— 它会在 30 分钟后被超时补偿
		//     任务关掉、库存回补（service/sweep.go）。代价是那段时间里的少卖，
		//     **有界且可恢复**。
		//   · 反过来不撤的代价是那把钥匙锁死 24 小时，客户端除了换钥匙没有出路，
		//     而换钥匙这件事恰恰是幂等协议要它别做的。
		//
		// 所以撤。草稿订单**刻意不一起关掉**：正因为那笔 SAGA 可能是活的 ——
		// 关掉它会让一个正在跑的建单分支撞上 errOrderNotDraft，把一笔本来能成的
		// 订单变成一次全局补偿。留给超时补偿任务是对的，它有 30 分钟可以等。
		//
		// 撤销失败只记日志：这一路本来就已经在返回错误了，把撤销的失败盖在
		// 原因上面，会让排查从「提交事务失败」变成「删一行失败」。
		if relErr := s.releaseKey(ctx, id.UserID, idemKey); relErr != nil {
			s.log.ErrorContext(ctx, "撤销幂等键抢占失败，这把键会一直返回 409 处理中直到过期",
				"gid", gid, "err", relErr)
		}
		return CreateResult{}, fmt.Errorf("提交下单事务失败（gid=%s）: %w", gid, err)
	}

	status, waitErr := s.tc.WaitFinal(gid, int(sagaWaitTimeout/time.Millisecond))
	if waitErr != nil {
		// 超时不是失败：SAGA 可能还在跑。不存档，返回「处理中」让客户端退避重试。
		//
		// **这里留着上一轮那笔账的另一半，本轮刻意没有收，理由写在这儿。**
		// 幂等行会停在「处理中」，而 SAGA 之后可能成功了却没人把存档补上 ——
		// 于是那把钥匙在 24 小时里一直回 409。
		//
		// 两条候选的改法各有一个要先想清楚的问题，都不在本任务的范围里：
		//
		//   ① 起一个后台 goroutine 接着等，等到终态再补存档。它占的是
		//      internal/dtm 那个**有界信号量**的通行证（32 张），而超时最可能
		//      成批发生 —— 协调器一卡，每个超时请求都留下一个续等的 goroutine，
		//      通行证很快被续等占满，**新的 SubmitSaga 再也进不去**。
		//      要做就得给续等单独一份更小的预算，而「两份预算怎么分」需要实测，
		//      examples 里那组数据是按提交并发测的，不含续等。
		//   ② 把草稿单号写进幂等行，重试时按单号自愈。这条能覆盖进程崩溃，
		//      是真正完整的解，但它要一列（或者把 response_body 当成一个
		//      「还没最终」的暂存位），也就要动数据模型 §12 的 DDL。
		//
		// 不收的代价是**有界的，而且不含钱与货**：那一笔订单要么被建单分支推到
		// 10（没人付钱，30 分钟后被超时补偿任务关掉、库存回补），要么被补偿关到
		// 90。库存不会漏，订单不会重复。退化的只是那一把钥匙，客户端换一把新的
		// 就能继续下单。
		s.log.WarnContext(ctx, "等待下单事务终态超时", "gid", gid, "err", waitErr)
		return CreateResult{}, fmt.Errorf("%w（gid=%s）: %v", ErrIdempotencyInFlight, gid, waitErr)
	}

	// ---- 第三段：把结果存档。----
	if status != "succeed" {
		bizErr := s.notes.get(gid)
		if bizErr == nil {
			bizErr = fmt.Errorf("%w: 事务终态是 %q，但没有分支留下失败原因", ErrOrderSagaFailed, status)
		}
		s.archiveFailure(ctx, id.UserID, idemKey, bizErr)
		return CreateResult{}, bizErr
	}

	// 重新读一遍订单，而不是把 draft 的字段改改交出去。
	//
	// 建单分支写的 status = 10 发生在**另一个事务**里，draft 那份快照对它一无所知。
	// 手工把内存里的 Status 改成 10 是「相信分支干了它该干的」，而这里正好有
	// 一次便宜的机会去核实它真的干了。
	final, err := s.loadOrder(ctx, draft.OrderNo)
	if err != nil {
		return CreateResult{}, err
	}
	if final.Status != orderStatusPending {
		return CreateResult{}, fmt.Errorf(
			"事务报告成功，但订单 %s 的状态是 %d 而不是 %d —— 建单分支没有生效",
			final.OrderNo, final.Status, orderStatusPending)
	}
	s.archiveSuccess(ctx, id.UserID, idemKey, final)
	return CreateResult{Order: final}, nil
}

// placeDraft 校验、定价、把 status = 0 的订单与订单项写进库。
func (s *OrderService) placeDraft(ctx context.Context, tx repository.Tx,
	userID int64, req CreateRequest) (repository.Order, error) {
	addr, err := tx.FindAddress(ctx, req.AddressID, userID)
	if errors.Is(err, repository.ErrAddressNotFound) {
		return repository.Order{}, fmt.Errorf("%w: address_id=%d", ErrAddressNotFound, req.AddressID)
	}
	if err != nil {
		return repository.Order{}, err
	}

	sc, err := orderScope(ctx, tx, req.StoreID)
	if err != nil {
		return repository.Order{}, err
	}

	// 券在这里只是**算**（判能不能用、减多少、怎么分摊），不锁。锁券是 SAGA 的
	// 券分支（order_saga.go 的 lockCoupon），它按订单行上的 user_coupon_id 去做
	// 条件更新 —— 这里算过的只是预告，那里才是判定点：两笔并发订单用同一张券，
	// 两边都能算过，只有一边锁得上，另一边的 SAGA 失败并补偿掉建单。
	dest := destinationOf(addr)
	q, err := priceOrder(ctx, tx, sc, &dest, req.Items, couponOf(userID, req, s.now()))
	if err != nil {
		return repository.Order{}, err
	}
	// 每人限购在这里是预告，库存分支（order_saga.go 的 deductStock）在行锁之下做最终判定：
	// 同一个买家两笔并发订单都能过这一道，只有一笔扣得到限购额度。
	if err := q.checkPromotionLimits(); err != nil {
		return repository.Order{}, err
	}

	// 金额一致性（契约明写的 409）。
	//
	// 它必须在**这一次**试算的结果上比，而不是信客户端上一次拿到的那个数：
	// 这条检查的全部意义就是「价格在两次之间变过没有」。
	if req.ExpectedPayableCents != nil && *req.ExpectedPayableCents != q.PayableCents {
		return repository.Order{}, fmt.Errorf("%w: 前端拿的是 %d，服务端现在算出来是 %d",
			ErrPriceChanged, *req.ExpectedPayableCents, q.PayableCents)
	}

	snapshot, err := json.Marshal(receiverSnapshot{
		ReceiverName: addr.ReceiverName,
		Phone:        addr.Phone,
		Province:     addr.Province,
		City:         addr.City,
		District:     addr.District,
		Street:       addr.Street,
		Detail:       addr.Detail,
		RegionCode:   addr.RegionCode,
		PostalCode:   addr.PostalCode,
	})
	if err != nil {
		return repository.Order{}, err
	}

	promoSnapshot, err := json.Marshal(orderPromotionSnapshots(q.Promotions))
	if err != nil {
		return repository.Order{}, err
	}
	// 运费明细的快照（orders.freight_snapshot）：下单那一刻用的哪个模板、
	// 命中哪条规则、为什么包邮。之后改模板不影响它 —— 与 receiver_snapshot 同一条道理。
	// priceOrder 拿到了地址就一定有明细，这里为 nil 只可能是那边被改坏了。
	if q.Freight == nil {
		return repository.Order{}, fmt.Errorf("下单时没有算出运费明细（priceOrder 拿到了地址却没算运费）")
	}
	freightSnapshot, err := json.Marshal(q.Freight)
	if err != nil {
		return repository.Order{}, err
	}

	orderNo, err := newOrderNo(s.now())
	if err != nil {
		return repository.Order{}, err
	}
	draft, err := tx.CreateOrderDraft(ctx, repository.NewOrderDraft{
		OrderNo:          orderNo,
		UserID:           userID,
		StoreID:          sc.StoreID,
		GoodsAmountCents: q.GoodsAmountCents,
		FreightCents:     q.FreightCents,
		// 包邮券抵掉的运费已经在 DiscountCents 里；单列一份，售后退运费按实收算。
		FreightDiscountCents: q.FreightDiscountCents,
		FreightSnapshot:      freightSnapshot,
		DiscountCents:        q.DiscountCents,
		PayableCents:         q.PayableCents,
		ReceiverSnapshot:     snapshot,
		Remark:               req.Remark,
		ExpireAt:             s.now().Add(orderExpireIn),
		UserCouponID:         q.UserCouponID,

		// 活动优惠合计与命中活动的快照。DiscountCents 已经是活动 + 券（含包邮券抵掉的运费）。
		PromotionDiscountCents: q.PromotionDiscountCents,
		Promotions:             promoSnapshot,
	})
	if err != nil {
		return repository.Order{}, err
	}
	for _, ln := range q.Lines {
		if err := tx.CreateOrderItem(ctx, repository.NewOrderItem{
			OrderID:       draft.ID,
			SKUID:         ln.SKUID,
			ProductID:     ln.ProductID,
			TitleSnapshot: ln.Title,
			SpecSnapshot:  ln.SpecValues,
			ImageSnapshot: ln.ImageURL,
			PriceCents:    ln.PriceCents,
			Quantity:      ln.Quantity,
			AmountCents:   ln.AmountCents,
			DiscountCents: ln.DiscountCents,

			ListPriceCents:         ln.ListPriceCents,
			PricePromotionID:       ln.PricePromotionID,
			PromotionDiscountCents: ln.PromotionDiscountCents,
		}); err != nil {
			return repository.Order{}, err
		}
	}
	return draft, nil
}

// replay 处理「这个幂等键已经存在」那一支（数据模型 §12 的三态表）。
//
// 返回三样东西：重放结果（命中成功存档时非 nil）、要回放的业务错误
// （命中失败存档时非 nil）、以及真正的 error（读库失败）。
func (s *OrderService) replay(ctx context.Context, tx repository.Tx, userID int64,
	idemKey, hash string) (*CreateResult, error, error) {
	rec, err := tx.FindIdempotencyKey(ctx, idempotencyScope, repository.BuyerSubject(userID), idemKey)
	if err != nil {
		return nil, nil, err
	}
	if rec.RequestHash != hash {
		// 三种状态下都是 422（§12 的表）：同一个键配了不同的请求体。
		// 宁可显式失败，也不把不同的请求当成重放静默吞掉。
		return nil, fmt.Errorf("%w: 这个键上次用的是另一个请求体", ErrIdempotencyKeyReused), nil
	}
	switch rec.Status {
	case repository.IdempotencyInFlight:
		return nil, fmt.Errorf("%w: 另一个并发请求抢先占住了这个键", ErrIdempotencyInFlight), nil
	case repository.IdempotencySucceeded:
		var order repository.Order
		if err := json.Unmarshal(rec.ResponseBody, &order); err != nil {
			return nil, nil, fmt.Errorf("幂等存档解不出订单: %w", err)
		}
		return &CreateResult{Order: order, Replayed: true}, nil, nil
	case repository.IdempotencyFailed:
		return nil, decodeArchivedFailure(rec.ResponseBody), nil
	default:
		return nil, nil, fmt.Errorf("幂等记录的状态是 %d，不是 0/1/2", rec.Status)
	}
}

// loadOrder 按订单号读回一笔订单。
func (s *OrderService) loadOrder(ctx context.Context, orderNo string) (repository.Order, error) {
	var out repository.Order
	err := s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		var err error
		out, err = tx.FindOrderByNo(ctx, orderNo)
		return err
	})
	return out, err
}

// archiveSuccess 把成功存档写回去。
//
// 存的是**领域订单**的 JSON，不是渲染好的 HTTP 响应体：service 不认得
// internal/api（那是契约生成的类型，归 handler）。重放时把它解回来，由 handler
// 用同一段代码渲染，于是两次的响应体一模一样 —— 前提是渲染逻辑没变，
// 而那正是一次部署之间该允许变的东西。
//
// 存档失败只记日志不返回错误：订单已经建成了，这时候把整个请求报成失败，
// 客户端会重试，而重试会拿到一笔**新的**订单。少一条存档的代价是下一次重试
// 会撞 409 处理中；报错的代价是重复下单。
func (s *OrderService) archiveSuccess(ctx context.Context, userID int64, idemKey string, o repository.Order) {
	body, err := json.Marshal(o)
	if err != nil {
		s.log.ErrorContext(ctx, "序列化幂等存档失败", "order_no", o.OrderNo, "err", err)
		return
	}
	code := archivedCreateStatus
	if err := s.finishKey(ctx, userID, idemKey, repository.IdempotencySucceeded, &code, body); err != nil {
		s.log.ErrorContext(ctx, "写幂等存档失败，同一个键的重试会拿到 409 处理中",
			"order_no", o.OrderNo, "err", err)
	}
}

// archiveFailure 把失败存档写回去（§12：失败也回放，最保守，绝不会重复扣款）。
func (s *OrderService) archiveFailure(ctx context.Context, userID int64, idemKey string, bizErr error) {
	body, err := json.Marshal(encodeArchivedFailure(bizErr))
	if err != nil {
		s.log.ErrorContext(ctx, "序列化失败存档失败", "err", err)
		return
	}
	// response_code 存 nil：失败响应的状态码由 handler 按 sentinel 映射，
	// 在这里再存一份数字，同一件事就有了两个可能对不上的真相。
	if err := s.finishKey(ctx, userID, idemKey, repository.IdempotencyFailed, nil, body); err != nil {
		s.log.ErrorContext(ctx, "写失败存档失败", "err", err)
	}
}

// releaseKey 撤销一次幂等键抢占。只在 SubmitSaga 失败那一路调用。
//
// 撤不到行（返回 false）不是错误：那意味着这条记录已经不是「处理中」了，
// 而那时本来就不该撤。当成错误报出来的话，一条正常的竞态会变成一条 Error 日志。
func (s *OrderService) releaseKey(ctx context.Context, userID int64, idemKey string) error {
	return s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		released, err := tx.ReleaseIdempotencyKey(ctx, idempotencyScope, repository.BuyerSubject(userID), idemKey)
		if err != nil {
			return err
		}
		if !released {
			s.log.WarnContext(ctx, "想撤销幂等键抢占，但那一行已经不是「处理中」了",
				"scope", idempotencyScope, "user_id", userID)
		}
		return nil
	})
}

func (s *OrderService) finishKey(ctx context.Context, userID int64, idemKey string,
	status int16, code *int32, body []byte) error {
	return s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		return tx.FinishIdempotencyKey(ctx, idempotencyScope, repository.BuyerSubject(userID), idemKey, status, code, body)
	})
}

// receiverSnapshot 是拍进 orders.receiver_snapshot 的形状。
//
// 字段名与契约的 ReceiverSnapshot 一致。它刻意不复用 api 里的生成类型：
// **快照的字段集一旦定下就不能再动**（契约自己也这么说），而生成类型会随契约
// 演进 —— 哪天契约给 ReceiverSnapshot 加一列，历史订单里那些没有这一列的
// JSONB 就解析不了了。
type receiverSnapshot struct {
	ReceiverName string  `json:"receiver_name"`
	Phone        string  `json:"phone"`
	Province     string  `json:"province"`
	City         string  `json:"city"`
	District     string  `json:"district"`
	Street       string  `json:"street"`
	Detail       string  `json:"detail"`
	RegionCode   *string `json:"region_code,omitempty"`
	PostalCode   *string `json:"postal_code,omitempty"`
}

// OrderPromotionSnapshot 是拍进 orders.promotions 的一项（契约的 OrderPromotion）。
//
// 与 receiverSnapshot 同一条道理：**快照的字段集一旦定下就不能再动**，所以它是一个
// 手写的类型，不复用契约生成的类型，也不直接存 PromotionHit（那个类型会随试算的展示需求演进）。
type OrderPromotionSnapshot struct {
	PromotionID   int64   `json:"promotion_id"`
	Name          string  `json:"name"`
	Type          int16   `json:"promotion_type"`
	DiscountCents int64   `json:"discount_cents"`
	SKUIDs        []int64 `json:"sku_ids"`
}

// orderPromotionSnapshots 只留命中了的活动：「还差 50 元」是给试算看的，不是这一单的事实。
func orderPromotionSnapshots(hits []PromotionHit) []OrderPromotionSnapshot {
	out := []OrderPromotionSnapshot{}
	for _, h := range hits {
		if !h.Applied {
			continue
		}
		out = append(out, OrderPromotionSnapshot{
			PromotionID: h.PromotionID, Name: h.Name, Type: h.Type,
			DiscountCents: h.DiscountCents, SKUIDs: h.SKUIDs,
		})
	}
	return out
}

// DecodeOrderPromotions 把 orders.promotions 解回来。空字节（没取这一列的查询）当成空数组。
func DecodeOrderPromotions(raw []byte) ([]OrderPromotionSnapshot, error) {
	out := []OrderPromotionSnapshot{}
	if len(raw) == 0 {
		return out, nil
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("订单的活动快照解不开: %w", err)
	}
	return out, nil
}

// requestHash 是 §12 那一列 request_hash。
//
// **它是这张表里最容易被省掉、也最不能省的一列**（§12 原话）：没有它，客户端拿
// 同一个键配不同的请求体，第二次会被当成重放静默吞掉 —— 用户以为下单成功了，
// 实际什么都没发生。
//
// 规范化靠 encoding/json 对结构体的确定性序列化（字段按声明顺序，不是 map 的
// 随机序）。不拿原始请求体去哈希：那样多一个空格、字段换个顺序就成了另一个请求，
// 而客户端重试时序列化出来的字节本来就不保证一模一样。
func requestHash(req CreateRequest) (string, error) {
	raw, err := json.Marshal(req)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

// orderNoRandomBytes 是订单号里随机部分的字节数。
//
// 订单号必须**不可枚举**（数据模型 §2）：它会出现在支付渠道的对账单上、
// 出现在用户的截图里。9 字节 = 72 bit 随机，猜中一个存在的订单号在任何
// 现实的流量下都不可能。
const orderNoRandomBytes = 9

// newOrderNo 生成对外订单号：14 位时间前缀 + 18 位十六进制随机。
//
// 时间前缀是给人看的（客服报单号时能一眼看出是哪天的），随机部分是给机器看的。
// 只用时间的话相邻两单就是相邻两个数字，等于把「今天有多少单」写在单号里。
//
// 不含 '-'、不含空白：gid 是 order-{merchant_id}-{order_no}，解析时按第一个
// '-' 切租户段，订单号里的 '-' 无害，但空白会让「看起来一样的两个 gid」
// 是两笔不同的事务（internal/dtm/gid.go 明写这条）。十六进制两样都不会有。
func newOrderNo(now time.Time) (string, error) {
	var b [orderNoRandomBytes]byte
	if _, err := rand.Read(b[:]); err != nil {
		// 熵源坏了。绝不回落到时间戳或伪随机：那会让订单号可枚举，
		// 而这是一个不会报错、只会在半年后被人发现的降级。
		return "", fmt.Errorf("生成订单号失败: %w", err)
	}
	return now.UTC().Format("20060102150405") + hex.EncodeToString(b[:]), nil
}
