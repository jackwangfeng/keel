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

# 入库的 TS 产物能不能编译。contract-check 只 grep 了其中一行，
# 证明不了这 176KB 整体是合法的 TypeScript。
printf '\n=== schema-check ===\n'
if ! make schema-check; then
    fail=1
fi

if [ "$fail" -ne 0 ]; then
    echo ''
    echo '校验未通过。'
    exit 1
fi
echo ''
echo '全部校验通过。'
