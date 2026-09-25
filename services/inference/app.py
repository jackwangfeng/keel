"""Keel 推理引擎：语义检索层 §10 定的 POST /v1/embed。

这个服务存在的理由只有一句：**验收标准是「搜『连衣裙』能返回相关商品」**。
一个哈希凑出来的伪 embedding 能让所有测试变绿，而搜索结果毫无意义。
所以这里跑的是真模型（BGE-M3），替身在 Go 侧且被编译标签挡在生产构建之外。

## 三条写死的约定，改它们等于改 DDL

维度 1024、模型 BGE-M3、余弦距离——这三条不是这个文件的选择，是
`电商系统-数据模型设计.md` §8 的 `embedding vector(1024)` 加
`vector_cosine_ops` 定下的。`vector(N)` 的 N 写在**列类型**上，
换模型往往同时换维度，于是「换模型」= 「新建表 + 双写 + 切换」
（语义检索层 §2.2 的影子表方案），不是改一行配置。

所以这里对 `model` 与 `dim` 都是**当场拒绝**而不是尽力而为：
请求里的 model 和服务实际加载的对不上就回 400。放行的代价是一批
1024 维但来自别的模型的向量混进同一张表——余弦距离照算不误，
没有任何东西会报错，只有召回质量安静地烂掉。

## 归一化

语义检索层 §2.3：向量必须 L2 归一化后写入，配合 `vector_cosine_ops`；
**写入未归一化的向量不会报错，只会悄悄拉低召回质量**。
归一化在这里做（`normalize_embeddings=True`），但 Go 客户端**不相信**
这个参数传到了——它会自己算一遍 L2 范数再决定收不收。两道都要有：
这里是实现，那里是闸门，而闸门要守的正是「实现悄悄退化」这件事。

## 超时

§10：「服务端超时后立刻返回，由调用方走降级链」。所以有 KEEL_EMBED_TIMEOUT_MS，
超了回 504 而不是把连接挂着——挂着的话调用方的降级链根本不会被触发，
它只会一直等到自己的超时，而那时它已经吃掉了整个 §8 的延迟预算。

编码被**串行化**（单线程 executor + 一个信号量）。torch 自己已经多线程了，
再让多个请求同时 encode 只会互相抢核；串行之后「排队时间」也算进上面那个
预算里，这正是调用方需要的语义：它问的是「多久能拿到向量」，
不是「模型跑了多久」。
"""

from __future__ import annotations

import asyncio
import concurrent.futures
import logging
import os
import re
import time
from contextlib import asynccontextmanager

import numpy as np
from fastapi import FastAPI
from fastapi.responses import JSONResponse
from pydantic import BaseModel, Field

log = logging.getLogger("keel.inference")

# 数据模型 §8 把这两个值写死在列类型里了。这里只是把它们复述一遍，
# 好让对不上的时候在**请求边界**就报错，而不是在 INSERT 那一刻。
MODEL_NAME = "bge-m3"
DIM = 1024

# 默认的 HF 仓库。换它 = 换模型 = 要改 DDL，所以它不是一个「随便配配」的项：
# 服务启动后会核对加载出来的维度是不是 DIM，对不上就拒绝启动（见 lifespan）。
MODEL_REPO = os.environ.get("KEEL_EMBED_MODEL_REPO", "BAAI/bge-m3")

# 索引侧批大小 32–64（§10）。上限取 64：再大不会更快（CPU 上是算力受限，
# 不是调度受限），但会让单个请求占住 executor 更久，把别人的排队时间推过超时。
MAX_BATCH = int(os.environ.get("KEEL_EMBED_MAX_BATCH", "64"))

# 服务端预算。默认 5s 是**索引侧**的量级（批 64 条在 CPU 上就要几秒）；
# 查询侧（§8 给 query embedding 的预算是 15 ms）应当由调用方自己的 context
# 来卡，而不是指望这个值——它是「引擎自己认输」的那条线，不是 SLO。
TIMEOUT_MS = int(os.environ.get("KEEL_EMBED_TIMEOUT_MS", "5000"))

# 单条文本的长度上限。BGE-M3 支持 8192 token，但一条超长文本会独占 executor，
# 把同批别的请求全部拖过超时。索引进来的是标题+副标题+类目（§2.1），几十字。
MAX_CHARS = int(os.environ.get("KEEL_EMBED_MAX_CHARS", "4096"))

_state: dict = {"model": None, "version": "", "loaded_at": 0.0}
_pool: concurrent.futures.ThreadPoolExecutor | None = None
_slot = asyncio.Semaphore(1)


def _resolve_version(model_path: str) -> str:
    """模型版本 = HF 快照的 commit sha。

    §10 要求「模型名与版本随响应返回」，而它最终会写进
    `product_text_vectors.model_version`——那一列的用途是回答
    「这批向量是哪个模型算的、要不要重算」。写一个 "v1" 之类的自造版本号
    答不了这个问题：同一个 repo 的权重被上游改过，"v1" 还是 "v1"。
    commit sha 能。

    **拿不到 sha 时拒绝启动，不回 "unknown"。** 这是 KEEL_DTM_DSN 那条纪律
    （没有默认值，空着拒绝启动）的同一条：一个答不了问题的版本号，
    会让每一次「这批向量要不要重算」的判定都拿到一个看起来有效的答案。
    自己挂载权重目录的部署请显式配 KEEL_EMBED_MODEL_VERSION。
    """
    if v := os.environ.get("KEEL_EMBED_MODEL_VERSION"):
        return v
    # snapshot_download 返回的路径形如
    # <HF_HOME>/hub/models--BAAI--bge-m3/snapshots/<sha>
    m = re.search(r"/snapshots/([0-9a-f]{6,40})", model_path or "")
    if m:
        return m.group(1)
    raise RuntimeError(
        f"解析不出模型版本（路径 {model_path!r} 里没有 HF 快照的 commit sha），"
        f"而且没有配 KEEL_EMBED_MODEL_VERSION。拒绝启动：model_version 会写进 "
        f"product_text_vectors.model_version，那一列回答的是「这批向量要不要重算」，"
        f"填一个 'unknown' 等于让每一次重算判定都拿到一个看起来有效的假答案"
    )


@asynccontextmanager
async def lifespan(app: FastAPI):
    """模型在**启动时**加载，不是第一次请求时。

    懒加载的代价是 /healthz 会在模型还没下载完的时候就回 ok，编排系统于是
    把流量切进来，第一个真实请求等上几十秒下载 2GB 权重然后超时。
    启动时加载让「容器 ready」和「能算向量」是同一件事。
    """
    global _pool
    from sentence_transformers import SentenceTransformer

    t0 = time.monotonic()
    # 先把快照取下来再加载，而不是直接把 repo id 交给 SentenceTransformer：
    # 后者内部也会走 snapshot_download，但它不把落地路径还给我们，
    # 而那个路径里的 commit sha 正是 model_version 的唯一可靠来源。
    # 目录形式的 MODEL_REPO（自己挂权重的部署）直接用，版本走环境变量。
    local_path = MODEL_REPO
    if not os.path.isdir(MODEL_REPO):
        from huggingface_hub import snapshot_download

        local_path = snapshot_download(
            MODEL_REPO,
            # onnx/ 那一套与稀疏、ColBERT 的头都不参与稠密检索，
            # 少下 22 MB + 一堆没人用的权重。
            ignore_patterns=["onnx/*", "imgs/*", "colbert_linear.pt", "sparse_linear.pt"],
        )
    model = SentenceTransformer(local_path, device="cpu")
    dim = model.get_sentence_embedding_dimension()
    if dim != DIM:
        # 拒绝启动，而不是「就这么跑着」。维度对不上时每一次写入都会被
        # Postgres 拒掉（vector(1024) 收不下别的维度），但那要等到入库那一步，
        # 报错指向的是写入代码，不是这里配错的模型。
        raise RuntimeError(
            f"模型 {MODEL_REPO} 的维度是 {dim}，而数据模型 §8 把 "
            f"product_text_vectors.embedding 定死为 vector({DIM})。"
            f"换模型要连 DDL 一起改（影子表 + 切换，见语义检索层 §2.2），"
            f"不能只改这里的 KEEL_EMBED_MODEL_REPO"
        )
    _state["model"] = model
    _state["version"] = _resolve_version(local_path)
    _state["loaded_at"] = time.monotonic() - t0
    # max_workers=1：见模块头「超时」那一段。
    _pool = concurrent.futures.ThreadPoolExecutor(max_workers=1, thread_name_prefix="embed")
    log.info("模型已加载 repo=%s version=%s dim=%d 耗时=%.1fs",
             MODEL_REPO, _state["version"], dim, _state["loaded_at"])
    yield
    _pool.shutdown(wait=False)


app = FastAPI(title="keel-inference", lifespan=lifespan)


class EmbedRequest(BaseModel):
    # 字段名与语义检索层 §10 的报文逐字对应。
    model: str = Field(default=MODEL_NAME)
    texts: list[str]
    normalize: bool = True


def _bad(status: int, detail: str):
    return JSONResponse(status_code=status, content={"error": detail})


@app.get("/healthz")
def healthz():
    ready = _state["model"] is not None
    return JSONResponse(
        status_code=200 if ready else 503,
        content={
            "ready": ready,
            "model": MODEL_NAME,
            "model_version": _state["version"],
            "dim": DIM,
            "load_seconds": round(_state["loaded_at"], 2),
        },
    )


def _encode(texts: list[str], normalize: bool) -> np.ndarray:
    """编码并**自己**归一化，不指望模型那边替我们做。

    这里有一件实测出来的事，值得写下来，因为它不符合直觉：

      BAAI/bge-m3 的 modules.json 里第三个模块是
      `sentence_transformers.models.Normalize`。也就是说这个模型**无论如何**
      都会输出单位向量——把 `normalize_embeddings` 改成 False，输出的
      L2 范数照样是 1。实测过：改成 False 重新建镜像跑，三条向量的范数仍是
      0.99999999 / 1.00000003 / 1.00000004。

    推论有两条，都不舒服：

      ① `normalize` 这个参数对当前模型是**空转的**。照抄它而不说破，
         等于在协议里留一句只在别的模型上才成立的话。
      ② 更要紧的：**「引擎那边归没归一化」这件事，在这个模型上没法用
         「把开关关掉」来验证**。所以下面自己做一遍归一化，让这个保证
         属于本服务，而不是属于上游某个 repo 的 modules.json——
         换一个不带 Normalize 模块的模型时，这里不会跟着悄悄失效。

    顺带接住零向量：全空白输入会让模长为 0，除下去是 NaN。NaN 写进
    vector(1024) 不报错，但它会毁掉整张表的检索——所以宁可当场 500。
    """
    model = _state["model"]
    vecs = model.encode(
        texts,
        batch_size=len(texts),
        normalize_embeddings=normalize,
        convert_to_numpy=True,
        show_progress_bar=False,
    ).astype(np.float32, copy=False)

    norms = np.linalg.norm(vecs, axis=1, keepdims=True)
    if not np.all(np.isfinite(norms)) or float(norms.min()) <= 0:
        raise ValueError("模型输出里有零向量或 NaN，拒绝返回")
    vecs = vecs / norms

    after = np.linalg.norm(vecs, axis=1)
    if not np.allclose(after, 1.0, atol=1e-5):
        raise ValueError(f"归一化之后范数仍不是 1（min={after.min()} max={after.max()}）")
    return vecs


@app.post("/v1/embed")
async def embed(req: EmbedRequest):
    if req.model != MODEL_NAME:
        return _bad(400, f"本服务只提供 {MODEL_NAME}（{DIM} 维），收到 model={req.model!r}。"
                         f"维度写在 product_text_vectors.embedding 的列类型上，换模型要改 DDL")
    if not req.normalize:
        # 收窄了 §10 的报文，理由写在这里而不是默默照做：
        # §2.3 说向量**必须** L2 归一化后写入，配 vector_cosine_ops 使用，
        # 而未归一化的向量入库不会报错、只会悄悄拉低召回。这个服务只服务
        # product_text_vectors 这一个去处，那里没有「不归一化」这种用法。
        # 放行 normalize=false 的代价是：某天有人传了它，拿回一批能入库、
        # 能查询、就是召回质量莫名其妙差一截的向量。
        return _bad(400, "normalize=false 不支持：向量必须 L2 归一化后写入 "
                         "product_text_vectors（配 vector_cosine_ops，语义检索层 §2.3）")
    if not req.texts:
        return _bad(400, "texts 为空")
    if len(req.texts) > MAX_BATCH:
        # 不截断。截断会让调用方拿回比它发出去少的向量，而它按下标对齐——
        # 那是一批张冠李戴的向量，没有任何东西会报错。
        return _bad(400, f"一次最多 {MAX_BATCH} 条（§10：索引侧批大小 32–64），收到 {len(req.texts)} 条")
    for i, t in enumerate(req.texts):
        if not t.strip():
            return _bad(400, f"texts[{i}] 是空白文本；空文本的向量没有意义，请在调用方过滤掉")
        if len(t) > MAX_CHARS:
            return _bad(400, f"texts[{i}] 长度 {len(t)} 超过 {MAX_CHARS}")

    t0 = time.monotonic()
    budget = TIMEOUT_MS / 1000.0
    loop = asyncio.get_running_loop()
    try:
        async with asyncio.timeout(budget):
            # 信号量也在超时范围内：调用方问的是「多久拿到向量」。
            async with _slot:
                vecs = await loop.run_in_executor(_pool, _encode, req.texts, req.normalize)
    except ValueError as e:
        # _encode 的自检。回 500 而不是把坏向量交出去：
        # 它们入库之后不会报错，只会毁掉检索。
        return _bad(500, f"引擎自检未通过: {e}")
    except TimeoutError:
        # 立刻返回，让调用方走降级链（§8）。不返回任何向量——半截结果比没有结果坏：
        # 零向量写进库之后余弦距离对它恒等于 1，它会出现在每一次检索的结果里。
        return _bad(504, f"超过服务端预算 {TIMEOUT_MS}ms（{len(req.texts)} 条），"
                         f"请走降级链（语义检索层 §8）")

    took = (time.monotonic() - t0) * 1000
    return {
        "embeddings": [[float(x) for x in row] for row in vecs],
        "dim": int(vecs.shape[1]),
        # §10：「模型名与版本随响应返回，写入 search_logs 以便归因」。
        # 它同时是 product_text_vectors.model_name / model_version 的来源。
        "model": MODEL_NAME,
        "model_version": _state["version"],
        "took_ms": round(took, 1),
    }
