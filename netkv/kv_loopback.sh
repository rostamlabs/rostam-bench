#!/usr/bin/env bash
# KV-only co-located competitive sweep, HARDENED for a public-IP box:
#   - every engine bound to 127.0.0.1 ONLY (no public port)
#   - engine-agnostic safety net: aborts if ANY new non-loopback listener appears
#   - trap-EXIT cleanup: no container or listener is left behind
#   - 12-core cpusets (engine 0-5, generator 6-11)
#   - Rostam engine = the published v0.6.0 image; competitors = images already on the box
#
# Derived from rostam-bench/netkv/charts/local_full.sh. DIRECTIONAL ranking under one
# workload — see the NIC / hardware caveats in netkv/README.md.
set -uo pipefail

BENCH="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"       # .../rostam-bench/netkv
NETKV="$BENCH/bin/netkv"
IMAGE="${IMAGE:-ghcr.io/rostamlabs/rostam:v0.6.0}"
RES="$BENCH/results"; TSV="$RES/kv_loopback.tsv"
RUN="${RUN:-/root/kvbench-run}"

# 12 vCPU: engine 0-5, generator 6-11.
ENGINE_CPUS="${ENGINE_CPUS:-0-5}"; GEN_CPUS="${GEN_CPUS:-6-11}"; GEN_PROCS="${GEN_PROCS:-6}"; EMAX="${EMAX:-6}"
KEYS="${KEYS:-100000}"; VALSIZE=256
DURATION="${DURATION:-10}"; WARMUP=3; REPEATS="${REPEATS:-3}"
CONNS="${CONNS:-8 64 256 512}"
ENGINES="${ENGINES:-rostam valkey redis dragonfly keydb memcached aerospike}"
ULIMIT="--ulimit nofile=1048576:1048576"

# Bench ports (all bound to 127.0.0.1 below).
P_ROSTAM=17000; P_REDIS=6379; P_DFLY=6380; P_KEYDB=6381; P_VALKEY=6382; P_MC=11211; P_AERO=3000

# Competitor images already present on this box (avoid re-pull).
IMG_REDIS=redis:7; IMG_VALKEY=valkey/valkey:8; IMG_DFLY=docker.dragonflydb.io/dragonflydb/dragonfly:latest
IMG_KEYDB=eqalpha/keydb:latest; IMG_MC=memcached:1.6; IMG_AERO=aerospike/aerospike-server:latest

CN=kvlb0   # single reused container name

log() { echo "[$(date +%H:%M:%S)] $*"; }
port_open() { (exec 3<>/dev/tcp/127.0.0.1/"$1") 2>/dev/null; }
wait_up() { for _ in $(seq 1 90); do port_open "$1" && return 0; sleep 1; done; return 1; }

# --- Safety net -------------------------------------------------------------
# The set of NON-loopback listeners (TCP and UDP). A loopback-only bench must never
# grow it. UDP is included because memcached and aerospike can open UDP ports too, so
# a TCP-only check would miss a UDP exposure.
public_listeners() {
  { ss -ltnH 2>/dev/null; ss -lunH 2>/dev/null; } | awk '{print $4}' \
    | grep -vE '^127\.|^\[::1\]|^\[::ffff:127\.' | sort -u
}
BASELINE_PUB=""
assert_no_new_public() { # $1 = context label
  local now new
  now="$(public_listeners)"
  new="$(comm -13 <(printf '%s\n' "$BASELINE_PUB") <(printf '%s\n' "$now"))"
  if [ -n "$new" ]; then
    log "FATAL: new PUBLIC listener(s) after $1 -> aborting to honour no-public-port:"
    printf '    %s\n' $new
    cleanup; exit 3
  fi
}

# --- Cleanup (always runs) --------------------------------------------------
cleanup() {
  docker rm -f "$CN" >/dev/null 2>&1
  log "cleanup: removed container $CN (if any)"
}
trap cleanup EXIT INT TERM

init() {
  mkdir -p "$RES" "$RES/raw" "$RUN"
  [ -f "$TSV" ] || printf 'tier\tconfig\tengine\tworkload\tconns\tgens\trep\tops_s\tops\terrs\tp50_us\tp99_us\tp999_us\n' > "$TSV"
  echo '{"cache":{"max_memory":"2GiB"}}' > "$RUN/cache.json"; chmod 644 "$RUN/cache.json"
  # Aerospike conf bound to loopback (service address 127.0.0.1; heartbeat/fabric already local).
  sed 's/address any/address 127.0.0.1/' "$BENCH/aerospike-mem.conf" > "$RUN/aerospike.conf"; chmod 644 "$RUN/aerospike.conf"
  # Fail loud if the rewrite no-op'd (conf spacing/stanza drift): shipping a lingering
  # "address any" would bind aerospike's service port to every interface — a public port.
  if grep -q 'address any' "$RUN/aerospike.conf"; then
    log "FATAL: aerospike conf still contains 'address any' after loopback rewrite — conf format drifted"; exit 4
  fi
}

# drun runs a bench container with the shared flags (detached, host network, the fd
# ulimit, and the engine cpuset); "$@" is the per-engine tail (extra docker flags, then
# the image and its args). Extra docker flags like dragonfly's --ulimit memlock=-1 pass
# fine as leading args since docker accepts run-flags in any order before the image.
drun() { docker run -d --name "$CN" --network host $ULIMIT --cpuset-cpus "$ENGINE_CPUS" "$@" >/dev/null; }

start_engine() {
  local e="$1"; docker rm -f "$CN" >/dev/null 2>&1; sleep 1
  case "$e" in
    rostam)
      drun -e GOMAXPROCS=$EMAX -e GOGC=40 -v "$RUN/cache.json:/etc/rc.json:ro" "$IMAGE" \
        -tcp "127.0.0.1:$P_ROSTAM" -http '' -data '' -config /etc/rc.json
      wait_up $P_ROSTAM ;;
    redis)
      drun $IMG_REDIS redis-server --bind 127.0.0.1 --port $P_REDIS --save '' --appendonly no \
        --maxmemory 4gb --protected-mode no
      wait_up $P_REDIS ;;
    valkey)
      drun $IMG_VALKEY valkey-server --bind 127.0.0.1 --port $P_VALKEY --save '' \
        --appendonly no --maxmemory 4gb --protected-mode no
      wait_up $P_VALKEY ;;
    dragonfly)
      drun --ulimit memlock=-1 $IMG_DFLY --bind 127.0.0.1 --port $P_DFLY --maxmemory=4gb
      wait_up $P_DFLY ;;
    keydb)
      drun $IMG_KEYDB keydb-server --bind 127.0.0.1 --port $P_KEYDB --save '' --appendonly no \
        --protected-mode no
      wait_up $P_KEYDB ;;
    memcached)
      drun $IMG_MC -l 127.0.0.1 -m 4096 -c 20000 -t $EMAX -p $P_MC
      wait_up $P_MC ;;
    aerospike)
      drun -v "$RUN/aerospike.conf:/etc/aerospike/aerospike.conf" $IMG_AERO
      wait_up $P_AERO && sleep 8 ;;
    *) log "unknown engine $e"; return 1 ;;
  esac || { log "FATAL: $e never listened"; docker logs "$CN" 2>&1 | tail -8; return 1; }
  docker ps --format '{{.Names}}' | grep -qx "$CN" || {
    log "FATAL: $e container died"; docker logs "$CN" 2>&1 | tail -8; return 1; }
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
  local e="$1" wl="$2" c="$3" readpct raw rep row flags
  case "$wl" in get) readpct=100;; put) readpct=0;; esac
  raw="$RES/raw/kvlb-$e-$wl-c$c"; mkdir -p "$raw"
  flags="$(eflags "$e")"   # constant across reps; resolve once, before the timer
  for rep in $(seq 1 "$REPEATS"); do
    GOMAXPROCS=$GEN_PROCS taskset -c "$GEN_CPUS" "$NETKV" $flags \
      -conns "$c" -readpct "$readpct" -keys "$KEYS" -valsize "$VALSIZE" \
      -duration "$DURATION" -warmup "$WARMUP" > "$raw/rep$rep.out" 2>"$raw/rep$rep.err"
    # One awk reads the output, keeps the last "ops/s" line (netkv prints:
    # "engine=.. conns=.. <wl>  <ops> ops/s  ops=.. errs=.. p50=..µs p99=..µs p999=..µs | .."),
    # normalises "p50=  123.4µs" (the %6.1f width inserts spaces) into a single token, and
    # emits the TSV row. Each metric is a key=value token, so one generic split arm handles
    # all of them (the unit-strip is a harmless no-op on the integer ops/errs).
    row="$(awk -v e="$e" -v wl="$wl" -v c="$c" -v rep="$rep" -v gp="$GEN_PROCS" '
      /ops\/s/ { last=$0 }
      END {
        if (last=="") exit 1
        gsub(/= +/,"=",last); n=split(last,F," "); ops_s=""; delete m
        for (i=1;i<=n;i++) {
          if (F[i]=="ops/s") ops_s=F[i-1]
          else if (split(F[i],kv,"=")==2) { val=kv[2]; sub(/[^0-9.].*$/,"",val); m[kv[1]]=val }
        }
        printf "1\tkvlb\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
               e, wl, c, gp, rep, ops_s, m["ops"], m["errs"], m["p50"], m["p99"], m["p999"]
      }' "$raw/rep$rep.out")"
    if [ -n "$row" ]; then
      printf '%s\n' "$row" >> "$TSV"
      local ops_s errs p50
      IFS=$'\t' read -r _ _ _ _ _ _ _ ops_s _ errs p50 _ _ <<< "$row"
      log "  ok $e $wl c=$c rep=$rep -> ops/s=$ops_s errs=$errs p50us=$p50"
    else
      log "  !! no result line: $e $wl c=$c rep=$rep"; tail -n 2 "$raw/rep$rep.err"
    fi
  done
}

init
BASELINE_PUB="$(public_listeners)"
log "=== KV LOOPBACK SWEEP: $ENGINES ==="
log "conns: $CONNS | reps: $REPEATS | ${DURATION}s | engine cpus $ENGINE_CPUS / gen $GEN_CPUS | image $IMAGE"
log "baseline public listeners (must not grow):"; printf '    %s\n' $BASELINE_PUB
for e in $ENGINES; do
  log "--- $e ---"
  start_engine "$e" || { docker rm -f "$CN" >/dev/null 2>&1; continue; }
  assert_no_new_public "$e start"      # hard stop if this engine exposed a public port
  for wl in get put; do for c in $CONNS; do point "$e" "$wl" "$c"; done; done
  docker rm -f "$CN" >/dev/null 2>&1
done
log "=== DONE ==="
# Final safety confirmation.
assert_no_new_public "sweep end"
log "verified: no public listener added by the sweep"

# Median-of-reps summary straight from the TSV (no external deps).
log "=== SUMMARY (median ops/s across reps) ==="
awk -F'\t' 'NR>1 && $8!="" {
    k=$3"\t"$4"\t"$5; n[k]++; v[k","n[k]]=$8
  }
  END {
    printf "engine\tworkload\tconns\tmedian_ops_s\tsamples\n"
    for (k in n) { c=n[k]
      # collect and sort the samples for this cell
      for (i=1;i<=c;i++) a[i]=v[k","i]
      for (i=1;i<=c;i++) for (j=i+1;j<=c;j++) if (a[j]<a[i]) { t=a[i];a[i]=a[j];a[j]=t }
      med = (c%2) ? a[(c+1)/2] : (a[c/2]+a[c/2+1])/2
      printf "%s\t%.0f\t%d\n", k, med, c
      delete a
    }
  }' "$TSV" | sort -t$'\t' -k1,1 -k2,2 -k3,3n | column -t
