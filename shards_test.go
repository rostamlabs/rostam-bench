// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 RostamLabs

package cachebench

import (
	"strconv"
	"testing"

	"github.com/rostamlabs/rostam/cache"
)

// BenchmarkRostamShards measures whether raising Rostam's shard count closes the
// gap to fastcache (512 buckets) and Otter (lock-free). Same FAIR workload as the
// headline GetHit (owned copy, fully consumed, no elision). If the curve flattens
// quickly, RLock contention isn't the bottleneck — the per-entry work is — and
// bumping the default buys little. If it keeps dropping toward fastcache (~18 ns
// fair) / Otter (~12 ns fair), more shards is a cheap real win.
func BenchmarkRostamShards(b *testing.B) {
	for _, n := range []int{256, 512, 1024, 2048, 4096} {
		cfg := cache.DefaultConfig()
		cfg.NumShards = n
		c, err := cache.New(cfg)
		if err != nil {
			b.Fatal(err)
		}
		for i := range wl.keys {
			if err := c.Put(wl.keys[i], wl.value, 0); err != nil {
				b.Fatal(err)
			}
		}
		b.Run("shards="+strconv.Itoa(n), func(b *testing.B) {
			b.ReportAllocs()
			b.RunParallel(func(pb *testing.PB) {
				buf := make([]byte, 0, valueSize)
				var local byte
				i := 0
				for pb.Next() {
					buf, _ = c.GetInto(buf[:0], wl.keys[i&(numKeys-1)])
					local ^= consumeAll(buf)
					i++
				}
				sink ^= local
			})
		})
		_ = c.Close()
	}
}
