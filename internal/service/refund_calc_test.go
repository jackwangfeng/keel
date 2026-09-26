package service

import (
	"errors"
	"testing"

	"github.com/keel/keel/internal/repository"
)

// planRefund / refundLineAmount 是退款金额的唯一一份实现（refund_calc.go）。
// 不碰数据库，把 §11 的两条式子与 planRefund 的每一条拒绝逐分核对。

// 一行买 3 件、净额 100 分（除不尽）：逐件退，前两次 floor，最后一次吃掉余数，合计 = 净额。
func TestRefundLineAmountSumsToTheNetAmount(t *testing.T) {
	it := repository.RefundableItem{ID: 1, Quantity: 3, AmountCents: 150, DiscountCents: 50}
	var total int64
	want := []int64{33, 33, 34}
	for i := 0; i < 3; i++ {
		got := refundLineAmount(it, 1)
		if got != want[i] {
			t.Fatalf("第 %d 件退 %d 分，期望 %d", i+1, got, want[i])
		}
		it.RefundedQty++
		it.RefundedCents += got
		total += got
	}
	if total != 100 {
		t.Fatalf("三件退完合计 %d，期望 = 净额 100（「我退完了，钱没退全」）", total)
	}

	// 先退 2 件再退 1 件，合计同样 = 净额。
	it = repository.RefundableItem{ID: 1, Quantity: 3, AmountCents: 150, DiscountCents: 50}
	a := refundLineAmount(it, 2)
	it.RefundedQty, it.RefundedCents = 2, a
	b := refundLineAmount(it, 1)
	if a != 66 || a+b != 100 {
		t.Fatalf("先 2 后 1：%d + %d，期望 66 + 34 = 100", a, b)
	}

	// 一次退完 = 净额，不管之前有没有退过。
	it = repository.RefundableItem{ID: 1, Quantity: 3, AmountCents: 150, DiscountCents: 50}
	if got := refundLineAmount(it, 3); got != 100 {
		t.Fatalf("一次退完 3 件是 %d，期望 100", got)
	}
}

// 分摊到优惠的那一份不退：净额 = 行金额 - 分摊优惠，而不是按原价退。
// 这是 README「优惠按行分摊，所以部分退款能算对金额」的反例守卫 ——
// 把 DiscountCents 从式子里拿掉，这条会红。
func TestRefundLineAmountExcludesTheAllocatedDiscount(t *testing.T) {
	it := repository.RefundableItem{ID: 1, Quantity: 2, AmountCents: 12000, DiscountCents: 1412}
	if got := refundLineAmount(it, 1); got != 5294 {
		t.Fatalf("退 1 件是 %d，期望 floor((12000 - 1412) / 2) = 5294", got)
	}
}

func TestPlanRefundRejectsWhatTheContractRejects(t *testing.T) {
	items := []repository.RefundableItem{
		{ID: 1, Quantity: 2, AmountCents: 200, DiscountCents: 20},
		{ID: 2, Quantity: 1, AmountCents: 100, DiscountCents: 10, RefundedQty: 1, RefundedCents: 90},
		{ID: 3, Quantity: 3, AmountCents: 300},
	}
	inflight := map[int64]int32{3: 1}
	cases := []struct {
		name string
		req  []RefundLineInput
		want error
	}{
		{"空", nil, ErrRefundBadRequest},
		{"件数 0", []RefundLineInput{{1, 0}}, ErrRefundBadRequest},
		{"同一行两次", []RefundLineInput{{1, 1}, {1, 1}}, ErrRefundBadRequest},
		{"不属于这一单", []RefundLineInput{{9, 1}}, ErrOrderItemMismatch},
		{"在途的行", []RefundLineInput{{3, 1}}, ErrRefundAlreadyInProgress},
		{"超过购买 - 已退", []RefundLineInput{{2, 1}}, ErrRefundQuantityExceeded},
		{"超过购买", []RefundLineInput{{1, 3}}, ErrRefundQuantityExceeded},
	}
	for _, c := range cases {
		if _, err := planRefund(items, inflight, c.req); !errors.Is(err, c.want) {
			t.Errorf("%s：期望 %v，实得 %v", c.name, c.want, err)
		}
	}
}

// CoversEverything 只在「没有任何在途、每一行剩下的都退了」时为真 ——
// 它决定订单进不进 50、运费退不退。
func TestPlanRefundCoversEverything(t *testing.T) {
	items := []repository.RefundableItem{
		{ID: 1, Quantity: 2, AmountCents: 200, DiscountCents: 20},
		{ID: 2, Quantity: 1, AmountCents: 100, DiscountCents: 10, RefundedQty: 1, RefundedCents: 90},
	}
	p, err := planRefund(items, nil, []RefundLineInput{{1, 2}})
	if err != nil || !p.CoversEverything || p.Goods != 180 {
		t.Fatalf("退完第 1 行（第 2 行早已退完）应是整单、货款 180：%+v %v", p, err)
	}
	p, err = planRefund(items, nil, []RefundLineInput{{1, 1}})
	if err != nil || p.CoversEverything || p.Goods != 90 {
		t.Fatalf("只退 1 件不是整单、货款 90：%+v %v", p, err)
	}
	three := append(items, repository.RefundableItem{ID: 3, Quantity: 1, AmountCents: 50})
	p, err = planRefund(three, map[int64]int32{3: 1}, []RefundLineInput{{1, 2}})
	if err != nil || p.CoversEverything {
		t.Fatalf("还有别的行在途时不算整单：%+v %v", p, err)
	}
}
