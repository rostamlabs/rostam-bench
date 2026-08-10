#!/usr/bin/env bash
# FULL local competitive sweep — 7 engines, co-located, everything in Docker.
#
# Adds Valkey (netkv has had an -engine valkey since the redis-wire refactor;
# it had simply never been run). Re-measures every engine on one machine under
# one set of conditions, which the cloud+laptop split never allowed: the wire
# run capped the fast engines at the NIC and the loopback run only covered
# three of them.
#
# FAIRNESS (each of these was a real bug at some point in this suite):
#   - every engine in Docker, Rostam included (Rostam ran natively in the cloud
#     run and so skipped container network/cgroup overhead)
#   - identical --ulimit nofile on every container (Docker's 1024 default
#     throttles competitors above ~1000 conns while a native server keeps the
#     host limit -> inflates Rostam)
#   - identical --cpuset-cpus: engine 0-7, generator 8-15, 16-19 left to the
#     desktop. Without pinning, each engine's threading model takes a different
#     share of the generator's cores and the result measures appetite, not speed
#   - identical workload, keyspace and value size; only the client call differs
#
# HONESTY: P/E-hybrid laptop with ~10GB still in swap. DIRECTIONAL — good for
# ranking engines under one workload, not for absolute throughput. See the
# hardware caveats in README.
set -uo pipefail
BENCH="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
NETKV="$BENCH/bin/netkv"; IMAGE=rostam-server:bench
RES="$BENCH/results"; TSV="$RES/local_full.tsv"
RUN=/tmp/claude-1000/-home-vahid-projects-rostam/70703370-4c1e-472a-9893-2039278513e7/scratchpad/localfull

ENGINE_CPUS=${ENGINE_CPUS:-0-7}; GEN_CPUS=${GEN_CPUS:-8-15}; GEN_PROCS=8
KEYS=${KEYS:-100000}; VALSIZE=256
DURATION=${DURATION:-10}; WARMUP=3; REPEATS=${REPEATS:-3}
CONNS="${CONNS:-8 64 256 512}"
ENGINES="${ENGINES:-rostam valkey redis dragonfly keydb memcached aerospike}"
ULIMIT="--ulimit nofile=1048576:1048576"

P_ROSTAM=17000; P_REDIS=6379; P_DFLY=6380; P_KEYDB=6381; P_VALKEY=6382
P_MC=11211; P_AERO=3000

log() { echo "[$(date +%H:%M:%S)] $*"; }
stop() { docker rm -f lf0 >/dev/null 2>&1; sleep 1; }
port_open() { (exec 3<>/dev/tcp/127.0.0.1/"$1") 2>/dev/null; }
wait_up() { for _ in $(seq 1 90); do port_open "$1" && return 0; sleep 1; done; return 1; }

init() {
  mkdir -p "$RES" "$RUN"
  [ -f "$TSV" ] || printf 'tier\tconfig\tengine\tworkload\tconns\tgens\trep\tops_s\tops\terrs\tp50_us\tp99_us\tp999_us\n' > "$TSV"
  # Explicit budget: the default is 25% of host RAM, which on this box is ~7.5GB
  # and (with GOGC=40) implies far more RSS than we want next to the generator.
  echo '{"cache":{"max_memory":"2GiB"}}' > "$RUN/cache.json"; chmod 644 "$RUN/cache.json"
  cp "$BENCH/aerospike-mem.conf" "$RUN/aerospike.conf" 2>/dev/null; chmod 644 "$RUN/aerospike.conf" 2>/dev/null
}

start_engine() {
  local e="$1"; stop
  case "$e" in
    rostam)
      docker run -d --name lf0 --network host $ULIMIT --cpuset-cpus "$ENGINE_CPUS" \
        -e GOMAXPROCS=8 -e GOGC=40 -v "$RUN/cache.json:/etc/rc.json:ro" "$IMAGE" \
        -tcp "127.0.0.1:$P_ROSTAM" -http '' -data '' -config /etc/rc.json >/dev/null
      wait_up $P_ROSTAM ;;
    redis)
      docker run -d --name lf0 --network host $ULIMIT --cpuset-cpus "$ENGINE_CPUS" \
        redis:7-alpine redis-server --port $P_REDIS --save '' --appendonly no \
        --maxmemory 4gb --protected-mode no >/dev/null
      wait_up $P_REDIS ;;
    valkey)
      # Valkey is the Redis fork; same wire protocol, so netkv drives it with
      # the same client and the comparison stays apples-to-apples.
      docker run -d --name lf0 --network host $ULIMIT --cpuset-cpus "$ENGINE_CPUS" \
        valkey/valkey:8-alpine valkey-server --port $P_VALKEY --save '' \
        --appendonly no --maxmemory 4gb --protected-mode no >/dev/null
      wait_up $P_VALKEY ;;
    dragonfly)
      docker run -d --name lf0 --network host $ULIMIT --ulimit memlock=-1 \
        --cpuset-cpus "$ENGINE_CPUS" docker.dragonflydb.io/dragonflydb/dragonfly \
        --port $P_DFLY --maxmemory=4gb >/dev/null
      wait_up $P_DFLY ;;
    keydb)
      docker run -d --name lf0 --network host $ULIMIT --cpuset-cpus "$ENGINE_CPUS" \
        eqalpha/keydb keydb-server --port $P_KEYDB --save '' --appendonly no \
        --protected-mode no >/dev/null
      wait_up $P_KEYDB ;;
    memcached)
      docker run -d --name lf0 --network host $ULIMIT --cpuset-cpus "$ENGINE_CPUS" \
        memcached:alpine -m 4096 -c 20000 -t 8 -p $P_MC >/dev/null
      wait_up $P_MC ;;
    aerospike)
      docker run -d --name lf0 --network host $ULIMIT --cpuset-cpus "$ENGINE_CPUS" \
        -v "$RUN/aerospike.conf:/etc/aerospike/aerospike.conf" \
        aerospike/aerospike-server:8.1.2.4 >/dev/null
      wait_up $P_AERO && sleep 8 ;;
    *) log "unknown engine $e"; return 1 ;;
  esac || { log "FATAL: $e never listened"; docker logs lf0 2>&1 | tail -8; return 1; }
  docker ps --format '{{.Names}}' | grep -qx lf0 || {
    log "FATAL: $e container died"; docker logs lf0 2>&1 | tail -8; return 1; }
}

eflags() {
  case "$1" in
    rostam)    echo "-engine rostam -rostam 127.0.0.1:$P_ROSTAM" ;;
    redis)     echo "-engine redis -redis 127.0.0.1:$P_REDIS" ;;
    valkey)    echo "-engine valkey -valkey 127.0.0.1:$P_VALKEY" ;;
    dragonfly) echo "-engine dragonfly -dragonfly 127.0.0.1:$P_DFLY" ;;
    keydb)     echo "-engine keydb -keydb 127.0.0.1:$P_KEYDB" ;;
    memcached) echo "-engine memcached -memcached 127.0.0.1:$P_MC" ;;
    aerospike) echo "-engine aerospike -aero 127.0.0.1 -aeroport $P_AERO" ;;
  esac
}

point() { # point <engine> <workload> <conns>
  local e="$1" wl="$2" c="$3" readpct
  case "$wl" in get) readpct=100;; put) readpct=0;; esac
  local raw="$RES/raw/full-$e-$wl-c$c"; mkdir -p "$raw"
  for rep in $(seq 1 "$REPEATS"); do
    GOMAXPROCS=$GEN_PROCS taskset -c "$GEN_CPUS" "$NETKV" $(eflags "$e") \
      -conns "$c" -readpct "$readpct" -keys "$KEYS" -valsize "$VALSIZE" \
      -duration "$DURATION" -warmup "$WARMUP" > "$raw/rep$rep.out" 2>"$raw/rep$rep.err"
    if python3 "$BENCH/parse_netkv.py" 6 full "$e" "$wl" "$c" 0 "$rep" \
         < "$raw/rep$rep.out" >> "$TSV"; then
      local row ops errs; row=$(tail -1 "$TSV"); ops=$(echo "$row"|cut -f9); errs=$(echo "$row"|cut -f10)
      if [ "${errs:-0}" -gt 0 ] 2>/dev/null && [ "$(( errs*100/(ops>0?ops:1) ))" -ge 1 ]; then
        log "  ** ERRORS $e $wl c=$c rep=$rep -> $errs/$ops -- NOT PUBLISHABLE"
      else
        log "  ok $e $wl c=$c rep=$rep -> $(echo "$row"|cut -f8,10,11)"
      fi
    else
      log "  !! no output: $e $wl c=$c rep=$rep"; tail -n 2 "$raw/rep$rep.err"
    fi
  done
}

init
log "=== FULL LOCAL SWEEP: $ENGINES ==="
log "conns: $CONNS | reps: $REPEATS | ${DURATION}s | engine cpus $ENGINE_CPUS / gen $GEN_CPUS | DIRECTIONAL"
for e in $ENGINES; do
  log "--- $e ---"
  start_engine "$e" || continue
  for wl in get put; do for c in $CONNS; do point "$e" "$wl" "$c"; done; done
done
stop
log "=== DONE ==="
python3 "$BENCH/analyze.py" "$TSV" 2>&1 | tail -40
