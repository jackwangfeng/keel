// Command keel-loadtest 是 Keel 自己的小压测器（docs/性能压测-2026-10.md）。
//
// 只用标准库：闭环并发（每个 worker 发完一个请求再发下一个），按场景生成请求，
// 输出每个接口标签的 RPS、p50 / p95 / p99 / max、按状态码与 problem type 的错误分布；
// 可选地在压测期间采样 docker stats（各容器 CPU）与 pg_stat_activity（连接按 state / 等锁计数）。
//
// 输入是 scripts/loadtest/seed 写出的 fixture（商品 ID 范围、门店与各店有货 SKU 抽样、买家与地址）。
// 买家令牌按契约 POST /auth/login 换，缓存在 -tokens 文件里（access_token 2 小时有效）。
//
// 用法见 scripts/loadtest/README.md。典型：
//
//	keel-loadtest -fixture fx.json -scenario list-default -c 32,64,128 -d 60s \
//	    -sample keelchaos-app-1,keelchaos-inventory-1,keelchaos-postgres-1,keelchaos-postgres-inventory-1 \
//	    -pg keelchaos-postgres-1:keel,keelchaos-postgres-inventory-1:keel_inventory
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type storeFix struct {
	ID   int64   `json:"id"`
	Code string  `json:"code"`
	Lat  float64 `json:"lat"`
	Lng  float64 `json:"lng"`
	SKUs []int64 `json:"skus"`
}

type userFix struct {
	ID        int64   `json:"id"`
	Phone     string  `json:"phone"`
	AddressID int64   `json:"address_id"`
	StoreID   int64   `json:"store_id"`
	Lat       float64 `json:"lat"`
	Lng       float64 `json:"lng"`
	Token     string  `json:"-"`
}

type fixture struct {
	MerchantID     int64      `json:"merchant_id"`
	DefaultStoreID int64      `json:"default_store_id"`
	Password       string     `json:"password"`
	ProductIDMin   int64      `json:"product_id_min"`
	ProductIDMax   int64      `json:"product_id_max"`
	TopCategoryIDs []int64    `json:"top_category_ids"`
	LeafCategories []int64    `json:"leaf_category_ids"`
	Stores         []storeFix `json:"stores"`
	Users          []userFix  `json:"users"`
	HotSKU         int64      `json:"hot_sku"`
	OrderNos       []string   `json:"order_nos"`
	Phones         []string   `json:"receiver_phones"`

	storeByID map[int64]*storeFix
}

type config struct {
	base       string
	fx         *fixture
	adminToken string
	flashSKU   int64
	pay        bool
	client     *http.Client
}

func main() {
	base := flag.String("base", "http://127.0.0.1:38180", "服务地址（不含 /api/v1）")
	fxPath := flag.String("fixture", "loadtest-fixture.json", "scripts/loadtest/seed 写出的 fixture")
	scenario := flag.String("scenario", "", "场景名（-list 看全部）")
	list := flag.Bool("list", false, "列出场景")
	concs := flag.String("c", "32", "并发数；逗号分隔表示逐级加压（拐点测试）")
	dur := flag.Duration("d", 60*time.Second, "每一级的持续时间")
	warm := flag.Duration("warmup", 5*time.Second, "每一级开始统计前的预热时间")
	pause := flag.Duration("pause", 10*time.Second, "两级之间停多久")
	nUsers := flag.Int("users", 1000, "参与的买家数（从 fixture 里取前 N 个，登录换令牌）")
	tokens := flag.String("tokens", "loadtest-tokens.json", "买家令牌缓存（手机号 → access_token）")
	adminTok := flag.String("admin-token", os.Getenv("KEEL_LT_ADMIN_TOKEN"), "后台会话令牌（后台场景要；也可设 KEEL_LT_ADMIN_TOKEN）")
	flashSKU := flag.Int64("flash-sku", 0, "秒杀场景的 SKU（setup-flash 场景会建活动并打印它）")
	pay := flag.Bool("pay", true, "下单场景是否接着发起沙箱支付并回调")
	sample := flag.String("sample", "", "压测期间 docker stats 采样的容器（逗号分隔）")
	pg := flag.String("pg", "", "压测期间采样 pg_stat_activity：容器:库名，逗号分隔")
	jsonOut := flag.String("json", "", "把结果追加写成 JSON 行到这个文件")
	cats := flag.String("categories", "", "list-category 只用这几个类目 ID（逗号分隔；默认用 fixture 里的压测类目）")
	flag.Parse()

	if *list || *scenario == "" {
		names := make([]string, 0, len(scenarios))
		for n := range scenarios {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			fmt.Printf("%-20s %s\n", n, scenarios[n].desc)
		}
		return
	}
	sc, ok := scenarios[*scenario]
	if !ok {
		log.Fatalf("没有场景 %q（-list 看全部）", *scenario)
	}

	fx := loadFixture(*fxPath)
	if *cats != "" {
		fx.TopCategoryIDs = nil
		for _, c := range splitNonEmpty(*cats) {
			id, err := strconv.ParseInt(c, 10, 64)
			if err != nil {
				log.Fatalf("-categories 不对：%q", *cats)
			}
			fx.TopCategoryIDs = append(fx.TopCategoryIDs, id)
		}
		fx.LeafCategories = fx.TopCategoryIDs
	}
	maxC := 0
	for _, s := range strings.Split(*concs, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(s))
		if err != nil || n <= 0 {
			log.Fatalf("-c 不对：%q", *concs)
		}
		maxC = max(maxC, n)
	}
	// 不走代理：本机常设 HTTP_PROXY，压测流量不该绕出去。
	tr := &http.Transport{
		Proxy:               nil,
		DialContext:         (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		MaxIdleConns:        maxC * 2,
		MaxIdleConnsPerHost: maxC * 2,
		IdleConnTimeout:     90 * time.Second,
	}
	cfg := &config{base: strings.TrimRight(*base, "/"), fx: fx, adminToken: *adminTok, flashSKU: *flashSKU, pay: *pay,
		client: &http.Client{Transport: tr, Timeout: 30 * time.Second}}

	if sc.needUsers {
		if *nUsers > len(fx.Users) {
			*nUsers = len(fx.Users)
		}
		fx.Users = fx.Users[:*nUsers]
		loginUsers(cfg, *tokens)
	}
	if sc.needAdmin && cfg.adminToken == "" {
		log.Fatal("这个场景要后台令牌：-admin-token 或 KEEL_LT_ADMIN_TOKEN")
	}
	if sc.setup != nil {
		sc.setup(cfg)
		return
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	levels := strings.Split(*concs, ",")
	for i, s := range levels {
		n, _ := strconv.Atoi(strings.TrimSpace(s))
		res := runLevel(ctx, cfg, sc, *scenario, n, *dur, *warm, splitNonEmpty(*sample), splitNonEmpty(*pg))
		res.print(os.Stdout)
		if *jsonOut != "" {
			res.appendJSON(*jsonOut)
		}
		if ctx.Err() != nil {
			break
		}
		if i < len(levels)-1 {
			select {
			case <-time.After(*pause):
			case <-ctx.Done():
			}
		}
	}
}

func splitNonEmpty(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func loadFixture(path string) *fixture {
	b, err := os.ReadFile(path)
	if err != nil {
		log.Fatal(err)
	}
	var fx fixture
	if err := json.Unmarshal(b, &fx); err != nil {
		log.Fatal(err)
	}
	fx.storeByID = map[int64]*storeFix{}
	for i := range fx.Stores {
		fx.storeByID[fx.Stores[i].ID] = &fx.Stores[i]
	}
	return &fx
}

// loginUsers 按契约 POST /auth/login 给每个买家换令牌，缓存到文件里（1.5 小时内复用）。
func loginUsers(cfg *config, cache string) {
	type entry struct {
		Token string    `json:"token"`
		At    time.Time `json:"at"`
	}
	cached := map[string]entry{}
	if b, err := os.ReadFile(cache); err == nil {
		_ = json.Unmarshal(b, &cached)
	}
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)
	fails := 0
	start := time.Now()
	todo := 0
	for i := range cfg.fx.Users {
		u := &cfg.fx.Users[i]
		mu.Lock()
		e, ok := cached[u.Phone]
		mu.Unlock()
		if ok && time.Since(e.At) < 90*time.Minute {
			u.Token = e.Token
			continue
		}
		todo++
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			body := fmt.Sprintf(`{"phone":%q,"password":%q}`, u.Phone, cfg.fx.Password)
			st, b, _, err := doReq(cfg.client, "POST", cfg.base+"/api/v1/auth/login", body, nil)
			var lr struct {
				AccessToken string `json:"access_token"`
			}
			if err == nil && st == 200 && json.Unmarshal(b, &lr) == nil && lr.AccessToken != "" {
				mu.Lock()
				u.Token = lr.AccessToken
				cached[u.Phone] = entry{lr.AccessToken, time.Now()}
				mu.Unlock()
				return
			}
			mu.Lock()
			fails++
			if fails <= 3 {
				log.Printf("登录失败 %s：%d %v %.200s", u.Phone, st, err, b)
			}
			mu.Unlock()
		}()
	}
	wg.Wait()
	if todo > 0 {
		log.Printf("登录 %d 个买家用时 %.1fs，失败 %d", todo, time.Since(start).Seconds(), fails)
		b, _ := json.Marshal(cached)
		_ = os.WriteFile(cache, b, 0o600)
	}
	ok := cfg.fx.Users[:0]
	for _, u := range cfg.fx.Users {
		if u.Token != "" {
			ok = append(ok, u)
		}
	}
	cfg.fx.Users = ok
	if len(ok) == 0 {
		log.Fatal("一个买家都没登录上")
	}
}
