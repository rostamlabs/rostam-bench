#!/usr/bin/env python3
"""Qdrant SIFT-1M latency + saturated-throughput benchmark — the fair networked
counterpart to Rostam's in-process TestSIFTLatencyQPS.

Two numbers per efSearch, the way you must measure a service:
  * latency  — single sequential client; p50/p99 of one query's round-trip.
  * satQPS   — many concurrent clients (QDRANT_CONC, default 32) hammering the
               service for a few seconds; this is real throughput, NOT the
               single-client number (which only measures round-trip latency).

The earlier qdrant_bench.py reported a single-sequential-client QPS, which badly
understates Qdrant — it is latency-bound on one in-flight request. This harness
saturates the server so QPS reflects its actual capacity.

Start Qdrant, then run:
    docker run -d --name qdrant-bench -p 6333:6333 -p 6334:6334 qdrant/qdrant
    /tmp/sift-venv/bin/python bench/sift1m/qdrant_latency_qps.py
"""
import multiprocessing as mp
import os
import time

import numpy as np
from qdrant_client import QdrantClient, models

SIFT = "/tmp/rostam-sift1m/sift"
COLL = "sift"
K = 10
EF_SWEEP = [16, 64, 128]
CONC = int(os.getenv("QDRANT_CONC", "32"))
DUR = float(os.getenv("QDRANT_QPS_SECONDS", "3"))
LAT_N = 2000  # queries timed for the latency percentiles


def read_fvecs(path):
    x = np.fromfile(path, dtype="int32")
    d = int(x[0])
    return x.reshape(-1, d + 1)[:, 1:].copy().view("float32")


def read_ivecs(path):
    x = np.fromfile(path, dtype="int32")
    d = int(x[0])
    return x.reshape(-1, d + 1)[:, 1:].copy()


def new_client():
    return QdrantClient(host="localhost", port=6333, grpc_port=6334,
                        prefer_grpc=True, timeout=600)


def build(client, base, dim):
    if client.collection_exists(COLL):
        client.delete_collection(COLL)
    client.create_collection(
        COLL,
        vectors_config=models.VectorParams(size=dim, distance=models.Distance.EUCLID),
        hnsw_config=models.HnswConfigDiff(m=16, ef_construct=200),
    )
    n = len(base)
    client.upload_collection(collection_name=COLL, vectors=base, ids=range(n),
                             batch_size=2000, parallel=2)
    while True:
        info = client.get_collection(COLL)
        indexed = info.indexed_vectors_count or 0
        if str(info.status).endswith("green") and indexed >= int(n * 0.99):
            break
        time.sleep(2)


def latency(client, queries, gt, ef):
    params = models.SearchParams(hnsw_ef=ef)
    lats, hits = [], 0
    for i in range(LAT_N):
        t0 = time.perf_counter()
        res = client.query_points(COLL, query=queries[i], limit=K, search_params=params).points
        lats.append((time.perf_counter() - t0) * 1e6)  # microseconds
        ids = {pt.id for pt in res}
        hits += len(ids & set(int(x) for x in gt[i]))
    lats.sort()
    p50 = lats[len(lats) // 2]
    p99 = lats[int(len(lats) * 0.99)]
    mean = sum(lats) / len(lats)
    recall = hits / (LAT_N * K)
    return recall, p50, p99, mean


_QUERIES = None  # set before the pool forks; workers inherit it copy-on-write


def _qps_worker(arg):
    """One OS process: its own client, querying flat-out for DUR seconds. Uses
    processes (not threads) so the Python GIL never serializes the clients — a
    threaded client caps out well below the server's real capacity."""
    ef, wid = arg
    c = new_client()
    params = models.SearchParams(hnsw_ef=ef)
    q = _QUERIES
    stop = time.time() + DUR
    i, n = wid, 0
    while time.time() < stop:
        for _ in range(16):  # amortize the clock check
            c.query_points(COLL, query=q[i % len(q)], limit=K, search_params=params)
            i += 1
            n += 1
    return n


def saturated_qps(queries, ef):
    global _QUERIES
    _QUERIES = queries
    t0 = time.time()
    with mp.Pool(CONC) as pool:
        total = sum(pool.map(_qps_worker, [(ef, w) for w in range(CONC)]))
    return total / (time.time() - t0)


def main():
    base = read_fvecs(f"{SIFT}/sift_base.fvecs")
    queries = read_fvecs(f"{SIFT}/sift_query.fvecs")
    gt = read_ivecs(f"{SIFT}/sift_groundtruth.ivecs")[:, :K]
    n, dim = base.shape

    client = new_client()
    skip = False
    if client.collection_exists(COLL):
        info = client.get_collection(COLL)
        if (info.indexed_vectors_count or 0) >= int(n * 0.99):
            skip = True
    if skip:
        print(f"[qdrant] reusing existing collection ({n}x{dim})", flush=True)
    else:
        print(f"[qdrant] building {n}x{dim} (M=16, efC=200)...", flush=True)
        t0 = time.time()
        build(client, base, dim)
        print(f"[qdrant] built in {time.time()-t0:.1f}s", flush=True)
    print(f"[qdrant] latency=single-client; satQPS={CONC} concurrent processes x {DUR:.0f}s", flush=True)
    print(f"{'ef':<6} {'recall':<9} {'p50(us)':<10} {'p99(us)':<10} {'mean(us)':<10} {'satQPS':<12}")
    for ef in EF_SWEEP:
        recall, p50, p99, mean = latency(client, queries, gt, ef)
        qps = saturated_qps(queries, ef)
        print(f"{ef:<6} {recall:<9.4f} {p50:<10.1f} {p99:<10.1f} {mean:<10.1f} {qps:<12.0f}", flush=True)


if __name__ == "__main__":
    main()
