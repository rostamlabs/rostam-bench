# SPDX-License-Identifier: Apache-2.0
# Copyright 2026 RostamLabs
#
# VectorDBBench config for the Rostam client: connection (DBConfig) and the
# per-case HNSW index/search parameters (DBCaseConfig).
from pydantic import BaseModel, SecretStr

from ..api import DBCaseConfig, DBConfig, MetricType


class RostamConfig(DBConfig):
    """Connection to a running rostam-server's HTTP/JSON API.

    Start one with:  rostam-server -http 127.0.0.1:8080 -data ""
    """

    url: SecretStr = SecretStr("http://127.0.0.1:8080")
    api_key: SecretStr | None = None
    # How insert_embeddings writes.
    #   "bulk"    (default) stage ids + vectors for the concurrent multi-core
    #             build in optimize() — the fast initial-load path. Carries no
    #             payload, so filter cases are refused rather than mis-measured.
    #   "payload" stage ids + vectors + a filterable scalar per point on the same
    #             route, built the same way. What filter cases should use.
    #   "batch"   upsert through /points/batch, indexed inline by a single
    #             writer. Also filter-capable, and much slower; kept as the
    #             control that "payload" is measured against.
    load_path: str = "bulk"
    # Ship vectors as raw f32 over Rostam's binary bulk wire instead of as JSON
    # text. On by default; the client falls back to JSON on its own against a
    # server that does not understand the framing, so this only exists to force
    # the JSON path (e.g. to A/B the wire itself).
    binary_wire: bool = True
    # The same switch for the QUERY wire ("RVQ1"), separate from the bulk one
    # because a server can speak one framing and not the other — the bulk wire
    # shipped first. Setting this False forces base-10 text queries, which is
    # what the wire's A/B needs and the only reason it is exposed.
    binary_query: bool = True

    def to_dict(self) -> dict:
        return {
            "url": self.url.get_secret_value(),
            "api_key": self.api_key.get_secret_value() if self.api_key else None,
            "load_path": self.load_path,
            "binary_wire": self.binary_wire,
            "binary_query": self.binary_query,
        }


class RostamHNSWConfig(BaseModel, DBCaseConfig):
    """HNSW index + search parameters, matched to the other engines' knobs.

    Note: Rostam's ``ef_search`` is a *collection-level* setting applied at
    creation time — the search API has no per-query ef field — so it lives in
    ``index_param()`` and ``search_param()`` is empty. To sweep ef you re-run the
    case with ``drop_old=True`` (rebuild). A per-query ef would need a small
    engine-side addition to the search request; tracked as a future enhancement.
    """

    metric_type: MetricType | None = None
    m: int = 16
    ef_construction: int = 200
    ef_search: int = 64
    # Optional Rostam quantization: "" (float32) | "sq8" | "bq1" | "pq" | "sq" | "prq"
    quant: str = ""

    def parse_metric(self) -> str:
        if self.metric_type == MetricType.L2:
            return "l2"
        if self.metric_type == MetricType.IP:
            return "dot"
        return "cosine"

    def index_param(self) -> dict:
        return {
            "metric": self.parse_metric(),
            "m": self.m,
            "ef_construction": self.ef_construction,
            "ef_search": self.ef_search,
            "quant": self.quant,
        }

    def search_param(self) -> dict:
        # ef is baked into the collection (see class docstring); nothing per-query.
        return {}
