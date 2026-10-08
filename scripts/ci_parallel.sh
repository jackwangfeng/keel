#!/usr/bin/env bash
# 把 CI 的四路闸门放在同一个 job 里同时跑。
#
# 这台机器只有一个 keel runner。四个 job 会排队，2026-10-07 那次成功的 run
# 墙钟 9 分 50 秒，等于各 job 相加：
#   Go 测试 5 分 04 秒（其中 make test-db 4 分 22 秒）
#   Flutter     57 秒
#   文档闸门    33 秒
#   端到端    2 分 27 秒（起全栈 49 秒 + 多商家 73 秒，前后脚）
# 收成四路并发之后，墙钟是最慢的那一路，不是总和。
#
# Go 那一路慢在 internal/handler 一个包内部串行（实测约 5 分钟）。
# scripts/test_db_shards.sh 把它按文件拆开。4 片是本机默认 Postgres
# （max_connections=100）的上限；这一路的测试库用 -c max_connections=300，
# 所以这里开 8 片。
#
# 端到端慢在单商家栈和多商家栈串行，而且第二遍还把第一遍拆掉再建。
# 镜像只编一次，两套栈用不同的项目名和端口同时起来。
set -uo pipefail

cd "$(dirname "$0")/.."

logdir=$(mktemp -d "${TMPDIR:-/tmp}/keel-ci.XXXXXX")
echo "日志目录 $logdir"

command -v node >/dev/null || { echo "::error::PATH 里没有 node（sdk-smoke 与闸门都要）"; exit 1; }
command -v docker >/dev/null || { echo "::error::PATH 里没有 docker"; exit 1; }

declare -a PIDS NAMES T0
start_lane() {
    local name=$1
    shift
    NAMES+=("$name")
    T0+=("$(date +%s)")
    (
        set -euo pipefail
        "$@"
    ) >"$logdir/$name.log" 2>&1 &
    PIDS+=($!)
    echo "启动 $name pid=${PIDS[-1]}"
}

go_lane() {
    # 这两个若指到沙箱缓存，fetch-dtmrs / go test 会写到别处或读到残缺模块树。
    unset CARGO_TARGET_DIR GOMODCACHE
    trap 'docker rm -f -v keelci-pg >/dev/null 2>&1 || true' EXIT
    export PGPORT=15432 GOTOOLCHAIN=local
    docker rm -f -v keelci-pg >/dev/null 2>&1 || true

    # 起库与 go build / vet 叠着跑：串行时起库 + 探测大约十几秒，那段 CPU 在空等。
    local pg_log=$logdir/go-pg.log
    (
        set -euo pipefail
        # shellcheck disable=SC2046
        docker build $(scripts/ci_build_proxy_args.sh) -t keel-postgres:16 docker/postgres
        # fsync/full_page_writes 关掉：测试库跑完就扔，崩溃一致性无意义；
        # 迁移与几百次模板克隆的墙钟会少一大截。
        docker run -d --name keelci-pg --shm-size=1g \
            -e POSTGRES_USER=keel -e POSTGRES_PASSWORD=keel -e POSTGRES_DB=keel \
            -p 15432:5432 keel-postgres:16 \
            -c max_connections=500 -c shared_buffers=256MB \
            -c fsync=off -c full_page_writes=off -c synchronous_commit=off
        local i
        for i in $(seq 1 90); do
            if docker exec keelci-pg pg_isready -h 127.0.0.1 -U keel -d keel >/dev/null 2>&1; then
                echo "数据库就绪（第 ${i} 次探测）"
                exit 0
            fi
            sleep 1
        done
        echo "::error::数据库 90 秒内没有就绪"
        docker logs keelci-pg
        exit 1
    ) >"$pg_log" 2>&1 &
    local pg_pid=$!

    # shellcheck disable=SC1091
    source scripts/ci_go_toolchain.sh

    local key cache
    key=$(sha256sum scripts/fetch-dtmrs.sh | awk '{print $1}')
    cache="$HOME/.cache/keel-ci/libdtmrs/$key"
    if [ -f "$cache/lib/libdtmrs.so" ] && [ -f "$cache/bin/dtmrs" ]; then
        mkdir -p third_party/dtmrs/lib third_party/dtmrs/include third_party/dtmrs/bin
        cp -a "$cache/lib/." third_party/dtmrs/lib/
        cp -a "$cache/include/." third_party/dtmrs/include/
        cp -a "$cache/bin/." third_party/dtmrs/bin/
        echo "libdtmrs 用本机缓存 $cache"
    else
        if [ -x "$HOME/.cargo/bin/cargo" ]; then
            export PATH="$HOME/.cargo/bin:$PATH"
        fi
        command -v cargo >/dev/null || {
            echo "::error::runner 上没有 cargo，编不了 libdtmrs"
            exit 1
        }
        make dtmrs-deps
        mkdir -p "$cache/lib" "$cache/include" "$cache/bin"
        cp -a third_party/dtmrs/lib/. "$cache/lib/"
        cp -a third_party/dtmrs/include/. "$cache/include/"
        cp -a third_party/dtmrs/bin/. "$cache/bin/"
    fi

    # vet 与起库叠着；migrate 在库就绪后立刻开。
    # 不再单独 go build ./...：后面 test-db 的 rest 组会 `go test` 到所有包
    # （没测试的包也会编译），再编一遍只是重复付钱。
    go vet ./... &
    local vet_pid=$!

    if ! wait "$pg_pid"; then
        cat "$pg_log"
        wait "$vet_pid" 2>/dev/null || true
        exit 1
    fi
    cat "$pg_log"

    # 十几个包各迁一遍是串行的，比测试本身还慢。这里迁一次，后面全从这份克隆。
    (
        set -euo pipefail
        docker exec keelci-pg psql -v ON_ERROR_STOP=1 -U keel -d keel \
            -c "DROP DATABASE IF EXISTS keel_test_ci_tmpl WITH (FORCE)" \
            -c "CREATE DATABASE keel_test_ci_tmpl"
        PGDATABASE=keel_test_ci_tmpl make migrate
    ) &
    local mig_pid=$!

    local compile_fail=0
    wait "$vet_pid" || compile_fail=1
    if ! wait "$mig_pid"; then
        echo "::error::共享模板迁移失败"
        compile_fail=1
    fi
    [ "$compile_fail" -eq 0 ] || exit 1

    # contract_* 已拆成多文件，12 片能把它们摊开；repository 4 片。
    # 连接预算：约 12+4+2+2+4+rest+fake ≈ 26 进程 × 17 ≈ 440，贴着 max_connections=500。
    KEEL_TEST_TEMPLATE=keel_test_ci_tmpl \
        SHARDED='internal/handler:12 internal/repository:4 internal/worker:2 internal/tenant:2 internal/db:6' \
        SHARDS=12 make test-db
}

flutter_lane() {
    renice -n 10 -p $$ >/dev/null 2>&1 || true
    export PUB_HOSTED_URL=https://pub.flutter-io.cn
    export FLUTTER_STORAGE_BASE_URL=https://storage.flutter-io.cn
    export PATH="$HOME/development/flutter/bin:$PATH"
    export FLUTTER_GIT_URL=
    local want got
    want=$(cat flutter_app/.flutter-version)
    got=$(flutter --version --machine | python3 -c 'import sys,json; print(json.load(sys.stdin)["frameworkVersion"])')
    if [ "$got" != "$want" ]; then
        echo "::error::runner 的 ~/development/flutter 是 $got，要 $want"
        exit 1
    fi
    (cd flutter_app && flutter pub get --enforce-lockfile)
    make flutter-analyze
    make flutter-test
    (cd flutter_app && flutter build web)
    git diff --exit-code -- flutter_app
}

gates_lane() {
    # shellcheck disable=SC1091
    source scripts/ci_go_toolchain.sh
    make tools-versions
    make admin-install
    ./scripts/check-all.sh
}

e2e_lane() {
    export COMPOSE_PROJECT_NAME=keelci
    export KEEL_HTTP_PORT=28180
    export KEEL_CONSOLE_PORT=28181
    export GOPROXY="${GOPROXY:-https://goproxy.cn,direct}"
    # Dockerfile 用了 RUN --mount=type=cache；没开 BuildKit 时那几行会直接失败。
    export DOCKER_BUILDKIT=1
    export COMPOSE_DOCKER_CLI_BUILD=1
    cleanup() {
        docker compose -p keelci down -v --remove-orphans >/dev/null 2>&1 || true
        docker compose -p keelcimulti -f compose.yaml -f compose.multi.yaml down -v --remove-orphans >/dev/null 2>&1 || true
    }
    trap cleanup EXIT
    cleanup
    # shellcheck disable=SC2046
    docker compose -p keelci build $(scripts/ci_build_proxy_args.sh)
    local s
    for s in app console migrate; do
        docker tag "keelci-$s" "keelcimulti-$s"
    done

    local secret_file secret
    secret_file="${KEEL_MULTI_AUTH_SECRET_FILE:-$HOME/.config/keel/multi-auth-secret}"
    if [ ! -s "$secret_file" ]; then
        mkdir -p "$(dirname "$secret_file")"
        chmod 700 "$(dirname "$secret_file")"
        (umask 077; openssl rand -base64 48 >"$secret_file")
    fi
    secret=$(cat "$secret_file")

    docker compose -p keelci up -d &
    local up_single=$!
    # 1s：多商家验收里「停用 / 摘域名」要等解析缓存过期，默认 30 秒会把这一路撑过两分钟。
    KEEL_AUTH_SECRET="$secret" COMPOSE_PROJECT_NAME=keelcimulti \
        KEEL_HTTP_PORT=29185 KEEL_CONSOLE_PORT=29186 \
        KEEL_TENANT_CACHE_TTL=1s \
        docker compose -p keelcimulti -f compose.yaml -f compose.multi.yaml up -d --no-build &
    local up_multi=$!
    local s1 s2
    set +e
    wait "$up_single"; s1=$?
    wait "$up_multi"; s2=$?
    set -e
    [ "$s1" -eq 0 ] && [ "$s2" -eq 0 ]

    # 单商家的冒烟和多商家的验收同时跑。后台那两条 curl 要等栈起来，所以跟在冒烟后面。
    (
        ./scripts/smoke.sh
        make sdk-smoke
        code=$(curl -fsS -o /dev/null -w '%{http_code}' "http://localhost:${KEEL_CONSOLE_PORT}/")
        echo "console 首页 $code"
        code=$(curl -fsS -o /dev/null -w '%{http_code}' "http://localhost:${KEEL_CONSOLE_PORT}/api/v1/products")
        echo "console 反代 /api $code"
    ) &
    local single_pid=$!
    KEEL_AUTH_SECRET="$secret" COMPOSE_PROJECT_NAME=keelcimulti \
        KEEL_HTTP_PORT=29185 KEEL_CONSOLE_PORT=29186 \
        KEEL_SMOKE_HOST=shop-a.example.com \
        ./scripts/multi-verify.sh &
    local multi_pid=$!
    local m1 m2
    set +e
    wait "$single_pid"; m1=$?
    wait "$multi_pid"; m2=$?
    set -e
    [ "$m1" -eq 0 ] && [ "$m2" -eq 0 ]
}

start_lane go go_lane
start_lane flutter flutter_lane
start_lane gates gates_lane
start_lane e2e e2e_lane

fail=0
declare -a BAD=()
for i in "${!PIDS[@]}"; do
    if wait "${PIDS[$i]}"; then
        verdict=ok
    else
        verdict=FAIL
        fail=1
        BAD+=("$i")
    fi
    # 用日志的修改时间，而不是 wait 返回的时刻：先结束的那一路不能被后面还在跑的一路垫高。
    ended=$(stat -c %Y "$logdir/${NAMES[$i]}.log")
    printf '%-6s %-8s %ss\n' "$verdict" "${NAMES[$i]}" "$((ended - T0[$i]))"
done

if [ "$fail" -ne 0 ]; then
    for i in "${BAD[@]}"; do
        printf '\n==== %s 失败（日志尾部）====\n' "${NAMES[$i]}"
        tail -80 "$logdir/${NAMES[$i]}.log"
    done
    printf '\n完整日志：%s\n' "$logdir"
    exit 1
fi

echo "四路都过了。日志：$logdir"
