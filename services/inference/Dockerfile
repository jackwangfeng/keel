# 推理引擎的镜像。语义检索层 §10 的 POST /v1/embed，跑真的 BGE-M3。
#
# ## 为什么是 python:3.12-slim 而不是某个 pytorch 官方镜像
#
# pytorch/pytorch 那些镜像自带 CUDA 运行时，压缩前 7 GB 起步，而架构 §6 形态 A
# 的前提是**没有 GPU**。slim + CPU 轮子把这一串全省掉：torch 的 CPU 轮子
# 从 download.pytorch.org/whl/cpu 装，不会拖进任何 nvidia-* 包。
#
# ## 模型权重不进镜像
#
# BAAI/bge-m3 的权重是 2.27 GB（而且上游只有 pytorch_model.bin，没有
# safetensors）。烤进镜像的话镜像直接翻倍，每一次 `docker compose build`
# 都要重新拉一遍；而它是**不变的数据**，不是代码。
#
# 所以它落在一个具名卷上（compose.inference.yaml 里的 hfcache），
# 容器第一次启动时下载。代价说清楚，别让人以为是免费的：
#   - 第一次起容器要下 2.27 GB，网络慢的地方是十几分钟；
#   - 这段时间 /healthz 回 503（模型在 lifespan 里加载，不是懒加载），
#     所以编排系统不会把流量切进来，而不是切进来之后每个请求都超时。
# 离线环境把权重预先放进卷、再设 HF_HUB_OFFLINE=1 即可。
FROM python:3.12-slim

# 先装依赖再拷代码：改一行 app.py 时这一层还能复用，不必重下 torch。
WORKDIR /srv
COPY requirements.txt ./
RUN pip install --no-cache-dir --index-url https://download.pytorch.org/whl/cpu \
        --extra-index-url https://pypi.org/simple \
        -r requirements.txt

COPY app.py ./

# 模型缓存的位置。它是卷的挂载点——不设的话 HF 会写进 /root/.cache，
# 而那是容器层，容器一重建 2.27 GB 白下一次。
ENV HF_HOME=/models
RUN mkdir -p /models && chown 65532:65532 /models

# 不用 root 跑。这个进程要做的事只有「读模型、算矩阵、回 JSON」，
# 其中没有一件需要 root。
USER 65532:65532

EXPOSE 8000

# 单 worker。模型有 1.3 GB 常驻内存，多开一个 worker 就是多一份——
# 而 CPU 上的吞吐受限于算力不是并发度，多 worker 只会互相抢核。
# 要扩就横向多起几个容器，那时每个容器自己那份内存是必须付的。
CMD ["uvicorn", "app:app", "--host", "0.0.0.0", "--port", "8000", "--workers", "1"]
