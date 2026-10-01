-- +goose NO TRANSACTION
--
-- 围栏判定改成平面几何之后的索引（2026-10-01，破坏性测试 P2）。
--
-- fence 列仍是 GEOGRAPHY(POLYGON, 4326)，但「点在不在围栏内」一律改成
-- ST_Intersects(fence::geometry, 点::geometry)：geography 的边是大圆弧，而后台地图
-- （Leaflet / flutter_map）上画的是经纬度平面里的直线。实测 20km 宽的围栏，东西向的南边线上的点
-- 被判在外（南边界往北缩约 3.3m、北边界往外放约 3m；1° 宽时约 80m），与契约「边界上算在内」、
-- 与运营在地图上看到的那条线都对不上。判据、理由见 db/queries/stores.sql 文件头「围栏判定」。
--
-- 原来的 idx_stores_fence 建在 geography 上，规划器只拿它配 geography 的 &&；判定换成
-- fence::geometry 之后它一条查询都用不上，所以换成表达式索引，旧的删掉。
-- 距离（distance_m）仍按 geography 算米，走的是 location，与这两条索引无关。
--
-- stores 的行数以「几十」计，不加 CONCURRENTLY 也是瞬间的事；照 CONTRIBUTING.md「迁移怎么写」的规矩写，
-- 免得这份迁移成为「小表可以不 CONCURRENTLY」的先例被照抄到大表上。
--
-- CONCURRENTLY 失败会留下一条 INVALID 索引，IF NOT EXISTS 会把它当成已存在跳过：
-- 重跑前先 DROP INDEX CONCURRENTLY idx_stores_fence_geom。

-- +goose Up
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_stores_fence_geom
    ON stores USING GIST ((fence::geometry));
DROP INDEX CONCURRENTLY IF EXISTS idx_stores_fence;

-- +goose Down
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_stores_fence ON stores USING GIST (fence);
DROP INDEX CONCURRENTLY IF EXISTS idx_stores_fence_geom;
