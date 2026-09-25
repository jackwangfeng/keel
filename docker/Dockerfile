# Keel 的镜像：一个构建阶段，两个产物（应用、迁移器）。
#
# 两个产物共用一个构建阶段，是为了让它们必然来自同一份源码与同一套钉死的依赖。
# 分开两个 Dockerfile 的话，「迁移器镜像是上周的、应用是今天的」这种组合
# 随时会出现，症状是应用连上一个 schema 比它旧一版的库 —— 查询报
# 「column does not exist」，而没有任何东西指向真因。

# ---------- 构建 ----------
# 版本下限是 go.mod 里的 go 1.26.0。它不是随手挑的：tools/ 里钉住的 sqlc 与 goose
# 都声明了 go 1.26，下限由它们传染而来。用更低的 golang 镜像会在 go mod download
# 那一步就失败。
FROM golang:1.26 AS build

# 模块代理可由构建方指定。不写死任何一个镜像源：写死 proxy.golang.org 会让
# 部分网络环境下的构建卡到超时，写死某个地区的镜像又是把本地环境塞进开源仓库。
# 留空时就是 Go 自己的默认值。compose 会把宿主机的 GOPROXY 透传进来。
ARG GOPROXY
ENV GOPROXY=${GOPROXY}

WORKDIR /src

# 先只拷两个模块的 go.mod / go.sum 并下载依赖：改一行 Go 代码时这两层还能复用，
# 不必重下整棵依赖树。tools/ 是独立模块（sqlc 会拖进 270+ 个模块，
# 那正是它不在主模块里的原因），所以要分别下。
COPY go.mod go.sum ./
RUN go mod download
COPY tools/go.mod tools/go.sum ./tools/
RUN go -C tools mod download

COPY . .

# CGO_ENABLED=0 —— **M2 要改这一行**。
#
# M1 不接 dtmrs，纯 Go 依赖，静态编译可用，产物能跑在 scratch 上。
# M2 接入嵌入式事务协调器之后就不行了：dtmrs 是 Rust 实现，经 cgo 以 C ABI
# 动态库嵌入（CONTRIBUTING「为什么需要 Rust 工具链」那一节），届时
#   - 这里要变成 CGO_ENABLED=1，构建阶段还要装 Rust 工具链并编出 libdtmrs.so；
#   - 最终镜像也不能再是 scratch 或 distroless/static —— 动态链接的二进制需要
#     glibc 与动态链接器，那时的底座是 debian-slim 一类，并且要把 .so 一起拷进去。
# 留着 CGO_ENABLED=0 而不删，是因为它在 M1 是对的；改它的时机由上面这段话界定。
RUN CGO_ENABLED=0 go build -trimpath -o /out/keel ./cmd/keel

# goose 从 tools/ 编译，版本由 tools/go.mod 锁定 —— 和 `make migrate` 用的是
# 同一个版本。镜像里 go install @latest 的话，本地与容器会在某天装到两个版本的
# 迁移工具，代价是生产库上一次不一致的 schema。
RUN CGO_ENABLED=0 go -C tools build -trimpath \
        -o /out/goose github.com/pressly/goose/v3/cmd/goose

# ---------- 迁移器 ----------
# 只有 goose 和迁移文件。它跑完就退出，compose 里的应用等它成功。
FROM scratch AS migrate
COPY --from=build /out/goose /goose
COPY db/migrations /db/migrations
ENV GOOSE_DRIVER=postgres \
    GOOSE_MIGRATION_DIR=/db/migrations
ENTRYPOINT ["/goose"]
CMD ["up"]

# ---------- 应用 ----------
# 底座是 scratch，不是 gcr.io/distroless/static-debian12。
#
# 两者对这个二进制而言是等价的（都没有 shell、没有包管理器、没有 libc），
# 而 scratch 不需要从第三个 registry 拉东西 —— gcr.io 在不少网络环境下
# 根本连不上（本机实测 `docker pull gcr.io/distroless/static-debian12` 超时），
# 那会让「clone 下来 docker compose up」这句承诺在那些环境里当场作废。
# 代价是要自己把 CA 证书拷进来，见下。
FROM scratch AS app

# 眼下这个二进制不发起任何 TLS 连接（pgx 走内网 sslmode=disable），证书是为
# 将来备的：配 sslmode=verify-full、或调外部支付/物流 API 的那一天，缺证书的
# 症状是 "x509: certificate signed by unknown authority"，排查会从应用代码开始，
# 而真因是镜像里少一个文件。一层几百 KB，换掉那次排查很划算。
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=build /out/keel /keel

# 非 root 运行。scratch 里没有 /etc/passwd，所以只能写数字 UID —— 这没问题，
# 容器运行时不需要把 UID 解析成名字。65532 是 distroless 的 nonroot，沿用它。
USER 65532:65532

EXPOSE 8080
ENTRYPOINT ["/keel"]
