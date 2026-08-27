#!/usr/bin/env bash
# Inject the Rostam client into a VectorDBBench checkout and register it.
#
#   ./install.sh [path-to-vectordbbench-checkout]
#
# If no path is given, clones VectorDBBench next to this script. Idempotent:
# re-running copies the latest plugin source and re-applies registration only if
# missing.
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
VDB="${1:-$HERE/VectorDBBench}"

if [ ! -d "$VDB" ]; then
  echo "cloning VectorDBBench -> $VDB"
  git clone --depth 1 https://github.com/zilliztech/VectorDBBench.git "$VDB"
fi

CLIENTS="$VDB/vectordb_bench/backend/clients"
[ -d "$CLIENTS" ] || { echo "ERROR: $CLIENTS not found — is $VDB a VectorDBBench checkout?"; exit 1; }

echo "copying rostam/ plugin -> $CLIENTS/rostam"
rm -rf "$CLIENTS/rostam"          # avoid cp nesting into an existing dir
cp -r "$HERE/rostam" "$CLIENTS/rostam"

echo "registering Rostam in clients/__init__.py"
python3 - "$CLIENTS/__init__.py" <<'PY'
import sys, re
p = sys.argv[1]
src = open(p).read()
if "Rostam" in src and "DB.Rostam" in src:
    print("  already registered — skipping")
    sys.exit(0)

# 1) enum member
src = src.replace(
    '    QdrantLocal = "QdrantLocal"\n',
    '    QdrantLocal = "QdrantLocal"\n    Rostam = "Rostam"\n', 1)

# 2) init_cls / config_cls / case_config_cls — insert a Rostam branch before each
#    QdrantLocal branch (stable anchors in current main).
def insert_before(src, anchor, branch):
    return src.replace(anchor, branch + anchor, 1)

src = insert_before(src,
    '        if self == DB.QdrantLocal:\n            from .qdrant_local.qdrant_local import QdrantLocal',
    '        if self == DB.Rostam:\n            from .rostam.rostam import Rostam\n\n            return Rostam\n\n')
src = insert_before(src,
    '        if self == DB.QdrantLocal:\n            from .qdrant_local.config import QdrantLocalConfig',
    '        if self == DB.Rostam:\n            from .rostam.config import RostamConfig\n\n            return RostamConfig\n\n')
src = insert_before(src,
    '        if self == DB.QdrantLocal:\n            from .qdrant_local.config import QdrantLocalIndexConfig',
    '        if self == DB.Rostam:\n            from .rostam.config import RostamHNSWConfig\n\n            return RostamHNSWConfig\n\n')

open(p, "w").write(src)
print("  registered Rostam (enum + init_cls + config_cls + case_config_cls)")
PY

echo "registering RostamHNSW CLI command"
python3 - "$VDB/vectordb_bench/cli/vectordbbench.py" <<'PY'
import sys
p = sys.argv[1]
src = open(p).read()
if "rostam.cli" in src:
    print("  CLI already registered — skipping")
    sys.exit(0)
imp = "from ..backend.clients.rostam.cli import RostamHNSW\n"
# insert after the first per-client cli import (stable anchor)
anchor = "from ..backend.clients."
idx = src.find(anchor)
nl = src.find("\n", idx) + 1
src = src[:nl] + imp + src[nl:]
open(p, "w").write(src)
print("  registered RostamHNSW in cli/vectordbbench.py")
PY

# ── HyperspaceDB (fair Euclidean-HNSW comparison) ───────────────────────────────
echo "copying hyperspace/ plugin -> $CLIENTS/hyperspace"
rm -rf "$CLIENTS/hyperspace"
cp -r "$HERE/hyperspace" "$CLIENTS/hyperspace"

echo "registering Hyperspace in clients/__init__.py"
python3 - "$CLIENTS/__init__.py" <<'PY'
import sys
p = sys.argv[1]
src = open(p).read()
if "Hyperspace" in src and "DB.Hyperspace" in src:
    print("  already registered — skipping")
    sys.exit(0)

# 1) enum member
src = src.replace(
    '    QdrantLocal = "QdrantLocal"\n',
    '    QdrantLocal = "QdrantLocal"\n    Hyperspace = "Hyperspace"\n', 1)

# 2) init_cls / config_cls / case_config_cls — insert a Hyperspace branch before
#    each QdrantLocal branch (same stable anchors the Rostam block uses).
def insert_before(src, anchor, branch):
    return src.replace(anchor, branch + anchor, 1)

src = insert_before(src,
    '        if self == DB.QdrantLocal:\n            from .qdrant_local.qdrant_local import QdrantLocal',
    '        if self == DB.Hyperspace:\n            from .hyperspace.hyperspace import Hyperspace\n\n            return Hyperspace\n\n')
src = insert_before(src,
    '        if self == DB.QdrantLocal:\n            from .qdrant_local.config import QdrantLocalConfig',
    '        if self == DB.Hyperspace:\n            from .hyperspace.config import HyperspaceConfig\n\n            return HyperspaceConfig\n\n')
src = insert_before(src,
    '        if self == DB.QdrantLocal:\n            from .qdrant_local.config import QdrantLocalIndexConfig',
    '        if self == DB.Hyperspace:\n            from .hyperspace.config import HyperspaceHNSWConfig\n\n            return HyperspaceHNSWConfig\n\n')

open(p, "w").write(src)
print("  registered Hyperspace (enum + init_cls + config_cls + case_config_cls)")
PY

echo "registering HyperspaceHNSW CLI command"
python3 - "$VDB/vectordb_bench/cli/vectordbbench.py" <<'PY'
import sys
p = sys.argv[1]
src = open(p).read()
if "hyperspace.cli" in src:
    print("  CLI already registered — skipping")
    sys.exit(0)
imp = "from ..backend.clients.hyperspace.cli import HyperspaceHNSW\n"
anchor = "from ..backend.clients."
idx = src.find(anchor)
nl = src.find("\n", idx) + 1
src = src[:nl] + imp + src[nl:]
open(p, "w").write(src)
print("  registered HyperspaceHNSW in cli/vectordbbench.py")
PY

echo
echo "done. Next:"
echo "  pip install -e '$VDB'                 # or: pip install requests pydantic numpy"
echo "  rostam-server -http 127.0.0.1:8080 -data \"\"   # start a Rostam server"
echo "  python $HERE/smoke_test.py            # validate the Rostam adapter end-to-end"
echo "  vectordbbench rostamhnsw --help"
echo
echo "  # HyperspaceDB (validate its 3 TODO(validate) SDK calls against a live server first):"
echo "  pip install hyperspacedb"
echo "  # start a HyperspaceDB gRPC server on :50051, then:"
echo "  vectordbbench hyperspacehnsw --help"
