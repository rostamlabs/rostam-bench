#!/usr/bin/env python3
"""Rostam SIFT-1M latency + saturated-throughput benchmark, via a PURE-PYTHON
client speaking Rostam's binary protocol over TCP. This is the matched-client
counterpart to qdrant_latency_qps.py: same language (Python), same shape
(single-client latency p50/p99, satQPS via 32 concurrent processes), so the only
remaining differences vs Qdrant are the protocol (Rostam binary vs gRPC) and the
server (Go vs Rust) — the client-language confound is removed.

Start the server first (loads SIFT, serves on 127.0.0.1:7700):
    ROSTAM_SIFT_DIR=/tmp/rostam-sift1m/sift go run ./bench/sift1m/rostam_server
Then:
    /tmp/sift-venv/bin/python bench/sift1m/rostam_py_client.py

Wire (all big-endian), mirroring server/protocol.go + ops/vector.go:
  request  : [len:4][opNameLen:1]["vector_search"][argsLen:4][args]
  args     : [flags:1=0][colLen:1]["sift"][k:4][dim:4][query: dim*float32]
  response : [len:4][status:1][payloadLen:4][payload]
  payload  : [count:4][ count * (id:8, distBits:4) ]
"""
import multiprocessing as mp
import os
import socket
import struct
import time

import numpy as np

SIFT = os.getenv("ROSTAM_SIFT_DIR", "/tmp/rostam-sift1m/sift")
ADDR = os.getenv("ROSTAM_BENCH_ADDR", "127.0.0.1:7700")
K = 10
CONC = int(os.getenv("ROSTAM_CONC", "32"))
DUR = float(os.getenv("ROSTAM_QPS_SECONDS", "3"))
LAT_N = 2000
OP = b"vector_search"
COLL = b"sift"


def read_fvecs(path):
    x = np.fromfile(path, dtype="int32")
    d = int(x[0])
    return x.reshape(-1, d + 1)[:, 1:].copy().view("float32")


def read_ivecs(path):
    x = np.fromfile(path, dtype="int32")
    d = int(x[0])
    return x.reshape(-1, d + 1)[:, 1:].copy()


def connect():
    host, port = ADDR.split(":")
    s = socket.create_connection((host, int(port)))
    s.setsockopt(socket.IPPROTO_TCP, socket.TCP_NODELAY, 1)
    return s


def encode_search(query, k):
    dim = len(query)
    # args: [flags:1][colLen:1][col][k:4][dim:4][query floats, big-endian]
    args = struct.pack(">BB", 0, len(COLL)) + COLL + struct.pack(">II", k, dim)
    args += struct.pack(">%df" % dim, *query)
    # body: [opNameLen:1][op][argsLen:4][args]
    body = struct.pack(">B", len(OP)) + OP + struct.pack(">I", len(args)) + args
    # frame: [len:4][body]
    return struct.pack(">I", len(body)) + body


def _recvn(sock, n):
    buf = bytearray(n)
    view = memoryview(buf)
    got = 0
    while got < n:
        r = sock.recv_into(view[got:], n - got)
        if r == 0:
            raise ConnectionError("server closed")
        got += r
    return buf


def search(sock, frame):
    sock.sendall(frame)
    (blen,) = struct.unpack(">I", _recvn(sock, 4))
    body = _recvn(sock, blen)
    status = body[0]
    if status != 0:
        raise RuntimeError(f"server status {status}")
    count = struct.unpack(">I", body[5:9])[0]
    ids = []
    off = 9
    for _ in range(count):
        ids.append(struct.unpack(">Q", body[off:off + 8])[0])
        off += 12  # 8 id + 4 dist
    return ids


def latency(queries, gt):
    sock = connect()
    frames = [encode_search(queries[i], K) for i in range(LAT_N)]
    lats, hits = [], 0
    for i in range(LAT_N):
        t0 = time.perf_counter()
        ids = search(sock, frames[i])
        lats.append((time.perf_counter() - t0) * 1e6)  # us
        truth = set(int(x) + 1 for x in gt[i])  # Rostam ids are 1-based
        hits += len(set(ids) & truth)
    sock.close()
    lats.sort()
    return hits / (LAT_N * K), lats[len(lats) // 2], lats[int(len(lats) * 0.99)], sum(lats) / len(lats)


_FRAMES = None  # precomputed request frames; inherited by workers via fork


def _qps_worker(wid):
    sock = connect()
    n, i, m = 0, wid, len(_FRAMES)
    stop = time.time() + DUR
    while time.time() < stop:
        for _ in range(16):
            search(sock, _FRAMES[i % m])
            i += 1
            n += 1
    sock.close()
    return n


def saturated_qps(queries):
    global _FRAMES
    _FRAMES = [encode_search(q, K) for q in queries]
    t0 = time.time()
    with mp.Pool(CONC) as pool:
        total = sum(pool.map(_qps_worker, range(CONC)))
    return total / (time.time() - t0)


def main():
    queries = read_fvecs(f"{SIFT}/sift_query.fvecs")
    gt = read_ivecs(f"{SIFT}/sift_groundtruth.ivecs")[:, :K]
    print(f"[rostam-py] {ADDR}, pure-Python client; latency=single conn, "
          f"satQPS={CONC} concurrent processes x {DUR:.0f}s (ef fixed at server=64)", flush=True)
    recall, p50, p99, mean = latency(queries, gt)
    qps = saturated_qps(queries)
    print(f"{'ef':<6} {'recall':<9} {'p50(us)':<10} {'p99(us)':<10} {'mean(us)':<10} {'satQPS':<12}")
    print(f"{64:<6} {recall:<9.4f} {p50:<10.1f} {p99:<10.1f} {mean:<10.1f} {qps:<12.0f}", flush=True)


if __name__ == "__main__":
    main()
