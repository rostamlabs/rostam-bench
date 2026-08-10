#!/bin/bash
# Vector-write replication sweep: Rostam PB RF=2, commit-all vs commit-master.
# Self-contained: brings each posture up, gates it, measures, tears it down.
#
# EVERY listener is 127.0.0.1 — raft, tcp, pb AND http. This host has ufw
# inactive and iptables -P INPUT ACCEPT, so a 0.0.0.0 bind would expose an
# unauthenticated database to the internet. The gate below aborts if the
# non-loopback listener set ever differs from the baseline captured at start.
set -u
BIN=${BIN:?}; NETVEC=${NETVEC:?}; ROOT=${ROOT:?}
CONNS=${CONNS:-"8 32 128"}
REPS=${REPS:-2}
SHARDS=8
DIM=128
POINTS=100000

BASELINE=$(ss -ltn | awk '$4 !~ /^127\./ && $4 !~ /^\[::1\]/ {print $4}' | sort -u | tr '\n' ' ')
echo "BASELINE non-loopback listeners: $BASELINE"

port_gate() {
  now=$(ss -ltn | awk '$4 !~ /^127\./ && $4 !~ /^\[::1\]/ {print $4}' | sort -u | tr '\n' ' ')
  if [ "$now" != "$BASELINE" ]; then
    echo "!!! PORT GATE TRIPPED: '$now' != baseline '$BASELINE' — killing everything"
    for p in $(cat "$ROOT/pids" 2>/dev/null); do kill -9 "$p" 2>/dev/null; done
    exit 1
  fi
}

up() {
  posture=$1; EXTRA=""
  [ "$posture" = "commit-master" ] && EXTRA="-pb-commit-primary"
  DATA="$ROOT/data"; rm -rf "$DATA"; mkdir -p "$DATA"
  PEERS="n1@127.0.0.1:7401@127.0.0.1:7001@127.0.0.1:7201,n2@127.0.0.1:7402@127.0.0.1:7002@127.0.0.1:7202,n3@127.0.0.1:7403@127.0.0.1:7003@127.0.0.1:7203"
  : > "$ROOT/pids"
  for i in 1 2 3; do
    boot=""; [ "$i" -eq 1 ] && boot="-bootstrap"
    GOGC=40 GOMAXPROCS=3 "$BIN" -cluster $boot -node-id "n$i" \
      -raft-addr "127.0.0.1:740$i" -tcp "127.0.0.1:700$i" -http "127.0.0.1:800$i" \
      -shards $SHARDS -replication-factor 2 \
      -replication-mode pb -min-isr 2 -pb-addr "127.0.0.1:720$i" \
      -data "$DATA/n$i" -peers "$PEERS" $EXTRA \
      > "$DATA/n$i.log" 2>&1 &
    echo $! >> "$ROOT/pids"
  done

  # Readiness gate, not a fixed sleep: a fixed wait races PB primary designation
  # and the first create returns "shard: not leader", which reads as a broken
  # cluster but is only an impatient client.
  ready=0
  for _ in $(seq 1 60); do
    sleep 2
    out=$(curl -s --max-time 3 -X POST "http://127.0.0.1:8001/v1/collections" \
          -H 'Content-Type: application/json' \
          -d '{"name":"__ready__","config":{"dim":4,"metric":"cosine"}}' 2>/dev/null)
    case "$out" in *'"name"'*|*'already exists'*) ready=1; break ;; esac
  done
  curl -s --max-time 3 -X DELETE "http://127.0.0.1:8001/v1/collections/__ready__" >/dev/null 2>&1
  [ "$ready" -eq 1 ] || { echo "REFUSING: cluster never accepted a write"; tail -25 "$DATA/n1.log"; return 1; }

  # Never take a number from a cluster that is not actually at RF=2.
  rep=$(curl -s --max-time 5 "http://127.0.0.1:8001/v1/replication")
  case "$rep" in *'"under_replicated":true'*) echo "REFUSING: shard under-replicated"; return 1 ;; esac
  port_gate
  return 0
}

down() {
  for p in $(cat "$ROOT/pids" 2>/dev/null); do kill -9 "$p" 2>/dev/null; done
  sleep 4
}

for posture in commit-all commit-master; do
  echo "########## POSTURE=$posture ##########"
  up "$posture" || { down; continue; }
  # Create the collection ONCE up front with a throwaway probe run, so a
  # transient bringup failure cannot consume the "drop" and leave later rows
  # measuring a collection that does not exist yet (that is what corrupted the
  # first server sweep). Every measured row then runs with -drop=false.
  "$NETVEC" -addr 127.0.0.1:8001,127.0.0.1:8002,127.0.0.1:8003 \
    -conns 4 -dim $DIM -points $POINTS -partitions $SHARDS \
    -duration 2 -warmup 1 -drop=true -setup=true -label "$posture/seed" 2>&1 | grep -E 'engine=|FAILED'

  for rep in $(seq 1 "$REPS"); do
    for c in $CONNS; do
      # Do NOT filter to result lines only: a row that fails must be VISIBLE, not
      # a gap that reads as "not run".
      "$NETVEC" -addr 127.0.0.1:8001,127.0.0.1:8002,127.0.0.1:8003 \
        -conns "$c" -dim $DIM -points $POINTS -partitions $SHARDS \
        -duration 15 -warmup 5 -setup=false -label "$posture/r$rep" 2>&1 | grep -E 'engine=|FAILED|note:'
    done
  done
  port_gate
  down
done
echo "########## COMPLETE ##########"
port_gate
echo "final non-loopback listeners:"
ss -ltn | awk 'NR==1 || ($4 !~ /^127\./ && $4 !~ /^\[::1\]/)'
