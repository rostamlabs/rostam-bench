# SPDX-License-Identifier: Apache-2.0
#
# VectorDBBench config for HyperspaceDB (github.com/YARlabs/hyperspace-db),
# driven through its `hyperspacedb` Python SDK. Connection (DBConfig) + the
# per-case HNSW index/search parameters (DBCaseConfig).
#
# FAIRNESS: this benchmarks HyperspaceDB in its *Euclidean HNSW* mode with the
# same HNSW knobs VectorDBBench sweeps on every other engine, read at matched
# recall. It deliberately does NOT use HyperspaceDB's hyperbolic (Poincaré /
# Lorentz) modes — those assume hierarchical data and are not an apples-to-apples
# comparison against Euclidean engines.
from pydantic import BaseModel, SecretStr

from ..api import DBCaseConfig, DBConfig, MetricType


class HyperspaceConfig(DBConfig):
    """Connection to a running HyperspaceDB gRPC server.

    The SDK connects to a host:port endpoint (default gRPC port 50051):
        client = HyperspaceClient("localhost:50051", api_key=...)
    """

    endpoint: SecretStr = SecretStr("localhost:50051")
    api_key: SecretStr | None = None

    def to_dict(self) -> dict:
        return {
            "endpoint": self.endpoint.get_secret_value(),
            "api_key": self.api_key.get_secret_value() if self.api_key else None,
        }


class HyperspaceHNSWConfig(BaseModel, DBCaseConfig):
    """HNSW index + search parameters, matched to the other engines' knobs.

    NOTE (fairness caveat): HyperspaceDB's SDK exposes ef_construction and
    ef_search (via ``configure()``) but does NOT expose the HNSW ``M`` parameter,
    so ``m`` is accepted for interface parity but not applied — HyperspaceDB uses
    its internal default M. ef_construction / ef_search ARE matched to the swept
    values, which are the knobs that dominate the recall/QPS tradeoff.
    """

    metric_type: MetricType | None = None
    m: int = 16  # accepted for parity; not applied (SDK exposes no M)
    ef_construction: int = 200
    ef_search: int = 64

    def parse_metric(self) -> str:
        if self.metric_type == MetricType.L2:
            return "l2"
        # HyperspaceDB uses "cosine"; IP is not a distinct metric in its SDK.
        return "cosine"

    def index_param(self) -> dict:
        return {
            "metric": self.parse_metric(),
            "ef_construction": self.ef_construction,
            "ef_search": self.ef_search,
        }

    def search_param(self) -> dict:
        # ef_search is applied collection-wide via configure() at build time,
        # not per query, so nothing is returned here.
        return {}
