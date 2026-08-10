#!/usr/bin/env python3
"""Rostam-over-gRPC SIFT-1M latency + saturated-throughput benchmark, using a
Python gRPC client — the SAME transport (gRPC/HTTP2/protobuf) and same client
style as qdrant_latency_qps.py. This isolates the *server implementation*
(Rostam's Go HNSW behind gRPC vs Qdrant's Rust pipeline): client language and
wire framework are now identical on both sides.

Start the gRPC server first (nested module, loads SIFT, serves :7701):
    ROSTAM_SIFT_DIR=/tmp/rostam-sift1m/sift go run ./bench/sift1m/rostam_grpc
Then:
    /tmp/sift-venv/bin/python bench/sift1m/rostam_grpc_client.py
"""
import multiprocessing as mp
import os
import sys
import time

import grpc
import numpy as np

sys.path.insert(0, os.path.join(os.path.dirname(__file__), "grpc_stubs"))
import vector_pb2          # noqa: E402
import vector_pb2_grpc     # noqa: E402

SIFT = os.getenv("ROSTAM_SIFT_DIR", "/tmp/rostam-sift1m/sift")
ADDR = os.getenv("ROSTAM_GRPC_ADDR", "127.0.0.1:7701")
K = 10
CONC = int(os.getenv("ROSTAM_CONC", "32"))
DUR = float(os.getenv("ROSTAM_QPS_SECONDS", "3"))
LAT_N = 2000


def read_fvecs(path):
    x = np.fromfile(path, dtype="int32")
    d = int(x[0])
    return x.reshape(-1, d + 1)[:, 1:].copy().view("float32")


def read_ivecs(path):
    x = np.fromfile(path, dtype="int32")
    d = int(x[0])
    return x.reshape(-1, d + 1)[:, 1:].copy()


def new_stub():
    ch = grpc.insecure_channel(ADDR)
    return vector_pb2_grpc.VectorSearchStub(ch)


def latency(queries, gt):
    stub = new_stub()
    reqs = [vector_pb2.SearchRequest(collection="sift", k=K, query=queries[i].tolist())
            for i in range(LAT_N)]
    lats, hits = [], 0
    for i in range(LAT_N):
        t0 = time.perf_counter()
        resp = stub.Search(reqs[i])
        lats.append((time.perf_counter() - t0) * 1e6)  # us
        ids = {h.id for h in resp.hits}
        truth = set(int(x) + 1 for x in gt[i])  # Rostam ids are 1-based
        hits += len(ids & truth)
    lats.sort()
    return hits / (LAT_N * K), lats[len(lats) // 2], lats[int(len(lats) * 0.99)], sum(lats) / len(lats)


_REQS = None  # precomputed SearchRequest protos; inherited by workers via fork


def _qps_worker(wid):
    stub = new_stub()
    n, i, m = 0, wid, len(_REQS)
    stop = time.time() + DUR
    while time.time() < stop:
        for _ in range(16):
            stub.Search(_REQS[i % m])
            i += 1
            n += 1
    return n


def saturated_qps(queries):
    global _REQS
    _REQS = [vector_pb2.SearchRequest(collection="sift", k=K, query=q.tolist()) for q in queries]
    t0 = time.time()
    with mp.Pool(CONC) as pool:
        total = sum(pool.map(_qps_worker, range(CONC)))
    return total / (time.time() - t0)


def main():
    queries = read_fvecs(f"{SIFT}/sift_query.fvecs")
    gt = read_ivecs(f"{SIFT}/sift_groundtruth.ivecs")[:, :K]
    print(f"[rostam-grpc] {ADDR}, Python gRPC client; latency=single channel, "
          f"satQPS={CONC} concurrent processes x {DUR:.0f}s (ef fixed at server=64)", flush=True)
    recall, p50, p99, mean = latency(queries, gt)
    qps = saturated_qps(queries)
    print(f"{'ef':<6} {'recall':<9} {'p50(us)':<10} {'p99(us)':<10} {'mean(us)':<10} {'satQPS':<12}")
    print(f"{64:<6} {recall:<9.4f} {p50:<10.1f} {p99:<10.1f} {mean:<10.1f} {qps:<12.0f}", flush=True)


if __name__ == "__main__":
    main()
