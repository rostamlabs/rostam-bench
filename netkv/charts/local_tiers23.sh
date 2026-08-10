#!/usr/bin/env bash
# TIER 2 (cost of replication) + TIER 3 (sharding / scale-out), local, Docker.
#
# Every bug that wrecked the cloud attempt is fixed here:
#   - wait for LISTENING SOCKETS to be released, never a fixed sleep
#     (a 1s sleep let the next cluster hit "bind: address already in use")
#   - assert EVERY node listens, not just node 0
#     (probing only node 0 let a 1-of-3 "cluster" run with no raft quorum,
#      which returned "internal error" on every write)
#   - wipe data dirs as root INSIDE a container
#     (containers run as uid 10001, so a host rm silently fails and the next
#      "clean" run starts from the previous run's full cache)
#   - state the cache budget explicitly
#     (default is 25% of host RAM *per node*; 3 nodes would claim ~22GB)
#   - flag any point with >=1% errors as NOT PUBLISHABLE
#
# HONESTY: P/E-core hybrid laptop, partly swapped. DIRECTIONAL ONLY -- good for
# comparing configurations under an identical workload, not for absolute numbers.
set -uo pipefail

BENCH="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
NETKV="$BENCH/bin/netkv"
IMAGE=rostam-server:bench
RESULTS_DIR="$BENCH/results"
TSV="$RESULTS_DIR/local_tiers23.tsv"
RUN=/tmp/claude-1000/-home-vahid-projects-rostam/70703370-4c1e-472a-9893-2039278513e7/scratchpad/localt23

TCP_BASE=17600; PB_BASE=17700; RAFT_BASE=17800
# 20 cores: node i -> 4 cores each (0-3, 4-7, 8-11), generator 12-17,
# 18-19 left for the desktop so the user's session and the load generator
# do not fight each other.
node_cpus() { echo "$(( $1 * 4 ))-$(( $1 * 4 + 3 ))"; }
GEN_CPUS=12-17; GEN_PROCS=6
NODE_PROCS=4

KEYS=${KEYS:-100000}; VALSIZE=256
DURATION=${DURATION:-15}; WARMUP=5; REPEATS=${REPEATS:-3}
# 512MiB was far too small: with eviction disabled on replicated shards the
# budget is consumed by cumulative bytes written, and a 3-rep point at ~200k
# writes/s appends ~1.8GB per node. Sized from the measured burn rate, not from
# the 25MB live set. See pb_capacity_probe.sh for the numbers.
CACHE_MEM=${CACHE_MEM:-3GiB}

log() { echo "[$(date +%H:%M:%S)] $*"; }

init() {
  mkdir -p "$RESULTS_DIR" "$RUN"
  [ -f "$TSV" ] || printf 'tier\tconfig\tengine\tworkload\tconns\tgens\trep\tops_s\tops\terrs\tp50_us\tp99_us\tp999_us\n' > "$TSV"
  echo "{\"cache\":{\"max_memory\":\"$CACHE_MEM\"}}" > "$RUN/cache.json"; chmod 644 "$RUN/cache.json"
}

stop_all() {
  for i in 0 1 2; do docker rm -f "rt$i" >/dev/null 2>&1; done
  for _ in $(seq 1 30); do
    ss -ltn 2>/dev/null | grep -qE ":(1760[0-2]|1770[0-2]|1780[0-2])\b" || return 0
    sleep 1
  done
  log "WARN: ports still bound after 30s"; return 1
}

wipe() {
  docker run --rm --user 0 --entrypoint /bin/rm -v "$RUN:/w" \
    debian:bookworm-slim -rf /w/n0 /w/n1 /w/n2 >/dev/null 2>&1
  for i in 0 1 2; do mkdir -p "$RUN/n$i"; chmod 777 "$RUN/n$i"; done
  local left; left=$(find "$RUN"/n* -mindepth 1 -maxdepth 1 2>/dev/null | wc -l)
  [ "$left" -eq 0 ] || { log "FATAL: wipe failed ($left entries) -- would run against stale state"; return 1; }
}

port_open() { (exec 3<>/dev/tcp/127.0.0.1/"$1") 2>/dev/null; }

start_single() { # one node, no cluster, no replication
  stop_all; wipe || return 1
  docker run -d --name rt0 --network host --ulimit nofile=1048576:1048576 \
    --cpuset-cpus "$(node_cpus 0)" -e GOMAXPROCS=$NODE_PROCS -e GOGC=40 \
    -v "$RUN/cache.json:/etc/rc.json:ro" "$IMAGE" \
    -tcp "127.0.0.1:$TCP_BASE" -http '' -data '' -config /etc/rc.json >/dev/null
  for _ in $(seq 1 40); do port_open $TCP_BASE && return 0; sleep 1; done
  log "FATAL: single node never listened"; docker logs rt0 2>&1 | tail -6; return 1
}

start_cluster() { # start_cluster <mode> <nodes> <shards> <rf>
  local mode="$1" nodes="$2" shards="$3" rf="$4" i
  stop_all; wipe || return 1
  local peers=()
  for i in $(seq 0 $((nodes-1))); do
    if [ "$mode" = pb ]; then
      peers+=("n$i@127.0.0.1:$((RAFT_BASE+i))@127.0.0.1:$((TCP_BASE+i))@127.0.0.1:$((PB_BASE+i))")
    else
      peers+=("n$i@127.0.0.1:$((RAFT_BASE+i))@127.0.0.1:$((TCP_BASE+i))")
    fi
  done
  local PEERS; PEERS=$(IFS=,; echo "${peers[*]}")

  for i in $(seq 0 $((nodes-1))); do
    local boot="" extra=""
    [ "$i" = 0 ] && boot="-bootstrap"
    if [ "$mode" = pb ]; then
      local minisr=2; [ "$rf" -lt 2 ] && minisr=1
      extra="-replication-mode pb -min-isr $minisr -pb-addr 127.0.0.1:$((PB_BASE+i))"
    else
      # Durability parity: PB has no WAL and never fsyncs, so the raft
      # baseline must not fsync either or this becomes a disk A/B.
      extra="-nosync -volatile-log"
    fi
    docker run -d --name "rt$i" --network host --ulimit nofile=1048576:1048576 \
      --cpuset-cpus "$(node_cpus "$i")" -e GOMAXPROCS=$NODE_PROCS -e GOGC=40 \
      -v "$RUN/n$i:/data" -v "$RUN/cache.json:/etc/rc.json:ro" "$IMAGE" \
      -cluster $boot -node-id "n$i" \
      -raft-addr "127.0.0.1:$((RAFT_BASE+i))" -tcp "127.0.0.1:$((TCP_BASE+i))" \
      -http '' -shards "$shards" -replication-factor "$rf" $extra \
      -config /etc/rc.json -data /data -peers "$PEERS" >/dev/null
  done

  local ok=1
  for i in $(seq 0 $((nodes-1))); do
    local up=0
    for _ in $(seq 1 40); do port_open $((TCP_BASE+i)) && { up=1; break; }; sleep 1; done
    [ "$up" = 1 ] || { log "FATAL: node n$i never listened"; docker logs "rt$i" 2>&1 | tail -6; ok=0; }
  done
  [ "$ok" = 1 ] || return 1
  sleep 12   # PB lease-keeper cycle / raft election settle
}

addrs_for() { local n="$1" a=() i; for i in $(seq 0 $((n-1))); do a+=("127.0.0.1:$((TCP_BASE+i))"); done; local IFS=,; echo "${a[*]}"; }

run_point() { # run_point <tier> <config> <workload> <conns> <addrs> [-- <start-fn...>]
  local tier="$1" cfg="$2" wl="$3" c="$4" addrs="$5"; shift 5
  local readpct; case "$wl" in get) readpct=100;; put) readpct=0;; esac
  local raw="$RESULTS_DIR/raw/local-$cfg-$wl-c$c"; mkdir -p "$raw"
  for rep in $(seq 1 "$REPEATS"); do
    # RESTART PER REP, not per point. Measured: a replicated cluster dies after
    # ~8M cumulative writes on a 3GiB budget (eviction is disabled on replicated
    # shards, so the budget tracks bytes WRITTEN, not live-set size). Two 15s
    # reps at 263k ops/s reach that, so rep 3 could not even preload. One rep
    # per cluster lifetime keeps every measurement inside the budget AND gives
    # each rep an identical cold start, which is better methodology anyway.
    if [ "$#" -gt 0 ]; then
      "$@" || { log "  SKIP $cfg c=$c rep=$rep (cluster failed to start)"; continue; }
    fi
    GOMAXPROCS=$GEN_PROCS taskset -c "$GEN_CPUS" "$NETKV" \
      -engine rostam -rostam "$addrs" -conns "$c" -readpct "$readpct" \
      -keys "$KEYS" -valsize "$VALSIZE" -duration "$DURATION" -warmup "$WARMUP" \
      > "$raw/rep$rep.out" 2>"$raw/rep$rep.err"
    if python3 "$BENCH/parse_netkv.py" "$tier" "$cfg" rostam "$wl" "$c" 0 "$rep" \
         < "$raw/rep$rep.out" >> "$TSV"; then
      local row ops errs; row=$(tail -1 "$TSV"); ops=$(echo "$row"|cut -f9); errs=$(echo "$row"|cut -f10)
      if [ "${errs:-0}" -gt 0 ] 2>/dev/null && [ "$(( errs * 100 / (ops>0?ops:1) ))" -ge 1 ]; then
        log "  ** ERRORS $cfg $wl c=$c rep=$rep -> $errs/$ops -- NOT PUBLISHABLE"
      else
        log "  ok $cfg $wl c=$c rep=$rep -> $(echo "$row"|cut -f8,10,11)"
      fi
    else
      log "  !! no output: $cfg $wl c=$c rep=$rep"; tail -n 2 "$raw/rep$rep.err"
    fi
  done
}

CONNS="${CONNS:-64 256 512}"
init

# THE CLUSTER IS RESTARTED BEFORE EVERY POINT, not once per config.
#
# Measured (pb_capacity_probe.sh): on a REPLICATED shard the cache cannot evict
# (cross-replica determinism, shard/pb_applier.go), and the cache is
# append-only, so the budget is consumed by CUMULATIVE BYTES WRITTEN, not by
# live-set size. With a fixed 20k-key set: a 256MiB budget died inside the
# first 2M-write burst, 1GiB died in the second, 4GiB survived four. Reusing
# one cluster across a whole config's sweep therefore poisons every point after
# the first -- which is exactly what produced 85% errors on the first attempt.
# Restarting per point also makes the points mutually comparable.
point() { # point <tier> <config> <conns> <start-fn...>
  local tier="$1" cfg="$2" c="$3"; shift 3
  local n=1; case "$cfg" in *nodes-1*) n=1;; *nodes-2*) n=2;; local-single) n=1;; *) n=3;; esac
  # The start function is handed to run_point, which invokes it once per rep.
  run_point "$tier" "$cfg" put "$c" "$(addrs_for $n)" "$@"
}

# ---------------- TIER 2: cost of replication ----------------
# NOTE the comparison being made: 1 node x 4 cores (no replication) vs 3 nodes
# x 4 cores (replicated). The cluster has 3x the CPU, so this is a
# DEPLOYMENT-vs-DEPLOYMENT comparison ("what do I get if I add nodes for
# redundancy"), NOT a per-core efficiency measure. Writes-only: reads do not
# replicate, so PUT is where replication actually costs something.
log "=== TIER 2: cost of replication (PUT) ==="
log "--- A: single node, no replication ---"
for c in $CONNS; do point 2 local-single "$c" start_single; done

log "--- B: 3-node PB RF=2 (waits for the full ISR) ---"
for c in $CONNS; do point 2 local-pb-rf2 "$c" start_cluster pb 3 8 2; done

log "--- C: 3-node raft RF=3 (-nosync -volatile-log, majority commit) ---"
for c in $CONNS; do point 2 local-raft-rf3 "$c" start_cluster raft 3 8 3; done

# ---------------- TIER 3a: shard-count sweep ----------------
log "=== TIER 3a: shard-count sweep (3-node PB RF=2, PUT) ==="
for shards in 4 8 16 32; do
  log "--- shards=$shards ---"
  for c in 128 512; do point 3 "local-shards-$shards" "$c" start_cluster pb 3 "$shards" 2; done
done

# ---------------- TIER 3b: node-count scale-out ----------------
# CONFOUND, stated so the write-up cannot forget it: the 1-node point is
# necessarily RF=1, so 1->2 changes node count AND switches replication on.
# The clean scaling ratio is 2->3, where RF=2 is held constant.
log "=== TIER 3b: node-count scale-out (PB, 8 shards/node, PUT) ==="
for nodes in 1 2 3; do
  rf=2; [ "$nodes" = 1 ] && rf=1
  log "--- nodes=$nodes rf=$rf shards=$((nodes*8)) ---"
  for c in 128 512; do point 3 "local-nodes-$nodes" "$c" start_cluster pb "$nodes" $((nodes*8)) "$rf"; done
done

stop_all
log "=== DONE ==="
python3 "$BENCH/analyze.py" "$TSV" 2>&1 | tail -60
