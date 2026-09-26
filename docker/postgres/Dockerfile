# 同时带 PostGIS 与 pgvector 的 PostgreSQL 16。
#
# 为什么要自建：多门店的电子围栏是多边形（用户定的，不是圆形），判定要
# ST_Contains / ST_Distance 与 GiST 索引，那些在 PostGIS 里；而 M3 的语义检索
# 要 pgvector，且 hnsw.iterative_scan 要它 ≥ 0.8。没有一个官方镜像同时带两样。
#
# 底座选 postgis 上游而不是反过来（pgvector 镜像上装 postgis），理由是
# PostGIS 依赖 GEOS / PROJ / GDAL 三个 C 库的版本配套，上游已经调好了；
# 反向做要自己搭那套配套，而搭错的症状是运行期几何函数行为异常，不是构建失败。
#
# 否掉的两条：supabase/postgres 与 timescale/timescaledb-ha 都带这两样，但各自
# 还带一堆本项目不用的扩展与角色约定，1 GB 起。用「顺便带了我要的两样」的发行版
# 当基线，等于把别人的产品决策接进来。
#
# pgvector 来自 PGDG 官方 apt 源（postgis 镜像里已经配好了那个源）。
#
# 实测（本机建出来之后逐条核过）：
#   postgis 3.4.3 / postgis_topology 3.4.3 / vector 0.8.6
#   PostgreSQL 16.15 (Debian 16.15-1.pgdg11+2)
#
# 最要紧的一行：16.15 与 vector 0.8.6 和换之前的 pgvector/pgvector:pg16
# **完全一致** —— 换镜像不改变 M3 依赖的任何东西。
#
# 体积 794 MB（vs pgvector 镜像 438 MB），多出的约 350 MB 是 GEOS/PROJ/GDAL。
# 构建约 90 秒，之后走层缓存。
#
# 基座差异记一笔：postgis 镜像是 Debian bullseye，pgvector 镜像是 bookworm。
FROM postgis/postgis:16-3.4

RUN apt-get update \
    && apt-get install -y --no-install-recommends postgresql-16-pgvector \
    && rm -rf /var/lib/apt/lists/*

# 注意：CREATE EXTENSION 仍然要在迁移里显式写。
#
# postgis 镜像的入口脚本只在**数据目录为空**时跑初始化（那时它会建 postgis
# 扩展）。挂着已有 pgdata 卷升级上来的库不会走那一步，于是「镜像里有」不等于
# 「这个库里建过」。vector 同理，本镜像根本不自动建它。
