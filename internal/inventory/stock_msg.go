package inventory

// 可售数跨过 0 时通知 core 重算商品列表的有货排序标记（product_store_stock，00087）。
// 约定见 docs/电商系统-总体架构.md「派生数据同步约定」；core 那一侧见 service/stock_flags.go。
//
// ===========================================================================
// 为什么是二阶段消息、为什么在这里发
// ===========================================================================
//
// 可售数只在库存服务里变（下单扣减、补偿 / 关单 / 退款回补、后台设值与调整、建 SKU 首行），
// core 不逐笔知道结果水位。拆分之前靠 core 每分钟全量刷一轮兜底，代价是门店数 × SKU 数、每个实例一份。
// 现在改成：改可售数的那个本地事务里判一下「这家店这个 SKU 是不是跨过了 0」，跨了就在**同一个事务里**
// 登记一条二阶段消息（dtm.TC.PrepareMsg + 本地表屏障，repository/msg_barrier.go），提交之后 submit。
// 本地事务提交了，消息就一定会送到；本地事务回滚了，消息就作废（或者被回查判成作废）。
//
// 判据与 core 算标记的判据是同一个：Available > 0（stock_flags.go：「这家店里任意一个在售 SKU 可售数 > 0」）。
// 商品级的「任意一个」只有 core 算得出来（SKU 属于哪件商品、在不在售都在 core），所以这里只报 SKU 级的
// 跨 0，由 core 去重算那件商品。不跨 0 的变动（10 → 9）不改变任何商品级标记，不发。
//
// 跨 0 的判断用的是这条路径**本来就拿到的**前后水位（扣减、回补、CAS、调整的返回值），在已经锁住的行上，
// 不多读一次、不多加一把锁。同一个事务里一个 SKU 被改了几次（一单里不会，但留着余地）取第一次的前值与
// 最后一次的后值。
//
// ===========================================================================
// 消息里有什么
// ===========================================================================
//
// dtmrs 的 msg 不带载荷（dtm/msg.go 文件头），所以内容编在 gid 里：
//
//	stock-{merchant_id}-{store_id}-{nonce}-{sku_id}[.{sku_id}...]
//
// 只有定位用的键，没有新水位：接收方不信消息内容，收到后自己去问库存服务当前水位
// （乱序、重复、延迟都不会算错）。nonce 让每一次跨 0 都是一条新消息 —— 同一个 SKU 先到 0 再回来是两条，
// 两条都要送到；gid 若只由键组成，第二条会被协调器当成第一条的重试。
// 一个事务里跨 0 的 SKU 多到 gid 装不下（128 字节）时拆成几条。
//
// ===========================================================================
// 回查
// ===========================================================================
//
// 回查分支（BranchStockMsgQuery）永远是**进程内**的：它回答「本地事务提交了没有」，而本地事务就在
// 发消息的这个进程的库存库里。单体时协调器只有一个；拆分时库存进程自己起一个协调器
// （app/split.go runInventory），目标地址是 core 的内网分支。

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/keel/keel/internal/dtm"
	"github.com/keel/keel/internal/repository"
	"github.com/keel/keel/internal/tenant"
)

// 分支名。BranchStockChanged 是 core 的接收分支（service.StockFlagService.StockMsgBranch），
// 地址单体是 local://stock_changed，拆分是 <KEEL_CORE_URL>/internal/v1/saga/stock_changed?bt=…；
// BranchStockMsgQuery 是库存这一侧的回查，永远是 local://。
const (
	BranchStockChanged  = "stock_changed"
	BranchStockMsgQuery = "inventory_stock_msg_query"
)

// StockMsgGIDPrefix 是跨 0 通知的 gid 前缀。
const StockMsgGIDPrefix = "stock-"

// stockMsgGraceSecs 是 prepare 之后多久开始回查。比 dtmrs 的默认 10 秒长：回查一旦抢在本地事务提交之前
// 插下 rollback 标记，这条消息就作废了（见 prepare 里对这种情况的处理）；而扣减事务在登记之后只剩几条
// 语句和一次提交，30 秒远超任何正常路径。
const stockMsgGraceSecs = 30

// StockMsg 是一条跨 0 通知的内容（全部来自 gid）。
type StockMsg struct {
	MerchantID int64
	StoreID    int64
	SKUIDs     []int64
}

// ParseStockMsgGID 解出一条跨 0 通知。严格：租户那一段与 dtm.ParseTenantGID 同一个规矩，
// 门店与 SKU 也必须是无前导零的正整数 —— 这些值会被拿去查库，一个宽容的解析器会让同一条消息有两种写法。
func ParseStockMsgGID(gid string) (StockMsg, error) {
	mid, rest, err := dtm.ParseTenantGID(StockMsgGIDPrefix, gid)
	if err != nil {
		return StockMsg{}, err
	}
	// 0.12 起的形状：stock-商家-随机串，门店与 SKU 在载荷里（StockMsgPayload）。
	if !strings.Contains(rest, "-") {
		if rest == "" {
			return StockMsg{}, fmt.Errorf("%w: %q 缺随机串", dtm.ErrBadGID, gid)
		}
		return StockMsg{MerchantID: mid}, nil
	}
	// 0.12 之前的形状：stock-商家-门店-随机串-SKU[.SKU…]（内容编在 gid 里）。升级时还在途的消息按它解。
	parts := strings.SplitN(rest, "-", 3)
	if len(parts) != 3 || parts[1] == "" {
		return StockMsg{}, fmt.Errorf("%w: %q 不是 stock-商家-门店-随机串-SKU 的形状", dtm.ErrBadGID, gid)
	}
	store, ok := canonicalID(parts[0])
	if !ok {
		return StockMsg{}, fmt.Errorf("%w: %q 的门店段 %q 不合法", dtm.ErrBadGID, gid, parts[0])
	}
	m := StockMsg{MerchantID: mid, StoreID: store}
	for _, s := range strings.Split(parts[2], ".") {
		id, ok := canonicalID(s)
		if !ok {
			return StockMsg{}, fmt.Errorf("%w: %q 的 SKU 段 %q 不合法", dtm.ErrBadGID, gid, s)
		}
		m.SKUIDs = append(m.SKUIDs, id)
	}
	return m, nil
}

// StockMsgPayload 是跨 0 通知的载荷（dtmrs 0.12 起消息能带载荷）：只有键，没有数量——接收方回源读水位，不信消息里的数。
type StockMsgPayload struct {
	StoreID int64   `json:"store_id"`
	SKUIDs  []int64 `json:"sku_ids"`
}

// DecodeStockMsg 解出一条跨 0 通知：租户只从 gid 来（tenant_context_test.go 的规矩），门店与 SKU 优先取载荷，
// 载荷为空（0.12 之前登记、升级时还在途的消息）时取 gid 里编的那一份。
func DecodeStockMsg(gid, payload string) (StockMsg, error) {
	m, err := ParseStockMsgGID(gid)
	if err != nil {
		return StockMsg{}, err
	}
	if p := strings.TrimSpace(payload); p != "" && p != "{}" {
		var pl StockMsgPayload
		if err := json.Unmarshal([]byte(p), &pl); err != nil {
			return StockMsg{}, fmt.Errorf("跨 0 通知的载荷解不开: %w", err)
		}
		if pl.StoreID <= 0 || len(pl.SKUIDs) == 0 {
			return StockMsg{}, fmt.Errorf("跨 0 通知的载荷缺门店或 SKU：%s", p)
		}
		m.StoreID, m.SKUIDs = pl.StoreID, pl.SKUIDs
	}
	if m.StoreID <= 0 || len(m.SKUIDs) == 0 {
		return StockMsg{}, fmt.Errorf("%w: %q 既没有载荷、也不是旧形状（gid 里没有门店与 SKU）", dtm.ErrBadGID, gid)
	}
	return m, nil
}

func canonicalID(s string) (int64, bool) {
	if s == "" || s[0] < '1' || s[0] > '9' {
		return 0, false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
	}
	v, err := strconv.ParseInt(s, 10, 64)
	return v, err == nil
}

// stockMsgGID 是一条跨 0 通知的 gid：stock-商家-随机串。随机串让同一家店的两次跨 0 是两条消息
// （否则第二条会被协调器当成第一条的重试）。
func stockMsgGID(merchantID int64) (string, error) {
	return dtm.TenantGID(StockMsgGIDPrefix, merchantID, nonce())
}

func nonce() string {
	var b [8]byte
	_, _ = rand.Read(b[:]) // crypto/rand 在 Linux 上不会失败；真失败了全零也只是让重复的 gid 变得可能
	return hex.EncodeToString(b[:])
}

// stockCrossings 记一个事务里各 (门店, SKU) 的第一个前值与最后一个后值。零值可用；nil 指针上的 record 什么都不做。
type stockCrossings struct {
	m     map[[2]int64][2]int32
	order [][2]int64
}

func (c *stockCrossings) record(storeID, skuID int64, before, after int32) {
	if c == nil {
		return
	}
	if c.m == nil {
		c.m = map[[2]int64][2]int32{}
	}
	k := [2]int64{storeID, skuID}
	if v, ok := c.m[k]; ok {
		c.m[k] = [2]int32{v[0], after}
		return
	}
	c.m[k] = [2]int32{before, after}
	c.order = append(c.order, k)
}

// crossed 按门店分组返回跨过 0 的 SKU（门店与 SKU 都升序，好让 gid 可复现、测试可断言）。
func (c *stockCrossings) crossed() map[int64][]int64 {
	if c == nil {
		return nil
	}
	var out map[int64][]int64
	for _, k := range c.order {
		v := c.m[k]
		if (v[0] > 0) == (v[1] > 0) {
			continue
		}
		if out == nil {
			out = map[int64][]int64{}
		}
		out[k[0]] = append(out[k[0]], k[1])
	}
	for _, ids := range out {
		sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	}
	return out
}

// changed 按门店分组返回可售数有变化的 SKU（首前值 ≠ 末后值），给渠道层的 stock.changed 用（channel_gate.go）。
func (c *stockCrossings) changed() map[int64][]int64 {
	if c == nil {
		return nil
	}
	var out map[int64][]int64
	for _, k := range c.order {
		if v := c.m[k]; v[0] == v[1] {
			continue
		}
		if out == nil {
			out = map[int64][]int64{}
		}
		out[k[0]] = append(out[k[0]], k[1])
	}
	for _, ids := range out {
		sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	}
	return out
}

// StockNotifier 负责登记、提交跨 0 通知，并实现回查分支。一个进程一个，所有 Local 共用
// （WithStockNotifier）。nil 表示不发通知 —— 测试与没配通知的拆分部署走这一支，有货标记只靠 core 的
// 低频全量刷新兜底（service.DefaultStockFlagInterval）。
type StockNotifier struct {
	store  *repository.InventoryStore
	action string       // 投递目标：单体 local://stock_changed；微服务 topic://stock.zero_crossing（谁订阅谁收）
	query  string       // 回查地址：单体 local://…；微服务是库存服务自己内网上的 HTTP 地址
	tc     atomic.Value // coordBox
	sent   atomic.Int64

	// 渠道层（channel_gate.go）：开了渠道的商家，可售数每次变化（不只是跨 0）另发一条 stock.changed。
	// gate 为 nil 或进程开关关闭时，这一支一行代码都不走、一次库都不查。
	gate          *ChannelGate
	changedAction string // 单体 local://channel_stock_changed；微服务 topic://stock.changed
	changedSent   atomic.Int64
}

// coordBox 让 atomic.Value 里永远是同一个具体类型（atomic.Value 不许换类型）。
type coordBox struct{ c dtm.Coordinator }

// NewStockNotifier 建通知器。store 是库存池上的仓储（回查要在那个库里插屏障），action 是 core 接收分支的
// 地址（dtm.BranchResolver.BranchURL(BranchStockChanged) 的结果：单体 local://，拆分 http://）。
//
// 协调器要等所有分支注册完才能启动，而回查分支就在这个对象上 —— 所以协调器是 Start 之后再 Attach 的，
// 与 service.OrderService.AttachCoordinator 同一个环、同一个解法。Attach 之前的改库存不发通知（只记一行日志）：
// 那段时间进程还没开始监听，正常路径上走不到。
func NewStockNotifier(store *repository.InventoryStore, action, query string) *StockNotifier {
	return &StockNotifier{store: store, action: action, query: query}
}

// 渠道层的库存变化通知：主题（微服务）与 core 的接收分支名（单体 local://）。
const (
	TopicStockChanged         = "stock.changed"
	BranchChannelStockChanged = "channel_stock_changed"
)

// WithChannels 打开渠道层的 stock.changed：gate 决定哪些商家发，action 是投递目标。
func (n *StockNotifier) WithChannels(g *ChannelGate, action string) *StockNotifier {
	n.gate, n.changedAction = g, action
	return n
}

// ChangedSent 是登记成功的 stock.changed 条数，只为可观察。
func (n *StockNotifier) ChangedSent() int64 {
	if n == nil {
		return 0
	}
	return n.changedSent.Load()
}

// TopicStockZeroCrossing 是跨 0 通知的主题（微服务形态）：库存只认这个名字，不知道谁在听；core 启动时订阅。
const TopicStockZeroCrossing = "stock.zero_crossing"

// Attach 接上已经启动的协调器。
func (n *StockNotifier) Attach(tc dtm.Coordinator) { n.tc.Store(coordBox{tc}) }

func (n *StockNotifier) coord() dtm.Coordinator {
	b, _ := n.tc.Load().(coordBox)
	return b.c
}

// Sent 是登记成功的消息数，只为可观察（测试断言「不跨 0 不发」）。
func (n *StockNotifier) Sent() int64 { return n.sent.Load() }

// QueryBranch 是回查分支（local://inventory_stock_msg_query）：本地事务提交了没有。
// 注册在发消息的那个协调器上，必须在它 Start 之前。
func (n *StockNotifier) QueryBranch() dtm.BranchFunc {
	return func(gid, branchID, op string) int {
		_, err := ParseStockMsgGID(gid)
		var ctx context.Context
		if err == nil {
			ctx, _, err = dtm.TenantContextFromTenantGID(context.Background(), StockMsgGIDPrefix, gid)
		}
		if err != nil {
			// 不是我们的 gid：答「没提交」让它作废，比让它一直停在 prepared 更好查。
			slog.Error("跨 0 通知的回查拿到的 gid 不成立，按未提交作废", "gid", gid, "err", err)
			return dtm.Failure
		}
		committed, err := n.store.QueryPreparedMsg(ctx, gid)
		if err != nil {
			slog.Warn("跨 0 通知的回查失败，协调器会再问", "gid", gid, "err", err)
			return dtm.Unknown
		}
		if !committed {
			slog.Info("跨 0 通知的回查：本地事务没提交，作废", "gid", gid)
			return dtm.Failure
		}
		return dtm.Success
	}
}

// prepare 在调用方的库存事务里登记跨 0 通知：先 PrepareMsg，再在同一个事务里插回查屏障。
// 返回登记了的 gid，调用方在事务结束后交给 finish。
//
// 登记失败（协调器的存储不可用）返回错误，让**整个库存事务回滚** —— 这是二阶段消息的本义：宁可这一次
// 改库存失败（SAGA 分支会重试，后台写回错让人再点一次），也不要改了而通知丢了。
func (n *StockNotifier) prepare(ctx context.Context, tx repository.InventoryStoreTx, c *stockCrossings) ([]string, error) {
	if n == nil {
		return nil, nil
	}
	crossed := c.crossed()
	var changed map[int64][]int64
	if n.gate.Enabled() {
		if ch := c.changed(); len(ch) > 0 {
			merchantID, err := tenant.FromContext(ctx)
			if err != nil {
				return nil, err
			}
			ok, err := n.gate.allows(ctx, tx, merchantID)
			if err != nil {
				return nil, err
			}
			if ok {
				changed = ch
			}
		}
	}
	if len(crossed) == 0 && len(changed) == 0 {
		return nil, nil
	}
	tc := n.coord()
	if tc == nil {
		slog.WarnContext(ctx, "可售数变了，但协调器还没接上，这次不发通知（有货标记等全量刷新兜底，渠道等对账兜底）")
		return nil, nil
	}
	merchantID, err := tenant.FromContext(ctx)
	if err != nil {
		return nil, err
	}
	var gids []string
	if err := n.register(ctx, tc, tx, merchantID, crossed, n.action, &gids); err != nil {
		return nil, err
	}
	sentCrossed := len(gids)
	if err := n.register(ctx, tc, tx, merchantID, changed, n.changedAction, &gids); err != nil {
		return nil, err
	}
	n.sent.Add(int64(sentCrossed))
	n.changedSent.Add(int64(len(gids) - sentCrossed))
	return gids, nil
}

// register 为每家门店登记一条消息（载荷只有门店与 SKU 的键），投到 action，并在本事务里占下回查屏障。
// 两种通知（跨 0、stock.changed）共用 gid 形状与回查分支：回查只问「本地事务提交了没有」，与内容无关。
func (n *StockNotifier) register(ctx context.Context, tc dtm.Coordinator, tx repository.InventoryStoreTx,
	merchantID int64, byStore map[int64][]int64, action string, gids *[]string) error {
	stores := make([]int64, 0, len(byStore))
	for s := range byStore {
		stores = append(stores, s)
	}
	sort.Slice(stores, func(i, j int) bool { return stores[i] < stores[j] })
	for _, storeID := range stores {
		gid, err := stockMsgGID(merchantID)
		if err != nil {
			return err
		}
		payload, err := json.Marshal(StockMsgPayload{StoreID: storeID, SKUIDs: byStore[storeID]})
		if err != nil {
			return err
		}
		// 发到主题时打开 allow_empty_topic：订阅方还没订阅上（刚起、或订阅被误删）不该挡住扣库存；漏的由兜底补。
		allowEmpty := strings.HasPrefix(action, dtm.TopicPrefix)
		if err := tc.PrepareMsgEx(gid, []string{action}, []string{string(payload)}, n.query, stockMsgGraceSecs, allowEmpty); err != nil {
			n.abort(ctx, tc, *gids)
			return err
		}
		*gids = append(*gids, gid)
		ok, err := tx.MarkMsgPrepared(ctx, gid)
		if err != nil {
			n.abort(ctx, tc, *gids)
			return err
		}
		if !ok {
			// 回查抢在我们之前判了「没提交」（登记之后 30 秒这个事务还没走到这里 —— 不正常，但可能）。
			// 这条消息已经作废，而库存变动照样要提交：回滚整个事务去保一条通知是本末倒置。
			// 换一个新 gid 在提交之后单独发一次（sendAfterCommit），通知不丢。
			return fmt.Errorf("%w: %s", errMsgLostToQuery, gid)
		}
	}
	return nil
}

var errMsgLostToQuery = errors.New("跨 0 通知的回查屏障已被回查抢占")

// finish 在库存事务结束之后提交或作废消息。提交失败只记日志：消息停在 prepared，
// grace 秒后回查看到屏障那一行，照样投递。
func (n *StockNotifier) finish(ctx context.Context, gids []string, committed bool) {
	if n == nil || len(gids) == 0 {
		return
	}
	tc := n.coord()
	if !committed {
		n.abort(ctx, tc, gids)
		return
	}
	for _, gid := range gids {
		if err := tc.SubmitMsg(gid); err != nil {
			slog.WarnContext(ctx, "跨 0 通知提交失败，回查会接着投递", "gid", gid, "err", err)
		}
	}
}

func (n *StockNotifier) abort(ctx context.Context, tc dtm.Coordinator, gids []string) {
	for _, gid := range gids {
		if err := tc.AbortMsg(gid); err != nil {
			// 作废失败不要紧：屏障那一行没提交，回查会插下 rollback 标记并作废它。
			slog.DebugContext(ctx, "作废跨 0 通知失败，交给回查", "gid", gid, "err", err)
		}
	}
}

// sendAfterCommit 是 prepare 撞上 errMsgLostToQuery 之后的补发：库存变动已经单独提交了，
// 这里用新的 gid 登记、在一个只插屏障的小事务里占位、再提交。
func (n *StockNotifier) sendAfterCommit(ctx context.Context, c *stockCrossings) {
	var gids []string
	err := n.store.WithTenant(ctx, func(tx repository.InventoryStoreTx) error {
		var e error
		gids, e = n.prepare(ctx, tx, c)
		return e
	})
	n.finish(ctx, gids, err == nil)
	if err != nil {
		slog.ErrorContext(ctx, "跨 0 通知补发失败（有货标记等全量刷新兜底）", "err", err)
	}
}

// WithStockNotifier 让这个 Local 在可售数跨 0 时发通知。一个进程里的所有 Local 应当共用同一个通知器。
func (l *Local) WithStockNotifier(n *StockNotifier) *Local {
	l.notify = n
	return l
}

// stockTx 是改可售数的那几条路径共用的事务外壳：run 开事务（WithTenant 或 WithSagaBranch），
// body 在事务里改库存并把前后水位记进 rec，外壳在同一个事务里登记跨 0 通知，事务结束后提交或作废。
func (l *Local) stockTx(ctx context.Context, run func(func(repository.InventoryStoreTx) error) error,
	body func(tx repository.InventoryStoreTx, rec *stockCrossings) error) error {
	var rec stockCrossings
	var gids []string
	lost := false
	err := run(func(tx repository.InventoryStoreTx) error {
		rec = stockCrossings{} // 事务被重跑时（不会，但 run 是调用方给的）从头记
		if err := body(tx, &rec); err != nil {
			return err
		}
		var e error
		gids, e = l.notify.prepare(ctx, tx, &rec)
		if e != nil && isLostToQuery(e) {
			lost, gids = true, nil
			return nil
		}
		return e
	})
	l.notify.finish(ctx, gids, err == nil)
	if err == nil && lost {
		slog.WarnContext(ctx, "跨 0 通知被回查抢先作废，库存变动已提交，换新 gid 补发")
		l.notify.sendAfterCommit(ctx, &rec)
	}
	return err
}

func isLostToQuery(err error) bool { return errors.Is(err, errMsgLostToQuery) }
