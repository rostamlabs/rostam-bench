# SPDX-License-Identifier: Apache-2.0
#
# VectorDBBench client for HyperspaceDB (github.com/YARlabs/hyperspace-db),
# driven through its `hyperspacedb` Python SDK (`from hyperspace import
# HyperspaceClient`).
#
# The point of this client is a FAIR, neutral comparison — the opposite of
# HyperspaceDB's own bundled harness, which runs competitors untuned and
# benchmarks them before their index finishes building. Here:
#   * standard Euclidean HNSW (not hyperbolic), same knobs as every other engine;
#   * ef_construction / ef_search matched to the swept values;
#   * optimize() BLOCKS until the index is fully built (polls indexing_queue -> 0)
#     before any search runs.
#
# ── VALIDATION STATUS ──────────────────────────────────────────────────────────
# HyperspaceDB's Python SDK is inconsistently documented (its README describes
# create_collection/configure/batch_insert/get_collection_stats, but the shipped
# hyperspace_client.py is a thinner gRPC client). The three calls marked
# `TODO(validate)` below — collection create/configure, batch insert, and the
# SEARCH-RESULT id extraction — must be confirmed against a running HyperspaceDB
# instance (`pip install hyperspacedb`; start the server) and adjusted if the
# real signatures differ. Everything else (methodology, batching, the
# wait-for-index poll) is engine-agnostic and correct as written.
import logging

import numpy as np

from ...filter import Filter, FilterOp
from ..api import VectorDB

log = logging.getLogger(__name__)

INSERT_BATCH = 1000
INDEX_POLL_SECONDS = 2.0


class Hyperspace(VectorDB):
    # Euclidean-HNSW comparison only; filtered cases are not wired (HyperspaceDB
    # filters are geometric — ball/box/cone — not scalar payload predicates).
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
                # TODO(validate): confirm the drop method name against the SDK.
                client.delete_collection(self.collection_name)
            except Exception as e:  # noqa: BLE001 — absent collection is fine
                log.info("Hyperspace drop_old: %s", e)
        self._create(client, dim, idx)
        self._client = None  # each worker process reconnects in init()

    def _connect(self):
        from hyperspace import HyperspaceClient  # deferred: benchmark-only dep

        # TODO(validate): confirm HyperspaceClient accepts api_key (README says
        # yes; the shipped client.py __init__ only showed host).
        if self.api_key:
            return HyperspaceClient(self.endpoint, api_key=self.api_key)
        return HyperspaceClient(self.endpoint)

    def _create(self, client, dim: int, idx: dict):
        # TODO(validate): confirm the schema shape + configure() signature.
        # Per the SDK README:
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
        client.configure(
            collection=self.collection_name,
            ef_construction=idx["ef_construction"],
            ef_search=idx["ef_search"],
        )

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

    # ---- load (insert; the build is waited on in optimize) ----
    def insert_embeddings(self, embeddings, metadata, labels_data=None, tenant_labels_data=None, **kwargs):
        client = self._c()
        ids = [int(x) for x in metadata]
        vecs = np.asarray(embeddings, dtype=np.float32)
        n = len(ids)
        try:
            for start in range(0, n, INSERT_BATCH):
                end = min(start + INSERT_BATCH, n)
                # TODO(validate): confirm batch_insert arg names/shapes.
                client.batch_insert(
                    vectors=vecs[start:end].tolist(),
                    ids=ids[start:end],
                    collection=self.collection_name,
                )
            return n, None
        except Exception as e:  # noqa: BLE001 — VDBBench expects (count, exc)
            return 0, e

    def optimize(self, data_size: int | None = None):
        """Block until the HNSW index has finished building.

        This is the fairness centerpiece: search must run on a *complete* index.
        HyperspaceDB indexes asynchronously and exposes no wait primitive, so we
        poll get_collection_stats()['indexing_queue'] until it drains to 0
        (rather than a fixed sleep, which is what their own harness does to
        competitors and why those numbers are meaningless).
        """
        import time

        client = self._c()
        deadline = time.monotonic() + 24 * 3600
        while True:
            # TODO(validate): confirm get_collection_stats() returns
            # 'indexing_queue'; adjust the key/method if the SDK differs.
            stats = client.get_collection_stats(self.collection_name)
            queued = int(stats.get("indexing_queue", 0)) if isinstance(stats, dict) else 0
            if queued <= 0:
                log.info("Hyperspace index built (indexing_queue drained to 0)")
                return
            if time.monotonic() > deadline:
                raise RuntimeError("Hyperspace index build did not complete within 24h")
            time.sleep(INDEX_POLL_SECONDS)

    # ---- query ----
    def prepare_filter(self, filters: Filter):
        if filters.type != FilterOp.NonFilter:
            raise RuntimeError("Hyperspace client supports only non-filter cases")

    def search_embedding(self, query, k: int = 100, *args, **kwargs):
        client = self._c()
        # TODO(validate): confirm search() signature + RESULT SHAPE. The shipped
        # client returns a raw gRPC response; _extract_ids below tries the common
        # shapes — pin it to the real one once run against a live instance.
        resp = client.search(np.asarray(query, dtype=np.float32), top_k=k, collection=self.collection_name)
        return _extract_ids(resp, k)


def _extract_ids(resp, k: int) -> list[int]:
    """Pull integer ids out of a HyperspaceDB search response.

    TODO(validate): HyperspaceDB's search returns a gRPC message whose exact
    field names aren't confirmable statically. This tries the shapes a vector DB
    typically returns; replace it with the one real call once verified.
    """
    # 1) object with an `.ids` / `.results` attribute
    for attr in ("ids", "results", "matches", "hits"):
        val = getattr(resp, attr, None)
        if val is not None:
            resp = val
            break
    # 2) a dict
    if isinstance(resp, dict):
        for key in ("ids", "results", "matches", "hits"):
            if key in resp:
                resp = resp[key]
                break
    # 3) now `resp` should be an iterable of results; pull an id from each
    out: list[int] = []
    try:
        for r in resp:
            if isinstance(r, (int, np.integer)):
                out.append(int(r))
            elif isinstance(r, dict):
                out.append(int(r.get("id", r.get("_id"))))
            else:
                out.append(int(getattr(r, "id", getattr(r, "_id"))))
            if len(out) >= k:
                break
    except TypeError:
        pass
    return out
