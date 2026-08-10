// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 RostamLabs
//
// netkv is a FAIR networked KV throughput/latency shootout. The same load
// generator (preload N keys, then C concurrent workers issue GET/PUT for D
// seconds, each op timed) drives every engine — only the per-op client call
// differs. This isolates the wire-protocol + server + storage stack, which is
// the real comparison for a networked store (the in-process cache is ~10 ns and
// vanishes under microsecond network round-trips).
//
// Durability/consistency semantics differ by engine and are NOT equalized —
// they're reported so the numbers are read honestly:
//   - Rostam: single-node Direct backend (no Raft), in-memory, no per-op fsync.
//     Read-write ops (PUT and the custom "aru" atomic update) serialize on one
//     mutex — the same contract Raft's single FSM-apply provides in Embedded.
//   - Redis:  single instance, in-memory, no per-op fsync, no replication.
//   - Aerospike: single node, in-memory namespace, no replication.
//
// Workloads (-mode): "kv" (GET/PUT by -readpct) plus five single-record op
// modes comparing Rostam's native op against Aerospike's own native path:
// "atomic" (multi-field update: Add + max expression), "incr" (AddOp),
// "cas" (optimistic concurrency: conditional write + retry, symmetric both
// sides), "append" (AppendOp), and "bitmask" (BitLShiftOp). All are a single
// round trip (cas is a read+conditional-write loop on both engines).
package main

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	as "github.com/aerospike/aerospike-client-go/v8"
	ast "github.com/aerospike/aerospike-client-go/v8/types"
	"github.com/bradfitz/gomemcache/memcache"
	redis "github.com/redis/go-redis/v9"

	"github.com/rostamlabs/rostam/client"
	"github.com/rostamlabs/rostam/ops"
)

// engine abstracts a networked KV store. Setup preloads the keyspace; get/put
// issue one round-trip. All methods must be safe for concurrent use.
type engine interface {
	name() string
	semantics() string
	setup(keys [][]byte, val []byte) error
	get(ctx context.Context, key []byte) error
	put(ctx context.Context, key, val []byte) error
	close()
}

// opEngine is implemented by engines that support the non-kv workloads
// (atomic | incr | cas | append | bitmask). setupOp seeds the per-mode record
// shape for every key; doOp issues the single measured round-trip for that mode.
// Both must be safe for concurrent use.
type opEngine interface {
	setupOp(mode string, keys [][]byte) error
	doOp(mode string, ctx context.Context, rng *rand.Rand, key []byte) error
}

func main() {
	var (
		engineName = flag.String("engine", "rostam", "rostam|redis|valkey|dragonfly|keydb|memcached|aerospike")
		conns      = flag.Int("conns", 64, "concurrent workers / pool size")
		durSec     = flag.Int("duration", 8, "load duration seconds (after preload)")
		nKeys      = flag.Int("keys", 100_000, "number of distinct keys")
		valSize    = flag.Int("valsize", 256, "value size bytes")
		readPct    = flag.Int("readpct", 100, "percent of ops that are GET (rest PUT)")
		rostamAddr = flag.String("rostam", "127.0.0.1:7000", "rostam tcp host:port (external server)")
		redisAddr  = flag.String("redis", "127.0.0.1:6379", "redis host:port")
		dflyAddr   = flag.String("dragonfly", "127.0.0.1:6380", "dragonfly host:port (redis wire)")
		keydbAddr  = flag.String("keydb", "127.0.0.1:6381", "keydb host:port (redis wire)")
		valkeyAddr = flag.String("valkey", "127.0.0.1:6382", "valkey host:port (redis wire)")
		mcAddr     = flag.String("memcached", "127.0.0.1:11211", "memcached host:port")
		pipeline   = flag.Int("pipeline", 0, "rostam only: requests in flight per pipelined conn (0 = classic 1-in-flight client)")
		pipeconns  = flag.Int("pipeconns", 4, "rostam only: pipelined conns per node (used with -pipeline)")
		aeroHost   = flag.String("aero", "127.0.0.1", "aerospike host")
		aeroPort   = flag.Int("aeroport", 3000, "aerospike port")
		warmSec    = flag.Int("warmup", 1, "warmup seconds (not counted)")
		mode       = flag.String("mode", "kv", "kv | atomic | incr | cas | append | bitmask")
		dist       = flag.String("dist", "uniform", "key distribution: uniform | zipf (hot-key, s=1.1)")
		// sync selects SYNCHRONOUS replication: a write is not acked until it has
		// reached a replica. This is the Tier-3 fairness axis — every engine in a
		// -sync run provides the same "no acked write lost if one node dies"
		// guarantee, so the numbers are comparable. Off (default) = each engine's
		// native async replication (ack immediately, replicate in the background).
		//   redis/valkey/keydb: SET then WAIT 1 (block until 1 replica acks)
		//   aerospike:          CommitLevel = COMMIT_ALL (vs COMMIT_MASTER async)
		//   rostam:             always synchronous (Raft majority) — has no async
		//                       mode, so it is only ever run with -sync.
		syncRepl = flag.Bool("sync", false, "synchronous replication: ack only after a replica has the write (see per-engine notes in source)")
		waitMs   = flag.Int("waitms", 1000, "sync-mode replica-ack timeout in ms (redis WAIT); a write that misses it counts as an error")
	)
	flag.Parse()

	keys := make([][]byte, *nKeys)
	for i := range keys {
		// 16-byte keys: fixed prefix + index, deterministic across engines.
		k := make([]byte, 16)
		k[0], k[1], k[2], k[3] = 'k', 'e', 'y', ':'
		k[4] = byte(i)
		k[5] = byte(i >> 8)
		k[6] = byte(i >> 16)
		k[7] = byte(i >> 24)
		keys[i] = k
	}
	val := make([]byte, *valSize)
	for i := range val {
		val[i] = byte('a' + i%26)
	}

	var eng engine
	var err error
	switch *engineName {
	case "rostam":
		if *pipeline > 0 {
			eng, err = newRostamPipe(*rostamAddr, *conns, *pipeline, *pipeconns)
		} else {
			eng, err = newRostam(*rostamAddr, *conns)
		}
	case "redis":
		eng, err = newRedisNamed(*redisAddr, *conns, "redis", *syncRepl, *waitMs)
	case "valkey":
		// The Linux Foundation fork of Redis (post-2024 licence change), where
		// much of the ecosystem moved. Valkey 8 added multi-threaded I/O, so
		// unlike Redis it need not plateau at one core — which is exactly why
		// testing Redis without it measures the engine people are leaving.
		eng, err = newRedisNamed(*valkeyAddr, *conns, "valkey", *syncRepl, *waitMs)
	case "dragonfly":
		eng, err = newRedisNamed(*dflyAddr, *conns, "dragonfly", *syncRepl, *waitMs)
	case "keydb":
		eng, err = newRedisNamed(*keydbAddr, *conns, "keydb", *syncRepl, *waitMs)
	case "memcached":
		eng, err = newMemcached(*mcAddr, *conns)
	case "aerospike":
		eng, err = newAerospike(*aeroHost, *aeroPort, *conns, *syncRepl)
	default:
		fmt.Fprintf(os.Stderr, "unknown engine %q\n", *engineName)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "setup %s: %v\n", *engineName, err)
		os.Exit(1)
	}
	defer eng.close()

	// op is the single round-trip issued per iteration; label describes the
	// workload in the result line. Both are chosen by -mode.
	var op func(ctx context.Context, rng *rand.Rand, key []byte) error
	var label string

	switch *mode {
	case "kv":
		fmt.Fprintf(os.Stderr, "[%s] preloading %d keys (%d B values)...\n", eng.name(), *nKeys, *valSize)
		if err := eng.setup(keys, val); err != nil {
			fmt.Fprintf(os.Stderr, "preload %s: %v\n", *engineName, err)
			os.Exit(1)
		}
		label = fmt.Sprintf("read%%=%d", *readPct)
		op = func(ctx context.Context, rng *rand.Rand, key []byte) error {
			if rng.Intn(100) < *readPct {
				return eng.get(ctx, key)
			}
			return eng.put(ctx, key, val)
		}
	case "atomic", "incr", "cas", "append", "bitmask":
		oe, ok := eng.(opEngine)
		if !ok {
			fmt.Fprintf(os.Stderr, "engine %q has no %q mode\n", eng.name(), *mode)
			os.Exit(2)
		}
		fmt.Fprintf(os.Stderr, "[%s] seeding %d records for %q...\n", eng.name(), *nKeys, *mode)
		if err := oe.setupOp(*mode, keys); err != nil {
			fmt.Fprintf(os.Stderr, "seed %s: %v\n", *engineName, err)
			os.Exit(1)
		}
		label = *mode
		op = func(ctx context.Context, rng *rand.Rand, key []byte) error {
			return oe.doOp(*mode, ctx, rng, key)
		}
	default:
		fmt.Fprintf(os.Stderr, "unknown mode %q (want kv|atomic|incr|cas|append|bitmask)\n", *mode)
		os.Exit(2)
	}

	if *dist == "zipf" {
		label += " dist=zipf"
	}
	res := run(keys, *conns, time.Duration(*durSec)*time.Second, time.Duration(*warmSec)*time.Second, *dist == "zipf", op)
	res.print(eng, *conns, label)
}

type result struct {
	ops     int64
	errs    int64
	elapsed time.Duration
	lat     []int64 // nanoseconds, merged
}

// run executes the load: `conns` workers each loop until the deadline, picking a
// random key per op and issuing GET (readPct%) or PUT. Latencies recorded only
// after the warmup window so connection-setup and JIT effects don't skew tails.
func run(keys [][]byte, conns int, dur, warm time.Duration, zipf bool, op func(ctx context.Context, rng *rand.Rand, key []byte) error) result {
	var ops int64
	var errs int64
	perWorker := make([][]int64, conns)

	start := time.Now()
	warmDeadline := start.Add(warm)
	deadline := start.Add(warm + dur)
	ctx := context.Background()

	var wg sync.WaitGroup
	for w := 0; w < conns; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(int64(w)*2654435761 + 1)) //nolint:gosec // benchmark workload, not crypto
			// Key picker: uniform, or Zipfian (hot-key, s=1.1) so a small head of
			// keys takes most traffic — the distribution that exposes read/lock
			// contention on both the store and its client pool.
			pick := func() int { return rng.Intn(len(keys)) }
			if zipf {
				z := rand.NewZipf(rng, 1.1, 1, uint64(len(keys)-1))
				pick = func() int { return int(z.Uint64()) } //nolint:gosec // bounded by len(keys)-1
			}
			lat := make([]int64, 0, 1<<16)
			var localOps, localErrs int64
			for {
				now := time.Now()
				if now.After(deadline) {
					break
				}
				idx := pick()
				t0 := time.Now()
				e := op(ctx, rng, keys[idx])
				d := time.Since(t0).Nanoseconds()
				if now.After(warmDeadline) {
					localOps++
					if e != nil {
						localErrs++
					}
					lat = append(lat, d)
				}
			}
			perWorker[w] = lat
			atomic.AddInt64(&ops, localOps)
			atomic.AddInt64(&errs, localErrs)
		}(w)
	}
	wg.Wait()
	elapsed := time.Since(warmDeadline) // measured window only

	total := 0
	for _, l := range perWorker {
		total += len(l)
	}
	merged := make([]int64, 0, total)
	for _, l := range perWorker {
		merged = append(merged, l...)
	}
	sort.Slice(merged, func(i, j int) bool { return merged[i] < merged[j] })
	return result{ops: ops, errs: errs, elapsed: elapsed, lat: merged}
}

func pct(sorted []int64, p float64) int64 {
	if len(sorted) == 0 {
		return 0
	}
	i := int(p / 100 * float64(len(sorted)))
	if i >= len(sorted) {
		i = len(sorted) - 1
	}
	return sorted[i]
}

func (r result) print(eng engine, conns int, label string) {
	sec := r.elapsed.Seconds()
	throughput := float64(r.ops) / sec
	us := func(ns int64) float64 { return float64(ns) / 1000.0 }
	fmt.Printf("engine=%-9s conns=%-3d %-20s  %10.0f ops/s   ops=%-9d errs=%-6d  p50=%6.1fµs p99=%7.1fµs p999=%8.1fµs  | %s\n",
		eng.name(), conns, label, throughput, r.ops, r.errs,
		us(pct(r.lat, 50)), us(pct(r.lat, 99)), us(pct(r.lat, 99.9)), eng.semantics())
}

// ---------- Rostam (pure client → EXTERNAL rostam-server process) ----------
//
// The server runs as a SEPARATE OS process (cmd/rostam-server -tcp ...), so
// every round-trip pays the same cross-process context switch that Redis and
// Aerospike pay. An embedded same-process server would skip that switch and
// hand Rostam an unfair ~3x latency edge — so we don't embed.

type rostamEngine struct {
	c *client.Client
	// How many endpoints we were pointed at. semantics() reports this instead
	// of asserting a topology the client cannot see: the same binary drives a
	// single-node direct server and a replicated cluster, and hardcoding
	// "single-node direct" made replicated runs print a flatly false claim.
	servers int
}

func newRostam(addr string, conns int) (engine, error) {
	// addr may be a comma-separated list so the client is cluster-aware (spreads
	// conns across nodes + routes to the shard leader), matching how a real
	// deployment and the Aerospike/Redis clients connect. A single addr forces
	// every write through one node, which forwards non-local-leader shards (an
	// extra hop) — a latency handicap on a distributed cluster.
	servers := strings.Split(addr, ",")
	// Give the client the ops registry so it does SHARD-AWARE LEADER ROUTING:
	// it extracts the key, computes shardOf(key)=xxhash%NumShards (identical to
	// the server), and sends straight to that shard's leader. Without this the
	// client round-robins over servers and ~2/3 of writes hit a non-leader ->
	// NotLeader bounce -> retry (2 round-trips). This matches how the Aerospike
	// client is partition-aware; omitting it was an accidental handicap.
	reg := ops.NewRegistry()
	if err := ops.RegisterBuiltins(reg); err != nil {
		return nil, err
	}
	c, err := client.New(client.Config{
		Servers:                 servers,
		MaxConnsPerServer:       int32(conns), //nolint:gosec // benchmark
		Ops:                     reg,
		TopologyRefreshInterval: 2 * time.Second, // required with Ops; keeps the shard-leader table current
	})
	if err != nil {
		return nil, err
	}
	// Probe so a dead server fails fast with a clear error.
	if _, err := c.Call(context.Background(), "put", ops.EncodePutArgs([]byte("__probe__"), []byte("x"), 0)); err != nil {
		return nil, fmt.Errorf("rostam: server at %s unreachable: %w", addr, err)
	}
	return &rostamEngine{c: c, servers: len(servers)}, nil
}

func (e *rostamEngine) name() string { return "rostam" }

// semantics reports only what this process can actually observe. Note there is
// no -sync knob here, unlike the Redis and Aerospike engines: Rostam's commit
// contract is a SERVER-side property (a pb-mode shard's Propose waits for its
// full ISR; a raft shard commits on a majority), so the client cannot and does
// not select it per write. The launch flags of rostam-server decide it.
func (e *rostamEngine) semantics() string {
	if e.servers > 1 {
		return fmt.Sprintf("cluster: %d endpoints, shard-leader routed; commit contract set server-side", e.servers)
	}
	return "single endpoint (direct or single-node cluster), in-memory"
}
func (e *rostamEngine) setup(keys [][]byte, val []byte) error {
	ctx := context.Background()
	for _, k := range keys {
		if _, err := e.c.Call(ctx, "put", ops.EncodePutArgs(k, val, 0)); err != nil {
			return err
		}
	}
	return nil
}
func (e *rostamEngine) get(ctx context.Context, key []byte) error {
	_, err := e.c.Call(ctx, "get", ops.EncodeKeyArgs(key))
	return err
}
func (e *rostamEngine) put(ctx context.Context, key, val []byte) error {
	_, err := e.c.Call(ctx, "put", ops.EncodePutArgs(key, val, 0))
	return err
}
func (e *rostamEngine) close() {
	_ = e.c.Close()
}

// rostamAppendSuffix is the fixed 8-byte suffix the "append" workload appends
// each op (matches the Aerospike side's 8-char "rostam!!" suffix).
var rostamAppendSuffix = []byte("rostam!!")

// setupOp seeds the per-mode record shape for every key via the "put" builtin.
func (e *rostamEngine) setupOp(mode string, keys [][]byte) error {
	ctx := context.Background()
	var seed []byte
	switch mode {
	case "atomic":
		seed = make([]byte, 24) // [games i64][best i64][score i64]
	case "incr", "cas", "bitmask":
		seed = make([]byte, 8) // one i64/uint64 counter
	case "append":
		seed = []byte{} // empty, grows by append
	default:
		return fmt.Errorf("rostam: unsupported mode %q", mode)
	}
	for _, k := range keys {
		if _, err := e.c.Call(ctx, "put", ops.EncodePutArgs(k, seed, 0)); err != nil {
			return err
		}
	}
	return nil
}

// doOp issues the single measured round-trip for the given workload mode.
func (e *rostamEngine) doOp(mode string, ctx context.Context, rng *rand.Rand, key []byte) error {
	switch mode {
	case "atomic":
		// streak varies so the max() branch is genuinely exercised; points is
		// fixed (a per-match score). Both go in args so the handler stays
		// deterministic (see the Raft determinism note in the README).
		return e.atomicUpdate(ctx, key, rng.Int63n(1000), 10)
	case "incr":
		_, err := e.c.Call(ctx, "incr", ops.EncodeIncrArgs(key, 1))
		return err
	case "cas":
		return e.casIncr(ctx, key)
	case "append":
		args := append(ops.EncodeKeyArgs(key), rostamAppendSuffix...)
		_, err := e.c.Call(ctx, "app", args)
		return err
	case "bitmask":
		_, err := e.c.Call(ctx, "shft", ops.EncodeKeyArgs(key))
		return err
	default:
		return fmt.Errorf("rostam: unsupported mode %q", mode)
	}
}

// getI64 reads an i64 counter via the "get" builtin (response is 8 bytes BE;
// a shorter/absent response reads as 0).
func (e *rostamEngine) getI64(ctx context.Context, key []byte) (int64, error) {
	res, err := e.c.Call(ctx, "get", ops.EncodeKeyArgs(key))
	if err != nil {
		return 0, err
	}
	if len(res) < 8 {
		return 0, nil
	}
	return int64(binary.BigEndian.Uint64(res[0:8])), nil
}

// casIncr is a read + conditional-write + retry-on-conflict increment loop,
// symmetric with the Aerospike generation-based CAS: read the counter, attempt
// a "casw" conditional write of cur+1, and on a lost race re-read and retry.
func (e *rostamEngine) casIncr(ctx context.Context, key []byte) error {
	cur, err := e.getI64(ctx, key)
	if err != nil {
		return err
	}
	for i := 0; i < 1000; i++ {
		args := ops.EncodeKeyArgs(key)
		var p [16]byte
		binary.BigEndian.PutUint64(p[0:8], uint64(cur))
		binary.BigEndian.PutUint64(p[8:16], uint64(cur+1))
		args = append(args, p[:]...)
		res, err := e.c.Call(ctx, "casw", args)
		if err != nil {
			return err
		}
		if len(res) >= 1 && res[0] == 1 {
			return nil
		}
		// lost the race: re-read and retry.
		cur, err = e.getI64(ctx, key)
		if err != nil {
			return err
		}
	}
	return fmt.Errorf("cas: too many retries")
}

// atomicUpdate calls the custom native Go op "aru". args = std [keyLen u16][key]
// prefix (so the op routes like the builtins) + [streak i64][points i64].
func (e *rostamEngine) atomicUpdate(ctx context.Context, key []byte, streak, points int64) error {
	args := ops.EncodeKeyArgs(key)
	var p [16]byte
	binary.BigEndian.PutUint64(p[0:8], uint64(streak))
	binary.BigEndian.PutUint64(p[8:16], uint64(points))
	args = append(args, p[:]...)
	_, err := e.c.Call(ctx, "aru", args)
	return err
}

// ---------- Redis (go-redis) ----------

type redisEngine struct {
	rdb    *redis.Client
	label  string
	sync   bool // sync replication: WAIT 1 after each write
	waitMs int  // WAIT timeout (ms)
}

// newRedisNamed drives any Redis-wire-protocol server (Redis, Dragonfly, KeyDB)
// through the same go-redis client; only the reported name differs.
//
// sync=true benchmarks SYNCHRONOUS replication: after each write we issue
// `WAIT 1 <waitMs>`, which blocks until one replica has acked the write (or the
// timeout fires, counted as an error). This requires a replica to be attached —
// with none, every WAIT burns the full timeout. sync=false is native async
// replication (the primary acks immediately).
func newRedisNamed(addr string, conns int, label string, sync bool, waitMs int) (engine, error) {
	rdb := redis.NewClient(&redis.Options{Addr: addr, PoolSize: conns, MinIdleConns: conns})
	if err := rdb.Ping(context.Background()).Err(); err != nil {
		return nil, err
	}
	return &redisEngine{rdb: rdb, label: label, sync: sync, waitMs: waitMs}, nil
}

func (e *redisEngine) name() string { return e.label }
func (e *redisEngine) semantics() string {
	// The sweep's output labels the tier; this only reflects the ack barrier,
	// which is the one thing this flag actually controls. -sync=false is a single
	// instance (Tier 1) OR an async replica (Tier 3) — both ack immediately, so
	// "no ack barrier" is accurate for both without claiming a topology.
	if e.sync {
		return "SYNC replication (WAIT 1 replica ack), no per-op fsync"
	}
	return "no replica-ack barrier (async or single-node), no per-op fsync"
}
func (e *redisEngine) setup(keys [][]byte, val []byte) error {
	ctx := context.Background()
	pipe := e.rdb.Pipeline()
	for i, k := range keys {
		pipe.Set(ctx, string(k), val, 0)
		if i%1000 == 999 {
			if _, err := pipe.Exec(ctx); err != nil {
				return err
			}
		}
	}
	_, err := pipe.Exec(ctx)
	return err
}
func (e *redisEngine) get(ctx context.Context, key []byte) error {
	return e.rdb.Get(ctx, string(key)).Err()
}
func (e *redisEngine) put(ctx context.Context, key, val []byte) error {
	if err := e.rdb.Set(ctx, string(key), val, 0).Err(); err != nil {
		return err
	}
	if !e.sync {
		return nil
	}
	// WAIT blocks until `numreplicas` replicas have acknowledged all writes this
	// connection issued, or the timeout fires. A return < 1 means the replica did
	// not ack in time — surface it as an error so an under-replicated run cannot
	// masquerade as fast (the whole point of the sync guarantee).
	n, err := e.rdb.Wait(ctx, 1, time.Duration(e.waitMs)*time.Millisecond).Result()
	if err != nil {
		return err
	}
	if n < 1 {
		return fmt.Errorf("%s: WAIT timed out, 0 replicas acked within %dms", e.label, e.waitMs)
	}
	return nil
}
func (e *redisEngine) close() { _ = e.rdb.Close() }

// ---------- Memcached (bradfitz/gomemcache) ----------
//
// Memcached keys must be printable ASCII with no whitespace/control bytes, but
// the workload keys are raw 16-byte binary, so they are hex-encoded on the wire
// (same logical key set, 32-char encoding). kv mode only — memcached has no
// equivalent of the custom native-op modes, so it does not implement opEngine.
type memcachedEngine struct {
	mc *memcache.Client
}

func newMemcached(addr string, conns int) (engine, error) {
	mc := memcache.New(addr)
	mc.MaxIdleConns = conns
	// Probe connectivity (gomemcache dials lazily otherwise).
	if err := mc.Set(&memcache.Item{Key: "netkv_probe", Value: []byte("1")}); err != nil {
		return nil, err
	}
	return &memcachedEngine{mc: mc}, nil
}

func (e *memcachedEngine) name() string      { return "memcached" }
func (e *memcachedEngine) semantics() string { return "in-memory, no persistence, no replication" }
func (e *memcachedEngine) setup(keys [][]byte, val []byte) error {
	for _, k := range keys {
		if err := e.mc.Set(&memcache.Item{Key: hex.EncodeToString(k), Value: val}); err != nil {
			return err
		}
	}
	return nil
}
func (e *memcachedEngine) get(_ context.Context, key []byte) error {
	_, err := e.mc.Get(hex.EncodeToString(key))
	return err
}
func (e *memcachedEngine) put(_ context.Context, key, val []byte) error {
	return e.mc.Set(&memcache.Item{Key: hex.EncodeToString(key), Value: val})
}
func (e *memcachedEngine) close() {}

// ---------- Aerospike (aerospike-client-go) ----------

type aerospikeEngine struct {
	cl       *as.Client
	ns, set  string
	wp       *as.WritePolicy
	rp       *as.BasePolicy
	binName  string
	sync     bool     // COMMIT_ALL (sync) vs COMMIT_MASTER (async)
	keyCache sync.Map // string(key) -> *as.Key, avoids rehashing on the hot path
}

func newAerospike(host string, port, conns int, sync bool) (engine, error) {
	pol := as.NewClientPolicy()
	pol.ConnectionQueueSize = conns*2 + 32 // headroom over workers for the client's tend/in-flight conns
	pol.Timeout = 5 * time.Second
	cl, err := as.NewClientWithPolicy(pol, host, port)
	if err != nil {
		return nil, err
	}
	wp := as.NewWritePolicy(0, 0)
	// CommitLevel is Aerospike's native sync/async replication knob:
	//   COMMIT_ALL    — ack only after the replica has the write (synchronous)
	//   COMMIT_MASTER — ack after the master write, replicate async
	// This is the direct counterpart to redis WAIT / rostam Raft-majority, and
	// the reason Aerospike appears in BOTH the sync and async Tier-3 tables.
	if sync {
		wp.CommitLevel = as.COMMIT_ALL
	} else {
		wp.CommitLevel = as.COMMIT_MASTER
	}
	rp := as.NewPolicy()
	return &aerospikeEngine{cl: cl, ns: "test", set: "kv", wp: wp, rp: rp, binName: "v", sync: sync}, nil
}

func (e *aerospikeEngine) name() string { return "aerospike" }
func (e *aerospikeEngine) semantics() string {
	if e.sync {
		return "in-memory, RF=2, SYNC (COMMIT_ALL)"
	}
	return "in-memory, RF=2, ASYNC (COMMIT_MASTER)"
}
func (e *aerospikeEngine) keyFor(key []byte) (*as.Key, error) {
	if v, ok := e.keyCache.Load(string(key)); ok {
		return v.(*as.Key), nil
	}
	k, err := as.NewKey(e.ns, e.set, key)
	if err != nil {
		return nil, err
	}
	e.keyCache.Store(string(key), k)
	return k, nil
}
func (e *aerospikeEngine) setup(keys [][]byte, val []byte) error {
	for _, key := range keys {
		k, err := e.keyFor(key)
		if err != nil {
			return err
		}
		if err := e.cl.Put(e.wp, k, as.BinMap{e.binName: val}); err != nil {
			return err
		}
	}
	return nil
}
func (e *aerospikeEngine) get(_ context.Context, key []byte) error {
	k, err := e.keyFor(key)
	if err != nil {
		return err
	}
	_, err = e.cl.Get(e.rp, k, e.binName)
	return err
}
func (e *aerospikeEngine) put(_ context.Context, key, val []byte) error {
	k, err := e.keyFor(key)
	if err != nil {
		return err
	}
	return e.cl.Put(e.wp, k, as.BinMap{e.binName: val})
}
func (e *aerospikeEngine) close() { e.cl.Close() }

// setupOp seeds each record with the bins the given workload mode touches,
// zeroed, via the native Put path.
func (e *aerospikeEngine) setupOp(mode string, keys [][]byte) error {
	var bins as.BinMap
	switch mode {
	case "atomic":
		// zero so the max expression always sees an existing "best" bin.
		bins = as.BinMap{"g": 0, "best": 0, "score": 0}
	case "incr", "cas":
		bins = as.BinMap{"c": 0}
	case "append":
		bins = as.BinMap{"v": ""}
	case "bitmask":
		bins = as.BinMap{"m": make([]byte, 8)} // 8-byte (64-bit) blob
	default:
		return fmt.Errorf("aerospike: unsupported mode %q", mode)
	}
	for _, key := range keys {
		k, err := e.keyFor(key)
		if err != nil {
			return err
		}
		if err := e.cl.Put(e.wp, k, bins); err != nil {
			return err
		}
	}
	return nil
}

// doOp issues the single measured native round-trip for the given workload mode.
func (e *aerospikeEngine) doOp(mode string, ctx context.Context, rng *rand.Rand, key []byte) error {
	if mode == "atomic" {
		return e.atomicUpdate(ctx, key, rng.Int63n(1000), 10)
	}
	k, err := e.keyFor(key)
	if err != nil {
		return err
	}
	switch mode {
	case "incr":
		_, err = e.cl.Operate(e.wp, k, as.AddOp(as.NewBin("c", 1)))
		return err
	case "cas":
		return e.casIncr(k)
	case "append":
		_, err = e.cl.Operate(e.wp, k, as.AppendOp(as.NewBin("v", "rostam!!")))
		return err
	case "bitmask":
		// native left-shift of the 64-bit blob bin "m" by 1 bit.
		_, err = e.cl.Operate(e.wp, k, as.BitLShiftOp(as.DefaultBitPolicy(), "m", 0, 64, 1))
		return err
	default:
		return fmt.Errorf("aerospike: unsupported mode %q", mode)
	}
}

// casIncr is Aerospike's native generation-based optimistic-concurrency
// increment, symmetric with Rostam's casw loop: read the counter + its
// generation, attempt a Put guarded by EXPECT_GEN_EQUAL, and on a generation
// conflict (another writer won) re-read and retry, capped at 1000 attempts.
func (e *aerospikeEngine) casIncr(k *as.Key) error {
	rec, err := e.cl.Get(e.rp, k, "c")
	if err != nil {
		return err
	}
	cur := asInt(rec.Bins["c"])
	gen := rec.Generation
	for i := 0; i < 1000; i++ {
		wpGen := *e.wp // copy so we don't mutate the shared write policy
		wpGen.GenerationPolicy = as.EXPECT_GEN_EQUAL
		wpGen.Generation = gen
		err := e.cl.Put(&wpGen, k, as.BinMap{"c": cur + 1})
		if err == nil {
			return nil
		}
		// Detect the generation conflict cleanly via the v8 Error API
		// (err.Matches reports the ResultCode); any other error is fatal.
		if !err.Matches(ast.GENERATION_ERROR) {
			return err
		}
		rec, err = e.cl.Get(e.rp, k, "c")
		if err != nil {
			return err
		}
		cur = asInt(rec.Bins["c"])
		gen = rec.Generation
	}
	return fmt.Errorf("cas: too many retries")
}

// asInt coerces an Aerospike integer bin value (returned as int, sometimes
// int64) to int64.
func asInt(v any) int64 {
	switch n := v.(type) {
	case int:
		return int64(n)
	case int64:
		return n
	default:
		return 0
	}
}

// atomicUpdate runs Aerospike's NATIVE single-record atomic multi-op: two
// integer increments (Add) plus a max via an operation expression
// (best = max(best, streak)). One round trip, atomic, no CAS, no Lua UDF —
// Aerospike's own fastest path for this update, the fair match to Rostam's op.
func (e *aerospikeEngine) atomicUpdate(_ context.Context, key []byte, streak, points int64) error {
	k, err := e.keyFor(key)
	if err != nil {
		return err
	}
	_, err = e.cl.Operate(e.wp, k,
		as.AddOp(as.NewBin("g", 1)),
		as.AddOp(as.NewBin("score", points)),
		as.ExpWriteOp("best", as.ExpMax(as.ExpIntBin("best"), as.ExpIntVal(streak)), as.ExpWriteFlagDefault),
	)
	return err
}
