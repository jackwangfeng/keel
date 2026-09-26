-- 搜索效果的离线统计：按店铺 × 排序策略 × 实际跑过的阶段分组（语义检索层 §9.2 / §9.3）。
--
-- 用法（要一个能读全部租户的连接，也就是管理员角色 —— search_logs 有 RLS，
-- 用 keel_app 连只看得到空表，因为这条连接上没有租户上下文）：
--
--     make search-metrics                      # 最近 7 天
--     make search-metrics PERIOD='30 days'     # 最近 30 天
--
-- 连接走 libpq 的环境变量（PGHOST / PGPORT / PGUSER / PGPASSWORD / PGDATABASE）。
-- 也可以直接：
--
--     psql -X -v ON_ERROR_STOP=1 -v period='7 days' -f scripts/search_metrics.sql
--
-- 这份文件里只有一个参数：psql 变量 period，internal/handler/search_metrics_test.go
-- 读的就是这份文件本身，把它换成 $1 在测试库上跑并逐格断言 —— 改了这里的口径，
-- 那条测试会红，逼人回去同时改测试里写明的期望值。
--
-- 口径：
--
--   searches        这一组的检索次数（search_logs 行数）
--   zero_rate       无结果率：ranked_ids 为空的比例。分母是 searches
--   ctr10           首屏点击率 CTR@10：首次点击落在前 10 名的检索 / 有结果的检索
--   cart_rate       搜索→加购率：有加购回传的检索 / 有结果的检索
--   order_rate      搜索→下单转化率：有下单回传的检索 / 有结果的检索
--   mrr_click       首次点击名次的倒数的均值（只在有点击的检索上算）。
--                   不是 §9.1 的 MRR（那个要人工标注的相关集），是它在线上的近似：
--                   用户第一下点的那件在第几名
--
-- 后三个率的分母刻意是「有结果的检索」而不是全部：一次无结果的检索不可能被点，
-- 把它算进分母，等于让无结果率在这三个数里再被计一次。
--
-- 按 stages 再分一层，是 search_logs 多出那一列的全部理由（迁移 00027 文件头 ①）：
-- 推理引擎挂了时 strategy 不变、向量那一路却没跑，只按 strategy 分组会把
-- 纯关键词结果算进「双路召回」那一桶。
--
-- **还算不出来的**：二次搜索率（§9.2 最后一行）要「同一个人的相邻两次检索」，
-- 而 /search 是公开接口，search_logs.session_id 目前恒为 NULL。
--
-- 注释里一个反引号都不许有，理由同 db/queries/inventories.sql 的第三条说明。

WITH w AS (
    SELECT l.merchant_id,
           l.strategy,
           array_to_string(l.stages, '+')                     AS stages,
           cardinality(l.ranked_ids) > 0                      AS has_results,
           array_position(l.ranked_ids, l.clicked_id)         AS click_rank,
           l.carted_id IS NOT NULL                            AS carted,
           l.ordered_id IS NOT NULL                           AS ordered
      FROM search_logs l
     WHERE l.created_at >= now() - :'period'::interval
)
SELECT m.code                                                         AS merchant,
       w.strategy,
       w.stages,
       count(*)                                                       AS searches,
       round(avg((NOT w.has_results)::int), 4)                        AS zero_rate,
       round(count(*) FILTER (WHERE w.click_rank <= 10)::numeric
             / nullif(count(*) FILTER (WHERE w.has_results), 0), 4)   AS ctr10,
       round(count(*) FILTER (WHERE w.carted)::numeric
             / nullif(count(*) FILTER (WHERE w.has_results), 0), 4)   AS cart_rate,
       round(count(*) FILTER (WHERE w.ordered)::numeric
             / nullif(count(*) FILTER (WHERE w.has_results), 0), 4)   AS order_rate,
       round(avg(1.0 / w.click_rank), 4)                              AS mrr_click
  FROM w
  JOIN merchants m ON m.id = w.merchant_id
 GROUP BY m.code, w.strategy, w.stages
 ORDER BY m.code, w.strategy, w.stages;
