// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 RostamLabs

package cachebench

import (
	"strconv"
	"testing"

	"github.com/rostamlabs/rostam/cache"
)

// shardCounts is the NumShards sweep. Powers of two (Config requires it).
// Extended down to 1 to expose RWMutex reader-lock contention: with few shards,
// all 20 goroutines pile onto the same per-shard readerCount word.
var shardCounts = []int{1, 2, 4, 8, 16, 64, 256}

// sweepConfig returns a cache config identical to DefaultConfig() EXCEPT:
//   - NumShards varies (the independent variable)
//   - PageSize is pinned to the 1 MiB minimum and the sweeper is off, so total
//     memory stays ~= NumShards MiB (avoids the multi-GiB footprint of the 16 MiB
//     default pages) and background goroutines don't add noise. With no TTL on the
//     workload, MaxMemoryPerShard=64 MiB gives ample headroom so nothing evicts.
func sweepConfig(numShards int) cache.Config {
	c := cache.DefaultConfig()
	c.NumShards = numShards
	c.PageSize = 1 << 20        // 1 MiB (minimum) — bounds memory across the sweep
	c.MaxMemoryPerShard = 64 << 20
	c.TTLSweepIntervalMs = 0 // no background sweeper
	return c
}

func BenchmarkRostamShards_GetHit(b *testing.B) {
	for _, n := range shardCounts {
		b.Run(shardLabel(n), func(b *testing.B) {
			c, err := cache.New(sweepConfig(n))
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
					buf, _ = c.GetInto(buf[:0], wl.keys[i&(numKeys-1)])
					local ^= consumeAll(buf)
					i++
				}
				sink ^= local
			})
		})
	}
}

func BenchmarkRostamShards_Put(b *testing.B) {
	for _, n := range shardCounts {
		b.Run(shardLabel(n), func(b *testing.B) {
			c, err := cache.New(sweepConfig(n))
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
		})
	}
}

func shardLabel(n int) string {
	return "shards=" + strconv.Itoa(n)
}
