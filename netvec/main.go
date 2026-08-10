// SPDX-License-Identifier: BUSL-1.1

// netvec is a vector-upsert load generator for measuring what a REPLICATION
// BARRIER costs Rostam's vector write path.
//
// It is deliberately NOT a recall/QPS benchmark — that is VectorDBBench's job
// (see ../vectordbbench). The only question here is the one the KV kit answers
// for KV: with a 3-node cluster at replication-factor 2, how much throughput and
// latency does an engine give up when every write must reach a second node
// before it acks?
//
// WHY THIS IS ROSTAM-ONLY. The KV sweep could line Rostam up against Aerospike,
// Redis, Valkey and KeyDB because each of those exposes a per-write replication
// posture (COMMIT_ALL / COMMIT_MASTER, WAIT 1). The vector engines do not:
// Qdrant, Milvus and turbopuffer have no comparable knob, so there is no honest
// cross-engine column to draw. What this measures is the DELTA between Rostam's
// own two postures, which is a statement about Rostam's replication cost — not
// about who wins.
//
// The server runs as a separate OS process, so every round-trip pays a real
// cross-process context switch rather than an embedded in-process call.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

func main() {
	var (
		addr     = flag.String("addr", "127.0.0.1:8001", "rostam HTTP api host:port(s), comma-separated; the first that can serve the collection is used")
		conns    = flag.Int("conns", 32, "concurrent connections")
		dim      = flag.Int("dim", 128, "vector dimension")
		points   = flag.Int("points", 100000, "id keyspace size")
		duration = flag.Int("duration", 15, "measured seconds")
		warmup   = flag.Int("warmup", 5, "warmup seconds (not measured)")
		coll     = flag.String("collection", "bench", "collection name")
		batch    = flag.Int("batch", 1, "points per request (1 = one write per round-trip)")
		label    = flag.String("label", "", "free-text label echoed in the result line")
		drop     = flag.Bool("drop", true, "drop and recreate the collection first")
		parts    = flag.Int("partitions", 8, "collection partitions; MUST be >1 to use more than one shard")
		doSetup  = flag.Bool("setup", true, "create the collection; false = assume it exists and probe with a real write")
		mode     = flag.String("mode", "upsert", "upsert = per-point indexed writes (the SLOW path, capped by the per-shard sequencing lock); bulk = stage + multi-core build (the designed fast path for loading a collection)")
		total    = flag.Int("total", 200000, "bulk mode only: total points to load")
		chunk    = flag.Int("chunk", 1000, "bulk mode only: points per staging request")
	)
	flag.Parse()

	// A vector op routes on the COLLECTION NAME, not the point id, so an
	// unpartitioned collection lives entirely on ONE shard and the whole load
	// lands on a single primary — measuring one node, not the cluster. Partitions
	// are what spread a collection (physical name "coll@gen#p", one per shard).
	// Set this to the shard count or the run silently measures a single node.
	if *parts <= 1 {
		fmt.Fprintln(os.Stderr, "WARNING: -partitions<=1 pins the whole collection to ONE shard;",
			"this measures a single primary, not the cluster.")
	}

	// Pick the node that can actually serve this collection. In PB mode a node
	// that hosts the target shard as a BACKUP answers "shard: not leader" with no
	// followable address (pbReplicator.LeaderAddr returns a node id, which
	// cluster.raftToServerAddr cannot map, so the redirect hint is dropped). A
	// generator hard-wired to one node would therefore post either a great number
	// or a wall of errors depending purely on where the collection hashed.
	// Probing keeps the measurement about the engine instead of about placement.
	// Creating an EXISTING collection answers 500 {"error":"internal error"} — the
	// real reason ("already partitioned") appears only in the server log, never on
	// the wire. So a repeated-create cannot be distinguished from a genuine
	// failure client-side, and matching on error text is hopeless. A sweep
	// therefore creates ONCE (-setup) and every measured row runs -setup=false,
	// probing with a real write instead.
	base := ""
	for _, a := range strings.Split(*addr, ",") {
		cand := "http://" + strings.TrimSpace(a)
		var err error
		if *doSetup {
			err = setup(cand, *coll, *dim, *parts, *drop)
		} else {
			err = probe(cand, *coll, *dim)
		}
		if err == nil {
			base = cand
			break
		}
		fmt.Fprintf(os.Stderr, "note: %s cannot serve %q (%v)\n", cand, *coll, err)
	}
	if base == "" {
		// Print a row-shaped failure to STDOUT. A sweep greps for result lines, so
		// a row that merely exits leaves a GAP that reads as "not run" instead of
		// "failed" — which is how an invalid sweep can look almost plausible.
		fmt.Printf("engine=rostam-vec conns=%-3d batch=%-3d dim=%-4d %-16s  FAILED: no node could serve the collection\n",
			*conns, *batch, *dim, *label)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "serving node: %s\n", base)

	// Pre-render the vector JSON. Marshalling a 128-float array per request would
	// put the GENERATOR on the critical path and understate the server: at 100k
	// ops/s that is 12.8M float formats/sec. A pool of pre-rendered vectors keeps
	// the hot loop to an integer format plus a copy.
	const pool = 4096
	rnd := rand.New(rand.NewSource(42))
	vecs := make([][]byte, pool)
	for i := range vecs {
		v := make([]float32, *dim)
		for j := range v {
			v[j] = rnd.Float32()
		}
		b, err := json.Marshal(v)
		if err != nil {
			fmt.Fprintln(os.Stderr, "marshal vector:", err)
			os.Exit(1)
		}
		vecs[i] = b
	}

	if *mode == "bulk" {
		runBulk(base, *coll, vecs, *conns, *total, *chunk, *dim, *label)
		return
	}
	r := run(base, *coll, vecs, *conns, *points, *batch, *duration, *warmup)
	r.print(*conns, *batch, *dim, *label)
}

// setup creates the collection. A benchmark that silently measured writes into a
// collection left over from a previous run — different dim, already-populated
// graph — would not be comparable across postures, so -drop is the default.
func setup(base, coll string, dim, parts int, drop bool) error {
	c := &http.Client{Timeout: 60 * time.Second}
	if drop {
		req, _ := http.NewRequest(http.MethodDelete, base+"/v1/collections/"+coll, nil)
		resp, err := c.Do(req)
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	}
	body, _ := json.Marshal(map[string]any{
		"name": coll,
		"config": map[string]any{
			"dim": dim, "metric": "cosine", "partitions": parts,
		},
	})
	// Retry the create: during bringup a partitioned create fans out to one shard
	// per partition and can transiently exceed the client's NotLeader hop budget
	// while primaries are still settling. Treating that first attempt as fatal
	// silently dropped whole rows from a sweep.
	var lastErr error
	for attempt := 0; attempt < 10; attempt++ {
		if attempt > 0 {
			time.Sleep(2 * time.Second)
		}
		resp, err := c.Post(base+"/v1/collections", "application/json", bytes.NewReader(body))
		if err != nil {
			lastErr = err
			continue
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode < 300 {
			return nil
		}
		msg := string(bytes.TrimSpace(b))
		// An existing collection is a USABLE collection. A sweep reuses one across
		// rows on purpose (only the first row of a posture drops), so treating
		// "already exists"/"already partitioned" as failure made every subsequent
		// row exit before printing — the harness bug that invalidated the first
		// server run.
		if strings.Contains(msg, "already exists") || strings.Contains(msg, "already partitioned") {
			return nil
		}
		lastErr = fmt.Errorf("create collection: %s: %s", resp.Status, msg)
		// Only leader/settling churn is worth another attempt; anything else is a
		// real rejection (bad dim, bad name) and retrying just hides it.
		if !strings.Contains(msg, "not leader") && !strings.Contains(msg, "NotLeader") {
			return lastErr
		}
	}
	return lastErr
}

// runBulk measures the DESIGNED ingest path: stage points cheaply and in
// parallel, then run one multi-core HNSW build. It reports points/sec TO
// SEARCHABLE — staging alone would be a meaningless number, because staged
// points are not queryable until the build completes.
//
// This exists because the per-point path (-mode upsert) is capped by the
// per-shard sequencing lock in pbisr.proposeSequenced, which holds writeMu
// across the whole local apply — i.e. across an entire HNSW insert — so a shard
// admits one write at a time. Bulk sidesteps that: many points per staging op,
// and the expensive graph construction happens once, concurrently, in the build.
// Reporting only the per-point number would understate ingest by roughly the
// ratio these two rows show.
func runBulk(base, coll string, vecs [][]byte, conns, total, chunk, dim int, label string) {
	stageURL := base + "/v1/collections/" + coll + "/points/bulk"
	buildURL := base + "/v1/collections/" + coll + "/points/bulk/build"

	type job struct{ lo, hi int }
	jobs := make(chan job)
	var staged, failed int64
	var firstErr atomic.Value

	start := time.Now()
	var wg sync.WaitGroup
	for w := 0; w < conns; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			tr := &http.Transport{MaxIdleConnsPerHost: 1, MaxConnsPerHost: 1, DisableCompression: true}
			defer tr.CloseIdleConnections()
			c := &http.Client{Transport: tr, Timeout: 120 * time.Second}
			rnd := rand.New(rand.NewSource(int64(w)*7919 + 3))
			buf := new(bytes.Buffer)
			for j := range jobs {
				buf.Reset()
				buf.WriteString(`{"points":[`)
				for id := j.lo; id < j.hi; id++ {
					if id > j.lo {
						buf.WriteByte(',')
					}
					writePoint(buf, uint64(id), vecs[rnd.Intn(len(vecs))])
				}
				buf.WriteString(`]}`)
				body := make([]byte, buf.Len())
				copy(body, buf.Bytes())
				resp, err := c.Post(stageURL, "application/json", bytes.NewReader(body))
				if err != nil {
					atomic.AddInt64(&failed, int64(j.hi-j.lo))
					firstErr.CompareAndSwap(nil, err.Error())
					continue
				}
				b, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				if resp.StatusCode >= 300 {
					atomic.AddInt64(&failed, int64(j.hi-j.lo))
					firstErr.CompareAndSwap(nil, resp.Status+": "+string(bytes.TrimSpace(b)))
					continue
				}
				atomic.AddInt64(&staged, int64(j.hi-j.lo))
			}
		}(w)
	}
	for lo := 0; lo < total; lo += chunk {
		hi := lo + chunk
		if hi > total {
			hi = total
		}
		jobs <- job{lo, hi}
	}
	close(jobs)
	wg.Wait()
	stageDone := time.Since(start)

	// The build is the point of the exercise; a staging-only number is not ingest.
	bc := &http.Client{Timeout: 60 * time.Minute}
	bresp, err := bc.Post(buildURL, "application/json", bytes.NewReader([]byte(`{}`)))
	buildErr := ""
	if err != nil {
		buildErr = err.Error()
	} else {
		b, _ := io.ReadAll(bresp.Body)
		bresp.Body.Close()
		if bresp.StatusCode >= 300 {
			buildErr = bresp.Status + ": " + string(bytes.TrimSpace(b))
		}
	}
	elapsed := time.Since(start)

	if buildErr != "" || failed > 0 {
		fmt.Printf("engine=rostam-vec mode=bulk  dim=%-4d %-16s  FAILED staged=%d failed=%d buildErr=%q firstErr=%v\n",
			dim, label, staged, failed, buildErr, firstErr.Load())
		os.Exit(1)
	}
	fmt.Printf("engine=rostam-vec mode=bulk  dim=%-4d %-16s  %10.0f pts/s to-searchable   pts=%-9d stage=%.1fs build=%.1fs total=%.1fs\n",
		dim, label, float64(staged)/elapsed.Seconds(), staged,
		stageDone.Seconds(), (elapsed - stageDone).Seconds(), elapsed.Seconds())
}

// probe verifies a node can actually serve writes for an EXISTING collection, by
// doing one real upsert. It replaces "try to create it and see" — which cannot
// work, since creating an existing collection returns an opaque 500 that is
// indistinguishable from a real failure.
//
// The id is outside the benchmark keyspace so the probe never perturbs a
// measured run's working set.
func probe(base, coll string, dim int) error {
	c := &http.Client{Timeout: 15 * time.Second}
	vec := make([]float32, dim)
	vb, _ := json.Marshal(vec)
	body := []byte(`{"id":18446744073709551615,"upsert":true,"vector":` + string(vb) + `}`)
	resp, err := c.Post(base+"/v1/collections/"+coll+"/points", "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("probe write: %s: %s", resp.Status, bytes.TrimSpace(b))
	}
	return nil
}

type result struct {
	ops, errs int64
	elapsed   time.Duration
	lat       []int64
}

func run(base, coll string, vecs [][]byte, conns, points, batch, duration, warmup int) result {
	url := base + "/v1/collections/" + coll + "/points"
	if batch > 1 {
		url += "/batch"
	}

	var ops, errs int64
	perWorker := make([][]int64, conns)

	start := time.Now()
	warmDeadline := start.Add(time.Duration(warmup) * time.Second)
	endDeadline := warmDeadline.Add(time.Duration(duration) * time.Second)

	var wg sync.WaitGroup
	for w := 0; w < conns; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			// One transport per worker with a single connection: N workers means N
			// real TCP connections, which is what -conns is supposed to mean. A
			// shared transport would pool arbitrarily and decouple the two.
			tr := &http.Transport{
				MaxIdleConnsPerHost: 1,
				MaxConnsPerHost:     1,
				DisableCompression:  true,
				DialContext: (&net.Dialer{
					Timeout:   10 * time.Second,
					KeepAlive: 30 * time.Second,
				}).DialContext,
			}
			defer tr.CloseIdleConnections()
			c := &http.Client{Transport: tr, Timeout: 30 * time.Second}

			rnd := rand.New(rand.NewSource(int64(w)*7919 + 1))
			lat := make([]int64, 0, 1<<16)
			var localOps, localErrs int64
			buf := new(bytes.Buffer)

			for {
				now := time.Now()
				if now.After(endDeadline) {
					break
				}
				measured := now.After(warmDeadline)

				buf.Reset()
				if batch > 1 {
					buf.WriteString(`{"upsert":true,"points":[`)
					for i := 0; i < batch; i++ {
						if i > 0 {
							buf.WriteByte(',')
						}
						writePoint(buf, uint64(rnd.Intn(points)), vecs[rnd.Intn(len(vecs))])
					}
					buf.WriteString(`]}`)
				} else {
					buf.WriteString(`{"upsert":true,`)
					buf.WriteString(`"id":`)
					buf.WriteString(strconv.FormatUint(uint64(rnd.Intn(points)), 10))
					buf.WriteString(`,"vector":`)
					buf.Write(vecs[rnd.Intn(len(vecs))])
					buf.WriteByte('}')
				}
				body := make([]byte, buf.Len())
				copy(body, buf.Bytes())

				t0 := time.Now()
				ok := doPost(c, url, body)
				el := time.Since(t0).Nanoseconds()

				if measured {
					if ok {
						localOps += int64(batch)
						lat = append(lat, el)
					} else {
						localErrs++
					}
				}
			}
			perWorker[w] = lat
			atomic.AddInt64(&ops, localOps)
			atomic.AddInt64(&errs, localErrs)
		}(w)
	}
	wg.Wait()
	elapsed := time.Since(warmDeadline)

	total := 0
	for _, l := range perWorker {
		total += len(l)
	}
	merged := make([]int64, 0, total)
	for _, l := range perWorker {
		merged = append(merged, l...)
	}
	sort.Slice(merged, func(i, j int) bool { return merged[i] < merged[j] })
	return result{ops: ops, errs: errs, elapsed: elapsed, lat: merged}
}

func writePoint(buf *bytes.Buffer, id uint64, vec []byte) {
	buf.WriteString(`{"id":`)
	buf.WriteString(strconv.FormatUint(id, 10))
	buf.WriteString(`,"vector":`)
	buf.Write(vec)
	buf.WriteByte('}')
}

// doPost returns false for ANY non-2xx or transport failure. A replication
// barrier that times out must count as an error, never as a fast write — that is
// the whole reason an under-replicated run cannot masquerade as a good number.
func doPost(c *http.Client, url string, body []byte) bool {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return false
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		return false
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp.StatusCode < 300
}

func pct(sorted []int64, p float64) int64 {
	if len(sorted) == 0 {
		return 0
	}
	i := int(p / 100 * float64(len(sorted)))
	if i >= len(sorted) {
		i = len(sorted) - 1
	}
	return sorted[i]
}

func (r result) print(conns, batch, dim int, label string) {
	sec := r.elapsed.Seconds()
	us := func(ns int64) float64 { return float64(ns) / 1000.0 }
	fmt.Printf("engine=rostam-vec conns=%-3d batch=%-3d dim=%-4d %-16s  %10.0f pts/s   pts=%-9d errs=%-6d  p50=%8.1fµs p99=%9.1fµs p999=%10.1fµs\n",
		conns, batch, dim, label, float64(r.ops)/sec, r.ops, r.errs,
		us(pct(r.lat, 50)), us(pct(r.lat, 99)), us(pct(r.lat, 99.9)))
}
