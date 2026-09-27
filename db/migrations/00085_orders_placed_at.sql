-- 订单「已下成」的标记（微服务拆分阶段 2 的 C 档验收发现的窗口）。
--
-- 下单 SAGA 是 建单（0 → 10）→ 锁券 → 扣库存 → 收尾。订单在第一步就是 10 待支付、出现在买家的
-- 订单列表里，而库存要到第三步才扣。单体下这个窗口是毫秒级；拆分部署下库存服务不在时，协调器
-- 按退避重试（封顶 5 分钟），这一单就以「待支付」的样子在列表里停几分钟 —— 买家点进去就能付。
-- 付完之后库存分支若被拒（卖完了），全局补偿关不掉一张已支付的单（closeOrder 里那条 ERROR），
-- 结果是「钱收了、货没扣、要人工退款」。
--
-- 所以：收尾分支（库存扣成、订单还在 10）在同一个屏障事务里写 placed_at；发起支付要求它非空，
-- 否则回与「状态不对」相同的 409（契约不变，文案说明「还在确认库存」）。
--
-- 回填：已经不在 0 的历史订单都算下成了（用 created_at）。上线那一刻恰好还在途的 SAGA 会被一并
-- 标上 —— 那就是今天的行为，窗口与改之前相同，不会更差。
-- +goose Up
ALTER TABLE orders ADD COLUMN placed_at TIMESTAMPTZ;
UPDATE orders SET placed_at = created_at WHERE status <> 0;

-- +goose Down
ALTER TABLE orders DROP COLUMN placed_at;
