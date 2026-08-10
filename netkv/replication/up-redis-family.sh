#!/bin/bash
# Redis-protocol engines, each as master + ONE replica, plus memcached.
#
# The replica is the whole point: netkv -sync issues SET then `WAIT 1`, which
# blocks until one replica has acked. Against a lone master WAIT can never be
# satisfied and every write burns the full -waitms timeout, so a single-instance
# run is not a replicated run — it is a broken one.
#
# All binds are 127.0.0.1 (unauthenticated data stores; benchmark hosts often
# have no firewall).
set -u
for c in kv-redis-m kv-redis-r kv-valkey-m kv-valkey-r kv-keydb-m kv-keydb-r \
         kv-dfly-m kv-dfly-r kv-mc; do
  docker rm -f "$c" >/dev/null 2>&1
done

run() { docker run -d --name "$1" --network host --ulimit nofile=100000:100000 "${@:2}" >/dev/null; }

# Persistence off everywhere: this measures the REPLICATION barrier, not disk.
run kv-redis-m  redis:7         redis-server  --bind 127.0.0.1 --port 6379 --save "" --appendonly no
run kv-redis-r  redis:7         redis-server  --bind 127.0.0.1 --port 6389 --save "" --appendonly no --replicaof 127.0.0.1 6379
run kv-valkey-m valkey/valkey:8 valkey-server --bind 127.0.0.1 --port 6382 --save "" --appendonly no
run kv-valkey-r valkey/valkey:8 valkey-server --bind 127.0.0.1 --port 6392 --save "" --appendonly no --replicaof 127.0.0.1 6382
run kv-keydb-m  eqalpha/keydb   keydb-server  --bind 127.0.0.1 --port 6381 --save "" --appendonly no
run kv-keydb-r  eqalpha/keydb   keydb-server  --bind 127.0.0.1 --port 6391 --save "" --appendonly no --replicaof 127.0.0.1 6381
run kv-dfly-m   docker.dragonflydb.io/dragonflydb/dragonfly --bind 127.0.0.1 --port 6380
run kv-dfly-r   docker.dragonflydb.io/dragonflydb/dragonfly --bind 127.0.0.1 --port 6390 --replicaof 127.0.0.1:6380
# memcached has NO replication of any kind. It is a barrier-removed reference
# point, never a peer of the rows above.
run kv-mc       memcached:1.6   memcached -l 127.0.0.1 -p 11211 -m 2048

sleep 12

# GATE. An engine whose replica never attached would post a great number for an
# invalid reason, so refuse to proceed instead of reporting it.
echo "=== replication status (each must be connected_slaves:1) ==="
fail=0
for pair in "redis:6379" "valkey:6382" "keydb:6381" "dragonfly:6380"; do
  name=${pair%%:*}; port=${pair#*:}
  got=$(docker exec kv-redis-m redis-cli -h 127.0.0.1 -p "$port" INFO replication 2>/dev/null \
        | tr -d '\r' | grep -E '^(role|connected_slaves)' | tr '\n' ' ')
  echo "  $name(:$port) -> ${got:-UNREACHABLE}"
  case "$got" in *connected_slaves:1*) ;; *) fail=1 ;; esac
done
[ "$fail" = 0 ] || { echo "REFUSING: at least one engine has no attached replica"; exit 1; }

echo "=== non-loopback listeners (expect ONLY sshd / your own pre-existing) ==="
ss -ltn 2>/dev/null | awk 'NR==1 || ($4 !~ /^127\./ && $4 !~ /^\[::1\]/)'
