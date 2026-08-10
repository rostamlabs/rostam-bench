#!/bin/bash
# 3-node Aerospike CE, replication-factor 2, in-memory namespace.
#
# This config did not exist in this repo. netkv/aerospike-mem.conf is
# replication-factor 1 with `address local` and no mesh seeds — a single-node
# island. That matters more than it looks: at RF=1 there is no replica, so
# COMMIT_ALL and COMMIT_MASTER are INDISTINGUISHABLE and a commit-posture
# comparison silently produces two identical columns.
#
# Three nodes share one host via distinct port triples. Everything binds
# 127.0.0.1: Aerospike CE has no authentication, and a benchmark host often has
# no firewall (check: `ufw status`, `iptables -S INPUT`).
set -u
for c in aero0 aero1 aero2; do docker rm -f "$c" >/dev/null 2>&1; done
mkdir -p /root/aeroconf

i=0
for base in 3000 3010 3020; do
  svc=$base; fab=$((base+1)); hb=$((base+2))
  # NOTE: Aerospike's parser REJECTS single-line brace blocks — writing
  # `logging { console { context any info } }` on one line fails with
  # "unknown config parameter name 'network'" on the NEXT stanza. Every block
  # must be expanded onto its own lines.
  cat > "/root/aeroconf/aero$i.conf" <<CONF
service {
    proto-fd-max 15000
    cluster-name rf2bench
}

logging {
    console {
        context any info
    }
}

network {
    service {
        address 127.0.0.1
        port $svc
        access-address 127.0.0.1
        access-port $svc
    }
    heartbeat {
        mode mesh
        address 127.0.0.1
        port $hb
        mesh-seed-address-port 127.0.0.1 3002
        mesh-seed-address-port 127.0.0.1 3012
        mesh-seed-address-port 127.0.0.1 3022
        interval 150
        timeout 10
    }
    fabric {
        address 127.0.0.1
        port $fab
    }
}

namespace test {
    replication-factor 2
    storage-engine memory {
        data-size 1G
    }
}
CONF
  docker run -d --name "aero$i" --network host --ulimit nofile=100000:100000 \
    -v "/root/aeroconf/aero$i.conf:/etc/aerospike/aerospike.conf:ro" \
    --entrypoint /usr/bin/asd aerospike/aerospike-server:latest \
    --foreground --config-file /etc/aerospike/aerospike.conf >/dev/null
  i=$((i+1))
done

# GATE. Never trust a number from an unverified cluster: a node that failed to
# join leaves RF=2 unsatisfiable, and the run would look fast for the wrong
# reason.
echo "waiting for the 3-node cluster to form..."
for _ in $(seq 1 30); do
  sz=$(docker exec aero0 asinfo -p 3000 -v statistics 2>/dev/null \
        | tr ';' '\n' | grep '^cluster_size=' | cut -d= -f2)
  if [ "${sz:-0}" = "3" ]; then
    erf=$(docker exec aero0 asinfo -p 3000 -v "namespace/test" 2>/dev/null \
          | tr ';' '\n' | grep '^effective_replication_factor=')
    echo "OK cluster_size=3 $erf"
    [ "$erf" = "effective_replication_factor=2" ] || {
      echo "REFUSING: effective RF is not 2"; exit 1; }
    exit 0
  fi
  sleep 2
done
echo "FAILED to form cluster (cluster_size=${sz:-none})"
docker logs aero0 2>&1 | tail -15
exit 1
