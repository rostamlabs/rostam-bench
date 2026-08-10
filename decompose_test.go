// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 RostamLabs

package cachebench

import (
	"math/rand"
	"strconv"
	"testing"

	otter "github.com/maypok86/otter"

	"github.com/rostamlabs/rostam/cache"
)

// BenchmarkDecompose isolates WHY Otter's Get beats Rostam's GetInto, by sweeping
// the value size on the SAME keys/harness.
//
//   - Rostam GetInto COPIES the value out of the slab store, so its cost should
//     scale with value size.
//   - Otter returns the STORED reference (no copy), so its cost should be ~flat
//     across value sizes.
//
// Read: if the gap grows with size, the 256-byte copy is the cause and "Otter
// faster" is just "Otter does no copy." The residual at the SMALLEST size (where
// the copy is negligible) is Rostam's read-path overhead — RWMutex RLock + index
// lookup + entry-header decode + key collision-check — vs Otter's lean
// concurrent-map read.
func BenchmarkDecompose(b *testing.B) {
	sizes := []int{8, 32, 128, 256}
	keys := wl.keys
	keysStr := wl.keysStr
	rng := rand.New(rand.NewSource(1)) //nolint:gosec // deterministic test data

	for _, sz := range sizes {
		val := make([]byte, sz)
		rng.Read(val)

		b.Run("rostam_GetInto/val="+strconv.Itoa(sz), func(b *testing.B) {
			c, err := cache.New(cache.DefaultConfig())
			if err != nil {
				b.Fatal(err)
			}
			defer c.Close()
			for i := range keys {
				if err := c.Put(keys[i], val, 0); err != nil {
					b.Fatal(err)
				}
			}
			b.ResetTimer()
			b.ReportAllocs()
			b.RunParallel(func(pb *testing.PB) {
				buf := make([]byte, 0, sz)
				i := 0
				for pb.Next() {
					buf, _ = c.GetInto(buf[:0], keys[i&(numKeys-1)])
					i++
				}
			})
		})

		b.Run("otter_Get/val="+strconv.Itoa(sz), func(b *testing.B) {
			c, err := otter.MustBuilder[string, []byte](4 * numKeys).WithTTL(ttl).Build()
			if err != nil {
				b.Fatal(err)
			}
			defer c.Close()
			for i := range keys {
				c.Set(keysStr[i], val)
			}
			b.ResetTimer()
			b.ReportAllocs()
			b.RunParallel(func(pb *testing.PB) {
				i := 0
				for pb.Next() {
					_, _ = c.Get(keysStr[i&(numKeys-1)])
					i++
				}
			})
		})
	}
}
