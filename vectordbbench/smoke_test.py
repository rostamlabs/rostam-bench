#!/usr/bin/env python3
# SPDX-License-Identifier: Apache-2.0
# Copyright 2026 RostamLabs
"""End-to-end smoke test for the Rostam VectorDBBench client, WITHOUT the full
VDBBench harness: it imports the installed plugin (the real VectorDB/DBConfig/
DBCaseConfig base classes) and drives create -> insert -> optimize -> search on a
SIFT-1M subset, then checks recall@10 against brute-force ground truth.

Prereqs:
    ./install.sh                                  # injects the plugin into ./VectorDBBench
    pip install requests pydantic numpy
    rostam-server -http 127.0.0.1:8080 -data ""   # a running Rostam server

Run:
    VDB=./VectorDBBench SIFT=/tmp/rostam-sift1m/sift python smoke_test.py
"""
import os
import sys
import time

import numpy as np

HERE = os.path.dirname(os.path.abspath(__file__))
VDB = os.getenv("VDB", os.path.join(HERE, "VectorDBBench"))
SIFT = os.getenv("SIFT", "/tmp/rostam-sift1m/sift")
URL = os.getenv("ROSTAM_URL", "http://127.0.0.1:8080")
N = int(os.getenv("N", "20000"))   # base subset
NQ = int(os.getenv("NQ", "200"))   # queries
EF = int(os.getenv("EF", "64"))
K = 10

sys.path.insert(0, VDB)
from vectordb_bench.backend.clients.api import MetricType  # noqa: E402
from vectordb_bench.backend.clients.rostam.config import (  # noqa: E402
    RostamConfig,
    RostamHNSWConfig,
)
from vectordb_bench.backend.clients.rostam.rostam import Rostam  # noqa: E402


def read_fvecs(path, limit=None):
    x = np.fromfile(path, dtype="int32", count=(limit * 129 if limit else -1)) if limit else np.fromfile(path, dtype="int32")
    d = int(x[0])
    return x.reshape(-1, d + 1)[:, 1:].copy().view("float32")


def main():
    base = read_fvecs(f"{SIFT}/sift_base.fvecs", limit=N)[:N]
    query = read_fvecs(f"{SIFT}/sift_query.fvecs", limit=NQ)[:NQ]
    n, dim = base.shape
    print(f"[smoke] base={n} query={len(query)} dim={dim} ef={EF}")

    # brute-force ground truth on the subset (ids are 0..n-1)
    gt = []
    for q in query:
        d = np.linalg.norm(base - q, axis=1)
        gt.append(set(np.argsort(d)[:K].tolist()))

    cfg = RostamConfig(url=URL).to_dict()
    case = RostamHNSWConfig(metric_type=MetricType.L2, m=16, ef_construction=200, ef_search=EF)

    db = Rostam(dim=dim, db_config=cfg, db_case_config=case,
                collection_name="smoke_sift", drop_old=True)

    with db.init():
        t0 = time.time()
        cnt, err = db.insert_embeddings(base.tolist(), list(range(n)))
        assert err is None, f"insert error: {err}"
        assert cnt == n, f"inserted {cnt} != {n}"
        print(f"[smoke] staged {cnt} vectors in {time.time()-t0:.1f}s; building...")

        t0 = time.time()
        db.optimize(data_size=n)
        print(f"[smoke] index built in {time.time()-t0:.1f}s; searching...")

        hits = 0
        t0 = time.time()
        for i, q in enumerate(query):
            ids = db.search_embedding(q.tolist(), k=K)
            hits += len(set(ids) & gt[i])
        dt = time.time() - t0
        recall = hits / (len(query) * K)
        print(f"[smoke] recall@{K}={recall:.4f}  qps={len(query)/dt:.0f}")

    assert recall > 0.90, f"recall {recall:.4f} too low — adapter likely wrong"
    print("SMOKE OK ✅  (create -> insert -> build -> search all work; recall sane)")


if __name__ == "__main__":
    main()
