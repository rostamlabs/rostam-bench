// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 RostamLabs

// Hot-key variant of the cache comparison. Identical engines and data as
// cache_bench_test.go, but the GetHit access pattern is HEAVILY SKEWED
// (Zipfian) instead of uniform-random. A uniform workload spreads readers
// evenly across shards/buckets and hides lock contention; a hot-key workload
// funnels most reads at a few keys — and thus a few shards — which is where a
// per-shard reader lock (Rostam today) contends and a lock-free read path
// (Otter, fastcache) does not. This benchmark makes that difference visible.
package cachebench

import (
	"math/rand"
	"testing"

	"github.com/VictoriaMetrics/fastcache"
	freecache "github.com/coocood/freecache"

	"github.com/rostamlabs/rostam/cache"
)

// hotSeqLen is a power of two so the access loop can mask instead of modulo.
const hotSeqLen = 1 << 18

// hotSeq is a shared, deterministic Zipfian sequence of key indices. Every
// engine walks the SAME sequence so the skew is identical across the board.
// s=1.1 gives a strong head: a handful of keys receive the large majority of
// accesses, the classic hot-key cache pattern.
var hotSeq = newHotSeq()

func newHotSeq() []uint32 {
	r := rand.New(rand.NewSource(0xC0FFEE)) //nolint:gosec // deterministic, non-cryptographic test data
	z := rand.NewZipf(r, 1.1, 1, numKeys-1)
	seq := make([]uint32, hotSeqLen)
	for i := range seq {
		seq[i] = uint32(z.Uint64())
	}
	return seq
}

// ---------------------------------------------------------------------------
// Rostam
// ---------------------------------------------------------------------------

// hotRostam runs the shared hot-key GetInto loop against a cache built from cfg.
func hotRostam(b *testing.B, cfg cache.Config) {
	c, err := cache.New(cfg)
	if err != nil {
		b.Fatal(err)
	}
	defer c.Close()
	for i := range wl.keys {
		if err := c.Put(wl.keys[i], wl.value, 0); err != nil {
			b.Fatal(err)
		}
	}
	b.ResetTimer()
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		buf := make([]byte, 0, len(wl.value))
		var local byte
		i := 0
		for pb.Next() {
			buf, _ = c.GetInto(buf[:0], wl.keys[hotSeq[i&(hotSeqLen-1)]])
			local ^= consumeAll(buf)
			i++
		}
		sink ^= local
	})
}

// BenchmarkHotRostam_GetHit uses the default PolicyRingbufEvict. In HEAP mode
// (no DataDir, which is what this suite runs) that read path is LOCK-FREE:
// shard.needsReadLockForGet gates the RLock on `AtCapPolicy == RingbufEvict &&
// region != nil`, and region is non-nil only under mmap. Heap eviction retires a
// page by swapping in a fresh frozen object rather than overwriting bytes a
// reader may hold, so no lock is needed (rostam 6caba11).
//
// Only mmap+RingbufEvict still takes the RLock: its pages wrap a fixed file
// region and cannot be swapped for a fresh allocation, so eviction drains them
// in place — the one case that would tear a lock-free reader.
func BenchmarkHotRostam_GetHit(b *testing.B) {
	hotRostam(b, rostamConfig())
}

// BenchmarkHotRostamReject_GetHit uses PolicyRejectWrites — the fully lock-free,
// zero-copy read path (no in-place overwrite, so no lock needed). Sized so the
// workload never rejects: rostamConfig() gives 2 MiB per shard against a
// ~16 MiB working set spread over 256 shards (~64 KiB each), so a shard fills
// well under one page. TestPopulationHitRate guards this — a reject would drop
// entries and the benchmark would time misses. This is the config that competes
// with Otter/fastcache on read contention.
func BenchmarkHotRostamReject_GetHit(b *testing.B) {
	cfg := rostamConfig()
	cfg.AtCapPolicy = cache.PolicyRejectWrites
	hotRostam(b, cfg)
}

// ---------------------------------------------------------------------------
// freecache — byte keys, GetWithBuf copies into a reused buffer (0 alloc).
// ---------------------------------------------------------------------------

func BenchmarkHotFreecache_GetHit(b *testing.B) {
	c := freecache.NewCache(capBytes)
	for i := range wl.keys {
		if err := c.Set(wl.keys[i], wl.value, 0); err != nil {
			b.Fatal(err)
		}
	}
	b.ResetTimer()
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		buf := make([]byte, 0, valueSize)
		var local byte
		i := 0
		for pb.Next() {
			buf, _ = c.GetWithBuf(wl.keys[hotSeq[i&(hotSeqLen-1)]], buf[:0])
			local ^= consumeAll(buf)
			i++
		}
		sink ^= local
	})
}

// ---------------------------------------------------------------------------
// Ristretto — zero-copy ref return, copied into an owned buffer for fairness.
// ---------------------------------------------------------------------------

func BenchmarkHotRistretto_GetHit(b *testing.B) {
	c := newRistretto(b)
	defer c.Close()
	for i := range wl.keys {
		c.Set(wl.keys[i], wl.value, int64(valueSize))
	}
	c.Wait()
	b.ResetTimer()
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		buf := make([]byte, 0, valueSize)
		var local byte
		i := 0
		for pb.Next() {
			v, _ := c.Get(wl.keys[hotSeq[i&(hotSeqLen-1)]])
			buf = append(buf[:0], v...)
			local ^= consumeAll(buf)
			i++
		}
		sink ^= local
	})
}

// ---------------------------------------------------------------------------
// BigCache — STRING keys; Get allocates an owned copy (its honest cost).
// ---------------------------------------------------------------------------

func BenchmarkHotBigCache_GetHit(b *testing.B) {
	c := newBigcache(b)
	defer c.Close()
	for i := range wl.keys {
		if err := c.Set(wl.keysStr[i], wl.value); err != nil {
			b.Fatal(err)
		}
	}
	b.ResetTimer()
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		var local byte
		i := 0
		for pb.Next() {
			v, _ := c.Get(wl.keysStr[hotSeq[i&(hotSeqLen-1)]])
			local ^= consumeAll(v)
			i++
		}
		sink ^= local
	})
}

// ---------------------------------------------------------------------------
// fastcache — byte keys; Get copies into a caller-supplied dst (0 alloc).
// ---------------------------------------------------------------------------

func BenchmarkHotFastcache_GetHit(b *testing.B) {
	c := fastcache.New(capBytes)
	for i := range wl.keys {
		c.Set(wl.keys[i], wl.value)
	}
	b.ResetTimer()
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		dst := make([]byte, 0, valueSize)
		var local byte
		i := 0
		for pb.Next() {
			dst = c.Get(dst[:0], wl.keys[hotSeq[i&(hotSeqLen-1)]])
			local ^= consumeAll(dst)
			i++
		}
		sink ^= local
	})
}

// ---------------------------------------------------------------------------
// Otter — STRING keys; zero-copy ref return, copied into an owned buffer.
// ---------------------------------------------------------------------------

func BenchmarkHotOtter_GetHit(b *testing.B) {
	c := newOtter(b)
	defer c.Close()
	for i := range wl.keys {
		c.Set(wl.keysStr[i], wl.value)
	}
	b.ResetTimer()
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		buf := make([]byte, 0, valueSize)
		var local byte
		i := 0
		for pb.Next() {
			v, _ := c.Get(wl.keysStr[hotSeq[i&(hotSeqLen-1)]])
			buf = append(buf[:0], v...)
			local ^= consumeAll(buf)
			i++
		}
		sink ^= local
	})
}
