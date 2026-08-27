# SPDX-License-Identifier: Apache-2.0
#
# VectorDBBench client for HyperspaceDB (github.com/YARlabs/hyperspace-db),
# driven through its `hyperspacedb` Python SDK (`from hyperspace import
# HyperspaceClient`). VALIDATED against a live HyperspaceDB v3.1.3 server.
#
# The point is a FAIR, neutral comparison — the opposite of HyperspaceDB's own
# bundled harness, which runs competitors untuned and searches them before their
# index finishes building. Here: standard Euclidean HNSW, and optimize() calls
# freeze_collection() to finalize/build the index *before* any search.
#
# ── What v3.1.3's SDK actually supports (probed live) ──────────────────────────
# WORKING:  create_collection(name, schema=...), batch_insert(vectors=, ids=,
#           collection=), freeze_collection(name) [synchronous build+finalize,
#           returns "Collection '<n>' frozen."], search(vec, top_k=, collection=)
#           -> list[dict] with an 'id' key, count(collection=), list_collections().
# BROKEN / no-op in v3.1.3:  configure(ef_construction/ef_search/m) returns False
#           (index tuning is NOT applied — HyperspaceDB runs at its DEFAULTS),
#           rebuild_index() returns False, get_collection_stats() returns {}.
# CONSEQUENCE: we cannot sweep ef for HyperspaceDB, so it yields a single
#           (recall, QPS) operating point at its default config — still fair, read
#           against the other engines' matched-recall curve at HyperspaceDB's own
#           recall. configure() is still called best-effort (in case a later
#           version honors it) but its result is not relied upon.
import logging

import numpy as np

from ...filter import Filter, FilterOp
from ..api import VectorDB

log = logging.getLogger(__name__)

INSERT_BATCH = 1000


class Hyperspace(VectorDB):
    # Euclidean-HNSW comparison only; HyperspaceDB filters are geometric
    # (ball/box/cone), not scalar payload predicates, so filtered cases are off.
    supported_filter_types: list[FilterOp] = [FilterOp.NonFilter]

    def __init__(
        self,
        dim: int,
        db_config: dict,
        db_case_config,
        collection_name: str = "VectorDBBenchCollection",
        drop_old: bool = False,
        **kwargs,
    ):
        self.dim = dim
        self.db_config = db_config
        self.case_config = db_case_config
        self.collection_name = collection_name
        self.endpoint = db_config["endpoint"]
        self.api_key = db_config.get("api_key")
        self._client = None

        idx = self.case_config.index_param()
        client = self._connect()
        if drop_old:
            try:
                client.delete_collection(self.collection_name)
            except Exception as e:  # noqa: BLE001 — absent collection is fine
                log.info("Hyperspace drop_old: %s", e)
        client.create_collection(
            self.collection_name,
            schema={
                "components": [
                    {
                        "name": "primary",
                        "metric": idx["metric"],   # "l2" (Euclidean) | "cosine"
                        "full_dimension": dim,
                        "weight": 1.0,
                    }
                ],
                "cascade_pipeline": [],
            },
        )
        # Best-effort tuning. v3.1.3 returns False and ignores it (see header);
        # kept so a future version that honors it needs no code change.
        try:
            applied = client.configure(
                collection=self.collection_name,
                ef_construction=idx["ef_construction"],
                ef_search=idx["ef_search"],
                m=idx["m"],
            )
            if not applied:
                log.warning(
                    "Hyperspace configure() returned False — HNSW tuning NOT applied; "
                    "running at engine defaults (SDK limitation in this version)"
                )
        except Exception as e:  # noqa: BLE001
            log.warning("Hyperspace configure() failed: %s", e)
        self._client = None  # each worker process reconnects in init()

    def _connect(self):
        from hyperspace import HyperspaceClient  # deferred: benchmark-only dep

        if self.api_key:
            return HyperspaceClient(self.endpoint, api_key=self.api_key)
        return HyperspaceClient(self.endpoint)

    # ---- connection lifecycle (one client per worker process) ----
    def init(self):
        from contextlib import contextmanager

        @contextmanager
        def _ctx():
            self._client = self._connect()
            try:
                yield
            finally:
                self._client = None

        return _ctx()

    def _c(self):
        return self._client if self._client is not None else self._connect()

    # ---- load ----
    def insert_embeddings(self, embeddings, metadata, labels_data=None, tenant_labels_data=None, **kwargs):
        client = self._c()
        ids = [int(x) for x in metadata]
        vecs = np.asarray(embeddings, dtype=np.float32)
        n = len(ids)
        try:
            for start in range(0, n, INSERT_BATCH):
                end = min(start + INSERT_BATCH, n)
                client.batch_insert(
                    vectors=vecs[start:end].tolist(),
                    ids=ids[start:end],
                    collection=self.collection_name,
                )
            return n, None
        except Exception as e:  # noqa: BLE001 — VDBBench expects (count, exc)
            return 0, e

    def optimize(self, data_size: int | None = None):
        """Finalize + build the index before any search runs (the fairness step).

        freeze_collection() is HyperspaceDB's synchronous build/finalize call — it
        returns "Collection '<name>' frozen." once done. Unlike a fixed sleep, this
        guarantees search runs on a fully built index.
        """
        client = self._c()
        result = client.freeze_collection(self.collection_name)
        log.info("Hyperspace freeze_collection -> %s", result)

    # ---- query ----
    def prepare_filter(self, filters: Filter):
        if filters.type != FilterOp.NonFilter:
            raise RuntimeError("Hyperspace client supports only non-filter cases")

    def search_embedding(self, query, k: int = 100, *args, **kwargs):
        client = self._c()
        resp = client.search(np.asarray(query, dtype=np.float32), top_k=k, collection=self.collection_name)
        # search() returns a list[dict] each with an 'id' key (validated live).
        return [int(r["id"]) for r in resp]
