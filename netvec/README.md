# What an RF=2 replication barrier costs the VECTOR write path

The companion to `../netkv/replication/`, which asks the same question of the KV
write path. This one is Rostam-only, and the answer turns out to be different in
kind rather than in degree.

## Why there is no competitor column

The KV sweep could line Rostam up against Aerospike, Redis, Valkey and KeyDB
because each exposes a per-write replication posture (`COMMIT_ALL` /
`COMMIT_MASTER`, `WAIT 1`). Qdrant, Milvus and turbopuffer expose **no
comparable knob**, so there is no honest cross-engine row to draw. What follows
is the delta between Rostam's own two postures — a statement about Rostam's
replication cost, not about who wins. For a competitive claim, VectorDBBench
(`../vectordbbench`) is the run that answers it; this is not a step toward it.

## The routing fact that dictates the setup

A vector op routes on the **collection name**, not the point id
(`ops/vector_routing.go`). An unpartitioned collection therefore lives entirely
on ONE shard, and the whole load lands on a single primary — measuring one node,
not a cluster. Partitions are what spread it (physical name `coll@gen#p`, one per
shard), so `-partitions` must equal the shard count. `netvec` warns when it is
≤1, because getting this wrong produces a plausible-looking number for a
single-node measurement.

## Running it

```sh
# The replication sweep (per-point writes, both commit postures).
BIN=./rostam-server NETVEC=./netvec ROOT=/tmp/vecbench ./vecsweep.sh

# The INGEST path you would actually load a collection with.
./netvec -addr 127.0.0.1:8001 -collection c -dim 128 -partitions 8 \
         -mode bulk -total 200000 -chunk 1000 -conns 16
```

The script brings each posture up, gates it, measures, and tears it down. It
refuses to proceed unless the cluster accepts a write and no shard reports
`under_replicated`, and it aborts outright if the non-loopback listener set ever
differs from the baseline it captured at start.

## Results — 2026-08-08, 12-vCPU EPYC-Genoa, 0 errors on every row

3 co-located nodes + generator, `GOMAXPROCS=3` per node, 8 shards, 8 partitions,
RF=2, dim 128 cosine, 100k id keyspace, single-point upserts over HTTP, 15 s
measured after 5 s warmup.

| conns | commit-all r1 | r2 | commit-master r1 | r2 | ratio r1 / r2 |
|--:|--:|--:|--:|--:|--:|
| 8 | 4305 | 3166 | 5165 | 3786 | **1.20x / 1.20x** |
| 32 | 4062 | 3748 | 4905 | 3765 | 1.21x / 1.00x |
| 128 | 4031 | 3813 | 3991 | 3825 | 0.99x / 1.00x |

p50 latency, commit-all: 1.8 ms / 7.7 ms / 37.4 ms at 8 / 32 / 128 conns.

### The table above measures the SLOW ingest path — read this first

Every row above uses single-point upserts, which is the path the replication
question needs (one write, one commit decision). It is **not** how you load a
collection, and quoting ~4k pts/s as "Rostam's vector ingest" would be wrong by
several times over.

Profiling found the cap, and it is architectural rather than incidental:
`shard/pbisr/engine.go` takes `writeMu` and then runs `e.ap.Apply(data)` **inside
it** — the entire local state-machine apply, which for a vector write is a whole
HNSW insert. A shard therefore admits ONE write at a time, so per-point ingest =
(primary shards) ÷ (insert latency), invariant to connection count and batch
size, with the machine ~90% idle. (The ordering is deliberate: applying before
assigning a seq means a failed apply burns no seq, and a phantom seq would
gap-reject every later write at the backups — a shard wedge.)

The designed loading path is stage + multi-core build, and it is not subject to
that lock. Same cluster, same 200k x 128d shape, fresh collection each:

| path | pts/s to searchable | |
|---|--:|---|
| `/points` per-point upsert | 4130 | capped by the per-shard sequencing lock |
| `/points/bulk` + `/bulk/build` | **14574** | stage 0.9s (~222k pts/s), build 12.8s |

**3.5x**, and the build dominates — staging itself is ~222k pts/s. `httpapi`'s own
comment reports ~6x on a larger 1M x 768d shape, so the multiple grows with the
load. Use bulk for ingestion; use the per-point number only for what it is, a
replication-barrier measurement.

**Throughput is flat in concurrency; latency is linear.** ~4k pts/s at every
connection count, with p50 rising 1.8 → 7.7 → 37.4 ms. 128 conns ÷ 4031 pts/s =
31.8 ms, which is the observed p50 — Little's Law for a fully saturated
bottleneck. This is qualitatively unlike the KV sweep, where throughput scaled
36k → 122k across the same connection range. The cap is the per-shard sequencing
lock described above, NOT replication, network, or offered concurrency — and not
raw compute either: the box is ~90% idle at saturation.

**The replication barrier is a much smaller share of a vector write than of a KV
write.** Commit-master buys **1.20x** at 8 connections, reproduced exactly across
both reps (1.1998 and 1.1959). The same knob buys **1.57x** on the KV path. HNSW
insert dominates the write, so relaxing the commit contract has proportionally
less to give.

**Past saturation the knob buys nothing at all** — 0.99x and 1.00x at 128
connections. Once the index build is the bottleneck, when the write acks stops
mattering.

**Commit-master trades tail latency for that throughput, badly.** At 128
connections its p99 is **503 ms** against commit-all's 75 ms, and p999 reaches
730 ms. Acking on local primary apply lets the replication backlog build, and it
discharges in stalls. The 1.20x at low concurrency is not free.

## Traps

**Rows are NOT independent samples — the index grows under them.** Every row after
the first inserts into a collection the previous rows already populated, and
upserts into a filling 100k id space become updates. Rep 2 is consistently below
rep 1 (commit-all @8: 4305 → 3166). Only **same-rep, cross-posture** comparisons
are valid — r1-vs-r1 and r2-vs-r2, which is why the table pairs them that way.
Each posture starts on a fresh data dir, so those pairings are apples-to-apples.
Do not average r1 and r2.

**Create the collection once, not per row.** Creating an EXISTING collection
answers `500 {"error":"internal error"}` — the real reason (`already
partitioned`) appears only in the server log, never on the wire. So a repeated
create is indistinguishable from a genuine failure client-side. `netvec -setup`
creates; measured rows run `-setup=false` and probe with a real write. An earlier
sweep that created per row silently lost 10 of 12 rows and still looked
superficially plausible — which is why a failed row now prints a `FAILED` line
rather than leaving a gap.

**These are not saturation ceilings for the engine.** The generator shares 12
vCPU with three server processes at `GOMAXPROCS=3`. A dedicated load host would
move every number.

**More shards is NOT the fix, and batching is not either — both were tested.**
8 → 32 shards gave only **1.28x** (3967 → 5073 pts/s) while CPU nearly doubled
(~1.8 → ~3.2 cores): the added per-group overhead eats the added parallelism,
exactly as the `-shards` flag's own "over-sharding burns CPU on goroutine churn"
warning predicts. HTTP batching gave **nothing at all** (batch 1/10/50 →
4352/4360/4282 pts/s), because `/points/batch` is documented as an inline
one-indexed-insert-per-point route — it never becomes one replicated entry. Do
not re-derive either of these.

**Verify which process owns the pprof port before trusting a profile.**
`ROSTAM_PPROF=127.0.0.1:6060` binds via `_ = http.ListenAndServe`, so a failed
bind is SILENT. A stale server surviving `pkill -x` (the kernel truncates `comm`
to 15 chars, so `-x rostam-server-fix2` matches nothing) kept the port, the new
node's pprof never bound, and a whole profiling round measured the wrong, idle
process. The tell was an absurd 0.24% CPU for a node serving 4k writes/s. Check
`ss -ltnp | grep :6060` against the PID you meant to profile.

**`pprof -top` cannot identify a lock; use `-traces`.** `-top` on the mutex
profile showed only `sync.(*Mutex).Unlock` at 99.86%, which names nothing.
`go tool pprof -traces` printed the full stack and pinned it to
`pbisr.(*Engine).proposeSequenced` on the first try.

**Posture is invisible in the output.** `-pb-commit-primary` is a server-side
flag; both postures print identical result lines. The sweep prints a banner per
posture for exactly this reason — do not reorder the blocks without moving it.

**Bind everything to loopback.** These are unauthenticated data stores, and the
benchmark host had `ufw` inactive with `iptables -P INPUT ACCEPT` — a `0.0.0.0`
bind would have exposed the database to the internet. The sweep's port gate
compares the non-loopback listener set against the baseline and kills the run on
any difference.

**Requires the PB leader-hint fixes.** Before them, HTTP writes to a shard the
contacted node backed failed outright, and partitioned-collection creation failed
on every node — so this benchmark could not run at all. See the `fix/pb-notleader-hint`
work.
