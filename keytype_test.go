// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 RostamLabs

package cachebench

import (
	"testing"

	otter "github.com/maypok86/otter"

	"github.com/rostamlabs/rostam/cache"
)

// BenchmarkKeyType shows that "key type" is a real, non-neutral fairness axis.
// The main comparison feeds every cache its NATIVE key type with no conversion.
// But your data is one type: if it's []byte, calling a string-keyed cache costs a
// string([]byte) allocation per Get; if it's string, calling a byte-keyed cache
// costs a []byte(string) allocation. Whichever type you pick penalizes the other
// camp — so there is no single "fair" key type, only a stated assumption.
//
// Expect: the "native" rows are 0 allocs; the "converted" rows add 1 alloc and
// slow down — the hidden cost the main benchmark omits by pre-converting keys.
func BenchmarkKeyType(b *testing.B) {
	oc, err := otter.MustBuilder[string, []byte](4 * numKeys).WithTTL(ttl).Build()
	if err != nil {
		b.Fatal(err)
	}
	defer oc.Close()
	for i := range wl.keys {
		oc.Set(wl.keysStr[i], wl.value)
	}

	rc, err := cache.New(cache.DefaultConfig())
	if err != nil {
		b.Fatal(err)
	}
	defer rc.Close()
	for i := range wl.keys {
		_ = rc.Put(wl.keys[i], wl.value, 0)
	}

	// Otter (string-keyed): native string vs converting from []byte data.
	b.Run("otter_native_string", func(b *testing.B) {
		b.ReportAllocs()
		b.RunParallel(func(pb *testing.PB) {
			var local byte
			i := 0
			for pb.Next() {
				v, _ := oc.Get(wl.keysStr[i&(numKeys-1)])
				if len(v) > 0 {
					local ^= v[0]
				}
				i++
			}
			sink ^= local
		})
	})
	b.Run("otter_from_byte_data", func(b *testing.B) {
		b.ReportAllocs()
		b.RunParallel(func(pb *testing.PB) {
			var local byte
			i := 0
			for pb.Next() {
				v, _ := oc.Get(string(wl.keys[i&(numKeys-1)])) // []byte data -> string conversion
				if len(v) > 0 {
					local ^= v[0]
				}
				i++
			}
			sink ^= local
		})
	})

	// Rostam (byte-keyed): native []byte vs converting from string data.
	b.Run("rostam_native_bytes", func(b *testing.B) {
		b.ReportAllocs()
		b.RunParallel(func(pb *testing.PB) {
			buf := make([]byte, 0, valueSize)
			var local byte
			i := 0
			for pb.Next() {
				buf, _ = rc.GetInto(buf[:0], wl.keys[i&(numKeys-1)])
				if len(buf) > 0 {
					local ^= buf[0]
				}
				i++
			}
			sink ^= local
		})
	})
	b.Run("rostam_from_string_data", func(b *testing.B) {
		b.ReportAllocs()
		b.RunParallel(func(pb *testing.PB) {
			buf := make([]byte, 0, valueSize)
			var local byte
			i := 0
			for pb.Next() {
				buf, _ = rc.GetInto(buf[:0], []byte(wl.keysStr[i&(numKeys-1)])) // string data -> []byte conversion
				if len(buf) > 0 {
					local ^= buf[0]
				}
				i++
			}
			sink ^= local
		})
	})
}
