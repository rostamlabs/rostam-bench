// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 RostamLabs

package cachebench

import (
	"testing"

	"github.com/VictoriaMetrics/fastcache"
	otter "github.com/maypok86/otter"

	"github.com/rostamlabs/rostam/cache"
)

func BenchmarkProfFastcache(b *testing.B) {
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
			dst = c.Get(dst[:0], wl.keys[i&(numKeys-1)])
			if len(dst) > 0 {
				local ^= dst[0]
			}
			i++
		}
		sink ^= local
	})
}

// BenchmarkProfRostam / BenchmarkProfOtter exist for CPU profiling the Get path
// with MINIMAL observation overhead (just read one byte) so a -cpuprofile shows
// the engine's own internals, not the consumeAll XOR loop. Profile with:
//
//	go test -run='^$' -bench=BenchmarkProfRostam -benchtime=5s \
//	   -cpuprofile=profiles/rostam_cpu.prof -o profiles/cachebench.test ./
//	go tool pprof -http=:8080 profiles/cachebench.test profiles/rostam_cpu.prof

func BenchmarkProfRostam(b *testing.B) {
	c, err := cache.New(cache.DefaultConfig())
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
		buf := make([]byte, 0, valueSize)
		var local byte
		i := 0
		for pb.Next() {
			buf, _ = c.GetInto(buf[:0], wl.keys[i&(numKeys-1)])
			if len(buf) > 0 {
				local ^= buf[0]
			}
			i++
		}
		sink ^= local
	})
}

func BenchmarkProfOtter(b *testing.B) {
	c, err := otter.MustBuilder[string, []byte](4 * numKeys).WithTTL(ttl).Build()
	if err != nil {
		b.Fatal(err)
	}
	defer c.Close()
	for i := range wl.keys {
		c.Set(wl.keysStr[i], wl.value)
	}
	b.ResetTimer()
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		var local byte
		i := 0
		for pb.Next() {
			v, _ := c.Get(wl.keysStr[i&(numKeys-1)])
			if len(v) > 0 {
				local ^= v[0]
			}
			i++
		}
		sink ^= local
	})
}
