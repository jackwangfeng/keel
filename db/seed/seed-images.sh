#!/bin/sh
# 给单商家种子（single.sql）里的商品配图。compose 的 seed 服务在 single.sql 之后跑它。
#
# 做三件事，都是幂等的（seed 会被重复执行：每次 docker compose up、每次演示库重置）：
#   1. 建一个停用的系统员工「种子数据」当这些图的上传者 —— uploads 表要求上传者是
#      员工或买家二选一（chk_upload_owner），而单商家种子里没有员工。停用（status 2）
#      意味着它永远登录不了，只是一个署名。
#   2. 把 db/seed/images/ 里的文件拷进上传目录（与 app 共用的 uploads 卷），写 uploads 行。
#      storage_key 的形状与 LocalDiskStore.Put 一致：<商家 id>/<两字符目录>/<文件名>，
#      只是文件名由内容哈希决定（seed-<sha 前 32 位>.jpg），重复执行时落到同一个 key。
#   3. 只给**还没有任何图片**的商品挂主图（product_images, sort_order 0），
#      商家自己在后台配过图的商品一张都不动。
#
# 文件归 65532（app 进程的 uid，见 docker/Dockerfile）：孤儿回收要能删它不再引用的文件。
# 图片来源与授权见 db/seed/images/CREDITS.md。
set -eu

SEED=${SEED_DIR:-/seed}
UPLOADS=${UPLOAD_DIR:-/uploads}
PSQL="psql -v ON_ERROR_STOP=1 -h ${PGHOST:-postgres} -U ${PGUSER:-keel} -d ${PGDATABASE:-keel} -qtA"

MID=$($PSQL -c "SELECT id FROM merchants WHERE code = 'demo'")
if [ -z "$MID" ]; then
  echo "seed-images: 没有 demo 商家，跳过"
  exit 0
fi

STAFF=$($PSQL <<SQL
INSERT INTO staff (merchant_id, email, name, role, status)
SELECT $MID, 'seed-images@keel.invalid', '种子数据', 2, 2
 WHERE NOT EXISTS (SELECT 1 FROM staff WHERE merchant_id = $MID AND email = 'seed-images@keel.invalid');
SELECT id FROM staff WHERE merchant_id = $MID AND email = 'seed-images@keel.invalid';
SQL
)

grep -v '^#' "$SEED/images/manifest.tsv" | while IFS="$(printf '\t')" read -r file title; do
  [ -n "$file" ] || continue
  src="$SEED/images/$file"
  sha=$(sha256sum "$src" | cut -c1-64)
  size=$(stat -c %s "$src")
  name="seed-$(echo "$sha" | cut -c1-32).jpg"
  key="$MID/se/$name"

  mkdir -p "$UPLOADS/$MID/se"
  if [ ! -f "$UPLOADS/$key" ]; then
    cp "$src" "$UPLOADS/$key"
  fi
  chown 65532:65532 "$UPLOADS/$MID" "$UPLOADS/$MID/se" "$UPLOADS/$key"
  chmod 0644 "$UPLOADS/$key"

  # 标题里没有单引号（manifest 与 single.sql 同源），psql 变量用 :'title' 引起来更稳。
  out=$($PSQL -v mid="$MID" -v staff="$STAFF" -v key="$key" -v sha="$sha" -v size="$size" -v title="$title" <<'SQL'
WITH up AS (
    INSERT INTO uploads (merchant_id, staff_id, purpose, driver, storage_key, content_type, size_bytes, sha256, referenced)
    SELECT :mid, :staff, 1, 1, :'key', 'image/jpeg', :size, :'sha', TRUE
     WHERE NOT EXISTS (SELECT 1 FROM uploads WHERE storage_key = :'key')
    RETURNING id
), u AS (
    SELECT id FROM up UNION ALL SELECT id FROM uploads WHERE storage_key = :'key'
), p AS (
    SELECT id FROM products WHERE merchant_id = :mid AND title = :'title' AND deleted_at IS NULL
), ins AS (
    INSERT INTO product_images (merchant_id, product_id, upload_id, sort_order)
    SELECT :mid, p.id, (SELECT id FROM u LIMIT 1), 0 FROM p
     WHERE NOT EXISTS (SELECT 1 FROM product_images pi WHERE pi.product_id = p.id)
    RETURNING product_id
)
SELECT (SELECT count(*) FROM ins) || ',' || (SELECT count(*) FROM p);
SQL
)
  case "$out" in
    1,*) echo "seed-images: $file → $title" ;;
    0,0) echo "seed-images: 找不到商品「$title」，跳过 $file" ;;
    *)   : ;; # 已经有图，不动
  esac
done
echo "seed-images: 完成（商家 $MID）"
