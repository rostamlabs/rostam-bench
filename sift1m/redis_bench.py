#!/usr/bin/env python3
"""RediSearch (Redis Stack) SIFT-1M benchmark: recall@10 vs single-client QPS
across an EF_RUNTIME sweep, plus build (load + index) time.

Same methodology as qdrant_bench.py — M=16, efConstruction=200, L2, KNN top-10,
recall@10 vs the dataset's exact ground truth, single-threaded query path.

IMPORTANT — not apples-to-apples on latency vs in-process libs: RediSearch is a
service, so its QPS includes the client + RESP round-trip and serialization.
That's the realistic Redis-vector experience, the right peer for networked
Rostam, but NOT a pure-algorithm number like in-process hnswlib.

Start Redis Stack first (RediSearch on :6380):
    docker run -d --name redisstack --network host \
      -e REDIS_ARGS="--port 6380" redis/redis-stack-server:latest
Then:
    /tmp/sift-venv/bin/python bench/sift1m/redis_bench.py
"""
import time

import numpy as np
import redis

SIFT = "/tmp/rostam-sift1m/sift"
IDX = "sift_idx"
PREFIX = "v:"
K = 10
EF_SWEEP = [16, 32, 64, 128, 256, 512]
PORT = 6380


def read_fvecs(path):
    x = np.fromfile(path, dtype="int32")
    d = int(x[0])
    return x.reshape(-1, d + 1)[:, 1:].copy().view("float32")


def read_ivecs(path):
    x = np.fromfile(path, dtype="int32")
    d = int(x[0])
    return x.reshape(-1, d + 1)[:, 1:].copy()


def main():
    base = read_fvecs(f"{SIFT}/sift_base.fvecs")
    query = read_fvecs(f"{SIFT}/sift_query.fvecs")
    gt = read_ivecs(f"{SIFT}/sift_groundtruth.ivecs")[:, :K]
    n, dim = base.shape

    # protocol=2 (RESP2): keeps FT.SEARCH's flat-array reply so the parser below
    # works; redis-py 8 defaults to RESP3 which returns a structured map instead.
    r = redis.Redis(host="127.0.0.1", port=PORT, decode_responses=False, protocol=2)
    r.ping()
    try:
        r.execute_command("FT.DROPINDEX", IDX, "DD")
    except redis.ResponseError:
        pass

    # HNSW index, L2, M=16, efConstruction=200 — matched to every other engine.
    r.execute_command(
        "FT.CREATE", IDX, "ON", "HASH", "PREFIX", "1", PREFIX,
        "SCHEMA", "vec", "VECTOR", "HNSW", "10",
        "TYPE", "FLOAT32", "DIM", str(dim),
        "DISTANCE_METRIC", "L2", "M", "16", "EF_CONSTRUCTION", "200",
    )

    t0 = time.time()
    pipe = r.pipeline(transaction=False)
    for i in range(n):
        pipe.hset(f"{PREFIX}{i}", mapping={"vec": base[i].tobytes()})
        if i % 2000 == 1999:
            pipe.execute()
    pipe.execute()
    load = time.time() - t0
    print(f"[load] {n} vectors HSET in {load:.1f}s ({n/load:.0f} vec/s); indexing...", flush=True)

    # Wait for the HNSW index to finish ingesting (RediSearch indexes on HSET).
    while True:
        info = r.execute_command("FT.INFO", IDX)
        d = {info[i]: info[i + 1] for i in range(0, len(info), 2)}
        pct = float(d.get(b"percent_indexed", d.get("percent_indexed", 1)) or 1)
        indexing = int(d.get(b"indexing", d.get("indexing", 0)) or 0)
        if pct >= 1.0 and indexing == 0:
            break
        time.sleep(2)
    build = time.time() - t0
    print(f"[mem-marker] index ready — snapshot `docker stats redisstack` now", flush=True)
    time.sleep(15)

    print(f"RediSearch SIFT-1M: corpus={n} dim={dim}  build(load+index)={build:.1f}s "
          f"({n/build:.0f} vec/s)  [M=16 efC=200]")
    print(f"{'efSearch':<8} {'recall@10':<12} {'QPS(svc round-trip)':<20}")
    for ef in EF_SWEEP:
        q = f"*=>[KNN {K} @vec $BLOB EF_RUNTIME {ef} AS score]"
        hits = 0
        t1 = time.time()
        for i in range(len(query)):
            res = r.execute_command(
                "FT.SEARCH", IDX, q,
                "PARAMS", "2", "BLOB", query[i].tobytes(),
                "SORTBY", "score", "DIALECT", "2",
                "LIMIT", "0", str(K), "RETURN", "1", "score",
            )
            # res = [count, key0, [fields...], key1, [...], ...]
            ids = set()
            for j in range(1, len(res), 2):
                key = res[j]
                key = key.decode() if isinstance(key, bytes) else key
                ids.add(int(key.split(":")[1]))
            hits += len(ids & set(int(x) for x in gt[i]))
        dt = time.time() - t1
        recall = hits / (len(query) * K)
        print(f"{ef:<8} {recall:<12.4f} {len(query)/dt:<20.0f}")


if __name__ == "__main__":
    main()
