-- 收货地址带坐标（POI，docs/POI-设计.md；号段 00100–00109 分给 POI）。
--
-- 买家端首页「选一条收货地址 → 按它的坐标重新解析门店（围栏）」要地址本身有坐标。坐标 WGS-84（与门店、围栏一致），
-- 由客户端在「搜索地点 / 地图选点」填地址时写入；手填的、老地址都没有（NULL）。两列要么都有要么都没有。
--
-- 为什么是两列双精度而不是 GEOGRAPHY：地址不参与任何空间查询（门店解析拿它的坐标去问围栏，那条查询在 stores 上），
-- 存成两个数读写都直白，也不用在每条返回地址的查询里写 ST_X / ST_Y。
-- +goose Up
ALTER TABLE user_addresses ADD COLUMN lat DOUBLE PRECISION;
ALTER TABLE user_addresses ADD COLUMN lng DOUBLE PRECISION;
ALTER TABLE user_addresses ADD CONSTRAINT chk_user_address_coord CHECK (
    (lat IS NULL AND lng IS NULL) OR (lat BETWEEN -90 AND 90 AND lng BETWEEN -180 AND 180)
);

-- +goose Down
ALTER TABLE user_addresses DROP CONSTRAINT chk_user_address_coord;
ALTER TABLE user_addresses DROP COLUMN lng;
ALTER TABLE user_addresses DROP COLUMN lat;
