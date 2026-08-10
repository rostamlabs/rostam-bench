#!/bin/bash
# 3-node Rostam PB cluster, replication-factor 2, both commit postures.
#
# Usage: up-rostam-pb-rf2.sh <rostam-server-binary> [commit-master]
#   (no 2nd arg) -> commit-all: every write waits for the FULL ISR, which at
#                   RF=2 is 2-of-2 — the same contract as Aerospike COMMIT_ALL
#                   and as a Raft majority at RF=2. This is the default.
#   commit-master -> adds -pb-commit-primary: commit on local primary apply,
#                    replicate asynchronously (Aerospike commit-master posture).
#                    DURABILITY DOWNGRADE: an acked write is lost if the primary
#                    dies before a backup received it.
#
# RF=2 is the shape worth measuring: it is where PB, Raft and Aerospike all wait
# for every copy, so the commit contracts are actually comparable. At RF=3, PB
# waits 3-of-3 while Raft waits 2-of-3.
#
# REQUIRES a rostam-server containing the shard-formation fix. Before it, a
# cluster with replication-factor < node count left every shard whose owner set
# excluded the -bootstrap node permanently leaderless, and writes to those keys
# hung forever. If you are on an older build, the workaround is to pass
# -bootstrap to ALL THREE nodes.
set -u
BIN=${1:?usage: up-rostam-pb-rf2.sh <rostam-server-binary> [commit-master]}
POSTURE=${2:-commit-all}
EXTRA=""
[ "$POSTURE" = "commit-master" ] && EXTRA="-pb-commit-primary"

DATA=${DATA:-/root/pb-rf2-data}
rm -rf "$DATA"; mkdir -p "$DATA"

PEERS="n1@127.0.0.1:7401@127.0.0.1:7001@127.0.0.1:7201,n2@127.0.0.1:7402@127.0.0.1:7002@127.0.0.1:7202,n3@127.0.0.1:7403@127.0.0.1:7003@127.0.0.1:7203"

PIDS=()
for i in 1 2 3; do
  boot=""; [ "$i" -eq 1 ] && boot="-bootstrap"
  # -tcp on 127.0.0.1 and -http "" keep this off every external interface.
  # GOMAXPROCS is capped so 3 nodes + the generator fit the box without
  # oversubscription; size it to your host.
  GOGC=40 GOMAXPROCS=${NODE_PROCS:-3} "$BIN" -cluster $boot -node-id "n$i" \
    -raft-addr "127.0.0.1:740$i" -tcp "127.0.0.1:700$i" -http "" \
    -shards 8 -replication-factor 2 \
    -replication-mode pb -min-isr 2 -pb-addr "127.0.0.1:720$i" \
    -data "$DATA/n$i" -peers "$PEERS" $EXTRA \
    > "$DATA/n$i.log" 2>&1 &
  PIDS+=($!)
done

sleep 45

alive=0
for p in "${PIDS[@]}"; do kill -0 "$p" 2>/dev/null && alive=$((alive+1)); done
echo "posture=$POSTURE nodes alive=$alive/3"
echo "${PIDS[*]}" > "$DATA/pids"
[ "$alive" -eq 3 ] || { echo "REFUSING: not all nodes came up"; tail -20 "$DATA/n1.log"; exit 1; }

# GATE: with replication-factor < node count, some shards are owned only by
# nodes other than the bootstrap node. If formation is broken those shards never
# elect and writes to them hang rather than fail, which looks like a slow
# benchmark instead of a broken cluster. A leaderless shard logs this.
if grep -qh "no leader within bringup wave budget" "$DATA"/n*.log; then
  echo "WARNING: a shard reported no leader during bringup."
  echo "  Transient is OK (the formation driver forms it shortly after)."
  echo "  If writes then time out, the build predates the shard-formation fix."
fi
echo "ready: netkv -engine rostam -rostam 127.0.0.1:7001 ..."
