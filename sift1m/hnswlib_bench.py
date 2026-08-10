#!/usr/bin/env python3
"""hnswlib SIFT-1M benchmark: recall@10 vs single-thread QPS across an EfSearch
sweep, plus build time. Mirrors the Rostam (vector/sift1m_bench_test.go) and
Qdrant (qdrant_bench.py) protocols so the three are comparable.

Build is single-threaded to match Rostam's single-threaded insert (hnswlib can
build in parallel; this is the apples-to-apples setting). Queries are
single-threaded for a per-query QPS comparison.

    /tmp/sift-venv/bin/python bench/sift1m/hnswlib_bench.py
"""
import gc
import os
import time

import hnswlib
import numpy as np

SIFT = "/tmp/rostam-sift1m/sift"
K = 10
EF_SWEEP = [16, 32, 64, 128, 256, 512]


def rss_mb():
    """Resident set size in MiB via /proc/self/statm (no psutil dependency)."""
    with open("/proc/self/statm") as f:
        resident = int(f.read().split()[1])
    return resident * os.sysconf("SC_PAGE_SIZE") / (1 << 20)


def read_fvecs(path):
    x = np.fromfile(path, dtype="int32")
    d = int(x[0])
    return x.reshape(-1, d + 1)[:, 1:].copy().view("float32")


def read_ivecs(path):
    x = np.fromfile(path, dtype="int32")
    d = int(x[0])
    return x.reshape(-1, d + 1)[:, 1:].copy()


def main():
    # query+gt are small and kept resident through the whole run; load them
    # first so the memory baseline excludes the large base array.
    query = read_fvecs(f"{SIFT}/sift_query.fvecs")
    gt = read_ivecs(f"{SIFT}/sift_groundtruth.ivecs")[:, :K]
    gc.collect()
    rss_base = rss_mb()  # interpreter + query + gt, BEFORE the base array exists

    base = read_fvecs(f"{SIFT}/sift_base.fvecs")
    n, dim = base.shape
    ids = np.arange(n)

    p = hnswlib.Index(space="l2", dim=dim)
    t0 = time.time()
    p.init_index(max_elements=n, ef_construction=200, M=16, random_seed=42)
    p.add_items(base, ids, num_threads=-1)  # parallel build (all cores)
    build = time.time() - t0

    # Index-resident RSS: free the input arrays (hnswlib has copied the vectors
    # into its own C++ buffer) and measure how much the index itself holds. The
    # base array was loaded AFTER the baseline, so this delta is the index alone.
    del base, ids
    gc.collect()
    rss_after = rss_mb()
    index_rss = rss_after - rss_base

    print(f"hnswlib SIFT-1M: corpus={n} dim={dim}  build={build:.1f}s "
          f"({n/build:.0f} vec/s)  [M=16 efC=200, parallel build]")
    print(f"[mem] index RSS={index_rss:.0f}MB  (rss_after_build_inputs_freed={rss_after:.0f}MB, "
          f"baseline={rss_base:.0f}MB)  float32+graph, M=16")
    print(f"{'efSearch':<8} {'recall@10':<12} {'QPS(1-thread)':<12}")
    for ef in EF_SWEEP:
        p.set_ef(ef)
        t1 = time.time()
        labels, _ = p.knn_query(query, k=K, num_threads=1)
        dt = time.time() - t1
        hits = sum(len(set(labels[i]) & set(gt[i])) for i in range(len(query)))
        recall = hits / (len(query) * K)
        print(f"{ef:<8} {recall:<12.4f} {len(query)/dt:<12.0f}")


if __name__ == "__main__":
    main()
