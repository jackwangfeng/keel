#!/usr/bin/env bash
# 跑全部文档校验。任一失败即整体失败。
set -uo pipefail

cd "$(dirname "$0")/.."

fail=0
for script in check_links check_promises check_openapi check_capabilities check_tenancy check_query_tenancy check_migrations check_uts_contract check_dart_contract check_flutter_pages; do
    printf '\n=== %s ===\n' "$script"
    if ! python3 "scripts/${script}.py"; then
        fail=1
    fi
done

# Dart 契约生成器的单测（flutter_app/lib/api/schema.g.dart 的映射规则）。
printf '\n=== test_gen_dart_schema ===\n'
if ! python3 -m unittest scripts/test_gen_dart_schema.py; then
    fail=1
fi

# 契约产物的闸门。
#
# 生成到临时目录，再和入库的产物比对 —— 这个脚本对工作区**只读**。
# 上一版直接重生成到源码树里再 `git diff`，有两个毛病：
#   - 生成失败会毁掉入库产物（generate-go 当时还是 shell 重定向），于是
#     「跑一次检查」这个动作本身有破坏性；
#   - 未 `git add` 的手改会被重生成悄悄冲掉，闸门看不见 —— 而那正是它该抓的。
# 比对临时产物两个毛病都没有：入库的文件一个字节都不会被碰。
#
# 需要 Node（TS 侧走 npx）。没有 Node 的环境里这一步会失败，那是诚实的失败：
# 契约产物确实没被验证过。
printf '\n=== contract-check（生成到临时目录，不碰工作区） ===\n'
tmpdir=$(mktemp -d)
trap 'rm -rf "$tmpdir"' EXIT

if make contract-check GO_OUT="$tmpdir/openapi.gen.go" TS_OUT="$tmpdir/schema.d.ts"; then
    for pair in "internal/api/openapi.gen.go:$tmpdir/openapi.gen.go" \
                "web/src/api/schema.d.ts:$tmpdir/schema.d.ts"; do
        committed=${pair%%:*}
        fresh=${pair#*:}
        if ! diff -q "$committed" "$fresh" >/dev/null 2>&1; then
            echo "FAIL: $committed 与契约重生成的结果不一致。"
            echo "      契约改了却没重生成，或者有人手改了生成文件。请跑 make generate 并一起提交："
            diff -u "$committed" "$fresh" | head -20
            fail=1
        fi
    done
    if [ "$fail" -eq 0 ]; then
        echo '契约产物与契约同步'
    fi
else
    fail=1
fi

# sqlc 产物的漂移闸门。
#
# oapi-codegen 与 openapi-typescript 的产物上面刚比对过，**sqlc 的没有**——
# 而真正执行的 SQL 就在 internal/repository/internal/db/ 里。
#
# 实测这个洞的样子：手改产物，给 ListProducts 加上
# `AND merchant_id = current_merchant()`（正是 check_query_tenancy 存在的全部
# 理由要防的那句话）→ **全绿**，而且脚本还会打印「check_query_tenancy OK：
# 1 个查询文件里没有应用层租户过滤」——因为它读的是 db/queries/products.sql，
# 而进程跑的是产物。源与产物之间没有任何东西把它们钉在一起。
#
# 做法与上面的 contract-check 一致：生成到临时目录再 diff，**对工作区只读**。
# 不要退回「重生成到源码树再 git diff」——那正是 e29f877 修掉的东西。
printf '\n=== sqlc-check（生成到临时目录，不碰工作区） ===\n'
sqlcroot="$tmpdir/sqlc"
mkdir -p "$sqlcroot"

if ! python3 scripts/sqlc_check_config.py "$PWD" "$sqlcroot" out; then
    fail=1
elif ! make generate-sql SQLC_CONFIG="$sqlcroot/sqlc.yaml" >/dev/null; then
    echo 'FAIL: sqlc 生成失败。'
    fail=1
elif ! diff -r -q internal/repository/internal/db "$sqlcroot/out" >/dev/null 2>&1; then
    echo 'FAIL: internal/repository/internal/db 与 sqlc 重生成的结果不一致。'
    echo '      迁移或 db/queries 改了却没重生成，或者有人手改了生成文件。'
    echo '      请跑 make generate-sql 并一起提交：'
    diff -r -u internal/repository/internal/db "$sqlcroot/out" | head -20
    fail=1
else
    echo 'sqlc 产物与 db/migrations + db/queries 同步'
fi

# 入库的 TS 产物能不能编译。contract-check 只 grep 了其中一行，
# 证明不了这 176KB 整体是合法的 TypeScript。
printf '\n=== schema-check ===\n'
if ! make schema-check; then
    fail=1
fi

# 客户端（app/，uni-app x）读契约字段的每一处能不能编译。
#
# 上面那条 check_uts_contract 只证明 app/src/api/schema.uts 与契约同步，
# 证明不了**客户端跟上了**：字段改了名，重生成之后读它的代码已经对不上，
# 而那一步不会红。这一条补上另一半。
#
# 和 schema-check 一样只用 npx 拉一个钉死版本的 tsc，全程零 node_modules。
# 真的用 DCloud 自己的编译器编一遍（含 .uvue 模板）是另一道，要 500 多个包，
# 跑在 CI 的独立 job 里 —— scripts/check_app_build.py。
printf '\n=== app-type-check ===\n'
if ! make app-type-check; then
    fail=1
fi

# 商家后台（web/admin）读契约字段的每一处能不能编译。
#
# 与上面那条 schema-check **不重复**：那一条守的是 web/src（契约产物 +
# 零依赖 SDK），范围里没有一个 .vue。后台是另一棵树、另一个 tsconfig、
# 另一个编译器（vue-tsc 才认 SFC）。为什么不合成一条，完整论证在
# scripts/check_admin_types.py 的文件头 —— 一句话：合成一条会把契约产物
# 那道闸门的成败绑在一棵 UI 框架依赖树上。
#
# 它要 web/admin/node_modules（`make admin-install`）。没装时这一步**失败**，
# 和上面 contract-check 没有 Node 时一样是诚实的失败：后台确实没被验证过。
printf '\n=== admin-type-check ===\n'
if ! make admin-type-check; then
    fail=1
fi

# 后台的单元测试：电子围栏的坐标序与坐标系换算。它守的错在服务端不会红——
# 写反的经纬度、没换算的 GCJ-02 都是合法多边形，只是位置偏了。见 Makefile。
printf '\n=== admin-test ===\n'
if ! make admin-test; then
    fail=1
fi

if [ "$fail" -ne 0 ]; then
    echo ''
    echo '校验未通过。'
    exit 1
fi
echo ''
echo '全部校验通过。'
