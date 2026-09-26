# Keel 的镜像：三个构建阶段，两个产物（应用、迁移器）。
#
# 应用与迁移器共用同一个 Go 构建阶段，是为了让它们必然来自同一份源码与同一套
# 钉死的依赖。分开两个 Dockerfile 的话，「迁移器镜像是上周的、应用是今天的」
# 这种组合随时会出现，症状是应用连上一个 schema 比它旧一版的库 —— 查询报
# 「column does not exist」，而没有任何东西指向真因。

# ---------- dtmrs（Rust → C ABI 动态库） ----------
# 单独一个阶段，而且**只 COPY 那一个脚本**。
#
# 这一层的缓存键因此只跟 scripts/fetch-dtmrs.sh 的内容有关：改一行 Go 代码、
# 改一份 markdown，这一层照样命中缓存。跟 Go 构建挤在一个阶段的话，`COPY . .`
# 一动就要重来一次 cargo build --release（本机实测 47 秒，runner 上 registry
# 是冷的还要更久），而它跟改的那行代码毫无关系。
#
# **底座必须是 trixie**，因为下面 build 阶段的 golang:1.26 就是 Debian 13 trixie
# （glibc 2.41，实测）。.so 在这里编、在 build 阶段被链接、在 app 阶段被加载，
# 三处的 glibc 必须兼容；底座低一档（bookworm，glibc 2.36）编出来的东西
# 在这里能用，反过来不行，而弄反了的症状是运行期 `GLIBC_2.41 not found`。
#
# **为什么不在 golang 镜像里 apt install cargo rustc。** 试过，不行：
# trixie 的 rustc 是 1.85.1，而 dtmrs v0.11.0 的 Cargo.lock 锁着一批要求更高的
# 依赖，实测报 `error: rustc 1.85.1 is not supported by the following packages:
# home@0.5.12 requires rustc 1.88 / icu_*@2.2.0 requires rustc 1.86`。
# **所以「Rust 1.82+」这个下限是错的** —— 那是 dtmrs 自己声明的 rust-version，
# 但真正卡住构建的是它锁定的依赖树，实测下限是 1.88。
# 用官方 rust 镜像还顺带把版本钉住了：apt 的 rustc 会随 Debian 点版本漂。
FROM rust:1.90-slim-trixie AS dtmrs

# 官方 rust 镜像里没有 git（slim），而取回脚本要 clone。
RUN apt-get update -o Acquire::Retries=5 -o Acquire::http::Timeout=30 \
    && apt-get install -y --no-install-recommends \
        -o Acquire::Retries=5 -o Acquire::http::Timeout=30 \
        git ca-certificates \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /src
COPY scripts/fetch-dtmrs.sh ./scripts/
# 版本（v0.11.1）钉在脚本里，不是 @latest —— 上游改了 C ABI 而这里悄悄跟着变，
# 是最难查的一类失败。产物是 /dtmrs/lib/libdtmrs.so 与 /dtmrs/include/dtmrs.h。
RUN ./scripts/fetch-dtmrs.sh /dtmrs

# ---------- Go 构建 ----------
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

# .so 与头文件从 dtmrs 阶段取，落到 internal/dtm 的 #cgo 指令指的那个路径。
# 排在 `COPY . .` 后面：反过来的话，构建上下文里万一有同名文件会把它盖掉
# （third_party 在 .dockerignore 里，正常不会有，但顺序不该依赖那一条）。
COPY --from=dtmrs /dtmrs /src/third_party/dtmrs

# CGO_ENABLED=1 —— M1 那行 `CGO_ENABLED=0` 在这里被 M2 改掉了，它的时机
# 就写在它自己的注释里：接入嵌入式事务协调器的那一刻。
#
# dtmrs 是 Rust 实现，经 cgo 以 C ABI 动态库嵌入（internal/dtm）。代价是确定的，
# 不要试图绕：静态编译没了，最终镜像也不能再是 scratch（见下面 app 阶段）。
# 静态链接 libdtmrs.a 是可行的（约 15 MB 单文件），但那是另一次权衡，
# 不是这次改动的范围。
# 版本号从外面传进来，**不是在这里算的**：.dockerignore 第 3 行把 .git 排除在
# 构建上下文之外（那是刻意的，整个 .git 进上下文会让每次构建多传几十 MB，
# 且任何一次提交都让缓存失效），所以这一层里既没有 git 也没有 VCS 信息。
#
# 三个都留空是允许的 —— internal/buildinfo 的默认值是 "dev"，一个没注入版本的
# 镜像会诚实地说自己是开发构建。CI 的 release 流水线会传真值（见
# .github/workflows/release.yml）。
ARG KEEL_VERSION=dev
ARG KEEL_COMMIT=
ARG KEEL_DATE=

RUN CGO_ENABLED=1 go build -trimpath \
        -ldflags "-X github.com/keel/keel/internal/buildinfo.Version=${KEEL_VERSION} \
                  -X github.com/keel/keel/internal/buildinfo.Commit=${KEEL_COMMIT} \
                  -X github.com/keel/keel/internal/buildinfo.Date=${KEEL_DATE}" \
        -o /out/keel ./cmd/keel

# goose 从 tools/ 编译，版本由 tools/go.mod 锁定 —— 和 `make migrate` 用的是
# 同一个版本。镜像里 go install @latest 的话，本地与容器会在某天装到两个版本的
# 迁移工具，代价是生产库上一次不一致的 schema。
#
# 它仍然是 CGO_ENABLED=0：goose 是纯 Go，静态编译可用，于是迁移器镜像还能留在
# scratch 上。只有应用被 cgo 传染，没必要让迁移器陪着换底座。
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
# 底座从 scratch 换成 debian:trixie-slim。
#
# 换的原因只有一个：cgo 之后的二进制是**动态链接**的，它需要 glibc 与动态链接器
# （ld-linux-x86-64.so.2），scratch 两样都没有，症状是容器起来就
# `exec /keel: no such file or directory` —— 一句指向文件不存在、而文件明明在那里的错。
#
# 为什么是 debian:trixie-slim 而不是 gcr.io/distroless/base-debian12：
#   - 可达性。gcr.io 在不少网络环境下根本连不上（本机实测 `docker pull
#     gcr.io/distroless/static-debian12` 超时），而「clone 下来 docker compose up
#     就能跑」是这个项目的承诺之一。
#   - trixie 不是随手挑的：上面两个构建阶段的底座 golang:1.26 就是 Debian 13
#     trixie（glibc 2.41，实测）。底座低一档（bookworm，glibc 2.36）的话，
#     二进制链接期用到的符号在运行期找不到，报的是 `GLIBC_2.41 not found`。
#
# M1 在 scratch 下记了三个缺失，换底座之后**逐条复核如下（都在 debian:trixie-slim
# 里实测过，不是推断）**：
#   - /tmp：**有了**（drwxrwxrwt）。商品图片上传时 ParseMultipartForm 超过内存
#     阈值会往 os.TempDir() 落盘，那条路现在是通的。
#   - tzdata：**有了**（/usr/share/zoneinfo/Asia/Shanghai 在）。
#     time.LoadLocation("Asia/Shanghai") 可用 —— 订单超时与促销的本地时间窗口
#     都需要它。所以 cmd/keel 里不必 `import _ "time/tzdata"`。
#   - /etc/passwd：**文件有了，但里面没有 65532 这个 uid**（实测最大的是
#     nobody:65534）。所以它不是「换底座就自动解决」的那一条 —— 下面显式建了
#     nonroot。不建的话 user.Current() 仍然会失败，只是失败的理由从
#     「没有这个文件」变成「这个文件里没有你」。
# 还有一条没变：**CA 证书仍然要自己拷**。debian slim 不带 ca-certificates
# （实测 /etc/ssl/certs/ca-certificates.crt 不存在），distroless 才自带。
FROM debian:trixie-slim AS app

# 眼下这个二进制不发起任何 TLS 连接（pgx 走内网 sslmode=disable），证书是为
# 将来备的：配 sslmode=verify-full、或调外部支付/物流 API 的那一天，缺证书的
# 症状是 "x509: certificate signed by unknown authority"，排查会从应用代码开始，
# 而真因是镜像里少一个文件。一层几百 KB，换掉那次排查很划算。
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/

# libdtmrs.so 必须跟着进来，否则二进制起不来。
#
# 放 /usr/local/lib 并 ldconfig，而不是靠编译期烧进去的 rpath：rpath 指的是
# 构建阶段那个 /src/third_party/dtmrs/lib，这个镜像里没有那个目录。
# ld.so 找不到 rpath 目录时会继续往默认路径找，所以两者不冲突 ——
# 但真正把它找到的是 ldconfig 建出来的缓存，不是 rpath。
COPY --from=dtmrs /dtmrs/lib/libdtmrs.so /usr/local/lib/
RUN ldconfig

COPY --from=build /out/keel /keel

# 非 root 运行。65532 是 distroless 的 nonroot，沿用它 —— 但 debian 的
# /etc/passwd 里没有这个 uid，所以这里显式建出来，好让 user.Current() 能用。
#
# 顺带建出协调器存储的目录：KEEL_DTM_DSN 指向的 sqlite 文件落在这里，
# compose 把一个具名卷挂到这个路径上。**属主必须在这里就设好** ——
# Docker 建新卷时会把镜像里该路径的内容与属主一起复制过去，路径不存在的话
# 卷是 root 所有的，应用写不进去，症状是启动时 `unable to open database file`。
#
# 上传文件（商品图、凭证）的目录同理，compose 给它单独挂一个卷。
RUN groupadd -g 65532 nonroot \
    && useradd -u 65532 -g 65532 -M -s /usr/sbin/nologin nonroot \
    && mkdir -p /var/lib/keel /var/lib/keel-uploads \
    && chown 65532:65532 /var/lib/keel /var/lib/keel-uploads
USER 65532:65532

EXPOSE 8080
ENTRYPOINT ["/keel"]
