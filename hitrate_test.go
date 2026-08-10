// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 RostamLabs

package cachebench

import (
	"context"
	"testing"

	"github.com/VictoriaMetrics/fastcache"
	bigcache "github.com/allegro/bigcache/v3"
	freecache "github.com/coocood/freecache"
	ristretto "github.com/dgraph-io/ristretto/v2"
	otter "github.com/maypok86/otter"

	"github.com/rostamlabs/rostam/cache"
)

// TestPopulationHitRate is a correctness GATE for the GetHit benchmarks: it
// populates each engine with the exact same numKeys entries the benchmark uses,
// then counts how many of those keys are actually present afterwards. A GetHit
// benchmark is only meaningful if the engine HIT rate is ~100% — an engine that
// silently dropped/evicted entries (cost/admission policies do this) would be
// timing misses, making it look artificially fast. Any engine well below 100%
// here means its benchmark number is NOT comparable.
func TestPopulationHitRate(t *testing.T) {
	correct := func(v []byte, ok bool) bool { return ok && len(v) == valueSize }

	// Rostam
	{
		c, _ := cache.New(rostamConfig())
		defer c.Close()
		for i := range wl.keys {
			_ = c.Put(wl.keys[i], wl.value, 0)
		}
		hits := 0
		for i := range wl.keys {
			v, err := c.Get(wl.keys[i])
			if correct(v, err == nil) {
				hits++
			}
		}
		t.Logf("Rostam   : %d/%d present (%.1f%%)", hits, numKeys, 100*float64(hits)/numKeys)
	}
	// freecache
	{
		c := freecache.NewCache(capBytes)
		for i := range wl.keys {
			_ = c.Set(wl.keys[i], wl.value, 0)
		}
		hits := 0
		for i := range wl.keys {
			v, err := c.Get(wl.keys[i])
			if correct(v, err == nil) {
				hits++
			}
		}
		t.Logf("freecache: %d/%d present (%.1f%%)", hits, numKeys, 100*float64(hits)/numKeys)
	}
	// Ristretto
	{
		c, _ := ristretto.NewCache(&ristretto.Config[[]byte, []byte]{NumCounters: 10 * numKeys, MaxCost: capBytes, BufferItems: 64})
		defer c.Close()
		for i := range wl.keys {
			c.Set(wl.keys[i], wl.value, int64(valueSize))
		}
		c.Wait()
		hits := 0
		for i := range wl.keys {
			v, ok := c.Get(wl.keys[i])
			if correct(v, ok) {
				hits++
			}
		}
		t.Logf("Ristretto: %d/%d present (%.1f%%)", hits, numKeys, 100*float64(hits)/numKeys)
	}
	// BigCache
	{
		cfg := bigcache.DefaultConfig(ttl)
		cfg.HardMaxCacheSize = 512
		c, _ := bigcache.New(context.Background(), cfg)
		defer c.Close()
		for i := range wl.keys {
			_ = c.Set(wl.keysStr[i], wl.value)
		}
		hits := 0
		for i := range wl.keys {
			v, err := c.Get(wl.keysStr[i])
			if correct(v, err == nil) {
				hits++
			}
		}
		t.Logf("BigCache : %d/%d present (%.1f%%)", hits, numKeys, 100*float64(hits)/numKeys)
	}
	// fastcache
	{
		c := fastcache.New(capBytes)
		for i := range wl.keys {
			c.Set(wl.keys[i], wl.value)
		}
		hits := 0
		var dst []byte
		for i := range wl.keys {
			dst = c.Get(dst[:0], wl.keys[i])
			if len(dst) == valueSize {
				hits++
			}
		}
		t.Logf("fastcache: %d/%d present (%.1f%%)", hits, numKeys, 100*float64(hits)/numKeys)
	}
	// Otter
	{
		c, _ := otter.MustBuilder[string, []byte](4 * numKeys).WithTTL(ttl).Build()
		defer c.Close()
		admitted := 0
		for i := range wl.keys {
			if c.Set(wl.keysStr[i], wl.value) {
				admitted++
			}
		}
		hits := 0
		for i := range wl.keys {
			v, ok := c.Get(wl.keysStr[i])
			if correct(v, ok) {
				hits++
			}
		}
		t.Logf("Otter    : %d/%d present (%.1f%%), Set admitted %d/%d (size=%d)", hits, numKeys, 100*float64(hits)/numKeys, admitted, numKeys, c.Size())
	}
}
