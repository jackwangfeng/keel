#!/usr/bin/env bash
# make test-db 的编排：把一个测试包**按测试名切片**，几片同时跑，各占一个测试库。
#
# ## 为什么要它
#
# `go test ./...` 的并行单位是包，一个包内部永远是一条串。2026-10-08 实测：
# internal/handler 一个包 296 秒（548 条测试、测试自身合计 241 秒、中位数 0.18 秒、
# 最慢 10 条只占 27%），而第二慢的包是 76 秒。于是 `make test-db` 的 262 秒里
# 一个包占掉九成，CI 的 Go job 304 秒基本就是它。包级并行（-p，默认等于 CPU 数）
# 对这种形状无能为力：把六条串并起来，长度还是最长那条。
#
# 能压它的只有把一个包拆开并发。拆的依据是「同包内的测试互不依赖顺序」——
# 这不是假设，是验证过的事实：`go test -shuffle=on ./internal/handler/` 全绿
# （2026-10-08，exit 0）。而且下面每一组都自己带着 -shuffle=on：顺序依赖一旦
# 被人写回来，先在这里红，而不是变成某次分片结果里一条偶发的红。
#
# ## 怎么拆
#
# 按**文件**分片，不是把测试名列表切成几段。切段会把 channel_* / admin_* 那一族
# 整堆塞进同一片。文件按字节数贪心装箱：大文件先放，每次丢进当前最轻的那片，
# 避免 contract / permission / search 那种重文件落在同一片上把墙钟顶穿。
# 每片是一个独立的测试进程，经 KEEL_TEST_DB_SUFFIX 落到
# keel_test_handler_<片号>（见 internal/testdb 的 run）—— 库名、迁移、种子、删库
# 全按库区分，所以片与片之间不需要任何协调：本来同名库就会被那把咨询锁拒绝。
#
# ## 日志为什么一片一个文件
#
# 十几条串的输出现场交错在一起，红了看不出是谁红的。每片写自己的文件，结束时
# 统一报每片的结论与耗时，只把失败那几片的尾部摊到标准输出上。
#
# ## 用法
#
#   scripts/test_db_shards.sh                    handler 切 8 片
#   SHARDS=4 scripts/test_db_shards.sh           切 4 片
#   SHARDED='internal/handler:6 internal/repository:2' scripts/test_db_shards.sh
#   LOGDIR=/tmp/x scripts/test_db_shards.sh      日志放这里（默认临时目录，跑完保留）
set -uo pipefail

cd "$(dirname "$0")/.."

SHARDS=${SHARDS:-4}
TEST_TIMEOUT=${TEST_TIMEOUT:-8m}

# 每个测试进程的池上限（KEEL_DB_MAX_CONNS，业务池与库存池共用这一个值）。
#
# 片数是被测试集群的 max_connections 卡住的，不是被 CPU：一个测试进程不是「用完
# 就关」，它攥着池 —— pgxpool 长到用过的峰值就留在那儿等复用（LazyConnectTimeout
# 默认 5 分钟），而 handler 的夹具除了业务池还会为「另一库」的事再开一个池，
# 外加 admin 那几个 helper 每次一条裸连接。默认池是 max(4, CPU 数) = 20，
# 于是**一个进程**实测能攥到 40 条上下。
#
# 2026-10-08 第一版按 8 片跑：`FATAL: sorry, too many clients already (SQLSTATE 53300)`
# 红遍四片，而 pg_stat_activity 的 2 秒采样峰值只有 81 / 上限 100 —— 采样看着有余量、
# 实际顶穿了，因为被拒的那一刻正是十几个进程同时建池的那几秒。所以预算要按
# 「所有组同时起步的最坏瞬时」算，不是按平均值：
#
#     每片 ≈ 2 个池 × DB_MAX_CONNS + 几条裸连接 ≈ 12～17 条
#     4 片 × 17 + 其余整包（实测峰值 21）+ 替身组 ≈ 95  —— 贴着 100，但只剩
#     6 条的余量，所以默认停在 4 片；5 片就得上限 300。
#
# 想快一档的正解是测试集群建库时 `-c max_connections=300`（CI 那个容器由
# .github/workflows/ci.yml 自己 docker run，加这一个参数就行），然后把 SHARDS 抬上去，
# 而不是继续压 DB_MAX_CONNS：池压到 2～3，那些故意并发压库存与配额那条路的测试
# 就只是在排队，不再是在压那条路。注意这不改变任何一条测试的**判据**，
# 它改的是排队；自己建小池去压并发的那条测试（TestConcurrentRefundAuditsDoNotExhaustATinyPool）
# 用的是它自己的配置，与这里无关。
DB_MAX_CONNS=${DB_MAX_CONNS:-6}

# 要切片的包：`目录:片数`，空格分隔可写多个。没列进来的包整包一组跑。
SHARDED=${SHARDED:-internal/handler:$SHARDS}

# 点名要哪几个包（`make test-db TEST_PKGS=./internal/tenant/`）时不切片，照原样跑：
# 切片名单是按 internal/handler 底下的文件算出来的，用户点名的时候硬塞别的包进去
# 才是错。SHARDS=1 是另一个出口（还走切片这条路，但 handler 只有一条串）。
TEST_PKGS=${TEST_PKGS:-./...}
if [ "$TEST_PKGS" != "./..." ]; then
    # shellcheck disable=SC2086
    exec go test -count=1 -timeout="$TEST_TIMEOUT" -shuffle=on $TEST_PKGS
fi
# 默认 mktemp：不选固定目录，是为了让上面那句 rm -rf 无从写起 —— 一个闸门脚本
# 里不该出现「删一个可被环境变量指到别处去的目录」。跑完不删，失败了要看。
LOGDIR=${LOGDIR:-$(mktemp -d "${TMPDIR:-/tmp}/keel-test-db.XXXXXX")}
mkdir -p "$LOGDIR"

declare -a PIDS LABELS LOGS

# spawn <标签> <命令...>：后台起一条，输出进 $LOGDIR/<标签>.log。
spawn() {
    local label=$1
    shift
    local log="$LOGDIR/${label//\//_}.log"
    LABELS+=("$label"); LOGS+=("$log")
    ( "$@" >"$log" 2>&1 ) &
    PIDS+=($!)
    printf '启动 %-30s %s\n' "$label" "$log"
}

# ---- 1. 切片 -----------------------------------------------------------------
# 按文件字节数贪心装箱，不是按文件名轮转。contract / permission / search
# 那几份偏重，轮转会把它们堆进同一片，墙钟就是最重那一片。
# 装箱：大文件先放，每次丢进当前总字节最少的那片。
# 写出 $1 个片的文件列表到 $2.0 … $2.$((n-1))。不能把路径塞进带 \0 的
# bash 变量——这里字符串遇到 NUL 会截断，上一版每片只跑到第一个文件。
pack_shards() {
    local n=$1
    local prefix=$2
    shift 2
    python3 - "$n" "$prefix" "$@" <<'PY'
import os, sys
n = int(sys.argv[1])
prefix = sys.argv[2]
files = sys.argv[3:]
items = []
for path in files:
    try:
        w = os.path.getsize(path)
    except OSError:
        w = 0
    # 字节 × max(测试数,1)：contract / migrate 那种「文件大、测试少但很重」
    # 和 admin_auth 那种「测试多」都能摊开；纯字节会把 migrate 独吞一片。
    # 注意：片数变量叫 n，测试数绝不能再赋给 n。
    try:
        import re as _re
        text = open(path, encoding="utf-8", errors="replace").read()
        ntests = len(_re.findall(r"^func Test", text, _re.M))
    except OSError:
        ntests = 0
    # 只有 helper / TestMain 的文件不进任何片：它们已经编进包二进制，
    # 再按体积装箱只会把重测试和空文件捆在一起，把墙钟顶高。
    if ntests == 0:
        continue
    items.append((w * ntests, path))
items.sort(reverse=True)
bins = [[] for _ in range(n)]
loads = [0] * n
for w, path in items:
    i = loads.index(min(loads))
    bins[i].append(path)
    loads[i] += w
for i, b in enumerate(bins):
    with open(f"{prefix}.{i}", "w", encoding="utf-8") as f:
        f.write("\n".join(b))
        if b:
            f.write("\n")
PY
}

declare -a SKIP=()
declare -a SHARD_DIRS SHARD_NS
for spec in $SHARDED; do
    dir=${spec%:*}
    n=${spec#*:}
    SKIP+=("$dir")
    SHARD_DIRS+=("$dir")
    SHARD_NS+=("$n")
    mapfile -t _files < <(ls -1 "$dir"/*_test.go 2>/dev/null | sort)
    if [ ${#_files[@]} -eq 0 ]; then
        echo "FAIL: $dir 底下没有 *_test.go，切不了片。" >&2
        exit 1
    fi
done

# 几个包的测试二进制同时编：串行的话 handler + repository + … 各十几秒叠起来
# 会先吃掉半分钟，而后面的片反正要等编译完才能开跑。
declare -a COMPILE_PIDS=()
for dir in "${SHARD_DIRS[@]}"; do
    bin="$LOGDIR/${dir//\//_}.test"
    (
        if ! go test -c -o "$bin" "./$dir"; then
            echo "FAIL: 编译 $dir 的测试二进制失败。" >&2
            exit 1
        fi
    ) &
    COMPILE_PIDS+=($!)
done
compile_fail=0
for pid in "${COMPILE_PIDS[@]}"; do
    wait "$pid" || compile_fail=1
done
[ "$compile_fail" -eq 0 ] || exit 1

for i in "${!SHARD_DIRS[@]}"; do
    dir=${SHARD_DIRS[$i]}
    n=${SHARD_NS[$i]}
    bin=$(readlink -f "$LOGDIR/${dir//\//_}.test")
    mapfile -t files < <(ls -1 "$dir"/*_test.go 2>/dev/null | sort)
    pack_prefix="$LOGDIR/pack_${dir//\//_}"
    pack_shards "$n" "$pack_prefix" "${files[@]}"
    for ((s = 0; s < n; s++)); do
        group=()
        [ -f "$pack_prefix.$s" ] || {
            echo "FAIL: 缺少分片清单 $pack_prefix.$s" >&2
            exit 1
        }
        mapfile -t group <"$pack_prefix.$s"
        [ ${#group[@]} -eq 0 ] && continue
        # 这一片跑哪些测试：它那几个文件里的 func TestXxx，去掉 TestMain（那是入口）。
        names=$(rg -o --no-filename -r '$1' '^func (Test[A-Za-z0-9_]+)\(' "${group[@]}" |
            sort -u | rg -v '^TestMain$' | paste -sd'|' - || true)
        # 只有 main_test.go 那种片没有一条测试可跑，别起进程（起了也要白等一遍建库迁移）。
        [ -z "$names" ] && continue
        # 工作目录必须是包目录：测试里的相对路径（testdata）和 go test 的约定一致。
        spawn "$dir#$((s + 1))/$n" env KEEL_DB_MAX_CONNS="$DB_MAX_CONNS" \
            KEEL_TEST_DB_SUFFIX="$((s + 1))" \
            bash -c 'cd "$1" && shift && exec "$@"' _ "$dir" "$bin" \
            -test.count=1 -test.timeout="$TEST_TIMEOUT" -test.shuffle=on \
            -test.run "^(?:$names)$"
    done
done

# ---- 2. 没被切片的包：一整组，与上面那些片同时 -------------------------------
#
# 被切片的包必须从这一组里剔掉：否则 handler 既跑切片、又整包跑一遍，
# 那条 296 秒的串还在，整个脚本就白写了。
# 比对用 import 路径全文匹配（-x -F），不用正则：正则里得转义斜杠，而这里
# 匹配错了的症状是「某个包一声不响地没跑」。
go list ./... | rg -F -x -v -f <(printf '%s\n' "${SKIP[@]/#/github.com/keel/keel/}") \
    >"$LOGDIR/.rest"
rest=$(tr '\n' ' ' <"$LOGDIR/.rest")
if [ -n "${rest// /}" ]; then
    # shellcheck disable=SC2086  # 要的就是按空格拆成多个包参数
    spawn rest env KEEL_DB_MAX_CONNS="$DB_MAX_CONNS" \
        go test -count=1 -timeout="$TEST_TIMEOUT" -shuffle=on $rest
fi

# ---- 3. 替身那一组：与主组并发 -----------------------------------------------
#
# 它是另一个构建标签（-tags keel_fake_embedder），必须单独一个 go test。以前排在
# 主组后面顺序跑。它跑的还是 internal/inference 那个包，和主组里同名的那份会抢
# 同一个库 keel_test_inference —— 并发的前提就是那个后缀，仅此而已。
spawn fake-embedder env KEEL_DB_MAX_CONNS="$DB_MAX_CONNS" KEEL_TEST_DB_SUFFIX=fake_embedder \
    go test -count=1 -timeout="$TEST_TIMEOUT" -shuffle=on \
    -tags keel_fake_embedder ./internal/inference/...

# ---- 4. 收 -------------------------------------------------------------------
printf '\n== 等 %s 个并发任务 ==\n' "${#PIDS[@]}"
fail=0
declare -a BAD=()
for i in "${!PIDS[@]}"; do
    if wait "${PIDS[$i]}"; then
        verdict=ok
    else
        verdict=FAIL; fail=1; BAD+=("$i")
    fi
    # 耗时取 go test 那行结尾的秒数；没跑过一条测试的组（比如全被 Skip）就没有这一项。
    secs=$(rg -o '[0-9.]+s\s*$' "${LOGS[$i]}" | tail -1 || true)
    printf '%-6s %-30s %s\n' "$verdict" "${LABELS[$i]}" "${secs:-—}"
done

if [ $fail -ne 0 ]; then
    printf '\n== 失败那几片的判据 ==\n'
    for i in "${BAD[@]}"; do
        printf '\n---- %s ----\n' "${LABELS[$i]}"
        # 不是 tail -40：这个仓库的测试把每一条判断都打在 INFO/ERROR 日志海里，
        # 尾部四十行全是上一秒的业务日志，真正的 `--- FAIL:` 与它下面那句
        # 「哪个文件第几行、期望什么实际什么」早被冲走了。红了要看的只有那几行，
        # 所以按判据抓：FAIL 标头 + 紧随的断言行 + 包级那两行汇总。
        # 抓不到（编译错、testdb 自己拒绝启动）才退回尾部。
        hits=$(rg -U -A 12 '^--- FAIL' "${LOGS[$i]}" || true)
        if [ -n "$hits" ]; then
            printf '%s\n' "$hits" | head -60
            rg '^(FAIL|ok|panic:|.*test timed out)' "${LOGS[$i]}" | tail -5
        else
            tail -25 "${LOGS[$i]}"
        fi
    done
    # 连接预算顶穿了：那条 FATAL 不指向分片，所以在这里把它翻译成本脚本的哪一行。
    if rg -ql 'too many clients already' "$LOGDIR"; then
        cat >&2 <<-EOF

		红的是测试集群的连接预算，不是任何一条断言：并发片数 × 每片的池
		超过了对端 max_connections。两条出路，任选：
		  DB_MAX_CONNS=4 $0            压每片的池（会让更多测试排队）
		  SHARDS=3 $0                  少开几片
		正解通常是第一条之外的：测试集群建库时给 -c max_connections=300。
		EOF
    fi
    printf '\n完整日志：%s\n' "$LOGDIR"
    exit 1
fi

printf '全部片通过。日志：%s\n' "$LOGDIR"
