package shopify

// 订单：取单规整（FetchOrder）与发货回传（Act ship → fulfillmentCreate）。字段与枚举见第三期计划「已核实的事实」。
//
// 规整约定：
//   - 金额是平台快照（字符串元 → 分，不走浮点）。Goods = Σ 原单价 × 下单数量；Freight = totalShippingPriceSet；
//     折扣（totalDiscountsSet）全记 MerchantSubsidy（Shopify 没有平台补贴与佣金）；BuyerPaid 不含税
//     = Goods + Freight − 折扣，税单独记 Tax；MerchantReceivable = BuyerPaid；Refunded = totalRefundedSet。
//   - 门店 = 未取消的 fulfillment order 的 assignedLocation；分到多个 location（或一个 FO 都没有）时为空，原因写 StoreError。
//   - Version = updatedAt 的 Unix 毫秒。
//   - 收货人：shippingAddress 为 null（没开受保护客户数据、或不需要发货）时全空，不报错。
//
// 发货回传的幂等（Review Focus 6）：先读这张单的 FO，没被取消的全是 CLOSED 才当成功——超时重试时上一次
// 其实已经生效，不再建第二条 fulfillment；mutation 本身还带 @idempotent(key: Action.IdemKey)。
// 有 FO 处在 ON_HOLD / SCHEDULED / INCOMPLETE 等（不是 OPEN / IN_PROGRESS / CLOSED / CANCELLED）时回可重试错误、一条都不发。

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/keel/keel/internal/channel"
)

const moneyFields = `shopMoney{ amount }`

const queryOrder = `query Order($id:ID!){ order(id:$id){
  id name createdAt updatedAt test cancelledAt displayFinancialStatus displayFulfillmentStatus phone email
  totalShippingPriceSet{ ` + moneyFields + ` } totalDiscountsSet{ ` + moneyFields + ` }
  totalTaxSet{ ` + moneyFields + ` } totalRefundedSet{ ` + moneyFields + ` }
  shippingAddress{ name phone address1 address2 city province zip countryCodeV2 }
  lineItems(first:250){ nodes{ id title quantity currentQuantity sku variant{ id } originalUnitPriceSet{ ` + moneyFields + ` } } }
  fulfillmentOrders(first:50){ nodes{ id status assignedLocation{ location{ id } } } }
  fulfillments(first:50){ id status createdAt trackingInfo(first:10){ company number } }
  refunds(first:100){ id createdAt totalRefundedSet{ ` + moneyFields + ` }
    refundLineItems(first:250){ nodes{ quantity restockType lineItem{ id } } } }
} }`

// queryOrderFOs 是发货前读 FO 的小查询（与取单同一个操作名，查询体不同）。
const queryOrderFOs = `query Order($id:ID!){ order(id:$id){ id
  fulfillmentOrders(first:50){ nodes{ id status assignedLocation{ location{ id } } } } } }`

const mutationFulfillmentCreate = `mutation FulfillmentCreate($fulfillment:FulfillmentInput!,$k:String!){
  fulfillmentCreate(fulfillment:$fulfillment) @idempotent(key:$k){ fulfillment{ id status } userErrors{ field message } } }`

type moneySet struct {
	ShopMoney struct {
		Amount string `json:"amount"`
	} `json:"shopMoney"`
}

type foNode struct {
	ID               string `json:"id"`
	Status           string `json:"status"`
	AssignedLocation struct {
		Location *struct {
			ID string `json:"id"`
		} `json:"location"`
	} `json:"assignedLocation"`
}

func (f foNode) location() string {
	if f.AssignedLocation.Location == nil {
		return ""
	}
	return f.AssignedLocation.Location.ID
}

type orderNode struct {
	ID              string     `json:"id"`
	Name            string     `json:"name"`
	CreatedAt       time.Time  `json:"createdAt"`
	UpdatedAt       time.Time  `json:"updatedAt"`
	Test            bool       `json:"test"`
	CancelledAt     *time.Time `json:"cancelledAt"`
	Financial       string     `json:"displayFinancialStatus"`
	Fulfillment     string     `json:"displayFulfillmentStatus"`
	Phone           *string    `json:"phone"`
	TotalShipping   moneySet   `json:"totalShippingPriceSet"`
	TotalDiscounts  moneySet   `json:"totalDiscountsSet"`
	TotalTax        moneySet   `json:"totalTaxSet"`
	TotalRefunded   moneySet   `json:"totalRefundedSet"`
	ShippingAddress *struct {
		Name        string  `json:"name"`
		Phone       *string `json:"phone"`
		Address1    string  `json:"address1"`
		Address2    *string `json:"address2"`
		City        string  `json:"city"`
		Province    string  `json:"province"`
		Zip         string  `json:"zip"`
		CountryCode string  `json:"countryCodeV2"`
	} `json:"shippingAddress"`
	LineItems struct {
		Nodes []struct {
			ID              string `json:"id"`
			Title           string `json:"title"`
			Quantity        int32  `json:"quantity"`
			CurrentQuantity int32  `json:"currentQuantity"`
			Variant         *struct {
				ID string `json:"id"`
			} `json:"variant"`
			OriginalUnitPrice moneySet `json:"originalUnitPriceSet"`
		} `json:"nodes"`
	} `json:"lineItems"`
	FulfillmentOrders struct {
		Nodes []foNode `json:"nodes"`
	} `json:"fulfillmentOrders"`
	Fulfillments []struct {
		ID           string    `json:"id"`
		Status       string    `json:"status"`
		CreatedAt    time.Time `json:"createdAt"`
		TrackingInfo []struct {
			Company *string `json:"company"`
			Number  *string `json:"number"`
		} `json:"trackingInfo"`
	} `json:"fulfillments"`
	Refunds []struct {
		ID              string    `json:"id"`
		CreatedAt       time.Time `json:"createdAt"`
		TotalRefunded   moneySet  `json:"totalRefundedSet"`
		RefundLineItems struct {
			Nodes []struct {
				Quantity    int32  `json:"quantity"`
				RestockType string `json:"restockType"`
				LineItem    struct {
					ID string `json:"id"`
				} `json:"lineItem"`
			} `json:"nodes"`
		} `json:"refundLineItems"`
	} `json:"refunds"`
}

// FetchOrder 回读一张订单（externalOrderID 是订单 gid）并规整。平台上没有这张单是普通错误（重试不会好）。
func (a *Adapter) FetchOrder(ctx context.Context, b channel.Binding, externalOrderID string) (channel.ChannelOrder, error) {
	var out struct {
		Order json.RawMessage `json:"order"`
	}
	if err := a.gql(ctx, b, "Order", queryOrder, map[string]any{"id": externalOrderID}, &out); err != nil {
		return channel.ChannelOrder{}, err
	}
	if len(out.Order) == 0 || string(out.Order) == "null" {
		return channel.ChannelOrder{}, fmt.Errorf("Shopify 上没有订单 %s", externalOrderID)
	}
	var n orderNode
	if err := json.Unmarshal(out.Order, &n); err != nil {
		return channel.ChannelOrder{}, fmt.Errorf("Shopify 订单 %s 解不开：%w", externalOrderID, err)
	}
	return normalizeOrder(n, out.Order)
}

func amount(field string, m moneySet) (int64, error) {
	if m.ShopMoney.Amount == "" {
		return 0, nil
	}
	c, err := parsePriceCents(m.ShopMoney.Amount)
	if err != nil {
		return 0, fmt.Errorf("Shopify 订单的 %s %q 认不出来：%v", field, m.ShopMoney.Amount, err)
	}
	return c, nil
}

// foLive：还算数的 FO。CANCELLED（被移走 / 取消）的不算；CLOSED 是发完或关掉了。
func foLive(status string) bool { return status != "CANCELLED" }

func foShippable(status string) bool { return status == "OPEN" || status == "IN_PROGRESS" }

func normalizeOrder(n orderNode, raw json.RawMessage) (channel.ChannelOrder, error) {
	o := channel.ChannelOrder{ExternalOrderID: n.ID, ExternalOrderName: n.Name, Version: n.UpdatedAt.UnixMilli(),
		Test: n.Test, PlacedAt: n.CreatedAt, Delivery: channel.DeliveryExpress, Raw: raw,
		PlatformStatus: n.Financial + "/" + n.Fulfillment}
	if n.CancelledAt != nil {
		o.PlatformStatus += "/CANCELLED"
	}

	variantOf := map[string]string{}
	var goods int64
	for _, l := range n.LineItems.Nodes {
		price, err := amount("行单价", l.OriginalUnitPrice)
		if err != nil {
			return channel.ChannelOrder{}, err
		}
		line := channel.OrderLine{ExternalLineID: l.ID, Title: l.Title, Qty: l.Quantity, PriceCents: price,
			RefundedQty: max(0, l.Quantity-l.CurrentQuantity)}
		if l.Variant != nil {
			line.ExternalSKUID = l.Variant.ID // 变体被删了是空：渠道层按「行没映射」处理
		}
		variantOf[l.ID] = line.ExternalSKUID
		goods += price * int64(l.Quantity)
		o.Lines = append(o.Lines, line)
	}
	var err error
	am := &o.Amounts
	am.GoodsCents = goods
	if am.FreightCents, err = amount("运费", n.TotalShipping); err != nil {
		return channel.ChannelOrder{}, err
	}
	if am.MerchantSubsidyCents, err = amount("折扣", n.TotalDiscounts); err != nil {
		return channel.ChannelOrder{}, err
	}
	if am.TaxCents, err = amount("税", n.TotalTax); err != nil {
		return channel.ChannelOrder{}, err
	}
	if am.RefundedCents, err = amount("已退金额", n.TotalRefunded); err != nil {
		return channel.ChannelOrder{}, err
	}
	am.BuyerPaidCents = am.GoodsCents + am.FreightCents - am.MerchantSubsidyCents
	am.MerchantReceivableCents = am.BuyerPaidCents

	// 门店：未取消的 FO 的 location 去重。
	locs := map[string]bool{}
	allClosed := true
	for _, fo := range n.FulfillmentOrders.Nodes {
		if !foLive(fo.Status) {
			continue
		}
		if fo.Status != "CLOSED" {
			allClosed = false
		}
		if l := fo.location(); l != "" {
			locs[l] = true
		}
	}
	switch len(locs) {
	case 1:
		for l := range locs {
			o.ExternalStoreID = l
		}
	case 0:
		o.StoreError = "订单没有分到任何 location（没有有效的 fulfillment order）"
	default:
		ls := make([]string, 0, len(locs))
		for l := range locs {
			ls = append(ls, l)
		}
		sort.Strings(ls)
		o.StoreError = "订单分到了多个 location：" + strings.Join(ls, "、")
	}

	if a := n.ShippingAddress; a != nil {
		addr := strings.TrimSpace(a.Address1)
		if a.Address2 != nil && strings.TrimSpace(*a.Address2) != "" {
			addr += " " + strings.TrimSpace(*a.Address2)
		}
		o.Receiver = channel.Receiver{Name: a.Name, Province: a.Province, City: a.City, Address: addr, Zip: a.Zip, Country: a.CountryCode}
		switch {
		case a.Phone != nil && *a.Phone != "":
			o.Receiver.Phone = *a.Phone
		case n.Phone != nil:
			o.Receiver.Phone = *n.Phone
		}
	}

	shipped := 0
	for _, f := range n.Fulfillments {
		if f.Status == "CANCELLED" || f.Status == "ERROR" || f.Status == "FAILURE" {
			continue
		}
		shipped++
		if len(f.TrackingInfo) == 0 {
			o.Shipments = append(o.Shipments, channel.Shipment{At: f.CreatedAt})
		}
		for _, t := range f.TrackingInfo {
			s := channel.Shipment{At: f.CreatedAt}
			if t.Company != nil {
				s.Company = *t.Company
			}
			if t.Number != nil {
				s.TrackingNo = *t.Number
			}
			o.Shipments = append(o.Shipments, s)
		}
	}

	for _, r := range n.Refunds {
		amt, err := amount("退款金额", r.TotalRefunded)
		if err != nil {
			return channel.ChannelOrder{}, err
		}
		rf := channel.Refund{ExternalID: r.ID, AmountCents: amt, At: r.CreatedAt}
		for _, rl := range r.RefundLineItems.Nodes {
			rf.Lines = append(rf.Lines, channel.ActionLine{ExternalSKUID: variantOf[rl.LineItem.ID], Qty: rl.Quantity})
			switch rl.RestockType {
			case "RETURN", "CANCEL", "LEGACY_RESTOCK":
				rf.Restock = true
			}
		}
		o.Refunds = append(o.Refunds, rf)
	}

	o.Status = orderStatus(n.CancelledAt != nil, n.Financial, shipped > 0 && allClosed && len(n.FulfillmentOrders.Nodes) > 0)
	return o, nil
}

// orderStatus 规整状态。PARTIALLY_PAID 也当待付款（钱没收齐不接单）；VOIDED / EXPIRED（授权作废 / 过期）当取消。
// 部分发货仍是 New（keel 只认整单发货）。
func orderStatus(cancelled bool, financial string, shipped bool) channel.OrderStatus {
	switch {
	case cancelled:
		return channel.OrderCancelled
	case financial == "PENDING" || financial == "AUTHORIZED" || financial == "PARTIALLY_PAID":
		return channel.OrderPendingPayment
	case financial == "VOIDED" || financial == "EXPIRED":
		return channel.OrderCancelled
	case shipped:
		return channel.OrderShipped
	case financial == "REFUNDED":
		return channel.OrderCancelled
	default:
		return channel.OrderNew
	}
}

// Act 只支持发货回传；Caps 不声明接单 / 拒单 / 拣货 / 申请类，渠道层不会调。
func (a *Adapter) Act(ctx context.Context, b channel.Binding, ref channel.OrderRef, act channel.Action) error {
	if act.Kind != channel.ActShip {
		return channel.ErrUnsupported
	}
	return a.ship(ctx, b, ref.ExternalOrderID, act)
}

// ship：把还能发的 FO 全部发掉（全部剩余行，fulfillmentOrderLineItems 省略），带物流单号，通知顾客。
// 按 location 分组，一组一次 fulfillmentCreate（平台要求同一次的 FO 在同一个 location；正常单只有一组）。
func (a *Adapter) ship(ctx context.Context, b channel.Binding, orderID string, act channel.Action) error {
	var out struct {
		Order *struct {
			FulfillmentOrders struct {
				Nodes []foNode `json:"nodes"`
			} `json:"fulfillmentOrders"`
		} `json:"order"`
	}
	if err := a.gql(ctx, b, "Order", queryOrderFOs, map[string]any{"id": orderID}, &out); err != nil {
		return err
	}
	if out.Order == nil {
		return fmt.Errorf("Shopify 上没有订单 %s", orderID)
	}
	groups := map[string][]string{}
	var order, held []string
	live := 0
	for _, fo := range out.Order.FulfillmentOrders.Nodes {
		if !foLive(fo.Status) {
			continue
		}
		live++
		if fo.Status == "CLOSED" {
			continue
		}
		if !foShippable(fo.Status) {
			held = append(held, fo.ID+"（"+fo.Status+"）")
			continue
		}
		l := fo.location()
		if _, ok := groups[l]; !ok {
			order = append(order, l)
		}
		groups[l] = append(groups[l], fo.ID)
	}
	switch {
	case len(held) > 0:
		// 暂停 / 预约 / 不完整：现在发不了，也不是发过了。一条都不发（keel 只认整单发货），等店员在 Shopify 后台放开；
		// 一直放不开就进死信，渠道单标异常「发货没回传上」。不算限流，计入重试次数。
		return &channel.RetryableError{Err: fmt.Errorf("Shopify 上的 fulfillment order %s 现在不能发货（暂停 / 预约 / 不完整），"+
			"请在 Shopify 后台放开后等 keel 重试", strings.Join(held, "、"))}
	case len(order) == 0 && live == 0:
		return &channel.RetryableError{Err: fmt.Errorf("Shopify 上订单 %s 没有有效的 fulfillment order，发不了货", orderID)}
	case len(order) == 0:
		return nil // 没被取消的 FO 全是 CLOSED：上一次已经生效（或在 Shopify 后台发过了）
	}
	base := act.IdemKey
	if base == "" {
		h := sha256.Sum256([]byte(orderID + "|" + act.TrackingCompany + "|" + act.TrackingNo))
		base = "keel-ship-" + hex.EncodeToString(h[:])[:32]
	}
	for i, l := range order {
		var by []map[string]any
		for _, id := range groups[l] {
			by = append(by, map[string]any{"fulfillmentOrderId": id})
		}
		in := map[string]any{"notifyCustomer": true, "lineItemsByFulfillmentOrder": by,
			"trackingInfo": map[string]any{"company": act.TrackingCompany, "number": act.TrackingNo}}
		key := base
		if len(order) > 1 {
			key = fmt.Sprintf("%s:%d", base, i)
		}
		var r struct {
			R struct {
				UserErrors []userError `json:"userErrors"`
			} `json:"fulfillmentCreate"`
		}
		if err := a.gql(ctx, b, "FulfillmentCreate", mutationFulfillmentCreate, map[string]any{"fulfillment": in, "k": key}, &r); err != nil {
			return err
		}
		if len(r.R.UserErrors) > 0 {
			msgs := make([]string, len(r.R.UserErrors))
			for j, u := range r.R.UserErrors {
				msgs[j] = u.Message
			}
			return fmt.Errorf("Shopify 没让发货：%s", truncate(strings.Join(msgs, "；"), 300))
		}
	}
	return nil
}
