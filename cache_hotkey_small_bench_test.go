// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 RostamLabs

// Small-value (8-byte) variant of the hot-key comparison. Same engines, same
// Zipfian hot-key access as cache_hotkey_bench_test.go, but a tiny value so the
// per-Get value materialization (the copy + consumeAll read) is negligible and
// the READ MECHANICS dominate — lock strategy, lookup, hashing. This isolates
// exactly the differences the 256-byte payload floor washes out: lock-free vs
// shared-RLock vs exclusive-lock read paths.
package cachebench

import (
	"testing"

	"github.com/VictoriaMetrics/fastcache"
	freecache "github.com/coocood/freecache"

	"github.com/rostamlabs/rostam/cache"
)

// smallVal is an 8-byte value: materialization cost ~= a single word copy, so
// the benchmark measures lookup+lock, not memmove.
var smallVal = make([]byte, 8)

func BenchmarkHotSmallRostam_GetHit(b *testing.B)       { hotSmallRostam(b, rostamConfig()) }
func BenchmarkHotSmallRostamReject_GetHit(b *testing.B) { hotSmallRostam(b, rejectCfg()) }

func rejectCfg() cache.Config {
	c := rostamConfig()
	c.AtCapPolicy = cache.PolicyRejectWrites
	return c
}

func hotSmallRostam(b *testing.B, cfg cache.Config) {
	c, err := cache.New(cfg)
	if err != nil {
		b.Fatal(err)
	}
	defer c.Close()
	for i := range wl.keys {
		if err := c.Put(wl.keys[i], smallVal, 0); err != nil {
			b.Fatal(err)
		}
	}
	b.ResetTimer()
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		buf := make([]byte, 0, len(smallVal))
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

func BenchmarkHotSmallFreecache_GetHit(b *testing.B) {
	c := freecache.NewCache(capBytes)
	for i := range wl.keys {
		if err := c.Set(wl.keys[i], smallVal, 0); err != nil {
			b.Fatal(err)
		}
	}
	b.ResetTimer()
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		buf := make([]byte, 0, len(smallVal))
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

func BenchmarkHotSmallRistretto_GetHit(b *testing.B) {
	c := newRistretto(b)
	defer c.Close()
	for i := range wl.keys {
		c.Set(wl.keys[i], smallVal, int64(len(smallVal)))
	}
	c.Wait()
	b.ResetTimer()
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		buf := make([]byte, 0, len(smallVal))
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

func BenchmarkHotSmallBigCache_GetHit(b *testing.B) {
	c := newBigcache(b)
	defer c.Close()
	for i := range wl.keys {
		if err := c.Set(wl.keysStr[i], smallVal); err != nil {
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

func BenchmarkHotSmallFastcache_GetHit(b *testing.B) {
	c := fastcache.New(capBytes)
	for i := range wl.keys {
		c.Set(wl.keys[i], smallVal)
	}
	b.ResetTimer()
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		dst := make([]byte, 0, len(smallVal))
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

func BenchmarkHotSmallOtter_GetHit(b *testing.B) {
	c := newOtter(b)
	defer c.Close()
	for i := range wl.keys {
		c.Set(wl.keysStr[i], smallVal)
	}
	b.ResetTimer()
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		buf := make([]byte, 0, len(smallVal))
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
