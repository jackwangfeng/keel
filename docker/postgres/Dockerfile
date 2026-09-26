# 同时带 PostGIS 与 pgvector 的 PostgreSQL 16。
#
# 为什么要自建：多门店的电子围栏是多边形（用户定的，不是圆形），判定要
# ST_Contains / ST_Distance 与 GiST 索引，那些在 PostGIS 里；而 M3 的语义检索
# 要 pgvector，且 hnsw.iterative_scan 要它 ≥ 0.8。没有一个官方镜像同时带两样。
#
# 否掉的两条：supabase/postgres 与 timescale/timescaledb-ha 都带这两样，但各自
# 还带一堆本项目不用的扩展与角色约定，1 GB 起。用「顺便带了我要的两样」的发行版
# 当基线，等于把别人的产品决策接进来。
#
# =============================================================================
# 底座从 postgis/postgis:16-3.4 换成了官方 postgres:16-bookworm
# =============================================================================
#
# 第一版的底座是 postgis 上游镜像，理由是「GEOS / PROJ / GDAL 三个 C 库的版本
# 配套上游已经调好了，反向做要自己搭，搭错的症状是运行期几何函数行为异常」。
# 那条理由本身是对的，**换底座没有放弃它**，见下面第三点。
#
# 换的原因是那一版文件头最后一句「基座差异记一笔：postgis 镜像是 Debian
# bullseye，pgvector 镜像是 bookworm」—— **差异记下了，后果没推下去**。
# 后果有两个，都是在重建演示栈时撞上的，不是推断：
#
#   1. **排序规则降级，会让已有库的文本索引悄悄失效。**
#      bullseye 的 glibc 是 2.31，bookworm 是 2.36。挂着一个由旧镜像
#      （pgvector/pgvector:pg16，bookworm）初始化的数据卷换到 bullseye 底座上，
#      PostgreSQL 当场报：
#        database "keel" has a collation version mismatch
#        created using collation version 2.36, but the OS provides version 2.31
#      glibc 的排序规则一变，建在 TEXT 列上的 btree 索引（uk_skus_code、类目
#      path、商家 code……）的有序性就不再成立 —— 查找会漏行、唯一约束会漏掉
#      重复，**而且不报错**。这是 PostgreSQL 升级里最出名的那类静默损坏。
#      那一版「与旧镜像逐项一致」的核对清单是 PG 16.15 / vector 0.8.6 /
#      postgis 3.4.3 三项，底座操作系统不在清单里 —— 所以它是「逐项一致」的，
#      只是那几项不包括会咬人的那一项。
#
#   2. **Debian 11 bullseye 已经完全停止支持。** 常规安全支持 2024-08 结束，
#      LTS 在 2026-06-30 结束。首个公开版本的数据库跑在一个不再有任何安全更新
#      的系统上，这不能带出去。
#
#   3. **postgis 上游 PG16 这一支没有 bookworm 变体。** 实测 16-3.4 与 16-3.5
#      都是 bullseye，只另有 alpine（musl，排序规则又是另一套，同样的问题换个
#      方向）。所以不是「换个 tag」能解决的。
#
# 于是反过来：官方 postgres:16-bookworm 做底座，PostGIS 与 pgvector 都从
# PGDG（apt.postgresql.org）装。这**保住了第一版那条理由**：PGDG 的
# postgresql-16-postgis-3 依赖的是 Debian bookworm 自己打包的 libgeos /
# libproj / libgdal，版本配套由 Debian 与 PGDG 的打包者负责，同样不是我们自己
# 搭的。官方镜像已经配好了 PGDG 源。
#
# 实测这个底座的 glibc 是 2.36-9+deb12u14，与旧 pgvector 镜像**连补丁号都
# 一样**，所以由旧镜像初始化的数据卷挂上来之后排序规则重新对上。
#
# =============================================================================
# 版本怎么钉
# =============================================================================
#
# 钉到**小版本**（postgis 3.6.* / pgvector 0.8.*），不钉到精确包号。
#
# 这个仓库的习惯是版本一律钉死，不写 @latest（Makefile 文件头有理由）。但 apt
# 源和 npm / Go 模块代理不一样：**PGDG 发一个补丁版就会把旧的从源里撤掉**。
# 钉死到 `3.6.4+dfsg-2.pgdg12+1` 那种精确号，PGDG 下一次发补丁时构建直接 404
# —— 一个「钉得很严」的构建脚本在某个毫无改动的早上突然红掉，是比漂一个补丁版
# 更难查的失败。钉小版本同时守住了真正要守的东西：
#   · pgvector 必须 ≥ 0.8（hnsw.iterative_scan，00016 文件头）；
#   · postgis 不能悄悄跨到 4.x（库名会变，已有库的扩展会加载失败）。
# 下面那道 RUN 里的断言在构建期把这两条变成硬失败，而不是运行期的怪现象。
#
# postgis 因此从 3.4.3 升到了 3.6。3.x 之内动态库名一直是 postgis-3，已有库
# 能直接加载新库；迁移里的 CREATE EXTENSION 对新库给出 3.6，对已有库要跑一次
# ALTER EXTENSION postgis UPDATE 同步 SQL 定义。00020 用到的 ST_IsValid /
# ST_Intersects / ST_Distance / GEOGRAPHY / GiST 在 3.x 里都是稳定的。
#
# 构建期要访问 apt 源。网络需要代理时从外面传（Docker 认得 http_proxy /
# https_proxy 这两个预定义的 build-arg，不必在这里声明 ARG），**不写死进来**
# —— 理由和 docker/Dockerfile 里 GOPROXY 那段一样：写死某个代理是把本地环境
# 塞进开源仓库。
FROM postgres:16-bookworm

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        'postgresql-16-postgis-3=3.6.*' \
        'postgresql-16-pgvector=0.8.*' \
    && rm -rf /var/lib/apt/lists/* \
    # 构建期断言：上面的版本通配真的落到了想要的地方。
    # apt 的版本通配匹配不上时会直接报错，这里再核一次是为了让「装上的到底是
    # 什么」出现在构建日志里 —— 出事时第一个要问的问题。
    && dpkg-query -W -f='${Package} ${Version}\n' \
        postgresql-16-postgis-3 postgresql-16-pgvector \
    && dpkg-query -W -f='${Version}' postgresql-16-postgis-3 | grep -q '^3\.6\.' \
    && dpkg-query -W -f='${Version}' postgresql-16-pgvector | grep -q '^0\.8\.'

# 注意：CREATE EXTENSION 仍然要在迁移里显式写。
#
# 官方 postgres 镜像的入口脚本只在**数据目录为空**时跑初始化，而且它根本不建
# 任何扩展。挂着已有 pgdata 卷升级上来的库更不会走那一步 —— 「镜像里有」
# 不等于「这个库里建过」。
