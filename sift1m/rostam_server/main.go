// SPDX-License-Identifier: BUSL-1.1

// Command rostam_server stands up a Rostam TCP server with a SIFT-1M collection
// preloaded, so an external client (e.g. bench/sift1m/rostam_py_client.py) can
// benchmark vector_search over the wire. Read-only search bypasses Raft, so this
// runs the bare server + CollectionStore — the same path the real Store uses for
// search. It builds the index, prints "ready", and serves until killed.
//
//	ROSTAM_SIFT_DIR=/tmp/rostam-sift1m/sift go run ./bench/sift1m/rostam_server
//	  (binds 127.0.0.1:7700; override with ROSTAM_BENCH_ADDR)
package main

import (
	"encoding/binary"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"runtime"

	"github.com/rostamlabs/rostam/ops"
	"github.com/rostamlabs/rostam/server"
	"github.com/rostamlabs/rostam/vector"
)

func main() {
	dir := os.Getenv("ROSTAM_SIFT_DIR")
	if dir == "" {
		dir = "/tmp/rostam-sift1m/sift"
	}
	addr := os.Getenv("ROSTAM_BENCH_ADDR")
	if addr == "" {
		addr = "127.0.0.1:7700"
	}

	base, err := readFvecs(filepath.Join(dir, "sift_base.fvecs"))
	if err != nil {
		log.Fatalf("read base: %v", err)
	}
	log.Printf("loaded %d x %d; building index...", len(base), len(base[0]))

	tmp, err := os.MkdirTemp("", "rostam-bench-coll")
	if err != nil {
		log.Fatal(err)
	}
	cs, err := vector.OpenCollectionStore(tmp)
	if err != nil {
		log.Fatal(err)
	}
	if err := cs.CreateCollection("sift", vector.Config{
		Dim: len(base[0]), Metric: vector.L2, M: 16, EfConstruction: 200, EfSearch: 64,
	}); err != nil {
		log.Fatal(err)
	}
	coll, ok := cs.Get("sift")
	if !ok {
		log.Fatal("collection missing after create")
	}
	ids := make([]uint64, len(base))
	for i := range ids {
		ids[i] = uint64(i + 1)
	}
	if err := coll.BuildConcurrent(ids, base, runtime.GOMAXPROCS(0)); err != nil {
		log.Fatalf("build: %v", err)
	}

	reg := ops.NewRegistry()
	if err := ops.RegisterBuiltins(reg); err != nil {
		log.Fatal(err)
	}
	disp := &dispatcher{reg: reg, tx: ops.NewTxContextWithVectors(nil, cs)}
	srv, err := server.New(server.Config{Addr: addr, Dispatcher: disp})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("ready addr=%s collection=sift n=%d dim=%d ef=64\n", srv.Addr(), len(base), len(base[0]))
	if err := srv.Serve(); err != nil {
		log.Fatalf("serve: %v", err)
	}
}

// dispatcher serves ops straight from the registry against the CollectionStore
// (read-only search path; no Raft — correct for a read benchmark).
type dispatcher struct {
	reg *ops.Registry
	tx  *ops.TxContext
}

func (d *dispatcher) Call(name string, args []byte) ([]byte, error) {
	h, _, _, ok := d.reg.Lookup(name)
	if !ok {
		return nil, fmt.Errorf("op %q not registered", name)
	}
	return h(d.tx, args)
}

func (d *dispatcher) LeaderAddr() string { return "" }

func readFvecs(path string) ([][]float32, error) {
	data, err := os.ReadFile(path) //nolint:gosec // benchmark utility: dataset path is a trusted CLI arg, not untrusted input
	if err != nil {
		return nil, err
	}
	var out [][]float32
	for off := 0; off < len(data); {
		if off+4 > len(data) {
			return nil, fmt.Errorf("fvecs: truncated header at %d", off)
		}
		dim := int(binary.LittleEndian.Uint32(data[off:]))
		off += 4
		if off+dim*4 > len(data) {
			return nil, fmt.Errorf("fvecs: truncated vector at %d", off)
		}
		v := make([]float32, dim)
		for i := range v {
			v[i] = math.Float32frombits(binary.LittleEndian.Uint32(data[off+i*4:]))
		}
		out = append(out, v)
		off += dim * 4
	}
	return out, nil
}
