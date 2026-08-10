# Replicated-write benchmark for KV engines

How to measure Rostam against other KV stores **when every engine must get the
write onto a second machine before acking**, and how to avoid the several ways
this measurement quietly lies.

Single-node numbers are a different benchmark. `netkv/README.md` covers those.
This kit is only about the replication barrier.

## Why replication factor 2

RF=2 is the one shape where the engines are contractually comparable:

| engine | what an ack means at RF=2 |
|---|---|
| Rostam PB (default) | full ISR = **2-of-2** in-memory acks |
| Rostam Raft | majority of 2 = **2-of-2** |
| Aerospike `COMMIT_ALL` | master + its **one** replica |
| Redis / Valkey / KeyDB | `SET` then `WAIT 1` = **one** replica ack |

Every engine waits for every copy, so a difference is a difference in the engine
rather than in what was promised. At RF=3 this breaks down: Rostam PB waits
3-of-3 while Raft waits 2-of-3, so PB is doing strictly more work for the same
row.

None of this is *local crash* durability. Every configuration here commits on
in-memory replication acks with no per-write fsync, so a simultaneous power loss
of the acking nodes loses acked writes. That is a deliberate, matched choice —
it isolates replication cost — but it is not what a single-node fsync benchmark
measures.

## Running it

```sh
# 1. Bring up whichever engines you are comparing (each gates itself and refuses
#    to proceed if replication did not actually attach).
./up-aerospike-rf2.sh
./up-redis-family.sh
./up-rostam-pb-rf2.sh /path/to/rostam-server                # commit-all
# ...or, for the commit-master posture:
./up-rostam-pb-rf2.sh /path/to/rostam-server commit-master

# 2. Sweep. ONE run at a time — see "Traps" below.
./run-repl-bench.sh /path/to/netkv 2

# 3. Tear down and prove nothing is left listening.
./down.sh
```

Rostam and Aerospike each want the box to themselves; on a machine that cannot
host both plus the generator, stop one while measuring the other.

## Results — 2026-08-05, 12-vCPU EPYC-Genoa, n=2 reps, 0 errors on every run

3 co-located nodes + generator, `GOMAXPROCS=3` per node, 8 shards, PUT-only,
100k keys, 256 B values, 15 s measured after 5 s warmup. Reps agreed within ±4%.

| engine (replica-ack) | 8 conns | 32 conns | 128 conns | p50 @128 |
|---|--:|--:|--:|--:|
| **Rostam PB RF=2 commit-all** | **36.0k** | **72.3k** | **121.8k** | **947 µs** |
| Rostam PB RF=2 commit-master | 56.7k | 86.5k | 151.0k | 624 µs |
| Aerospike RF=2 `COMMIT_ALL` | 36.4k | 51.0k | 81.5k | 1406 µs |
| Aerospike RF=2 `COMMIT_MASTER` | 57.6k | 84.6k | 107.1k | 976 µs |
| Redis 7 (`WAIT 1`) | 31.0k | 41.4k | 40.7k | 3174 µs |
| KeyDB (`WAIT 1`) | 26.2k | 42.9k | 39.7k | 3194 µs |
| Valkey 8 (`WAIT 1`) | 27.8k | 39.5k | 37.7k | 3368 µs |
| _memcached (NO replication)_ | _91.5k_ | _209.0k_ | _280.6k_ | _378 µs_ |

At matched commit semantics Rostam ties Aerospike at 8 connections and leads
**+42% / +49%** at 32/128; against the Redis-protocol engines it leads **1.16x /
1.75x / 2.99x**. Commit-master buys Rostam 1.57x at 8 connections, essentially
what it buys Aerospike (1.58x).

## Reproduction — 2026-08-09, same box, n=2 reps, 0 errors on every run

Same machine, same scripts, same shape. Aerospike CE 8.1.2.0 (the run above says
8.1). Medians of 2 reps:

| engine (replica-ack) | 8 conns | 32 conns | 128 conns | p50 @128 |
|---|--:|--:|--:|--:|
| Rostam PB RF=2 commit-all | 35.6k | **77.5k** | **123.1k** | **917 µs** |
| Aerospike RF=2 `COMMIT_ALL` | **37.0k** | 58.8k | 89.1k | 1305 µs |
| Rostam PB RF=2 commit-master | 57.8k | **103.0k** | **157.4k** | **549 µs** |
| Aerospike RF=2 `COMMIT_MASTER` | **65.3k** | 94.2k | 125.7k | 845 µs |

**Rostam reproduced within ~1-4% at 8 and 128 connections; Aerospike came out
9-17% faster than it did on 08-05.** So the lead at matched semantics narrows
from +42%/+49% to **+32%/+38%** at 32/128. Direction and rough magnitude hold;
the exact multiple does not, which is what this box's documented cross-session
drift predicts — it is why every row is quoted same-session.

**Two claims from the 08-05 run that this reproduction corrects:**

- *"Rostam ties Aerospike at 8 connections."* It does not, in either posture.
  Aerospike is ahead at 8 by 4% (commit-all) and 13% (commit-master), and holds
  a clearly better tail there (p99 288 µs against 369 µs). Rostam's advantage is
  a **scaling** property: it arrives at 32 connections and widens through 128.
  That is the more useful characterisation anyway — it tells a reader which
  regime they are in rather than declaring a winner.
- *"Commit-master buys Rostam 1.57x, essentially what it buys Aerospike (1.58x)."*
  Not here: at 8 connections it is worth **1.62x** to Rostam and **1.76x** to
  Aerospike. The posture is not quite neutral between them, so a comparison run
  in only one posture does carry a small bias — which is the argument for
  publishing the full 2x2 rather than either half.

### Full field, 2026-08-09 — and two things the kit does not say

Re-ran with every engine up, medians of 2 reps, 0 errors on every row:

| engine (replica-ack) | 8 conns | 32 conns | 128 conns | p50 @128 |
|---|--:|--:|--:|--:|
| **Rostam PB commit-all** | 35.6k | **80.9k** | **113.6k** | **0.99 ms** |
| Aerospike `COMMIT_ALL` | **38.0k** | 57.0k | 85.9k | 1.35 ms |
| Redis 7 (`WAIT 1`) | 28.7k | 43.2k | 42.9k | 2.95 ms |
| Valkey 8 (`WAIT 1`) | 28.0k | 42.2k | 45.9k | 2.72 ms |
| KeyDB (`WAIT 1`) | 28.9k | 42.1k | 43.9k | 2.86 ms |
| _memcached (NO replication)_ | _106.5k_ | _216.1k_ | _271.7k_ | _0.40 ms_ |

The Redis-protocol engines reproduce within a few percent of 08-05 — their curve
flattens above 32 connections, which is a per-instance ceiling rather than
anything that drifts. Aerospike is the only engine here that moved much.

**Dragonfly is excluded because it does not work under `WAIT 1`, not because it
is slow.** It posts 78-1249 ops/s at a p50 of ~102 ms at every concurrency, with
`errs=0` — the waits are *succeeding*, each taking about a hundred milliseconds.
That is a broken interaction with its replication path, not a measurement, and a
number that shape must never be published as a competitor's throughput.
`up-redis-family.sh` still starts it and the driver still sweeps it, so anyone
running this kit will see those rows; they are not a result. Its absence from
the tables above was correct and, until now, unexplained.

**Every row is measured with all the other engines resident and idle, which is
not free.** During a run where memcached was the engine under test, Aerospike's
three nodes were burning ~75% of a core between them and Rostam's three ~67%,
for ~1.6 of the box's 12 cores gone to processes not being measured, plus 6.5 GB
of RSS. Measured directly: at 128 connections Rostam drops 123.1k -> 115.6k
(-6%) and Aerospike 89.1k -> 85.9k (-4%) purely from bringing the rest of the
field up alongside them.

That tax is roughly symmetric between the two clustered engines, so their ratio
largely survives — but it is NOT symmetric for memcached, which is measured
while both 3-node clusters spin and is therefore understated. It also compresses
the whole table and penalises the fastest engine hardest, which is the same
direction as the co-located-client caveat already noted, from a second cause
nobody had written down. A one-engine-at-a-time protocol (bring up, measure,
tear down) would remove it, at the cost of no longer being comparable with any
row measured this way.

**This is system-vs-system at equal replication, not per-core efficiency.** Redis
and Valkey are single-threaded per instance and used ~1-2 cores; KeyDB and
Dragonfly are multi-threaded; the Rostam cluster spanned three processes at
`GOMAXPROCS=3`. A per-core comparison is a different measurement and would narrow
the gap substantially. The generator also shared the box, so no row here is a
saturation ceiling for any engine.

## Traps

**Never run two sweeps at once.** This is the one that actually produced a false
conclusion. An earlier pass showed Rostam's `-pb-commit-primary` doing nothing —
almost motivating a pointless rewrite of the replication send path — because a
second sweep was launched while the first was still running. Two benchmarks on 12
vCPU dropped every baseline ~50% (Rostam commit-all @128: 81k contended vs 122k
clean) and buried a real 1.57x effect in noise. Confirm the previous run exited.

**Verify replication actually attached.** `up-*.sh` gate on
`connected_slaves:1` and on Aerospike's `cluster_size=3` +
`effective_replication_factor=2`, and refuse to continue otherwise. Without the
gate an engine whose replica never attached posts a great number for an invalid
reason. Also check `errs=0` on every row: `netkv -sync` counts a `WAIT` timeout
as an error precisely so an under-replicated run cannot masquerade as fast.

**RF=1 makes the commit-posture axis vanish.** `netkv/aerospike-mem.conf` is
`replication-factor 1` with `address local` and no mesh seeds — a single-node
island. With no replica, `COMMIT_ALL` and `COMMIT_MASTER` are indistinguishable,
so a 2x2 run against it silently yields two identical Aerospike columns. That is
why `up-aerospike-rf2.sh` exists.

**Rostam's commit posture is invisible in the output.** `-sync` is ignored for
the rostam engine — the contract is server-side (full-ISR by default,
commit-master under `-pb-commit-primary`). Both postures print an identical
result line, so record which flag the server was launched with out of band.

**Dragonfly's `WAIT` measures a timer, not the engine.** It returned 78 / 313 /
1245 ops/s with p50 pinned at ~101.9 ms at *every* connection count. A latency
that does not move with offered load is a fixed interval — its replication-ack
granularity — so the harness was timing that. It is excluded rather than reported
as a ~1000x deficit. Comparing Dragonfly needs a different ack mechanism.

**memcached is not a peer.** No replication of any kind, so it is a
barrier-removed reference showing what this harness and box produce with the
guarantee taken away.

**Bind everything to loopback.** These are unauthenticated data stores and a
benchmark host frequently has no firewall — check `ufw status` and
`iptables -S INPUT`. Aerospike's `address any` will expose an unauthenticated
database to the internet. `down.sh` prints the non-loopback listeners so you can
confirm what is left.

**Rostam needs the shard-formation fix for RF < node count.** Older builds left
every shard whose owner set excluded the `-bootstrap` node permanently
leaderless, so writes to those keys hung — which reads as a slow benchmark rather
than a broken cluster. Workaround on an old build: pass `-bootstrap` to all
nodes.
