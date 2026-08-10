#!/usr/bin/env python3
"""Qdrant SIFT-1M benchmark: recall@10 vs single-client QPS across an EfSearch
sweep, plus build (upload + index) time.

IMPORTANT — not apples-to-apples on latency: Qdrant is a service, so its QPS
includes the client + gRPC round-trip and serialization, not just the index.
That's the realistic Qdrant experience, but it is NOT a pure-algorithm number
like the in-process hnswlib/Rostam harnesses. Read it accordingly.

Start Qdrant first:
    docker run -d --name qdrant-bench -p 6333:6333 -p 6334:6334 qdrant/qdrant
Then:
    /tmp/sift-venv/bin/python bench/sift1m/qdrant_bench.py
"""
import os
import time

import numpy as np
from qdrant_client import QdrantClient, models

SIFT = "/tmp/rostam-sift1m/sift"
COLL = "sift"
K = 10
EF_SWEEP = [16, 32, 64, 128, 256, 512]
# QDRANT_SQ8=1 enables Qdrant's own scalar (int8) quantization with the codes
# pinned in RAM (always_ram) and the original float32 stored on disk (on_disk).
# This is the like-for-like analogue of Rostam's QuantSQ8+Mmap, for a fair
# memory comparison (default mode keeps full float32, mmap-backed).
SQ8 = os.getenv("QDRANT_SQ8") == "1"


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

    client = QdrantClient(host="localhost", port=6333, grpc_port=6334, prefer_grpc=True, timeout=600)
    if client.collection_exists(COLL):
        client.delete_collection(COLL)
    quant = None
    if SQ8:
        quant = models.ScalarQuantization(
            scalar=models.ScalarQuantizationConfig(
                type=models.ScalarType.INT8, quantile=0.99, always_ram=True
            )
        )
    client.create_collection(
        COLL,
        # on_disk=True (in SQ8 mode) keeps the original float32 mmap-backed, so
        # only the int8 codes are pinned in RAM — same layout as Rostam's mmap.
        vectors_config=models.VectorParams(
            size=dim, distance=models.Distance.EUCLID, on_disk=SQ8
        ),
        hnsw_config=models.HnswConfigDiff(m=16, ef_construct=200),
        quantization_config=quant,
    )
    print(f"[config] quantization={'SQ8 (int8, always_ram, originals on_disk)' if SQ8 else 'none (float32)'}", flush=True)

    t0 = time.time()
    client.upload_collection(collection_name=COLL, vectors=base, ids=range(n), batch_size=2000, parallel=2)
    # Wait for the HNSW index to finish building in the background.
    while True:
        info = client.get_collection(COLL)
        indexed = info.indexed_vectors_count or 0
        if str(info.status).endswith("green") and indexed >= int(n * 0.99):
            break
        time.sleep(2)
    build = time.time() - t0

    # Marker for an external `docker stats` snapshot of container RAM (the real
    # Qdrant footprint). The index is resident and stable now; hold briefly so a
    # poller can catch it before the slow sweep starts.
    print("[mem-marker] index ready — snapshot docker stats now", flush=True)
    time.sleep(20)

    print(f"Qdrant SIFT-1M: corpus={n} dim={dim}  build(upload+index)={build:.1f}s "
          f"({n/build:.0f} vec/s)  [M=16 efC=200]")
    print(f"{'efSearch':<8} {'recall@10':<12} {'QPS(svc round-trip)':<20}")
    for ef in EF_SWEEP:
        params = models.SearchParams(hnsw_ef=ef)
        hits = 0
        t1 = time.time()
        for i in range(len(query)):
            res = client.query_points(COLL, query=query[i], limit=K, search_params=params).points
            ids = {pt.id for pt in res}
            hits += len(ids & set(int(x) for x in gt[i]))
        dt = time.time() - t1
        recall = hits / (len(query) * K)
        print(f"{ef:<8} {recall:<12.4f} {len(query)/dt:<20.0f}")


if __name__ == "__main__":
    main()
