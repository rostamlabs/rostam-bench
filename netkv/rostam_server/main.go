// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 RostamLabs
//
// Command rostam_server runs a single-node Rostam server for the netkv
// benchmark with a custom "aru" (atomic record update) op registered alongside
// the builtins. The benchmark talks to it over TCP exactly like Redis/Aerospike.
//
// The dispatcher mirrors the real Direct backend's apply path EXACTLY: an
// OpReadWrite op is serialized on a single mutex for the duration of the
// handler (just as rostam's directStore.Call holds d.mu, and as Raft's single
// FSM-apply goroutine provides in Embedded mode). So the read-modify-write the
// "aru" op performs is genuinely atomic here — no unfair lock-free shortcut.
//
//	go run ./netkv/rostam_server -addr 127.0.0.1:7000
package main

import (
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"log"
	"sync"

	"github.com/rostamlabs/rostam/cache"
	"github.com/rostamlabs/rostam/ops"
	"github.com/rostamlabs/rostam/server"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:7000", "tcp host:port to bind")
	flag.Parse()

	c, err := cache.New(cache.DefaultConfig())
	if err != nil {
		log.Fatalf("cache.New: %v", err)
	}
	defer func() { _ = c.Close() }()

	reg := ops.NewRegistry()
	if err := ops.RegisterBuiltins(reg); err != nil {
		log.Fatalf("register builtins: %v", err)
	}
	// Custom native Go op: one atomic multi-field record update, routed by the
	// standard [keyLen u16][key] arg prefix (same router the builtins use).
	if err := reg.RegisterRoutable("aru", ops.OpReadWrite, handleAtomicRecordUpdate, ops.KeyExtractorByHandle("std")); err != nil {
		log.Fatalf("register aru: %v", err)
	}
	// Custom native Go ops for the cas/append/bitmask benchmark workloads. Each
	// routes on the same std [keyLen u16][key] prefix and serializes on its
	// shard's mutex (so each read-modify-write is genuinely atomic).
	if err := reg.RegisterRoutable("casw", ops.OpReadWrite, handleCASWrite, ops.KeyExtractorByHandle("std")); err != nil {
		log.Fatalf("register casw: %v", err)
	}
	if err := reg.RegisterRoutable("app", ops.OpReadWrite, handleAppend, ops.KeyExtractorByHandle("std")); err != nil {
		log.Fatalf("register app: %v", err)
	}
	if err := reg.RegisterRoutable("shft", ops.OpReadWrite, handleShift, ops.KeyExtractorByHandle("std")); err != nil {
		log.Fatalf("register shft: %v", err)
	}

	disp := &benchDispatcher{reg: reg, tx: ops.NewTxContext(c), cache: c, opMu: make([]sync.Mutex, c.NumShards())}
	srv, err := server.New(server.Config{Addr: *addr, Dispatcher: disp})
	if err != nil {
		log.Fatalf("server.New: %v", err)
	}
	fmt.Printf("ready addr=%s op=aru\n", srv.Addr())
	if err := srv.Serve(); err != nil {
		log.Fatalf("serve: %v", err)
	}
}

// benchDispatcher serves ops from the registry against a single TxContext,
// mirroring rostam's directStore.Call EXACTLY: a routable read-write op locks
// only its routing key's shard (so writes to different shards run in parallel),
// while a shardless RW op takes the all-shards barrier. This is what makes the
// benchmark reflect the engine's real per-shard write concurrency.
type benchDispatcher struct {
	reg   *ops.Registry
	tx    *ops.TxContext
	cache *cache.Cache
	opMu  []sync.Mutex // one per cache shard; index = cache.ShardIndex(key)
}

func (d *benchDispatcher) Call(name string, args []byte) ([]byte, error) {
	h, kind, ke, ok := d.reg.Lookup(name)
	if !ok {
		return nil, fmt.Errorf("op %q not registered", name)
	}
	if kind == ops.OpReadWrite {
		if ke != nil && !d.reg.CrossShard(name) {
			if key, ok := ke(args); ok {
				mu := &d.opMu[d.cache.ShardIndex(key)]
				mu.Lock()
				defer mu.Unlock()
				return h(d.tx, args)
			}
		}
		for i := range d.opMu {
			d.opMu[i].Lock()
		}
		defer func() {
			for i := range d.opMu {
				d.opMu[i].Unlock()
			}
		}()
	}
	return h(d.tx, args)
}

func (d *benchDispatcher) LeaderAddr() string { return "" }

// handleAtomicRecordUpdate folds one "match result" into a packed 24-byte record
// ([games i64][best i64][score i64], big-endian) in a single atomic op:
//
//	games += 1;  best = max(best, streak);  score += points
//
// args = [keyLen u16 BE][key][streak i64 BE][points i64 BE] (the std key prefix
// the KeyExtractor routes on, followed by this op's payload).
func handleAtomicRecordUpdate(tx *ops.TxContext, args []byte) ([]byte, error) {
	if len(args) < 2 {
		return nil, errors.New("aru: short args")
	}
	klen := int(binary.BigEndian.Uint16(args[0:2]))
	off := 2 + klen
	if len(args) < off+16 {
		return nil, errors.New("aru: short payload")
	}
	key := args[2:off]
	streak := int64(binary.BigEndian.Uint64(args[off : off+8]))
	points := int64(binary.BigEndian.Uint64(args[off+8 : off+16]))

	var games, best, score int64
	switch cur, err := tx.Get(key); {
	case err == nil:
		if len(cur) >= 24 {
			games = int64(binary.BigEndian.Uint64(cur[0:8]))
			best = int64(binary.BigEndian.Uint64(cur[8:16]))
			score = int64(binary.BigEndian.Uint64(cur[16:24]))
		}
	case errors.Is(err, cache.ErrNotFound):
		// first write for this key: start from a zero record
	default:
		return nil, err
	}

	games++
	if streak > best {
		best = streak
	}
	score += points

	out := make([]byte, 24)
	binary.BigEndian.PutUint64(out[0:8], uint64(games))
	binary.BigEndian.PutUint64(out[8:16], uint64(best))
	binary.BigEndian.PutUint64(out[16:24], uint64(score))
	if err := tx.Put(key, out, 0); err != nil {
		return nil, err
	}
	return out, nil
}

// handleCASWrite conditionally writes an int64 counter under the std key prefix.
// args = [keyLen u16 BE][key][expected i64 BE][new i64 BE]. If the current value
// (8-byte BE; absent => 0) equals expected, it writes new (8 bytes BE) and
// returns []byte{1}; otherwise it writes nothing and returns []byte{0}.
// Atomicity comes from the dispatcher's per-shard lock — no extra locking here.
func handleCASWrite(tx *ops.TxContext, args []byte) ([]byte, error) {
	if len(args) < 2 {
		return nil, errors.New("casw: short args")
	}
	klen := int(binary.BigEndian.Uint16(args[0:2]))
	off := 2 + klen
	if len(args) < off+16 {
		return nil, errors.New("casw: short payload")
	}
	key := args[2:off]
	expected := int64(binary.BigEndian.Uint64(args[off : off+8]))
	newVal := int64(binary.BigEndian.Uint64(args[off+8 : off+16]))

	var cur int64
	switch v, err := tx.Get(key); {
	case err == nil:
		if len(v) >= 8 {
			cur = int64(binary.BigEndian.Uint64(v[0:8]))
		}
	case errors.Is(err, cache.ErrNotFound):
		// absent => 0
	default:
		return nil, err
	}

	if cur != expected {
		return []byte{0}, nil
	}
	out := make([]byte, 8)
	binary.BigEndian.PutUint64(out, uint64(newVal))
	if err := tx.Put(key, out, 0); err != nil {
		return nil, err
	}
	return []byte{1}, nil
}

// handleAppend appends payload bytes to the current value (absent => empty) and
// writes the concatenation. args = [keyLen u16 BE][key][suffix bytes].
func handleAppend(tx *ops.TxContext, args []byte) ([]byte, error) {
	if len(args) < 2 {
		return nil, errors.New("app: short args")
	}
	klen := int(binary.BigEndian.Uint16(args[0:2]))
	off := 2 + klen
	if len(args) < off {
		return nil, errors.New("app: short payload")
	}
	key := args[2:off]
	suffix := args[off:]

	var cur []byte
	switch v, err := tx.Get(key); {
	case err == nil:
		cur = v
	case errors.Is(err, cache.ErrNotFound):
		// absent => empty
	default:
		return nil, err
	}
	// Copy out of the cache's returned slice to avoid aliasing it.
	out := append(append([]byte(nil), cur...), suffix...)
	if err := tx.Put(key, out, 0); err != nil {
		return nil, err
	}
	return nil, nil
}

// handleShift performs a rolling left-shift of an 8-byte uint64 BE counter
// (absent => 0): v = v<<1 | 1, shifting in a 1 bit. args = [keyLen u16 BE][key]
// (any trailing payload is ignored).
func handleShift(tx *ops.TxContext, args []byte) ([]byte, error) {
	if len(args) < 2 {
		return nil, errors.New("shft: short args")
	}
	klen := int(binary.BigEndian.Uint16(args[0:2]))
	off := 2 + klen
	if len(args) < off {
		return nil, errors.New("shft: short payload")
	}
	key := args[2:off]

	var v uint64
	switch cur, err := tx.Get(key); {
	case err == nil:
		if len(cur) >= 8 {
			v = binary.BigEndian.Uint64(cur[0:8])
		}
	case errors.Is(err, cache.ErrNotFound):
		// absent => 0
	default:
		return nil, err
	}
	v = v<<1 | 1
	out := make([]byte, 8)
	binary.BigEndian.PutUint64(out, v)
	if err := tx.Put(key, out, 0); err != nil {
		return nil, err
	}
	return nil, nil
}
