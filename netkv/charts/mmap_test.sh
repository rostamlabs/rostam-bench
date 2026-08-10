#!/usr/bin/env bash
# What does mmap/file-backed storage cost, and can a replicated node avoid it?
#
# Background: `-data ""` gives a HEAP cache; `-data <dir>` gives an MMAP cache
# backed by per-shard pages.dat (cache/config.go DataDir, Linux only). Every
# earlier tier-1 number was heap; every cluster run was mmap, which matters
# because cache/config.go documents the ErrFull cliff as CLOSED on heap
# replicated shards (frozen-swap reclaims page bytes) but only
# RECOVERABLE-BY-RESTART on mmap ones (pages wrap a fixed file region and
# cannot be swapped, so ghost bytes climb while the process runs).
#
# PART 1  can a -cluster node run with a heap cache at all? If DataDir is
#         mandatory in cluster mode then EVERY replicated deployment is mmap,
#         and the capacity behaviour measured earlier is the general case
#         rather than a persistence-only footnote.
# PART 2  single-node throughput, heap vs mmap, identical workload.
set -uo pipefail
BENCH="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
NETKV="$BENCH/bin/netkv"; IMAGE=rostam-server:bench
RUN=/tmp/claude-1000/-home-vahid-projects-rostam/70703370-4c1e-472a-9893-2039278513e7/scratchpad/mmaptest
TSV="$BENCH/results/mmap.tsv"
TCP=17900; RAFTP=17910
ENGINE_CPUS=0-3; GEN_CPUS=12-17
KEYS=${KEYS:-100000}; VALSIZE=256
DURATION=${DURATION:-10}; WARMUP=3; REPEATS=${REPEATS:-3}
CACHE=${CACHE:-2GiB}

log() { echo "[$(date +%H:%M:%S)] $*"; }
stop() { docker rm -f mm0 >/dev/null 2>&1
  for _ in $(seq 1 25); do ss -ltn 2>/dev/null | grep -qE ":(17900|17910)\b" || return 0; sleep 1; done; }
wipe() { docker run --rm --user 0 --entrypoint /bin/rm -v "$RUN:/w" \
    debian:bookworm-slim -rf /w/d >/dev/null 2>&1; mkdir -p "$RUN/d"; chmod 777 "$RUN/d"; }
port_open() { (exec 3<>/dev/tcp/127.0.0.1/"$1") 2>/dev/null; }

init() {
  mkdir -p "$RUN" "$BENCH/results"
  [ -f "$TSV" ] || printf 'tier\tconfig\tengine\tworkload\tconns\tgens\trep\tops_s\tops\terrs\tp50_us\tp99_us\tp999_us\n' > "$TSV"
  echo "{\"cache\":{\"max_memory\":\"$CACHE\"}}" > "$RUN/cache.json"; chmod 644 "$RUN/cache.json"
}

# --- PART 1 -------------------------------------------------------------
probe_cluster_heap() {
  log "=== PART 1: can a -cluster node run with an EMPTY data dir (heap cache)? ==="
  stop
  docker run -d --name mm0 --network host --cpuset-cpus "$ENGINE_CPUS" \
    -e GOMAXPROCS=4 -v "$RUN/cache.json:/etc/rc.json:ro" "$IMAGE" \
    -cluster -bootstrap -node-id n0 -raft-addr "127.0.0.1:$RAFTP" \
    -tcp "127.0.0.1:$TCP" -http '' -shards 4 -replication-factor 1 \
    -nosync -volatile-log -config /etc/rc.json -data '' \
    -peers "n0@127.0.0.1:$RAFTP@127.0.0.1:$TCP" >/dev/null 2>&1
  local up=0; for _ in $(seq 1 25); do port_open $TCP && { up=1; break; }; sleep 1; done
  if [ "$up" = 1 ]; then
    echo "  RESULT: cluster mode ACCEPTS an empty data dir -> a replicated node CAN use a heap cache"
  else
    echo "  RESULT: cluster mode REJECTS an empty data dir -> every replicated node is MMAP-backed"
    docker logs mm0 2>&1 | grep -iE "error|fatal|data" | tail -3 | sed 's/^/    /'
  fi
  stop
}

# --- PART 2 -------------------------------------------------------------
start_single() { # start_single heap|mmap
  stop; wipe
  local datamount=() dataflag="''"
  if [ "$1" = mmap ]; then
    datamount=(-v "$RUN/d:/data"); dataflag=/data
  fi
  docker run -d --name mm0 --network host --ulimit nofile=1048576:1048576 \
    --cpuset-cpus "$ENGINE_CPUS" -e GOMAXPROCS=4 -e GOGC=40 \
    -v "$RUN/cache.json:/etc/rc.json:ro" "${datamount[@]}" "$IMAGE" \
    -tcp "127.0.0.1:$TCP" -http '' -data "$dataflag" -config /etc/rc.json >/dev/null
  for _ in $(seq 1 30); do port_open $TCP && return 0; sleep 1; done
  log "FATAL: $1 never listened"; docker logs mm0 2>&1 | tail -6; return 1
}

point() { # point <mode> <workload> <conns>
  local mode="$1" wl="$2" c="$3" readpct
  case "$wl" in get) readpct=100;; put) readpct=0;; esac
  local raw="$BENCH/results/raw/mmap-$mode-$wl-c$c"; mkdir -p "$raw"
  for rep in $(seq 1 "$REPEATS"); do
    start_single "$mode" || return 1
    GOMAXPROCS=6 taskset -c "$GEN_CPUS" "$NETKV" -engine rostam -rostam "127.0.0.1:$TCP" \
      -conns "$c" -readpct "$readpct" -keys "$KEYS" -valsize "$VALSIZE" \
      -duration "$DURATION" -warmup "$WARMUP" > "$raw/rep$rep.out" 2>"$raw/rep$rep.err"
    if python3 "$BENCH/parse_netkv.py" 5 "$mode" rostam "$wl" "$c" 0 "$rep" \
         < "$raw/rep$rep.out" >> "$TSV"; then
      local row ops errs; row=$(tail -1 "$TSV"); ops=$(echo "$row"|cut -f9); errs=$(echo "$row"|cut -f10)
      if [ "${errs:-0}" -gt 0 ] 2>/dev/null && [ "$(( errs*100/(ops>0?ops:1) ))" -ge 1 ]; then
        log "  ** ERRORS $mode $wl c=$c rep=$rep -> $errs/$ops -- NOT PUBLISHABLE"
      else
        log "  ok $mode $wl c=$c rep=$rep -> $(echo "$row"|cut -f8,10,11)"
      fi
    else
      log "  !! no output: $mode $wl c=$c rep=$rep"; tail -n 2 "$raw/rep$rep.err"
    fi
  done
  # How big did the backing file actually get?
  if [ "$mode" = mmap ]; then
    log "  pages on disk: $(du -sh "$RUN/d" 2>/dev/null | cut -f1)"
  fi
}

init
probe_cluster_heap

log "=== PART 2: single-node throughput, heap vs mmap (cache=$CACHE, NVMe) ==="
for mode in heap mmap; do
  log "--- $mode ---"
  for wl in get put; do for c in 64 512; do point "$mode" "$wl" "$c"; done; done
done
stop
log "=== DONE ==="
python3 "$BENCH/analyze.py" "$TSV" 2>&1 | tail -30
