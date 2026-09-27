-- 门店：后台的 8 条（契约 /admin/stores*）与买家侧的 2 条
-- （GET /stores、GET /stores/resolve）。数据模型 §4。
--
-- 不带 WHERE merchant_id（RLS 过滤），注释里不许有反引号。见 products.sql
-- 与 inventories.sql 的文件头。
--
-- ===========================================================================
-- 围栏怎么进出这一层
-- ===========================================================================
--
-- 库里是 GEOGRAPHY(POLYGON, 4326)，契约里是 GeoJSON Polygon。
-- 进：ST_GeomFromGeoJSON(text)::geography，出：ST_AsGeoJSON(fence)。
--
-- ### 出的那一侧为什么带一个 has_location 布尔，而不是让 lat/lng 可空
--
-- location 与 fence 都可空（一家还没定位、还没画围栏的门店是合法的中间态）。
-- 而 sqlc 对 ST_Y(...)::float8 这类带显式 cast 的表达式一律推断成**非空**，
-- 产物里于是是 float64 而不是 *float64 —— 一个 location 为 NULL 的门店
-- 在 row.Scan 那一步直接报错，而报错信息里没有任何东西指向「这家店还没定位」。
--
-- 试过 db/queries/inventories.sql 末尾那个 LEFT JOIN 技巧（把可空性显式写进
-- SQL），**对这里无效**：那个技巧生效的是数据修改 CTE，而这里是一个
-- LATERAL 子查询，sqlc 照样推断成非空（实测产物仍是 float64）。
--
-- 所以换成显式的三件套：has_location / has_fence 两个布尔 + COALESCE 到
-- 哨兵值。repository 那一层按布尔决定给不给指针，哨兵值一步都不外泄。
-- 这比让 Go 去猜「0,0 是不是真的坐标」强 —— 几内亚湾那个点是真实坐标。
-- 两边都在 SQL 里做，不在 Go 里拼 WKT —— 拼 WKT 要自己处理环的闭合、
-- 顶点顺序与转义，而那三样错了都不报错。
--
-- **坐标序是 [经度, 纬度]**，和中文口语里的「纬度、经度」相反。写反了不会
-- 报错，它会得到一个在地球另一侧的合法多边形，而所有判定都「正常工作」。
-- 范围校验（经度 ∈ [-180,180]、纬度 ∈ [-90,90]）在 service 层做，
-- 那是唯一能机械发现写反的办法。
--
-- 距离一律用 GEOGRAPHY 的球面运算，单位米。用 GEOMETRY 的话 ST_Distance
-- 返回「度」，而一度经度与一度纬度在中纬度差约 30% —— 排序会在东西向与南北向
-- 上系统性偏斜，且看起来完全正常（数据模型 §4）。

-- name: AdminListStores :many
-- 后台门店列表。region_name 一起带出来：后台列表要显示大区名，
-- 而让客户端拿 region_id 再查一遍等于把一次 JOIN 换成 N 次往返。
--
-- only_region_ids / only_store_ids 为空即不限：大区管理员只看得见本大区的门店，
-- 门店管理员只看得见自己那几家（00025）。同一租户内的权限过滤，不是租户过滤。
SELECT st.id, st.region_id, r.name AS region_name, st.code, st.name, st.phone,
       st.province, st.city, st.district, st.address,
       (st.location IS NOT NULL)::boolean AS has_location,
       COALESCE(ST_Y(st.location::geometry), 0)::float8 AS lat,
       COALESCE(ST_X(st.location::geometry), 0)::float8 AS lng,
       COALESCE(ST_AsGeoJSON(st.fence), '')::text       AS fence_geojson,
       st.is_default, st.status, st.deleted_at, st.created_at, st.updated_at
  FROM stores st
  JOIN regions r ON r.id = st.region_id
 WHERE (sqlc.arg(include_deleted)::boolean OR st.deleted_at IS NULL)
   AND (sqlc.narg(region_id)::bigint IS NULL OR st.region_id = sqlc.narg(region_id)::bigint)
   AND (sqlc.narg(only_region_ids)::bigint[] IS NULL OR st.region_id = ANY(sqlc.narg(only_region_ids)::bigint[]))
   AND (sqlc.narg(only_store_ids)::bigint[] IS NULL OR st.id = ANY(sqlc.narg(only_store_ids)::bigint[]))
 ORDER BY st.id
 LIMIT sqlc.arg(page_limit) OFFSET sqlc.arg(page_offset);

-- name: AdminCountStores :one
-- 条件必须与 AdminListStores 逐字一致。
SELECT count(*) FROM stores st
 WHERE (sqlc.arg(include_deleted)::boolean OR st.deleted_at IS NULL)
   AND (sqlc.narg(region_id)::bigint IS NULL OR st.region_id = sqlc.narg(region_id)::bigint)
   AND (sqlc.narg(only_region_ids)::bigint[] IS NULL OR st.region_id = ANY(sqlc.narg(only_region_ids)::bigint[]))
   AND (sqlc.narg(only_store_ids)::bigint[] IS NULL OR st.id = ANY(sqlc.narg(only_store_ids)::bigint[]));

-- name: HasDefaultStore :one
-- AdminStoreList.has_default，契约里是**必返**的。
--
-- 它不是冗余：没有默认门店时，「不在任何围栏内」与「没有位置」两条路径都无处
-- 可落，买家看到的是「不在服务范围」—— 也就是说一个没配默认店的商家，
-- 它的店面对所有未授权定位的访客都是空的。这是规则的必然结果，不是 bug，
-- 但后台必须说出来，否则症状（商品全空）和真因之间没有任何线索。
--
-- 谓词与 uk_stores_default 的部分索引谓词逐字一致（is_default AND
-- deleted_at IS NULL）—— 两处说的必须是同一件事。
SELECT EXISTS (SELECT 1 FROM stores st
                WHERE st.is_default AND st.deleted_at IS NULL) AS has_default;

-- name: AdminGetStore :one
-- 单个门店（含围栏）。含软删的：「不存在」与「已软删」要在上一层还分得开。
SELECT st.id, st.region_id, r.name AS region_name, st.code, st.name, st.phone,
       st.province, st.city, st.district, st.address,
       (st.location IS NOT NULL)::boolean AS has_location,
       COALESCE(ST_Y(st.location::geometry), 0)::float8 AS lat,
       COALESCE(ST_X(st.location::geometry), 0)::float8 AS lng,
       COALESCE(ST_AsGeoJSON(st.fence), '')::text       AS fence_geojson,
       st.is_default, st.status, st.deleted_at, st.created_at, st.updated_at
  FROM stores st
  JOIN regions r ON r.id = st.region_id
 WHERE st.id = $1;

-- name: CreateStore :one
-- 建店。**围栏不在这里传**（契约：建店是表单、画围栏是地图，是后台的两个界面），
-- 所以这条建出来的非默认门店 fence 为 NULL —— 那是一个「未完成」的中间态，
-- 契约把它定义成一个正常状态（后台列表据此挂「未完成」提示）。
-- 数据库上没有 CHECK 挡它：挡了的话这条端点一家非默认门店都建不出来
-- （实测 23514），完整论证在 00020 里 stores 的定义上。
--
-- region_id 不属于本租户时挂在复合外键上（23503），由 repository 翻成 422。
-- 不在这里先查一遍：先查后建之间的窗口里那个大区可以被软删掉。
--
-- 坐标从 lat/lng 两个 float 构造，不收 GeoJSON：门店坐标是一个点，
-- 而 ST_MakePoint 的入参顺序是 (经度, 纬度) —— 把这个顺序钉死在一处，
-- 比让每个调用方各拼一次 GeoJSON 安全。NULL 坐标是合法的（还没定位）。
INSERT INTO stores (region_id, code, name, phone, province, city, district,
                    address, location, is_default)
VALUES (sqlc.arg(region_id), sqlc.arg(code), sqlc.arg(name), sqlc.arg(phone),
        sqlc.arg(province), sqlc.arg(city), sqlc.arg(district), sqlc.arg(address),
        CASE WHEN sqlc.narg(lng)::float8 IS NULL OR sqlc.narg(lat)::float8 IS NULL
             THEN NULL
             ELSE ST_SetSRID(ST_MakePoint(sqlc.narg(lng)::float8,
                                          sqlc.narg(lat)::float8), 4326)::geography
        END,
        sqlc.arg(is_default))
RETURNING id;

-- name: UpdateStore :one
-- 部分更新。**改不了两样**：围栏走 SetStoreFence（要过 ST_IsValid），
-- is_default 走 SetDefaultStore（要在同一事务里先清旧再置新）。
-- 它们不是普通字段，混进这条 PATCH 就等于让那两条纪律有第二条绕过去的路。
--
-- 坐标是「两个都给或都不给」：set_location 这个开关区分「不动坐标」与
-- 「把坐标清空」，与 UpdateSKU 的 set_image_url 同一条理由。
WITH cur AS (
    SELECT st.id FROM stores st WHERE st.id = sqlc.arg(id) AND st.deleted_at IS NULL
), upd AS (
    UPDATE stores u
       SET region_id = COALESCE(sqlc.narg(region_id), u.region_id),
           code      = COALESCE(sqlc.narg(code), u.code),
           name      = COALESCE(sqlc.narg(name), u.name),
           phone     = COALESCE(sqlc.narg(phone), u.phone),
           province  = COALESCE(sqlc.narg(province), u.province),
           city      = COALESCE(sqlc.narg(city), u.city),
           district  = COALESCE(sqlc.narg(district), u.district),
           address   = COALESCE(sqlc.narg(address), u.address),
           status    = COALESCE(sqlc.narg(status), u.status),
           location  = CASE WHEN NOT sqlc.arg(set_location)::boolean THEN u.location
                            WHEN sqlc.narg(lng)::float8 IS NULL
                              OR sqlc.narg(lat)::float8 IS NULL THEN NULL
                            ELSE ST_SetSRID(ST_MakePoint(sqlc.narg(lng)::float8,
                                                         sqlc.narg(lat)::float8),
                                            4326)::geography END
     WHERE u.id = sqlc.arg(id) AND u.deleted_at IS NULL
    RETURNING u.id
)
SELECT (SELECT count(*) FROM cur) AS visible_rows,
       (SELECT count(*) FROM upd) AS updated_rows;

-- name: SoftDeleteStore :one
-- 软删，不是硬删：inventories / orders / inventory_logs 三张表都对 stores 有
-- 外键，一家接过单的门店在数据库层面删不掉。
--
-- **软删掉默认门店是允许的**，后果写在契约里：uk_stores_default 的谓词是
-- is_default AND deleted_at IS NULL，所以删掉之后这家商家就没有默认门店了，
-- 未授权定位的访客会看到「不在服务范围」。不拦，因为「先删旧的默认店再建
-- 新的」是合法的运维顺序。
UPDATE stores SET deleted_at = now()
 WHERE id = $1 AND deleted_at IS NULL
RETURNING id;

-- name: StoreLocationInFence :one
-- 门店坐标与围栏的包含关系，**写完之后、在同一个事务里**读一次。
--
-- 规则（2026-09-27）：门店必须有坐标；有围栏时门店自己必须在围栏内。
-- 一家开在自己配送范围之外的店，「按距离排」与围栏判定会给出互相矛盾的答案 ——
-- 买家离店 200 米却不在服务范围，或者被判进一家离他十公里的店。
--
-- 写之后读而不是写之前判：判定与写入在同一个事务里、读的是本事务刚写的那一行，
-- 不会与并发的另一次改坐标 / 改围栏交错出一个两边都没检查过的组合。违反时由调用方
-- 返回错误，整个事务回滚。
--
-- ST_Covers 而不是 ST_Intersects / ST_Within：点正好落在围栏边上算在内
-- （ST_Within 对边界上的点是假），而这两个参数的顺序是「面 covers 点」。
-- fence 为 NULL 时 covered 恒真：没有围栏就没有「在不在围栏内」这一说。
SELECT (st.location IS NOT NULL)::boolean AS has_location,
       (st.fence IS NULL OR (st.location IS NOT NULL AND ST_Covers(st.fence, st.location)))::boolean AS covered
  FROM stores st
 WHERE st.id = sqlc.arg(id) AND st.deleted_at IS NULL;

-- name: CheckPolygonValidity :one
-- 落库前的 ST_IsValid 校验。**在服务端，不在客户端。**
--
-- 运营在地图上画出来的环可以自交（8 字形）。自交多边形在 ST_Intersects 下的
-- 行为是**未定义**的 —— 不是报错，是给一个谁也说不清的答案。
-- 实测：[[0,0],[1,1],[1,0],[0,1],[0,0]]（领结形）的 ST_IsValid 为 f，
-- reason 是 Self-intersection at or near point 0.5 0.5，那句话直接进
-- Problem 的 detail。
--
-- 单独一条查询而不是塞进 SetStoreFence 的 CTE：校验失败要回 422 并把
-- reason 原样转述，而一条 UPDATE 的返回值里塞不下「为什么不合法」。
-- GeoJSON 本身解不开时 ST_GeomFromGeoJSON 直接抛错，由 repository 翻成同一个
-- 422 —— 两种不合法在契约里是同一个 problem type（invalid-fence）。
SELECT ST_IsValid(g)::boolean    AS valid,
       ST_IsValidReason(g)::text AS reason
  FROM (SELECT ST_GeomFromGeoJSON(sqlc.arg(geojson)::text) AS g) t;

-- name: SetStoreFence :one
-- 整体替换围栏。地图上画的是一个环，没有「改第三个顶点」这种操作。
--
-- geojson 传 NULL 即清空。清空只对默认门店合法 —— 给一家非默认门店清空围栏
-- 会让它永远接不到单。这一条由 repository.SetStoreFence 在调这条语句之前
-- 显式拒掉（409 store-fence-required），**不是数据库约束** ——
-- 那一版实测挡死了建普通店与切换默认店两条主路径（00020 里 stores 的定义）。
--
-- **不在应用层先判一次「是不是默认店」**：先判后改之间另一个会话可以把
-- is_default 改掉，而 CHECK 约束是唯一真正能挡住它的东西。
UPDATE stores
   SET fence = CASE WHEN sqlc.narg(geojson)::text IS NULL THEN NULL
                    ELSE ST_GeomFromGeoJSON(sqlc.narg(geojson)::text)::geography END
 WHERE id = sqlc.arg(id) AND deleted_at IS NULL
RETURNING id;

-- name: ClearDefaultStore :exec
-- 「设默认店」的前半句。必须和后半句在**同一个事务**里，且顺序不能反 ——
-- uk_stores_default 是一条部分唯一索引，先置新再清旧会自己撞自己。
--
-- 这条语句刻意不带「排除自己」的条件：把已经是默认的那一家先清掉再置回去，
-- 结果一样，而少一个「自己设自己」的特例分支。
UPDATE stores SET is_default = FALSE
 WHERE is_default AND deleted_at IS NULL;

-- name: SetDefaultStore :one
-- 「设默认店」的后半句。
--
-- 条件里带 status = 1：一家停业或已软删的门店不能作为回落目标（契约的 409
-- store-unavailable）—— 回落目标接不了单的话，「回落」就成了一个把用户送进
-- 死胡同的动作。零行由调用方分成 404（不存在）与 409（存在但不可用）两支。
UPDATE stores SET is_default = TRUE
 WHERE id = $1 AND deleted_at IS NULL AND status = 1
RETURNING id;

-- name: StoreExists :one
-- 只问「这个 id 在本租户里是不是一家未软删的门店」。
-- 给那些路径里带 store_id、但主查询不该因为门店不存在而返回空集的接口用
-- （空集与 404 是两件事：前者是「这家店什么都没有」，后者是「没有这家店」）。
SELECT st.id, st.region_id FROM stores st
 WHERE st.id = $1 AND st.deleted_at IS NULL;

-- ===========================================================================
-- 买家侧两条
-- ===========================================================================

-- name: ListOpenStores :many
-- GET /stores：手动选店用，只返回营业中（status = 1）且未软删的。
--
-- 这条端点的存在理由是 /stores/resolve 的回落：拒绝定位的买家会被落到默认
-- 门店，而他应该能自己改。没有这条，「回落」就成了一个用户无法纠正的结果。
SELECT st.id, st.name, st.phone, st.address, st.is_default,
       (st.location IS NOT NULL)::boolean AS has_location,
       COALESCE(ST_Y(st.location::geometry), 0)::float8 AS lat,
       COALESCE(ST_X(st.location::geometry), 0)::float8 AS lng
  FROM stores st
 WHERE st.deleted_at IS NULL AND st.status = 1
 ORDER BY st.is_default DESC, st.id
 LIMIT sqlc.arg(page_limit) OFFSET sqlc.arg(page_offset);

-- name: CountOpenStores :one
-- 条件必须与 ListOpenStores 逐字一致。
SELECT count(*) FROM stores st
 WHERE st.deleted_at IS NULL AND st.status = 1;

-- name: ResolveStoresByFence :many
-- 坐标落在哪些门店的围栏内，**按距离升序**。
--
-- 围栏重叠是产品要的（商圈交叠），所以返回多家，服务端不替客户端挑 ——
-- 挑哪一家涉及配送时效、是否自提、用户上次选过谁，那些服务端不知道。
-- 服务端只保证 distance_m 升序。
--
-- distance_m 是买家坐标到 stores.location（门店自身坐标）的球面距离，单位米。
-- location 为空的门店排在最后，而不是被丢掉：它有围栏、接得到单，只是算不出
-- 距离 —— 丢掉它会让一次配置疏忽变成「这家店消失了」。
-- distance_m 在那种行上是哨兵 -1，由 repository 按 has_location 翻成 null
-- （契约：distance_m 为 null 当且仅当算不出距离）。排序用
-- ORDER BY (location IS NULL), 8 —— 先把没坐标的压到最后，再按第 8 列
-- （distance_m）升序；写序号是因为那一列是 COALESCE 出来的表达式，
-- 在 ORDER BY 里重写一遍就是第二处会各自漂的公式。
--
-- 停业（status <> 1）与软删的不参与围栏判定。
--
-- ST_Intersects 而不是 ST_Contains：落在边界线上的点在 Contains 下是 false。
-- 一个买家站在围栏边界上被判成「不在服务范围」，而他向前走一米就好了 ——
-- 那种 bug 没有人能复现。
SELECT st.id, st.name, st.phone, st.address, st.is_default,
       (st.location IS NOT NULL)::boolean AS has_location,
       COALESCE(ST_Y(st.location::geometry), 0)::float8 AS lat,
       COALESCE(ST_X(st.location::geometry), 0)::float8 AS lng,
       COALESCE(ST_Distance(st.location,
                            ST_SetSRID(ST_MakePoint(sqlc.arg(lng)::float8,
                                                    sqlc.arg(lat)::float8),
                                       4326)::geography), -1)::float8 AS distance_m
  FROM stores st
 WHERE st.deleted_at IS NULL AND st.status = 1
   AND st.fence IS NOT NULL
   AND ST_Intersects(st.fence,
                     ST_SetSRID(ST_MakePoint(sqlc.arg(lng)::float8,
                                             sqlc.arg(lat)::float8), 4326)::geography)
 ORDER BY (st.location IS NULL), 8 ASC, st.id
 LIMIT sqlc.arg(page_limit);

-- name: GetDefaultStore :one
-- 回落目标。**「不在任何围栏内」与「根本没传坐标」走的是这同一条路径** ——
-- 那不是两条相似的规则，是一条规则，所以实现也只有这一条查询。
--
-- 它可能返回零行（这家商家没配默认店），那时 match_type 是 none、
-- stores 是空数组，HTTP 仍然 200 —— 「不在服务范围」是一个正常的查询结果，
-- 不是错误。用 404 表达它会让客户端的错误分支同时装着「网络失败」
-- 「鉴权失败」和「这个地方我们不送」，而第三种要渲染的是完全不同的页面。
SELECT st.id, st.region_id, st.name, st.phone, st.address, st.is_default,
       (st.location IS NOT NULL)::boolean AS has_location,
       COALESCE(ST_Y(st.location::geometry), 0)::float8 AS lat,
       COALESCE(ST_X(st.location::geometry), 0)::float8 AS lng
  FROM stores st
 WHERE st.is_default AND st.deleted_at IS NULL AND st.status = 1;
