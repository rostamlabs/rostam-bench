// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 RostamLabs

package cachebench

import (
	"bytes"
	"testing"

	otter "github.com/maypok86/otter"

	"github.com/rostamlabs/rostam/cache"
)

// TestReturnedValueAliasing demonstrates the SAFETY side of the copy-vs-zero-copy
// tradeoff that explains the benchmark gap. A zero-copy cache (Otter) returns the
// STORED []byte; a caller that mutates it corrupts the cached value seen by every
// other reader (and concurrently it is also a data race — run with -race).
// Rostam copies on Get/GetInto, so a caller mutating the result cannot affect the
// cache or any other reader.
func TestReturnedValueAliasing(t *testing.T) {
	const key = "k"
	orig := bytes.Repeat([]byte("A"), 16)

	// --- Otter: returns the stored reference (zero copy) ---
	oc, err := otter.MustBuilder[string, []byte](1024).Build()
	if err != nil {
		t.Fatal(err)
	}
	defer oc.Close()
	oc.Set(key, append([]byte(nil), orig...)) // store a private copy of orig

	v1, ok := oc.Get(key)
	if !ok {
		t.Fatal("otter: key missing")
	}
	for i := range v1 { // a caller mutates the slice Otter handed back
		v1[i] = 'Z'
	}
	v2, _ := oc.Get(key) // a different reader looks up the same key
	t.Logf("Otter : stored=%q ; after caller mutated the returned slice, re-Get=%q", orig, v2)
	if bytes.Equal(v2, orig) {
		t.Errorf("Otter returned an isolated copy (no aliasing) — unexpected")
	} else {
		t.Logf("  => CONFIRMED: Otter's returned slice ALIASES the stored value; the mutation corrupted the cache for all readers")
	}

	// --- Rostam: copies on Get ---
	c, err := cache.New(cache.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.Put([]byte(key), append([]byte(nil), orig...), 0)

	r1, err := c.Get([]byte(key))
	if err != nil {
		t.Fatal(err)
	}
	for i := range r1 { // caller mutates Rostam's returned slice
		r1[i] = 'Z'
	}
	r2, _ := c.Get([]byte(key))
	t.Logf("Rostam: stored=%q ; after caller mutated the returned slice, re-Get=%q", orig, r2)
	if !bytes.Equal(r2, orig) {
		t.Errorf("Rostam Get returned an aliased slice — mutation leaked into the cache")
	} else {
		t.Logf("  => CONFIRMED: Rostam returned an isolated COPY; the mutation left the cache intact")
	}
}
