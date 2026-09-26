// Command keel-index 把商品的派生数据**全量**过一遍：文本向量 + bigram 关键词串。
//
// # 它与进程里那个定时任务的分工
//
// 服务进程里跑的是**增量**（internal/service.IndexService.Run）：它靠
// products.updated_at 这个触发点找活干，30 秒一轮。触发点扫不到的东西有两类，
// 这条命令补的就是它们：
//
//	① **存量**。00016 落地的那一刻，全库 search_text 都是 NULL、向量表是空的，
//	   而没有任何一行 products.updated_at 因此前进。增量任务看不见它们。
//	   （ListStaleProductsForIndex 里那两个 `OR` 兜住了这一类，所以增量最终
//	   也会把它们扫完 —— 但那要 N/每轮预算 轮，而一次全量就完事。）
//	② **换模型 / 改拼接模板**。那时每一件商品的输入字面都没变，变的是
//	   「同样的输入该算出什么」。-force 跳过判定，无条件重算。
//
// # 用法
//
//	KEEL_EMBED_ENDPOINT=http://127.0.0.1:8001 go run ./cmd/keel-index
//	KEEL_EMBED_ENDPOINT=... go run ./cmd/keel-index -merchant 3
//	KEEL_EMBED_ENDPOINT=... go run ./cmd/keel-index -force      # 换模型之后
//
// 不带 -merchant 时遍历全部活跃商家（repository.ActiveMerchants，与定时任务
// 同一个入口，理由写在 repository/sweep.go 的文件头）。
//
// # -force 要花钱，所以它要人打出来
//
// 它不是配置项、也没有环境变量对应物。一个能被配成 true 的 force 意味着
// 某个部署会每 30 秒把全店商品重算一遍 —— 那笔钱会一直花，而搜索结果
// 看上去完全正常。
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/keel/keel/internal/db"
	"github.com/keel/keel/internal/inference"
	"github.com/keel/keel/internal/repository"
	"github.com/keel/keel/internal/service"
)

func main() {
	merchant := flag.Int64("merchant", 0, "只跑这一家商户的 id；0 = 全部活跃商家")
	force := flag.Bool("force", false,
		"跳过指纹判定，无条件重算（换模型或改拼接模板之后用，会真的花钱）")
	flag.Parse()

	if err := run(context.Background(), *merchant, *force); err != nil {
		fmt.Fprintln(os.Stderr, "全量索引失败:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, merchant int64, force bool) error {
	// 与服务进程同一个池构造函数：它把「这条连接能不能绕过 RLS」的自检挂在
	// 每条物理连接上。裸 pgxpool.New 建出来的池没有那道自检，而一条能绕过 RLS
	// 的连接会让这条命令把 A 店的商品写到 B 店名下也不报错。
	pool, err := db.NewPool(ctx)
	if err != nil {
		return fmt.Errorf("建连接池失败: %w", err)
	}
	defer pool.Close()

	emb, err := inference.FromEnv()
	if err != nil {
		return err
	}
	repo := repository.New(pool)
	log := slog.Default()
	idx, err := service.NewIndexService(repo, emb, service.IndexConfig{}, log)
	if err != nil {
		return err
	}

	merchants := []int64{merchant}
	if merchant == 0 {
		if merchants, err = repo.ActiveMerchants(ctx); err != nil {
			return err
		}
	}
	if len(merchants) == 0 {
		return fmt.Errorf("一家活跃商家都没有 —— 没有东西可索引")
	}

	started := time.Now()

	// 两段：先把全部商户的商品**入队**，再把队列抽干。
	//
	// 分两段而不是一家一家「入队 + 处理」跑完再下一家，是为了让每租户在途上限
	// 真的有事可做（数据模型 §12 / 商品理解服务设计 §8）：全部入队之后，
	// 出队那条 SQL 才同时看得见多家店的待办，公平调度才有得调。
	// 一家一家串着跑的话，队列里永远只有一家店的任务，那条上限一次都不生效 ——
	// 而这条命令恰恰是 §8 那个场景（「一个新商家导入一次商品目录」）的发源地。
	var total service.IndexReport
	for _, m := range merchants {
		rep, err := idx.Backfill(ctx, m, force)
		if err != nil {
			return fmt.Errorf("商户 %d: %w", m, err)
		}
		log.Info("全量入队完成一家商户", "merchant_id", m,
			"scanned", rep.Scanned, "enqueued", rep.Enqueued, "deduped", rep.Deduped)
		total = total.Merge(rep)
	}

	// 抽干。
	//
	// **它会连别人入队的任务一起做掉** —— 队列是共享的，出队按 (priority, id)
	// 给出全局顺序，不按「是谁入的」。这不是缺陷：这条命令的目的就是把活干完，
	// 而多做几条别人的任务与少做几条自己的相比，前者无害。
	// 真正要注意的是反过来：服务进程如果正开着，它自己的消费者也在抽同一个队列，
	// 于是这条命令报出来的数字会**少于**它入队的数量。那也是对的。
	work, err := idx.Drain(ctx)
	if err != nil {
		return fmt.Errorf("抽干队列失败: %w", err)
	}
	total = total.Merge(work)

	fmt.Printf("全量索引完成：%d 家商户，扫描 %d 件（入队 %d / 已在队列 %d），"+
		"判定 %d 件，重算向量 %d 条，重写 bigram 串 %d 条，跳过 %d 件，"+
		"竞态 %d 件，商品已不在 %d 件，失败 %d 件，死信 %d 条，引擎故障 %d 次，耗时 %s\n",
		len(merchants), total.Scanned, total.Enqueued, total.Deduped,
		total.Judged, total.Embedded, total.SearchTextWritten, total.Skipped,
		total.Raced, total.Gone, total.Failed, total.DeadLettered, total.EngineDown,
		time.Since(started).Round(time.Millisecond))

	// 失败要反映在退出码上。一条打印着「完成」却其实一半没做的命令，
	// 在 CI 或运维脚本里等于没跑。
	if total.Failed > 0 || total.EngineDown > 0 || total.DeadLettered > 0 {
		return fmt.Errorf("有 %d 件商品处理失败、%d 条任务进死信、%d 次引擎故障",
			total.Failed, total.DeadLettered, total.EngineDown)
	}
	return nil
}
