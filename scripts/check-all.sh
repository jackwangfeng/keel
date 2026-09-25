#!/usr/bin/env bash
# 跑全部文档校验。任一失败即整体失败。
set -uo pipefail

cd "$(dirname "$0")/.."

fail=0
for script in check_links check_promises check_openapi check_capabilities check_tenancy check_query_tenancy; do
    printf '\n=== %s ===\n' "$script"
    if ! python3 "scripts/${script}.py"; then
        fail=1
    fi
done

# 契约产物的闸门。它跑 make generate 重生成 Go 与 TS 两侧，再确认 3.1 的可空语义
# 两边都还在 —— 而重生成本身也是一道检查：产物入库之后，「有人手改了生成文件」
# 会在这里被原样覆盖掉，接着由下面的 git diff 喊出来。
#
# 它需要 Node（TS 侧走 npx）。没有 Node 的环境里这一步会失败，那是诚实的失败：
# 契约产物确实没被验证过。
printf '\n=== contract-check ===\n'
if ! make contract-check; then
    fail=1
fi

# 入库的产物必须与契约同步。契约改了却没重生成（或者有人手改了生成文件），
# 上面那次 make 会把差异写进工作区，这里把它变成一次失败。
#
# 只看这两个路径，不看整个工作区：check-all.sh 会在开发中途跑，别处有未提交的
# 改动是常态。
printf '\n=== 契约产物是否与契约同步 ===\n'
if git diff --quiet -- internal/api/openapi.gen.go web/src/api/schema.d.ts; then
    echo '契约产物与契约同步'
else
    echo 'FAIL: 重生成之后契约产物变了 —— 契约改了却没重生成，或者有人手改了生成文件。'
    echo '      请跑 make generate 并把产物一起提交：'
    git diff --stat -- internal/api/openapi.gen.go web/src/api/schema.d.ts
    fail=1
fi

if [ "$fail" -ne 0 ]; then
    echo ''
    echo '校验未通过。'
    exit 1
fi
echo ''
echo '全部校验通过。'
