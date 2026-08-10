// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 RostamLabs

package cachebench

import (
	"testing"

	otter "github.com/maypok86/otter"
)

// consumeAll reads EVERY byte of b. //go:noinline so the compiler cannot peek
// inside and prove the copy is unnecessary — forcing the appended bytes to
// actually be materialized.
//
//go:noinline
func consumeAll(b []byte) byte {
	var x byte
	for _, c := range b {
		x ^= c
	}
	return x
}

// BenchmarkProveOtterCopyElided proves WHY Otter stays ~4.7 ns even though its
// benchmark loop contains an append (a copy). Three variants on the SAME Otter
// Get:
//
//	A no_copy_read_v0    : v,_:=Get(); read v[0]                 (no append at all)
//	B copy_read_buf0     : append(buf,v); read buf[0]           (the bench pattern)
//	C copy_consume_all   : append(buf,v); consumeAll(buf)       (every byte forced)
//
// Expected:  A ≈ B  (the append in B is dead-code-eliminated — proving the
// "copy" never runs), and C ≫ B (when the bytes are genuinely consumed, the
// 256-byte copy finally costs ~3 ns — the work Otter normally skips and Rostam
// always pays).
func BenchmarkProveOtterCopyElided(b *testing.B) {
	c, err := otter.MustBuilder[string, []byte](4 * numKeys).WithTTL(ttl).Build()
	if err != nil {
		b.Fatal(err)
	}
	defer c.Close()
	for i := range wl.keys {
		c.Set(wl.keysStr[i], wl.value)
	}

	b.Run("A_no_copy_read_v0", func(b *testing.B) {
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
	})

	b.Run("B_copy_read_buf0", func(b *testing.B) {
		b.ReportAllocs()
		b.RunParallel(func(pb *testing.PB) {
			buf := make([]byte, 0, valueSize)
			var local byte
			i := 0
			for pb.Next() {
				v, _ := c.Get(wl.keysStr[i&(numKeys-1)])
				buf = append(buf[:0], v...) // <-- a copy is WRITTEN here...
				if len(buf) > 0 {
					local ^= buf[0] // ...but only buf[0] is read, so it's elided
				}
				i++
			}
			sink ^= local
		})
	})

	b.Run("C_copy_consume_all", func(b *testing.B) {
		b.ReportAllocs()
		b.RunParallel(func(pb *testing.PB) {
			buf := make([]byte, 0, valueSize)
			var local byte
			i := 0
			for pb.Next() {
				v, _ := c.Get(wl.keysStr[i&(numKeys-1)])
				buf = append(buf[:0], v...)
				local ^= consumeAll(buf) // every byte read -> the copy MUST happen
				i++
			}
			sink ^= local
		})
	})
}
