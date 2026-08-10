// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 RostamLabs

// Package cachebench runs an apples-to-apples in-memory cache comparison
// between the Rostam engine's public cache API and a set of popular Go
// caching libraries (freecache, Ristretto, BigCache, fastcache, Otter).
//
// Every engine is exercised on the SAME workload:
//   - numKeys (65536) random 16-byte keys
//   - 256-byte values
//   - a RunParallel "GetHit" benchmark against a pre-populated cache
//   - a RunParallel "Put" benchmark
//
// FAIRNESS — the GetHit benchmark measures ONE identical operation for every
// engine: "look up the key and end with the value in the CALLER'S reused buffer
// (a private, safely-mutable copy)." The zero-copy caches (Otter, Ristretto)
// therefore copy their returned reference into a buffer too — otherwise they'd be
// timing a different, UNSAFE operation: the slice they return aliases the stored
// value, so mutating it corrupts the cache for every other reader (proven in
// aliasing_test.go). Engines with a copy-into-buffer API (Rostam GetInto,
// fastcache Get(dst), freecache GetWithBuf) stay zero-alloc; BigCache has no such
// API, so its Get allocates an owned copy — that allocation is its honest cost.
//
// Caches are sized generously so the Get-hit benchmarks never evict. Where a
// library's API forces a deviation from the byte-key/byte-value baseline
// (string keys, async writes, cost-based capacity, no TTL), a // comment marks it.
package cachebench

import (
	"context"
	"math/rand"
	"testing"
	"time"

	"github.com/VictoriaMetrics/fastcache"
	bigcache "github.com/allegro/bigcache/v3"
	freecache "github.com/coocood/freecache"
	ristretto "github.com/dgraph-io/ristretto/v2"
	otter "github.com/maypok86/otter"

	"github.com/rostamlabs/rostam/cache"
)

const (
	numKeys   = 65536            // identical key-space across every engine
	keySize   = 16               // bytes per key
	valueSize = 256              // bytes per value
	capBytes  = 512 << 20        // 512 MiB byte budget for byte-sized caches
	ttl       = 10 * time.Minute // long enough that nothing expires mid-bench
)

// rostamConfig gives Rostam the SAME capBytes budget every other byte-sized
// cache in this suite gets (freecache, fastcache and bigcache are each handed
// capBytes). cache.DefaultConfig() is 256 shards x 256 MiB = 64 GiB — 128x this
// suite's budget.
//
// That gap is invisible on GetHit (the ~16 MiB working set never evicts at
// either size) but it invalidates Put. At 64 GiB a bench run cannot fill the
// cache, so it never reaches the ring-buffer steady state the byte-sized engines
// are measured in: it spends the whole run lazily allocating 16 MiB pages
// (Evictions=0, ~12.8 GB allocated per 3s run) and so reports page-fill cost
// rather than a steady-state write. Sized to capBytes it fills, evicts and
// recycles — the like-for-like comparison.
//
// Geometry: NumShards stays at the shipped default so ONLY the budget changes.
// cache.Config.Validate floors PageSize at 1 MiB, so capBytes/256 = 2 MiB per
// shard gives 2 pages/shard — the finest ring granularity reachable at this
// budget, against the default's 16. Rostam therefore retires half a shard per
// eviction here. That is a real consequence of the 1 MiB page floor at a 512 MiB
// budget, not a tuned knob, and it mirrors freecache's own 256 segments x 2 MiB.
func rostamConfig() cache.Config {
	c := cache.DefaultConfig()
	c.PageSize = 1 << 20                         // 1 MiB: cache.Config.Validate floor
	c.MaxMemoryPerShard = capBytes / c.NumShards // 512 MiB / 256 shards = 2 MiB
	return c
}

// workload holds the shared byte keys/values so every engine benchmarks the
// exact same data. Generated once via a fixed seed for reproducibility.
type workload struct {
	keys    [][]byte
	keysStr []string // string view of the same keys (for string-keyed APIs)
	value   []byte
}

func newWorkload() *workload {
	r := rand.New(rand.NewSource(0x0570541)) //nolint:gosec // deterministic, non-cryptographic test data
	w := &workload{
		keys:    make([][]byte, numKeys),
		keysStr: make([]string, numKeys),
		value:   make([]byte, valueSize),
	}
	for i := range w.keys {
		k := make([]byte, keySize)
		r.Read(k)
		w.keys[i] = k
		w.keysStr[i] = string(k)
	}
	r.Read(w.value)
	return w
}

// pkg-level workload so benchmarks share one allocation.
var wl = newWorkload()

// sink is read+written from every GetHit loop so the compiler cannot
// dead-code-eliminate the value copy (a real hazard for the ref-returning caches,
// whose external append into a never-read buffer would otherwise be optimized
// away — making them look artificially fast). Touching one byte forces the value
// to be materialized.
var sink byte

// ---------------------------------------------------------------------------
// Rostam (engine under test) — public cache API, byte keys + byte values.
// ---------------------------------------------------------------------------

func BenchmarkRostam_GetHit(b *testing.B) {
	c, err := cache.New(rostamConfig())
	if err != nil {
		b.Fatal(err)
	}
	defer c.Close()
	for i := range wl.keys {
		if err := c.Put(wl.keys[i], wl.value, 0); err != nil { // ttl 0 == no expiry
			b.Fatal(err)
		}
	}
	b.ResetTimer()
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		// GetInto appends into a reused buffer — the allocation-free Get path,
		// the fair analogue of fastcache's Get(dst, k). It still COPIES the value
		// out of the cache (torn-read-safe), unlike the zero-copy reference returns
		// of Otter/Ristretto (which hand back the stored []byte and do no copy).
		buf := make([]byte, 0, len(wl.value))
		var local byte
		i := 0
		for pb.Next() {
			buf, _ = c.GetInto(buf[:0], wl.keys[i&(numKeys-1)])
			local ^= consumeAll(buf) // read every byte so the copy cannot be elided
			i++
		}
		sink ^= local // combine once per goroutine — defeats DCE without false sharing
	})
}

func BenchmarkRostam_Put(b *testing.B) {
	c, err := cache.New(rostamConfig())
	if err != nil {
		b.Fatal(err)
	}
	defer c.Close()
	b.ResetTimer()
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			_ = c.Put(wl.keys[i&(numKeys-1)], wl.value, 0)
			i++
		}
	})
}

// ---------------------------------------------------------------------------
// freecache — byte keys + byte values; TTL in seconds (0 == no expiry).
// ---------------------------------------------------------------------------

func BenchmarkFreecache_GetHit(b *testing.B) {
	c := freecache.NewCache(capBytes)
	for i := range wl.keys {
		if err := c.Set(wl.keys[i], wl.value, 0); err != nil {
			b.Fatal(err)
		}
	}
	b.ResetTimer()
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		buf := make([]byte, 0, valueSize) // reused; GetWithBuf copies into it (0 alloc, owned)
		var local byte
		i := 0
		for pb.Next() {
			buf, _ = c.GetWithBuf(wl.keys[i&(numKeys-1)], buf[:0])
			local ^= consumeAll(buf)
			i++
		}
		sink ^= local
	})
}

func BenchmarkFreecache_Put(b *testing.B) {
	c := freecache.NewCache(capBytes)
	b.ResetTimer()
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			_ = c.Set(wl.keys[i&(numKeys-1)], wl.value, 0)
			i++
		}
	})
}

// ---------------------------------------------------------------------------
// Ristretto v2 — generic byte keys + byte values. NOTE: Set is ASYNC and
// cost-based, so we pass cost == len(value) and call Wait() after populating
// before the Get-hit benchmark so every key is admitted.
// ---------------------------------------------------------------------------

func newRistretto(b *testing.B) *ristretto.Cache[[]byte, []byte] {
	c, err := ristretto.NewCache(&ristretto.Config[[]byte, []byte]{
		NumCounters: 10 * numKeys, // ~10x items, per Ristretto guidance
		MaxCost:     capBytes,     // cost == value bytes, so this is a byte budget
		BufferItems: 64,
	})
	if err != nil {
		b.Fatal(err)
	}
	return c
}

func BenchmarkRistretto_GetHit(b *testing.B) {
	c := newRistretto(b)
	defer c.Close()
	for i := range wl.keys {
		c.Set(wl.keys[i], wl.value, int64(valueSize)) // async, cost-based
	}
	c.Wait() // drain the async admission buffer before reading
	b.ResetTimer()
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		buf := make([]byte, 0, valueSize) // copy the zero-copy ref into an owned buffer
		var local byte
		i := 0
		for pb.Next() {
			v, _ := c.Get(wl.keys[i&(numKeys-1)])
			buf = append(buf[:0], v...)
			local ^= consumeAll(buf)
			i++
		}
		sink ^= local
	})
}

func BenchmarkRistretto_Put(b *testing.B) {
	c := newRistretto(b)
	defer c.Close()
	b.ResetTimer()
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			_ = c.Set(wl.keys[i&(numKeys-1)], wl.value, int64(valueSize)) // async write
			i++
		}
	})
}

// ---------------------------------------------------------------------------
// BigCache v3 — STRING keys (byte keys converted up front into wl.keysStr).
// ---------------------------------------------------------------------------

func newBigcache(b *testing.B) *bigcache.BigCache {
	cfg := bigcache.DefaultConfig(ttl)
	cfg.HardMaxCacheSize = 512 // MiB cap, generous for the workload
	c, err := bigcache.New(context.Background(), cfg)
	if err != nil {
		b.Fatal(err)
	}
	return c
}

func BenchmarkBigCache_GetHit(b *testing.B) {
	c := newBigcache(b)
	defer c.Close()
	for i := range wl.keys {
		if err := c.Set(wl.keysStr[i], wl.value); err != nil { // string keys
			b.Fatal(err)
		}
	}
	b.ResetTimer()
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		var local byte
		i := 0
		for pb.Next() {
			v, _ := c.Get(wl.keysStr[i&(numKeys-1)]) // BigCache.Get allocates an owned copy
			local ^= consumeAll(v)
			i++
		}
		sink ^= local
	})
}

func BenchmarkBigCache_Put(b *testing.B) {
	c := newBigcache(b)
	defer c.Close()
	b.ResetTimer()
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			_ = c.Set(wl.keysStr[i&(numKeys-1)], wl.value) // string keys
			i++
		}
	})
}

// ---------------------------------------------------------------------------
// fastcache — byte keys + byte values. NOTE: no TTL support; Get appends into
// a caller-supplied dst buffer.
// ---------------------------------------------------------------------------

func BenchmarkFastcache_GetHit(b *testing.B) {
	c := fastcache.New(capBytes)
	for i := range wl.keys {
		c.Set(wl.keys[i], wl.value) // no TTL parameter exists
	}
	b.ResetTimer()
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		dst := make([]byte, 0, valueSize) // reused; Get copies into it (0 alloc, owned)
		var local byte
		i := 0
		for pb.Next() {
			dst = c.Get(dst[:0], wl.keys[i&(numKeys-1)])
			local ^= consumeAll(dst)
			i++
		}
		sink ^= local
	})
}

func BenchmarkFastcache_Put(b *testing.B) {
	c := fastcache.New(capBytes)
	b.ResetTimer()
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			c.Set(wl.keys[i&(numKeys-1)], wl.value)
			i++
		}
	})
}

// ---------------------------------------------------------------------------
// Otter v1.2.4 — builder API, STRING keys + byte values. NOTE: capacity is an
// ENTRY count (not bytes) unless a Cost func is set; we size it well above
// numKeys so nothing evicts.
// ---------------------------------------------------------------------------

func newOtter(b *testing.B) otter.Cache[string, []byte] {
	c, err := otter.MustBuilder[string, []byte](4 * numKeys). // entry-count capacity
									WithTTL(ttl).
									Build()
	if err != nil {
		b.Fatal(err)
	}
	return c
}

func BenchmarkOtter_GetHit(b *testing.B) {
	c := newOtter(b)
	defer c.Close()
	for i := range wl.keys {
		c.Set(wl.keysStr[i], wl.value) // string keys
	}
	b.ResetTimer()
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		buf := make([]byte, 0, valueSize) // copy the zero-copy ref into an owned buffer
		var local byte
		i := 0
		for pb.Next() {
			v, _ := c.Get(wl.keysStr[i&(numKeys-1)])
			buf = append(buf[:0], v...)
			local ^= consumeAll(buf)
			i++
		}
		sink ^= local
	})
}

func BenchmarkOtter_Put(b *testing.B) {
	c := newOtter(b)
	defer c.Close()
	b.ResetTimer()
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			_ = c.Set(wl.keysStr[i&(numKeys-1)], wl.value) // string keys
			i++
		}
	})
}
