# SIFT-1M head-to-head: Rostam vs. RediSearch vs. Qdrant vs. hnswlib

Recall@10 vs. query throughput on the canonical **SIFT-1M** dataset (1,000,000
× 128-dim SIFT descriptors, 10,000 queries, exact ground truth), at matched
index parameters, sweeping `efSearch`.

> **Engine roster.** Networked vector stores (Rostam, **RediSearch**, Qdrant) are
> the apples-to-apples peer group; hnswlib is the in-process C++ reference
> ceiling. See [Redis / RediSearch head-to-head](#redis--redisearch-headtohead)
> below for the newest comparison, and [More engines](#more-engines-roadmap) for
> the roadmap (Milvus, Weaviate, pgvector, Aerospike Vector Search).

## Methodology

- **Index params (all engines):** `M=16`, `efConstruction=200`, metric **L2**.
- **Sweep:** `efSearch ∈ {16, 32, 64, 128, 256, 512}`.
- **Recall@10:** overlap of each query's returned top-10 with the dataset's
  exact top-10 ground truth, averaged over all 10k queries.
- **QPS:** queries per second, **single-threaded query path**.
- **Build:** single-threaded for Rostam and hnswlib (apples-to-apples — Rostam
  inserts single-threaded; hnswlib *can* build in parallel but is pinned to one
  thread here). Qdrant build = upload + background index, and is not directly
  comparable (it's a service).

### Fairness caveats (read these)

- **hnswlib and Rostam are in-process libraries** — their QPS is the pure index.
- **Qdrant is a service** — its QPS includes the client + gRPC round-trip and
  serialization. That's the realistic Qdrant experience but **not** a
  pure-algorithm number; do not compare it head-to-head on latency.
- SIFT-1M is real data (SIFT image descriptors), not synthetic — this is a
  legitimate ANN benchmark, but a single dataset/machine. Treat as directional.

## Reproduce

```bash
# 1. Dataset (~168MB) → /tmp/rostam-sift1m/sift/
mkdir -p /tmp/rostam-sift1m && cd /tmp/rostam-sift1m
curl -O ftp://ftp.irisa.fr/local/texmex/corpus/sift.tar.gz && tar -xzf sift.tar.gz

# 2. Rostam (Go; TMPDIR must point at the dataset's parent)
TMPDIR=/tmp ROSTAM_SIFT1M=1 go test ./vector/ -run TestSIFT1MBench -v -timeout 40m

# 3. hnswlib (Python)
python3 -m venv /tmp/sift-venv && /tmp/sift-venv/bin/pip install hnswlib qdrant-client numpy
/tmp/sift-venv/bin/python bench/sift1m/hnswlib_bench.py

# 4. Qdrant (Docker service)
docker run -d --name qdrant-bench -p 6333:6333 -p 6334:6334 qdrant/qdrant
/tmp/sift-venv/bin/python bench/sift1m/qdrant_bench.py

# 5. Memory footprint
#  - hnswlib & Qdrant harnesses above print their own memory line.
#  - Qdrant container RAM (anon vs reclaimable file cache):
docker exec qdrant-bench sh -c 'cat /sys/fs/cgroup/memory.current; grep -E "^(anon|file) " /sys/fs/cgroup/memory.stat'
#  - Rostam, index-resident on the same SIFT corpus (input freed):
TMPDIR=/tmp ROSTAM_SIFT1M=1 go test ./vector/ -run TestMemCompareSIFT -v -timeout 20m

# 6. Latency (p50/p99) + saturated throughput
TMPDIR=/tmp ROSTAM_SIFT1M=1 go test ./vector/ -run TestSIFTLatencyQPS -v -timeout 30m   # Rostam in-process
TMPDIR=/tmp ROSTAM_SIFT1M=1 go test . -run TestVectorNetLatencyQPS -v -timeout 30m      # Rostam over TCP, Go client
/tmp/sift-venv/bin/python bench/sift1m/qdrant_latency_qps.py                            # Qdrant, 32 concurrent procs
#  - Rostam over TCP, matched pure-Python client (start server, then client):
ROSTAM_SIFT_DIR=/tmp/rostam-sift1m/sift go run ./bench/sift1m/rostam_server &           # loads SIFT, serves :7700
/tmp/sift-venv/bin/python bench/sift1m/rostam_py_client.py
#  - Rostam over gRPC, matched grpcio client (same wire+client as Qdrant):
/tmp/sift-venv/bin/pip install grpcio-tools                                             # one-time
ROSTAM_SIFT_DIR=/tmp/rostam-sift1m/sift go run ./bench/sift1m/rostam_grpc &             # nested module, serves :7701
/tmp/sift-venv/bin/python bench/sift1m/rostam_grpc_client.py
```

## Results

_Measured on: 20-core x86-64 (13th Gen Intel i9-13900H class), Linux. Query =
single-thread, in-process for Rostam/hnswlib; service round-trip (single gRPC
client) for Qdrant. Build = parallel for all three._

**Build (1M × 128, M=16, efConstruction=200):**

| Engine | Build time | vec/s | Notes |
|---|---|---|---|
| **Rostam** | 49.4 s | 20,246 | `BuildConcurrent`, 20 workers (FMA kernels, flat node slice, prefetch) |
| hnswlib | 38.3 s | 26,083 | parallel, all cores |
| Qdrant | 41.9 s | 23,867 | upload + background index |

**Recall@10 vs. query throughput (sweeping efSearch):**

| efSearch | Rostam recall / QPS | hnswlib recall / QPS | Qdrant recall / QPS* |
|---|---|---|---|
| 16  | 0.822 / 20,147 | 0.801 / 27,709 | 0.934 / 783 |
| 32  | 0.914 / 11,839 | 0.903 / 16,423 | 0.978 / 666 |
| 64  | 0.966 / 6,638  | 0.963 / 9,581  | 0.995 / 586 |
| 128 | 0.988 / 3,628  | 0.989 / 5,283  | 0.999 / 322 |
| 256 | 0.995 / 1,878  | 0.997 / 2,870  | 0.999 / 254 |
| 512 | 0.997 / 973    | 0.999 / 1,533  | 0.999 / 219 |

_Rostam QPS reflects three profile-driven optimizations: (1) FMA + 4-accumulator
distance kernels (the distance kernel was 62% of search time); (2) a flat node
slice replacing the `map[uint32]*node` (removes a per-candidate hash + cache
miss); (3) prefetching upcoming candidate vectors during traversal (the kernel
was memory-latency-bound — stalling on cache misses for the candidate vector,
not compute-bound). Cumulatively ~2.8× faster than the original (ef=64: 2,371 →
3,923 → 4,750 → 6,638 QPS), closing the gap vs hnswlib from ~4.0× to ~1.4×._

\* **Qdrant QPS includes the client + gRPC round-trip** (single sequential
client), not just the index — note it barely moves with efSearch (783 → 219),
i.e. it is round-trip-bound, not search-bound. Do not read it as an algorithm
comparison; it is the realistic networked-service throughput. The next section
re-measures it *properly* (concurrent, saturated).

### Latency & saturated throughput — embedded vs networked

The recall/QPS table above measures Rostam single-thread in-process and Qdrant
with one sequential client. Neither is a throughput number. This section fixes
that: **p50/p99 latency** (single client) and **saturated QPS** (all cores /
many concurrent clients busy), on the same 20-core box.

All on the same 20-core box, k=10, 32 concurrent clients for satQPS (processes,
not threads — the Python GIL caps a *threaded* client at ~280 QPS, an artifact).
Harnesses: **Rostam in-process** `TestSIFTLatencyQPS`; **Rostam binary, Go
client** `TestVectorNetLatencyQPS`; **Rostam binary, pure-Python client**
`rostam_server` + `rostam_py_client.py`; **Rostam over gRPC**
`rostam_grpc/` + `rostam_grpc_client.py` (the matched-transport-and-client
control — same gRPC + grpcio as Qdrant); **Qdrant over gRPC** `qdrant_latency_qps.py`.

| Setup (ef=64) | transport | client | recall | p50 | p99 | saturated QPS |
|---|---|---|---|---|---|---|
| Rostam **in-process** | — | — | 0.966 | 369 µs | 891 µs | 29,476 |
| Rostam **networked** | binary | Go | 0.961 | **267 µs** | 1.05 ms | **32,169** |
| Rostam **networked** | binary | pure-Python | 0.961 | 643 µs | 1.45 ms | 8,990 |
| Rostam **networked** | gRPC | Python (grpcio) | 0.960 | 565 µs | 1.34 ms | 25,484 |
| Qdrant **networked** | gRPC | Python (grpcio) | 0.995 | 4.83 ms | 6.85 ms | 1,634 |

Rostam in-process across the ef sweep, for reference: ef16 → 117 µs / 88,320 QPS;
ef128 → 657 µs / 16,427 QPS. Qdrant: ef16 → 4.23 ms / 2,426 QPS; ef128 → 4.65 ms
/ 1,269 QPS.

**Reading this honestly:**

- **Rostam over TCP ≈ Rostam in-process.** Putting Rostam behind its own TCP
  server costs essentially nothing — p50 267 µs and 32k QPS over loopback, the
  same as (within noise of) the in-process numbers. Its wire format is a few
  header bytes + the raw float32 query, one round-trip, Go server with
  `TCP_NODELAY`; the marshaling is a memcpy.
- **So the ~18× latency / ~20× QPS gap vs Qdrant is NOT "Rostam avoids the
  network."** When Rostam *also* pays for the wire it barely slows down. The gap
  is **Qdrant's per-query stack** — gRPC + protobuf + Python client + its request
  pipeline — versus Rostam's lean binary protocol. Both are networked here; one
  stack is just much heavier per call.
- **Apples-to-apples: same transport, same client, only the server differs.**
  The last two rows are *both* gRPC/HTTP2/protobuf, *both* the grpcio Python
  client, *both* 32 processes — Rostam behind a gRPC server (`rostam_grpc/` +
  `rostam_grpc_client.py`) vs Qdrant. **Rostam is ~8.5× lower latency (565 µs vs
  4.83 ms) and ~15.6× higher QPS (25,484 vs 1,634).** With the protocol and
  client language held identical, that gap is the **server implementation**:
  Rostam's lean Go HNSW search path vs Qdrant's full-DB request pipeline.
- **The client matters more than the protocol — and my first Python client was
  the bottleneck, not Rostam.** The pure-Python binary client (`struct` + socket,
  8,990 QPS) is *slower* than the gRPC client (25,484 QPS), because grpcio is
  C-accelerated while hand-rolled Python serialization is not. So the earlier
  "Python costs ~3.6× QPS" was really "a naive pure-Python client costs that"; a
  fast (C) client over gRPC gets Rostam to 25k QPS. Lesson: per-query client
  overhead dominates, and Rostam's *server* sustains ~25–32k QPS regardless of
  framing.
- **Qdrant does more per request** — it is a full database (payload indexing,
  versioning, segment/optimizer machinery), so part of the gap is generality
  Rostam doesn't carry. For *pure* vector kNN over gRPC, though, Rostam's server
  answers in ~0.5 ms where Qdrant takes ~4.8 ms on the identical wire.
- **Both are loopback on one box**; cross-machine adds real RTT to both equally.
  **Cross-engine `ef` isn't recall-matched** — at matched recall (Rostam ef64
  0.96 vs Qdrant ef16 0.935) the ratios hold.

Bottom line, decomposed honestly (p50 latency / saturated QPS):

| Factor | p50 | satQPS |
|---|---|---|
| Rostam HNSW search, in-process | ~0.37 ms | 29,476 |
| + Rostam's own TCP server + binary protocol (Go client) | 0.27 ms (noise) | 32,169 |
| + over gRPC with a fast (grpcio/C) client | 0.57 ms | 25,484 |
| **Qdrant, same gRPC + same client** | **4.83 ms** | **1,634** |

So: the embeddable in-process path is the headline (~0.37 ms), but even **Rostam
as a gRPC service, measured with Qdrant's own client and transport, is ~8.5×
lower latency and ~15× higher throughput** — that residual is the server, not the
wire or the client. The last purist step is a cross-machine run to confirm the
ratios survive real network RTT (which adds the same absolute latency to both).

**Memory footprint (1M × 128, M=16 — index resident, input vectors freed):**

| Engine / config | Hard RAM | Total resident | On disk (mmap) | Recall ceiling | What's where |
|---|---|---|---|---|---|
| **Qdrant** (default, f32) | **137 MB** (anon) | 909 MB (cgroup) | 608 MB | 0.999 | mmaps *everything* (vectors + graph) |
| **Rostam** `SQ8+Mmap+GraphMmap` | **281 MB** (heap) | 1000 MB | 488 MB + graph | ~0.97 | int8 codes in heap; float32 *and* level-0 graph mmap'd |
| **Qdrant + SQ8** (int8) | 321 MB (anon) | 1004 MB (cgroup) | 735 MB | 0.971 | int8 codes in RAM, float32 `on_disk` |
| **Rostam** `QuantSQ8+Mmap` | 411 MB (heap) | 1008 MB | 488 MB | ~0.97 | int8 codes + graph in heap; float32 mmap'd |
| **hnswlib** (f32) | 762 MB | 762 MB | — | 0.999 | vectors + graph, all anonymous RAM |
| **Rostam** `QuantNone` (f32) | 801 MB (heap) | 910 MB | — | 0.997 | vectors + graph on the Go heap |

_Rostam figures are after the flat level-0 adjacency slab (CSR-style graph
storage); before it they were 1379 MB (`QuantNone`) and 646 MB (`SQ8+Mmap`) —
see "The graph rewrite" below._

_Methodology: each engine builds the index, frees the input array, and reports
resident memory. "Hard RAM" is non-reclaimable anonymous memory — the floor your
server must provision: RSS for hnswlib, Go `HeapInuse` for Rostam, cgroup `anon`
for the Qdrant container. "On disk" is the mmap-backed segment the OS can evict
under pressure. Rostam: `go test -run TestMemCompareSIFT`; Qdrant: `docker exec …
cat /sys/fs/cgroup/memory.{current,stat}`. Qdrant + SQ8 = scalar quantization
with `always_ram=true` codes and `on_disk=true` originals (`QDRANT_SQ8=1`)._

### What the memory numbers show (honestly)

- **Float32: Rostam now ties hnswlib (801 vs 762 MB).** Before the graph rewrite
  Rostam's float32 index was 1379 MB — ~1.8× hnswlib — entirely because of how
  Go's `[][]uint32` graph was laid out (see below). With the flat slab the graph
  shrank from 746 MB to 262 MB and the gap to hnswlib's contiguous `linkLists`
  is now noise (~5%). The 488 MB of vectors are, of course, identical.
- **SQ8: Rostam now *beats* Qdrant-SQ8 on hard RAM — 281 MB vs 321 MB.** Two
  steps got here. First the flat slab cut SQ8 from 646 → 411 MB. Then
  `GraphMmapPath` moves the level-0 slab itself off-heap into an mmap file
  (`SQ8+Mmap+GraphMmap`), dropping hard heap another ~130 MB to **281 MB** — the
  vectors *and* the graph's largest array now live in (reclaimable) mmap files,
  leaving only the int8 codes, per-node lengths/upper levels, and the id table on
  the heap. This is the same disk-for-RAM lever Qdrant pulls, but Rostam keeps
  the *traversal-critical* int8 codes resident (Qdrant pages everything), so the
  hot path stays in RAM. Remaining heap levers if ever needed: dropping the
  `map[uint64]uint32` id table (36 MB) and per-node structs (46 MB).
- **Qdrant is still the most RAM-frugal by mmapping aggressively** (137 MB anon
  unquantized). That is a deliberate disk-for-RAM trade — every cache-cold hop is
  a page fault, which shows up in its QPS. Rostam keeps the hot path resident.

**Recall and speed were unaffected by the rewrite** — recall is bit-identical at
every efSearch and single-thread QPS rose ~4% (the flat slab has better cache
locality). So this was a pure memory win, no tradeoff.

**What Rostam wins overall: embeddability, and now a competitive memory
footprint.** The defensible edge remains the in-process, no-network-hop
deployment (≈2.4–6.9k QPS vs Qdrant's ≈586 at matched recall, purely from killing
the gRPC round-trip). Memory is now a *strength*, not a liability: Rostam ties
the hand-tuned C++ reference (hnswlib) on float32 and, with `GraphMmap`, holds
*less* hard RAM than Qdrant on quantized (281 vs 321 MB) — while keeping the
traversal hot path (int8 codes) resident rather than paging it from disk.

### What this shows (honestly)

- **Recall: Rostam ≈ hnswlib.** At every efSearch the two recall curves track
  within ~1% (Rostam is even marginally higher at low ef). Rostam's from-scratch
  HNSW is correct and competitive in *quality* against the reference C++ impl.
- **Query throughput: hnswlib ≈ 1.4× Rostam** in-process (e.g. @64: 9.6k vs 6.6k
  QPS). Three profile-driven optimizations (FMA kernels → flat node slice →
  prefetch) narrowed the gap from ~4.0× to ~1.4×. Rostam is now within striking
  distance of the hand-tuned C++ reference on raw single-thread QPS — with
  identical recall.
- **Build: hnswlib ≈ 1.3× Rostam.** `BuildConcurrent` (20 workers) plus the
  shared FMA kernels and prefetch brought Rostam's build to ~20.2k vec/s (from
  ~3k single-threaded originally); it still trails hnswlib's parallel build
  slightly (entrypoint-lock + hub contention cap scaling).
- **The embeddable angle is real and measured.** Rostam in-process does ~2.4k
  QPS @ ef=64; Qdrant over its service does ~586 QPS for the *same* recall —
  Rostam is ~4× higher **because there's no network round-trip**. A co-located
  Go service embedding Rostam genuinely avoids the overhead that dominates a
  networked vector DB. That — plus single-binary integration with no service to
  operate, and (since the graph rewrite) a memory footprint that ties hnswlib on
  float32 and is in Qdrant's league on quantized — is the honest Rostam story.

_Note: cross-engine `efSearch` is not perfectly comparable — Qdrant's higher
recall at low ef reflects differences in what each engine does per "ef" unit
(e.g. oversampling/rescore defaults), a known caveat in ANN benchmarking._

### Next levers (to close more of the query-QPS gap)

Profiling (`TestSearchProfile`) put the distance kernel at 62% of search time —
now addressed (FMA + 4 accumulators). The remaining hotspots, in order:

- ✅ **Per-node map lookup** — done: `nodes map[uint32]*node` → flat `[]*node`
  indexed by slot (removed the hash + a cascading cache miss; ~15–20% QPS).
- ✅ **Candidate-vector cache misses** — done: prefetch upcoming neighbors'
  vectors during traversal. The distance kernel was memory-latency-bound
  (stalling on the candidate-vector load, not the math); prefetching overlaps
  the miss with compute (~1.3–1.5× QPS).
- **`visitedSet.seen` (~9%)** — random-access cache misses on the epoch-stamp
  array; inherent to graph traversal, hard to beat much.
- ✅ **Scattered neighbor lists** — done: level-0 adjacency moved from a
  per-node `[][]uint32` to one flat slab (stride `2*M`, indexed by slot), upper
  levels kept as small per-node slices. This was primarily a **memory** win
  (graph 746 → 262 MB; see "The graph rewrite") and also lifted QPS ~4% from
  better cache locality.
- **Remaining memory levers (to beat Qdrant-SQ8, ~321 MB)** — mmap the level-0
  slab + codes (now flat arrays, so trivial to map), drop the
  `map[uint64]uint32` id table when ids are dense (use `slot = id-1`, −36 MB),
  and fold the per-node `*node` structs into parallel arrays (−~54 MB).

### The graph rewrite (flat level-0 slab)

`TestMemBreakdownSIFT` attributes the float32 heap by nulling each structure and
watching `HeapInuse` drop. Before, the graph alone cost **746 MB** to hold ~150
MB of edges — a ~4× blowup from the `neighbors [][]uint32` layout: ~3M tiny heap
objects (1M node structs, 1M outer header arrays, ~1.07M inner edge slices),
24-byte slice headers, and append-doubling capacity slack.

Level 0 (every node, cap `2*M`) now lives in a single flat `[]uint32` slab
indexed by slot, with a parallel `[]uint16` length array — mirroring hnswlib's
contiguous `linkLists`. Levels ≥1 (only ~6% of nodes) stay as small per-node
slices. Result at 1M × 128, M=16:

| | graph | QuantNone heap | SQ8+Mmap heap |
|---|---|---|---|
| before | 746 MB | 1379 MB | 646 MB |
| after | **262 MB** | **801 MB** | **411 MB** |

Behavior is preserved exactly: the back-edge overflow path re-selects `maxM`
among the same candidate set the old append-then-prune used (recall is
bit-identical at every efSearch), the snapshot wire format is unchanged, and the
concurrent build keeps its per-slot link-lock invariant (green under `-race`).

At ~1.4× of hand-tuned C++ with identical recall, Rostam's HNSW is firmly
production-grade (cf. Weaviate). Matching C++ exactly (1.0×) is unlikely; SQ8
quantization (4× less memory traffic) is the lever most likely to close the
rest for memory-bound workloads.

---

## Redis / RediSearch head-to-head

Rostam vs. **Redis Stack (RediSearch)** on SIFT-1M, both HNSW at **M=16,
efConstruction=200, L2** — same methodology as the rest of this doc.

- **RediSearch:** [`redis_bench.py`](./redis_bench.py) — RESP client (redis-py),
  HNSW vector index, `EF_RUNTIME` sweep, recall@10 vs. exact ground truth.
- **Rostam:** native binary TCP via the matched pure-Python client
  ([`rostam_py_client.py`](./rostam_py_client.py)). **Use the native-TCP path,
  not gRPC** — the Python-gRPC client's protobuf float serialization (128 floats
  per query) dominates single-thread latency and *understates* the engine by
  ~3.8× (614 µs vs 164 µs p50). RediSearch sends a raw float32 blob over RESP, so
  native-TCP-vs-RESP is the fair client/protocol match.

### Recall@10 — the fully fair metric (transport-independent)
| efSearch | **Rostam** | **RediSearch** |
|--:|:--:|:--:|
| 16  | 0.8219 | 0.8014 |
| 32  | 0.9145 | 0.9034 |
| 64  | 0.9661 | 0.9634 |
| 128 | 0.9883 | 0.9895 |
| 256 | 0.9954 | 0.9974 |
| 512 | 0.9968 | 0.9992 |

**Parity.** Two correct HNSW implementations — Rostam slightly ahead at low ef,
RediSearch slightly ahead at high ef, indistinguishable in the middle.

### Throughput & latency at ef=64 (recall ≈ 0.96)
*Both pure-Python clients, both native binary wire protocols, same 20-core box:*

| metric | **Rostam (native TCP)** | **RediSearch (RESP)** | Rostam (gRPC) |
|---|--:|--:|--:|
| single-thread p50 latency | **164 µs** | ~454 µs | 614 µs |
| single-thread QPS | **5,754** | 2,204 | 1,527 |
| saturated QPS (32 conc) | **47,943** | 5,679 | 26,504 |
| memory (1M × fp32, resident) | ~2.2 GiB | **1.81 GiB** | — |

On its native protocol Rostam wins both axes at equal recall — **~2.6×
single-thread, ~8.4× saturated**, at lower latency. RediSearch's limited query
threading caps it (~2.6× from 1→32 conc) the same way single-threaded Redis caps
in the [Networked KV suite](../netkv); Rostam scales across cores. RediSearch is
leaner on float32 RAM — Rostam's SQ8/mmap modes (see the memory section above)
close that but were not exercised in this run.

### Reproduce (RediSearch)
```bash
# Redis Stack (RediSearch) on :6380, host network
docker run -d --name redisstack --network host \
  -e REDIS_ARGS="--port 6380 --save '' --appendonly no" \
  redis/redis-stack-server:latest
/tmp/sift-venv/bin/pip install redis numpy
/tmp/sift-venv/bin/python redis_bench.py        # recall + networked QPS sweep
```

---

## More engines (roadmap)

This branch is where the networked-vector comparison grows. Same methodology
(M=16, efC=200, L2, efSearch sweep, recall@10 vs. exact ground truth, matched
client shape) for each new peer:

- [x] **Qdrant** — gRPC service (`qdrant_bench.py`, `qdrant_latency_qps.py`)
- [x] **RediSearch** — RESP service (`redis_bench.py`)
- [x] **hnswlib** — in-process C++ reference (`hnswlib_bench.py`)
- [ ] **Milvus** — gRPC service
- [ ] **Weaviate** — REST/gRPC service
- [ ] **pgvector** — Postgres HNSW
- [ ] **Aerospike Vector Search (AVS)** — *enterprise-gated: the
  `aerospike/aerospike-vector-search` image is not on public Docker Hub and needs
  an authenticated registry login + a feature-key file; deferred until creds are
  available.*
