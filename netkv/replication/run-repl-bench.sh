#!/bin/bash
# Replicated-write sweep. Every engine below acks only after a replica has the
# write, so the rows are contractually comparable.
#
# ONE BENCHMARK AT A TIME. Launching this while another run is still going
# contends for the same cores and silently invalidates BOTH: in the run this kit
# came from, overlapping two sweeps dropped every baseline ~50% (Rostam
# commit-all @128: 81k contended vs 122k clean) and buried a real 1.57x effect in
# the noise. Confirm the previous run exited before starting.
#
# Usage: run-repl-bench.sh <netkv-binary> [reps]
set -u
NETKV=${1:?usage: run-repl-bench.sh <netkv-binary> [reps]}
REPS=${2:-2}
CONNS=${CONNS:-"8 32 128"}
# Fixed shape for every row: PUT-only, 100k keys, 256 B values, 15 s measured
# after a 5 s warmup.
SHAPE="-readpct 0 -keys 100000 -valsize 256 -duration 15 -warmup 5"

ROSTAM_ADDR=${ROSTAM_ADDR:-127.0.0.1:7001}

for rep in $(seq 1 "$REPS"); do
  echo "@@@@@@@@@@ REP $rep @@@@@@@@@@"
  for c in $CONNS; do
    # -sync on the Redis-protocol engines = SET + WAIT 1 (replica ack). A write
    # that misses -waitms counts as an ERROR, so an under-replicated run cannot
    # masquerade as fast — always check errs=0 before believing a row.
    for spec in \
      "rostam:-engine rostam -rostam $ROSTAM_ADDR" \
      "aerospike:-engine aerospike -aero 127.0.0.1 -aeroport 3000 -sync" \
      "redis:-engine redis -redis 127.0.0.1:6379 -sync" \
      "valkey:-engine valkey -valkey 127.0.0.1:6382 -sync" \
      "keydb:-engine keydb -keydb 127.0.0.1:6381 -sync" \
      "dragonfly:-engine dragonfly -dragonfly 127.0.0.1:6380 -sync" \
      "memcached-NO-REPL:-engine memcached -memcached 127.0.0.1:11211" \
    ; do
      name=${spec%%:*}; args=${spec#*:}
      out=$($NETKV $args $SHAPE -conns "$c" 2>&1 | grep -E 'engine=' | head -1)
      echo "[$name] conns=$c :: ${out:-NO RESULT}"
    done
  done
done
echo "@@@@@@@@@@ COMPLETE @@@@@@@@@@"

# READING THE OUTPUT
#
# * rostam's commit posture is NOT visible in netkv's output. -sync is ignored
#   for the rostam engine because the contract is server-side: full-ISR by
#   default, commit-master under `rostam-server -pb-commit-primary`. Track which
#   posture the server was launched with OUT OF BAND.
# * memcached has no replication. Its row is a barrier-removed reference, not a
#   peer of the others.
# * Dragonfly: if p50 is pinned near ~100 ms and does not move with connection
#   count, you are measuring its WAIT replication-ack granularity, not its
#   throughput. A latency independent of offered load is a fixed interval, not a
#   limit. Exclude the row and say why rather than reporting a huge deficit.
# * These are system-vs-system at equal replication, NOT per-core efficiency:
#   Redis and Valkey are single-threaded per instance, KeyDB and Dragonfly are
#   multi-threaded, and a Rostam cluster spans several processes.
