#!/bin/sh
# 从单体（A 档）搬到拆分部署（B / C 档）：把库存那四张表搬进库存服务自己的库 / schema。
# 施工图：docs/电商系统-微服务拆分方案.md「部署档位」「数据库与迁移」。
#
# 三档只差「库存数据放在哪、用哪个账号连」，代码一行不变；所以 A → B、A → C 都是同一件事：
#   ① 目标处建好库存迁移（make migrate-inventory，B 档还要先 prepare-b 建 schema 与账号）；
#   ② copy：源库 public 里的 inventories / inventory_logs / activity_stocks / barrier 整表拷过去，
#      在目标库**一个事务**里导入、续上流水 id 的序列、逐表核对行数；
#   ③ cutover：收回 keel_app 对源库那三张库存表的权限。之后任何一个还按单体配置跑的进程
#      读写库存都当场 42501，而不是静默地读写一份再也没人更新的旧库存。
#   ④ 切连接串（KEEL_ROLE / KEEL_INVENTORY_DSN / KEEL_INVENTORY_URL），起服务。
#
# 用法（两个连接串都用**管理员**角色：要绕过 RLS 读写所有商家的行）：
#   SRC_DSN=postgres://keel:…@core-db:5432/keel \
#   DST_DSN='postgres://keel:…@inv-db:5432/keel_inventory' \
#       scripts/split-migrate.sh copy|verify|cutover|rollback
#   B 档的 DST_DSN 就是源库本身加上 search_path：
#       DST_DSN='postgres://keel:…@core-db:5432/keel?options=-csearch_path%3Dinventory'
#   B 档先跑一次：SRC_DSN=… KEEL_INVENTORY_PASSWORD=… scripts/split-migrate.sh prepare-b
#
# 切换时要停写：先停 core 的公网入口与后台任务，等协调器里没有未完成的全局事务
# （它们的库存分支屏障记在源库的 barrier 里；copy 会连 barrier 一起拷，但停写之后才拷得全）。
# copy 只在目标四张表**全空**时导入 —— 重复执行（compose 每次 up 都会跑）直接跳过并做一次核对，
# 目标里已有数据又和源对不上时报错退出，绝不覆盖。
#
# 回滚（B / C → A）：rollback 把 keel_app 的权限还回源库那三张表。**切换之后库存服务里产生的
# 变化不会搬回来**，所以回滚只适用于刚切完、还没接流量的时候；接过流量之后要回滚，先按同样的方式
# 把目标库的四张表反向拷回（把 SRC_DSN / DST_DSN 对调、源表先清空）。
set -eu

cmd=${1:-}
: "${SRC_DSN:?要设 SRC_DSN（源库 = core 库，管理员角色）}"
PSQL_SRC="psql -v ON_ERROR_STOP=1 -X -qtA $SRC_DSN"

# 列清单写死而不是 SELECT *：两边的列序可能不同（core 库里 store_id / reason 是后来 ALTER 加的，
# 库存目录里是一次建全的），COPY 按位置对列。
T_INV="inventories(sku_id, store_id, merchant_id, available_qty, warning_qty, updated_at)"
T_LOG="inventory_logs(id, merchant_id, store_id, sku_id, change_qty, biz_type, biz_id, before_available, after_available, created_at, reason)"
T_ACT="activity_stocks(merchant_id, promotion_id, sku_id, quota, sold, updated_at)"
T_BAR="barrier(trans_type, gid, branch_id, op, barrier_id, reason, create_time)"
TABLES="inventories inventory_logs activity_stocks barrier"
# cutover 收回权限的只有三张业务表：public.barrier 还是 core 自己的分支（锁券）在用。
BUSINESS="inventories inventory_logs activity_stocks"

cols() { # "t(a, b)" -> "a, b"
  echo "$1" | sed 's/^[^(]*(\(.*\))$/\1/'
}
spec() {
  case $1 in
    inventories) echo "$T_INV" ;; inventory_logs) echo "$T_LOG" ;;
    activity_stocks) echo "$T_ACT" ;; barrier) echo "$T_BAR" ;;
  esac
}
count() { # $1 = psql 命令，$2 = 表（带 schema 前缀或不带）
  $1 -c "SELECT count(*) FROM $2"
}

need_dst() {
  : "${DST_DSN:?要设 DST_DSN（目标 = 库存库或 inventory schema，管理员角色）}"
  PSQL_DST="psql -v ON_ERROR_STOP=1 -X -qtA $DST_DSN"
  # 源与目标是同一处（忘了给 B 档加 search_path）时，copy 会把表拷给自己。
  src_where=$($PSQL_SRC -c "SELECT current_database() || '/' || current_schema() || '@' || coalesce(inet_server_addr()::text, 'local') || ':' || current_setting('port')")
  dst_where=$($PSQL_DST -c "SELECT current_database() || '/' || current_schema() || '@' || coalesce(inet_server_addr()::text, 'local') || ':' || current_setting('port')")
  src_schema=$($PSQL_SRC -c "SELECT current_schema()")
  if [ "$src_schema" != public ]; then
    echo "split-migrate: 源库的 current_schema() 是 $src_schema，应当是 public（SRC_DSN 别带 search_path）" >&2
    exit 1
  fi
  if [ "$src_where" = "$dst_where" ]; then
    echo "split-migrate: 源与目标是同一处（$src_where）。B 档的 DST_DSN 要带 options=-csearch_path%3Dinventory" >&2
    exit 1
  fi
  if [ "$($PSQL_DST -c "SELECT to_regclass('goose_db_version_inventory') IS NOT NULL")" != t ]; then
    echo "split-migrate: 目标处还没跑过库存迁移（$dst_where 没有 goose_db_version_inventory），先 make migrate-inventory" >&2
    exit 1
  fi
}

# 已经 cutover 过（keel_app 对源库存表没有权限了）：源里那份是切换那一刻的旧数据，
# 目标里是之后一直在变的真数据，两者本来就不该再相等 —— copy 与 cutover 都跳过，也不核对。
# compose.split.yaml 每次 up（连 `compose start inventory` 也会）都重跑 copy + cutover，靠的就是这一条。
skip_if_cut() {
  if [ "$($PSQL_SRC -c "SELECT has_table_privilege('keel_app', 'public.inventories', 'SELECT')")" = f ]; then
    echo "split-migrate: 已经切换过（keel_app 对源库存表已无权限），跳过 $cmd"
    exit 0
  fi
}

verify() {
  bad=0
  for t in $TABLES; do
    s=$(count "$PSQL_SRC" "public.$t")
    d=$(count "$PSQL_DST" "$t")
    if [ "$s" = "$d" ]; then
      echo "  $t: $s 行，一致"
    else
      echo "  $t: 源 $s 行 / 目标 $d 行，不一致" >&2
      bad=1
    fi
  done
  # 行数一致还不够：库存水位按 (sku, 门店) 逐行比。流水只追加，行数 + 最大 id 已经足够。
  s=$($PSQL_SRC -c "SELECT md5(coalesce(string_agg(sku_id || ':' || store_id || ':' || available_qty || ':' || warning_qty, ',' ORDER BY sku_id, store_id), '')) FROM public.inventories")
  d=$($PSQL_DST -c "SELECT md5(coalesce(string_agg(sku_id || ':' || store_id || ':' || available_qty || ':' || warning_qty, ',' ORDER BY sku_id, store_id), '')) FROM inventories")
  if [ "$s" != "$d" ]; then
    echo "  inventories 逐行水位不一致" >&2
    bad=1
  fi
  s=$($PSQL_SRC -c "SELECT md5(coalesce(string_agg(promotion_id || ':' || sku_id || ':' || quota || ':' || sold, ',' ORDER BY merchant_id, promotion_id, sku_id), '')) FROM public.activity_stocks")
  d=$($PSQL_DST -c "SELECT md5(coalesce(string_agg(promotion_id || ':' || sku_id || ':' || quota || ':' || sold, ',' ORDER BY merchant_id, promotion_id, sku_id), '')) FROM activity_stocks")
  if [ "$s" != "$d" ]; then
    echo "  activity_stocks 逐行配额不一致" >&2
    bad=1
  fi
  return $bad
}

case $cmd in
prepare-b)
  # B 档：同一个 Postgres 里建 inventory schema 与 keel_inventory 账号。可重复执行。
  : "${KEEL_INVENTORY_PASSWORD:?要设 KEEL_INVENTORY_PASSWORD（keel_inventory 的口令，只在首次建角色时用）}"
  $PSQL_SRC -v pw="$KEEL_INVENTORY_PASSWORD" <<'SQL'
SELECT set_config('keel.inventory_password', :'pw', false);
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'keel_inventory') THEN
        EXECUTE format('CREATE ROLE keel_inventory LOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE PASSWORD %L',
                       current_setting('keel.inventory_password'));
    END IF;
END $$;
CREATE SCHEMA IF NOT EXISTS inventory;
-- 只有 keel_inventory 能进这个 schema；core 的 keel_app 连 USAGE 都没有，
-- 于是 `SELECT … FROM inventory.inventories` 在权限检查这一步就被拒，跨模块 join 写不出来。
REVOKE ALL ON SCHEMA inventory FROM PUBLIC;
-- 反方向：keel_inventory 对 public 里 core 的表没有任何表级授权（00003 / 00005 的默认授权只给 keel_app），
-- 所以它也读不到订单、商品。public 的 schema 级 USAGE 仍是 PUBLIC 的，没有表级授权时它什么也看不见。
SQL
  echo "split-migrate: inventory schema 与 keel_inventory 已就绪。下一步："
  echo "  KEEL_INVENTORY_ROLE=keel_inventory KEEL_INVENTORY_PASSWORD=… make migrate-inventory INVENTORY_GOOSE_DBSTRING='<SRC_DSN>?options=-csearch_path%3Dinventory'"
  ;;
copy)
  need_dst
  skip_if_cut
  nonempty=0
  for t in $TABLES; do
    [ "$(count "$PSQL_DST" "$t")" = 0 ] || nonempty=1
  done
  if [ $nonempty = 1 ]; then
    echo "split-migrate: 目标里已经有库存数据，不再导入，只做核对："
    verify || { echo "split-migrate: 目标已有数据且与源不一致 —— 不覆盖，人工处理" >&2; exit 1; }
    exit 0
  fi
  dir=$(mktemp -d)
  trap 'rm -rf "$dir"' EXIT
  for t in $TABLES; do
    c=$(cols "$(spec $t)")
    $PSQL_SRC -c "\\copy (SELECT $c FROM public.$t) TO '$dir/$t.copy'"
  done
  {
    for t in $TABLES; do
      # printf 而不是 echo：dash 的 echo 会把 \c 当成「到此为止」，整行吞掉，导入就成了空操作。
      printf '%s\n' "\\copy $(spec $t) FROM '$dir/$t.copy'"
    done
    # 流水 id 是 GENERATED ALWAYS：COPY 照原值写进去，但序列不会跟着走，不续上的话下一条流水撞主键。
    echo "SELECT setval(pg_get_serial_sequence('inventory_logs', 'id'), coalesce((SELECT max(id) FROM inventory_logs), 0) + 1, false);"
  } > "$dir/load.sql"
  $PSQL_DST -1 -f "$dir/load.sql" > /dev/null
  echo "split-migrate: 已导入，核对："
  verify
  ;;
verify)
  need_dst
  verify
  ;;
cutover)
  need_dst
  skip_if_cut
  verify || { echo "split-migrate: 核对不过，不切" >&2; exit 1; }
  for t in $BUSINESS; do
    $PSQL_SRC -c "REVOKE ALL ON public.$t FROM keel_app"
  done
  $PSQL_SRC -c "REVOKE ALL ON SEQUENCE public.inventory_logs_id_seq FROM keel_app"
  echo "split-migrate: 已收回 keel_app 对源库库存表的权限。现在切 KEEL_ROLE / KEEL_INVENTORY_DSN / KEEL_INVENTORY_URL。"
  ;;
rollback)
  for t in $BUSINESS; do
    $PSQL_SRC -c "GRANT SELECT, INSERT, UPDATE, DELETE ON public.$t TO keel_app"
  done
  $PSQL_SRC -c "GRANT SELECT, USAGE ON SEQUENCE public.inventory_logs_id_seq TO keel_app"
  echo "split-migrate: keel_app 的权限已还回源库库存表（切换后库存服务里的变化没有搬回来，见文件头）。"
  ;;
*)
  sed -n '2,32p' "$0" >&2
  exit 2
  ;;
esac
