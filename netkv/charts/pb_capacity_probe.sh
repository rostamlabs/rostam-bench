#!/usr/bin/env bash
# Is the PB write failure cache-capacity exhaustion, and does budget size fix it?
#
# HYPOTHESIS: Rostam's cache is append-only (every Put consumes fresh space even
# when overwriting the same key) and on a REPLICATED shard eviction is disabled
# for cross-replica determinism (shard/pb_applier.go), so ErrFull/ErrCannotEvict
# becomes pbInfraError -> "internal error". If so, a sustained PUT benchmark
# exhausts the budget in proportion to BYTES WRITTEN, not the live key set, and
# a bigger budget should push the failure later rather than prevent it.
#
# TEST: same cluster, three budgets, identical write volume. Report when errors
# first appear and what the server log says.
set -uo pipefail
BENCH="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
NETKV="$BENCH/bin/netkv"; IMAGE=rostam-server:bench
RUN=/tmp/claude-1000/-home-vahid-projects-rostam/70703370-4c1e-472a-9893-2039278513e7/scratchpad/pbprobe
TCP=17600; PB=17700; RAFT=17800
log() { echo "[$(date +%H:%M:%S)] $*"; }

stop() { for i in 0 1 2; do docker rm -f "rt$i" >/dev/null 2>&1; done
  for _ in $(seq 1 30); do ss -ltn 2>/dev/null | grep -qE ":(1760[0-2]|1770[0-2]|1780[0-2])\b" || return 0; sleep 1; done; }

wipe() { docker run --rm --user 0 --entrypoint /bin/rm -v "$RUN:/w" \
    debian:bookworm-slim -rf /w/n0 /w/n1 /w/n2 >/dev/null 2>&1
  for i in 0 1 2; do mkdir -p "$RUN/n$i"; chmod 777 "$RUN/n$i"; done; }

start_pb() { # start_pb <cache>
  stop; mkdir -p "$RUN"; wipe
  echo "{\"cache\":{\"max_memory\":\"$1\"}}" > "$RUN/cache.json"; chmod 644 "$RUN/cache.json"
  local peers=(); for i in 0 1 2; do
    peers+=("n$i@127.0.0.1:$((RAFT+i))@127.0.0.1:$((TCP+i))@127.0.0.1:$((PB+i))"); done
  local P; P=$(IFS=,; echo "${peers[*]}")
  for i in 0 1 2; do
    local boot=""; [ "$i" = 0 ] && boot="-bootstrap"
    docker run -d --name "rt$i" --network host --ulimit nofile=1048576:1048576 \
      --cpuset-cpus "$(( i*4 ))-$(( i*4+3 ))" -e GOMAXPROCS=4 -e GOGC=40 \
      -v "$RUN/n$i:/data" -v "$RUN/cache.json:/etc/rc.json:ro" "$IMAGE" \
      -cluster $boot -node-id "n$i" -raft-addr "127.0.0.1:$((RAFT+i))" \
      -tcp "127.0.0.1:$((TCP+i))" -http '' -shards 8 -replication-factor 2 \
      -replication-mode pb -min-isr 2 -pb-addr "127.0.0.1:$((PB+i))" \
      -config /etc/rc.json -data /data -peers "$P" >/dev/null
  done
  for i in 0 1 2; do
    local up=0; for _ in $(seq 1 40); do (exec 3<>/dev/tcp/127.0.0.1/$((TCP+i))) 2>/dev/null && { up=1; break; }; sleep 1; done
    [ "$up" = 1 ] || { log "FATAL n$i never listened"; docker logs "rt$i" 2>&1|tail -5; return 1; }
  done
  sleep 12
}

burst() { # burst <label> -- one 10s write burst, report ops/errs
  local out; out=$("$NETKV" -engine rostam \
    -rostam "127.0.0.1:$TCP,127.0.0.1:$((TCP+1)),127.0.0.1:$((TCP+2))" \
    -conns 64 -readpct 0 -keys 20000 -valsize 256 -duration 10 -warmup 2 2>&1 | tail -1)
  echo "  [$1] $(echo "$out" | grep -oE '[0-9]+ ops/s.*errs=[0-9]+' || echo "$out")"
}

for cache in 256MiB 1GiB 4GiB; do
  log "=== cache budget $cache (8 shards -> $(( ${cache%[MG]iB} )) units/shard) ==="
  start_pb "$cache" || continue
  # Repeated bursts against the SAME server: if the failure is cumulative bytes
  # written rather than live-set size, later bursts fail even though the key
  # count never grows.
  for b in 1 2 3 4; do burst "burst$b"; done
  echo "  --- server error lines (n0) ---"
  docker logs rt0 2>&1 | grep -oE 'err="[^"]*"' | sort | uniq -c | sort -rn | head -3
done
stop
log "=== probe done ==="
