#!/bin/bash
# Tear down everything this kit starts, and prove nothing is left exposed.
#
# NOTE ON KILLING: do NOT use `pkill -f <pattern>` over SSH. The pattern matches
# the SSH command's OWN command line, so it kills the session before it kills the
# target (this happened five times while building this kit). Match on the process
# NAME, or use explicit PIDs.
set -u

for c in aero0 aero1 aero2 \
         kv-redis-m kv-redis-r kv-valkey-m kv-valkey-r \
         kv-keydb-m kv-keydb-r kv-dfly-m kv-dfly-r kv-mc; do
  docker rm -f "$c" >/dev/null 2>&1
done

# comm is truncated to 15 chars by the kernel, so match a prefix, not the full
# binary name (`pgrep -x rostam-server-fix` silently matches nothing).
for p in $(ps -eo pid,comm | awk '$2 ~ /^rostam-server/ {print $1}'); do
  kill -9 "$p" 2>/dev/null
done
sleep 2

echo "=== leftover kit containers: $(docker ps -a --format '{{.Names}}' | grep -cE '^(kv-|aero[012]$)') (want 0) ==="
echo "=== leftover rostam processes: $(ps -eo comm | grep -c '^rostam-server') (want 0) ==="
echo "=== non-loopback listeners (want ONLY sshd and your own pre-existing) ==="
ss -ltn 2>/dev/null | awk 'NR==1 || ($4 !~ /^127\./ && $4 !~ /^\[::1\]/)'
