package service

import (
	"reflect"
	"testing"
	"time"

	"github.com/keel/keel/internal/repository"
)

func TestSegmentMembers(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	ago := func(d int) *time.Time { x := now.Add(-time.Duration(d) * 24 * time.Hour); return &x }
	stats := []repository.CustomerStat{
		{UserID: 1, PaidOrders: 3, Paid90dCents: 30000, LastPaidAt: ago(40)},                    // 沉睡（30 天口径）
		{UserID: 2, PaidOrders: 1, Paid90dCents: 5000, LastPaidAt: ago(10)},                     // 只买过一次
		{UserID: 3, PaidOrders: 1, Paid90dCents: 5000, LastPaidAt: ago(3)},                      // 只买过一次但才 3 天：不算
		{UserID: 4, PaidOrders: 0, LastCartAt: ago(2)},                                          // 加购未买（从没买过）
		{UserID: 5, PaidOrders: 2, Paid90dCents: 90000, LastPaidAt: ago(5), LastCartAt: ago(1)}, // 加购晚于上次付款
		{UserID: 6, PaidOrders: 2, Paid90dCents: 8000, LastPaidAt: ago(1), LastCartAt: ago(2)},  // 加购后付了款：不算
		{UserID: 7, PaidOrders: 0},                                                              // 只注册过
	}
	cases := map[string][]int64{
		SegmentLapsed:      {1},
		SegmentOneTime:     {2},
		SegmentCartAbandon: {4, 5},
		SegmentHighValue:   {5}, // 付过款的 5 人里前 20% = ceil(1) = 1 人，付得最多的是 5
	}
	for seg, want := range cases {
		if got := segmentMembers(stats, seg, 30, now); !reflect.DeepEqual(got, want) {
			t.Errorf("%s：得到 %v，期望 %v", seg, got, want)
		}
	}
	// 沉睡口径放宽到 7 天：上次付款在 10 天前的 2 也算
	if got := segmentMembers(stats, SegmentLapsed, 7, now); !reflect.DeepEqual(got, []int64{1, 2}) {
		t.Errorf("lapsed(7)：%v", got)
	}
}

func TestSplitHoldoutIsStableAndProportional(t *testing.T) {
	var members []int64
	for i := int64(1); i <= 200; i++ {
		members = append(members, i)
	}
	tr, ct := splitHoldout(42, members, 20)
	if len(ct) != 40 || len(tr) != 160 {
		t.Fatalf("20%% 对照：%d / %d", len(ct), len(tr))
	}
	tr2, ct2 := splitHoldout(42, members, 20)
	if !reflect.DeepEqual(ct, ct2) || !reflect.DeepEqual(tr, tr2) {
		t.Fatal("同一条提案重算一次，分组应当不变")
	}
	_, ct3 := splitHoldout(43, members, 20)
	if reflect.DeepEqual(ct, ct3) {
		t.Fatal("不同提案的对照组不该一模一样")
	}
	// 对照组不能只是 id 最小的那一截（那会和注册早晚相关）
	small := 0
	for _, id := range ct {
		if id <= 40 {
			small++
		}
	}
	if small > 20 {
		t.Fatalf("对照组偏向小 id：%d / 40", small)
	}
}

func TestCouponTargetReview(t *testing.T) {
	g := func(treated bool, n, conv, paid int64) repository.CouponTargetGroup {
		return repository.CouponTargetGroup{Treated: treated, Members: n, Converted: conv, PaidCents: paid}
	}
	// 发券组 160 人 32 人下单（20%）、人均 ¥30；对照组 40 人 4 人下单（10%）、人均 ¥15。
	// 提升 0.1 → 增量顾客 16；增量金额 (3000 − 1500) × 160 = 240000 分；券成本 32 × 500 = 16000 → positive
	var o ProposalOutcome
	couponTargetReview(&o, g(true, 160, 32, 480000), g(false, 40, 4, 60000), 16000)
	if o.Verdict != verdictPositive || *o.Lift != 0.1 || *o.IncrementalBuyers != 16 || *o.IncrementalCents != 240000 {
		t.Fatalf("positive：%s lift=%v buyers=%v inc=%v", o.Verdict, *o.Lift, *o.IncrementalBuyers, *o.IncrementalCents)
	}
	// 发券组不比对照组好 → negative
	o = ProposalOutcome{}
	couponTargetReview(&o, g(true, 160, 16, 160000), g(false, 40, 4, 40000), 8000)
	if o.Verdict != verdictNegative {
		t.Fatalf("没有提升应为 negative：%s", o.Verdict)
	}
	// 有提升但增量金额抵不过券成本 → neutral
	o = ProposalOutcome{}
	couponTargetReview(&o, g(true, 100, 12, 12000), g(false, 40, 4, 4000), 50000)
	if o.Verdict != verdictNeutral {
		t.Fatalf("没赚回券钱应为 neutral：%s", o.Verdict)
	}
	// 对照组太小：只给数
	o = ProposalOutcome{}
	couponTargetReview(&o, g(true, 40, 20, 100000), g(false, 8, 0, 0), 1000)
	if o.Verdict != verdictNeutral || o.Lift == nil || *o.HoldoutSize != 8 {
		t.Fatalf("对照组不到 30 人应只给数：%+v", o)
	}
}
