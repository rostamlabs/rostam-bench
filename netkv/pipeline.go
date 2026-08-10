// Pipelined Rostam engine: the same shard-aware leader routing as the classic
// client, but each node is reached over a few PIPELINED connections carrying
// many requests in flight (the server answers in request order — see
// rostam/server pipelining). Workers share the per-node conns; a full window
// blocks the caller (backpressure). NotLeader/errors fall back to the classic
// sync client, which owns redirects and topology healing.
package main

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cespare/xxhash/v2"

	"github.com/rostamlabs/rostam/client"
	"github.com/rostamlabs/rostam/ops"
	"github.com/rostamlabs/rostam/server"
)

var errPConnDead = errors.New("pipelined conn dead")

type presp struct {
	status  uint8
	payload []byte
	err     error
}

// pconn is one pipelined connection: callers append frames + a result slot
// under mu (write order == FIFO order), the read loop matches responses to
// slots in that order.
type pconn struct {
	c    net.Conn
	w    *bufio.Writer
	mu   sync.Mutex
	fifo chan chan presp // cap == window: fills => callers block (backpressure)
	dead atomic.Bool
}

func dialPConn(addr string, window int) (*pconn, error) {
	c, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		return nil, err
	}
	if tc, ok := c.(*net.TCPConn); ok {
		_ = tc.SetNoDelay(true)
	}
	p := &pconn{c: c, w: bufio.NewWriterSize(c, 64<<10), fifo: make(chan chan presp, window)}
	go p.readLoop()
	return p, nil
}

func (p *pconn) call(op string, args []byte) presp {
	if p.dead.Load() {
		return presp{err: errPConnDead}
	}
	rc := make(chan presp, 1)
	frame := server.EncodeRequest(op, args)
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(frame)))

	p.mu.Lock()
	if p.dead.Load() {
		p.mu.Unlock()
		return presp{err: errPConnDead}
	}
	p.fifo <- rc // may block when window is full; reader drains independently
	_, err1 := p.w.Write(hdr[:])
	_, err2 := p.w.Write(frame)
	err3 := p.w.Flush()
	p.mu.Unlock()
	if err1 != nil || err2 != nil || err3 != nil {
		p.fail()
		return presp{err: errPConnDead}
	}
	return <-rc
}

// fail marks the conn dead and closes it; the read loop then drains and fails
// every waiting slot exactly once.
func (p *pconn) fail() {
	if p.dead.CompareAndSwap(false, true) {
		_ = p.c.Close()
	}
}

func (p *pconn) readLoop() {
	r := bufio.NewReaderSize(p.c, 64<<10)
	var hdr [4]byte
	for {
		if _, err := io.ReadFull(r, hdr[:]); err != nil {
			break
		}
		n := binary.BigEndian.Uint32(hdr[:])
		body := make([]byte, n)
		if _, err := io.ReadFull(r, body); err != nil {
			break
		}
		if len(body) < 5 {
			break
		}
		plen := binary.BigEndian.Uint32(body[1:5])
		if uint32(len(body)) < 5+plen {
			break
		}
		rc := <-p.fifo // server answers in request order
		rc <- presp{status: body[0], payload: body[5 : 5+plen]}
	}
	p.fail()
	for {
		select {
		case rc := <-p.fifo:
			rc <- presp{err: errPConnDead}
		default:
			return
		}
	}
}

// ---------- engine ----------

type rostamPipeEngine struct {
	base    *client.Client // topology fetch, probe, and the fallback/redirect path
	topo    atomic.Pointer[ops.Topology]
	window  int
	perNode int

	mu    sync.Mutex
	conns map[string][]*pconn
	rr    atomic.Uint64

	stopRefresh chan struct{}
}

func newRostamPipe(addr string, conns, window, perNode int) (engine, error) {
	base, err := newRostam(addr, conns) // reuse the classic engine's construction (probe incl.)
	if err != nil {
		return nil, err
	}
	e := &rostamPipeEngine{
		base:        base.(*rostamEngine).c,
		window:      window,
		perNode:     perNode,
		conns:       make(map[string][]*pconn),
		stopRefresh: make(chan struct{}),
	}
	if err := e.refreshTopology(); err != nil {
		return nil, fmt.Errorf("rostam-pipe: initial topology: %w", err)
	}
	go func() {
		t := time.NewTicker(2 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				_ = e.refreshTopology()
			case <-e.stopRefresh:
				return
			}
		}
	}()
	return e, nil
}

func (e *rostamPipeEngine) refreshTopology() error {
	payload, err := e.base.Call(context.Background(), "__topology__", nil)
	if err != nil {
		return err
	}
	t, err := ops.DecodeTopology(payload)
	if err != nil {
		return err
	}
	e.topo.Store(&t)
	return nil
}

// route returns the pipelined conn for key's shard leader, or nil when routing
// is not possible (caller then uses the fallback client).
func (e *rostamPipeEngine) route(key []byte) *pconn {
	t := e.topo.Load()
	if t == nil || t.NumShards == 0 || len(t.Leaders) != t.NumShards {
		return nil
	}
	addr := t.Leaders[int(xxhash.Sum64(key)%uint64(t.NumShards))]
	if addr == "" {
		return nil
	}
	e.mu.Lock()
	pcs := e.conns[addr]
	if pcs == nil {
		for i := 0; i < e.perNode; i++ {
			pc, err := dialPConn(addr, e.window)
			if err != nil {
				e.mu.Unlock()
				return nil
			}
			pcs = append(pcs, pc)
		}
		e.conns[addr] = pcs
	}
	e.mu.Unlock()
	// Replace a dead conn lazily on next selection.
	i := int(e.rr.Add(1)) % len(pcs)
	if pcs[i].dead.Load() {
		if pc, err := dialPConn(addr, e.window); err == nil {
			e.mu.Lock()
			e.conns[addr][i] = pc
			e.mu.Unlock()
			return pc
		}
		return nil
	}
	return pcs[i]
}

func (e *rostamPipeEngine) call(ctx context.Context, op string, key, args []byte) error {
	if pc := e.route(key); pc != nil {
		r := pc.call(op, args)
		if r.err == nil {
			switch r.status {
			case server.StatusOK, server.StatusNotFound:
				return nil
			}
			// NotLeader / error: fall through to the classic client, which owns
			// redirect-following and will also trigger topology healing.
		}
	}
	_, err := e.base.Call(ctx, op, args)
	return err
}

func (e *rostamPipeEngine) name() string { return "rostam-pipe" }
func (e *rostamPipeEngine) semantics() string {
	return fmt.Sprintf("pipelined x%d window %d per conn", e.perNode, e.window)
}

func (e *rostamPipeEngine) setup(keys [][]byte, val []byte) error {
	ctx := context.Background()
	for _, k := range keys {
		if _, err := e.base.Call(ctx, "put", ops.EncodePutArgs(k, val, 0)); err != nil {
			return err
		}
	}
	return nil
}

func (e *rostamPipeEngine) get(ctx context.Context, key []byte) error {
	return e.call(ctx, "get", key, ops.EncodeKeyArgs(key))
}

func (e *rostamPipeEngine) put(ctx context.Context, key, val []byte) error {
	return e.call(ctx, "put", key, ops.EncodePutArgs(key, val, 0))
}

func (e *rostamPipeEngine) close() {
	close(e.stopRefresh)
	e.mu.Lock()
	for _, pcs := range e.conns {
		for _, pc := range pcs {
			pc.fail()
		}
	}
	e.mu.Unlock()
	_ = e.base.Close()
}
