// SPDX-License-Identifier: BUSL-1.1

// Command rostam_grpc serves Rostam's vector search over gRPC, with SIFT-1M
// preloaded — the same transport (gRPC/HTTP2/protobuf) as Qdrant, so a gRPC
// client benchmark isolates the server implementation from the wire protocol.
// Nested module so the gRPC deps never touch Rostam's core go.mod.
//
//	ROSTAM_SIFT_DIR=/tmp/rostam-sift1m/sift go run .   (binds 127.0.0.1:7701)
package main

import (
	"context"
	"encoding/binary"
	"fmt"
	"log"
	"math"
	"net"
	"os"
	"path/filepath"
	"runtime"

	"google.golang.org/grpc"

	"github.com/rostamlabs/rostam/vector"
	"rostamgrpcbench/pb"
)

type server struct {
	pb.UnimplementedVectorSearchServer
	coll *vector.Collection
}

func (s *server) Search(_ context.Context, req *pb.SearchRequest) (*pb.SearchResponse, error) {
	res, err := s.coll.Search(req.Query, int(req.K))
	if err != nil {
		return nil, err
	}
	hits := make([]*pb.Hit, len(res))
	for i, r := range res {
		hits[i] = &pb.Hit{Id: r.ID, Distance: r.Distance}
	}
	return &pb.SearchResponse{Hits: hits}, nil
}

func main() {
	dir := os.Getenv("ROSTAM_SIFT_DIR")
	if dir == "" {
		dir = "/tmp/rostam-sift1m/sift"
	}
	addr := os.Getenv("ROSTAM_GRPC_ADDR")
	if addr == "" {
		addr = "127.0.0.1:7701"
	}

	base, err := readFvecs(filepath.Join(dir, "sift_base.fvecs"))
	if err != nil {
		log.Fatalf("read base: %v", err)
	}
	log.Printf("loaded %d x %d; building index...", len(base), len(base[0]))

	tmp, err := os.MkdirTemp("", "rostam-grpc-coll")
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

	lis, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatal(err)
	}
	gs := grpc.NewServer()
	pb.RegisterVectorSearchServer(gs, &server{coll: coll})
	fmt.Printf("ready addr=%s collection=sift n=%d dim=%d ef=64\n", lis.Addr(), len(base), len(base[0]))
	if err := gs.Serve(lis); err != nil {
		log.Fatalf("serve: %v", err)
	}
}

func readFvecs(path string) ([][]float32, error) {
	data, err := os.ReadFile(path)
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
