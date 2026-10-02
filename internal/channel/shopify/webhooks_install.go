package shopify

import (
	"context"
	"fmt"

	"github.com/keel/keel/internal/channel"
)

// webhookTopics 是这个适配器要订阅的主题（第三期加订单主题）。
var webhookTopics = []string{"PRODUCTS_CREATE", "PRODUCTS_UPDATE", "PRODUCTS_DELETE", "INVENTORY_LEVELS_UPDATE", "APP_UNINSTALLED"}

const queryWebhooks = `query Webhooks{ webhookSubscriptions(first:100){ nodes{ topic uri } } }`

const mutationCreateWebhook = `mutation CreateWebhook($t:WebhookSubscriptionTopic!,$u:String!){
  webhookSubscriptionCreate(topic:$t, webhookSubscription:{uri:$u}){ userErrors{ field message } } }`

// EnsureWebhooks 装上缺的订阅：同主题同地址已有的不重复建；同主题、地址不同的旧订阅不删（可能是别的环境在用，交给人）。
func (a *Adapter) EnsureWebhooks(ctx context.Context, b channel.Binding, callbackURL string) error {
	var out struct {
		Subs struct {
			Nodes []struct {
				Topic string `json:"topic"`
				URI   string `json:"uri"`
			} `json:"nodes"`
		} `json:"webhookSubscriptions"`
	}
	if err := a.gql(ctx, b, "Webhooks", queryWebhooks, map[string]any{}, &out); err != nil {
		return err
	}
	have := map[string]bool{}
	for _, n := range out.Subs.Nodes {
		if n.URI == callbackURL {
			have[n.Topic] = true
		}
	}
	for _, t := range webhookTopics {
		if have[t] {
			continue
		}
		var r struct {
			R struct {
				UserErrors []userError `json:"userErrors"`
			} `json:"webhookSubscriptionCreate"`
		}
		if err := a.gql(ctx, b, "CreateWebhook", mutationCreateWebhook, map[string]any{"t": t, "u": callbackURL}, &r); err != nil {
			return err
		}
		if len(r.R.UserErrors) > 0 {
			return fmt.Errorf("Shopify 没让装 %s 回调：%s", t, r.R.UserErrors[0].Message)
		}
	}
	return nil
}
