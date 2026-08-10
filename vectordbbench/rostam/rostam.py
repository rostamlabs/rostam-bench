# SPDX-License-Identifier: Apache-2.0
# Copyright 2026 RostamLabs
#
# VectorDBBench client for Rostam, driving the engine's HTTP/JSON vector API:
#   create   POST /v1/collections
#   stage    POST /v1/collections/{c}/points/bulk        (insert_embeddings, load_path="bulk"/"payload")
#   build    POST /v1/collections/{c}/points/bulk/build  (optimize,          load_path="bulk"/"payload")
#   upsert   POST /v1/collections/{c}/points/batch       (insert_embeddings, load_path="batch")
#   search   POST /v1/collections/{c}/points/search      (search_embedding)
#
# Both insert routes are driven over Rostam's binary bulk wire ("RVB1", see
# _binary_body) when the server supports it, falling back to the JSON bodies
# above otherwise. Everything else stays JSON.
import logging
import struct
from contextlib import contextmanager

import numpy as np
import requests

from ...filter import Filter, FilterOp
from ..api import VectorDB

log = logging.getLogger(__name__)

INSERT_BATCH = 1000


class Rostam(VectorDB):
    # NumGE (int_field >= value) maps onto Rostam's filter tree; StrEqual would
    # need a string payload per point and is not wired yet.
    supported_filter_types: list[FilterOp] = [FilterOp.NonFilter, FilterOp.NumGE]

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
        self.base = db_config["url"].rstrip("/")
        self.api_key = db_config.get("api_key")
        # How the load is written. All three end in the same index; they differ
        # in what the wire carries and who does the indexing.
        #   "bulk"    (default) stage ids+vectors, build concurrently in optimize().
        #             No payload, so filter cases are refused.
        #   "payload" stage ids+vectors+payloads on the same route, build the same
        #             way. Filter-capable at bulk speed.
        #   "batch"   upsert each point through /points/batch, indexed inline by a
        #             single writer. Filter-capable, and the slow control that
        #             "payload" replaced — kept so the two can be A/B'd.
        self.load_path = db_config.get("load_path") or "bulk"
        if self.load_path not in ("bulk", "payload", "batch"):
            raise RuntimeError(f"unknown load_path {self.load_path!r}")
        self.query_filter: dict | None = None
        # Try the binary bulk wire first; insert_embeddings flips this off for
        # good if the server does not understand it.
        self._binary = db_config.get("binary_wire", True)
        # Set once a binary request has actually succeeded; after that a rejection
        # is a real error, not evidence of an old server.
        self._binary_proven = False

        s = self._new_session()
        idx = self.case_config.index_param()
        if drop_old:
            # DELETE is idempotent; ignore "not found".
            s.delete(self._url(self.collection_name), timeout=60)
        try:
            self._create(s, dim, idx)
        except _AlreadyExists:
            if drop_old:
                raise
            log.info("Rostam collection %s already exists; reusing", self.collection_name)
        s.close()
        self._session: requests.Session | None = None

    # ---- connection lifecycle (one session per worker process) ----
    def _new_session(self) -> requests.Session:
        s = requests.Session()
        if self.api_key:
            s.headers["Authorization"] = f"Bearer {self.api_key}"
        return s

    @contextmanager
    def init(self):
        self._session = self._new_session()
        try:
            yield
        finally:
            self._session.close()
            self._session = None

    def _sess(self) -> requests.Session:
        # Methods may be called inside or outside init(); ensure a session.
        return self._session if self._session is not None else self._new_session()

    def _url(self, *parts: str) -> str:
        return "/".join([self.base, "v1", "collections", *parts])

    # ---- schema ----
    def _create(self, s: requests.Session, dim: int, idx: dict):
        cfg = {
            "dim": dim,
            "metric": idx["metric"],
            "m": idx["m"],
            "ef_construction": idx["ef_construction"],
            "ef_search": idx["ef_search"],
        }
        if idx.get("quant"):
            cfg["quant"] = idx["quant"]
        r = s.post(self._url(), json={"name": self.collection_name, "config": cfg}, timeout=120)
        if r.status_code in (200, 201):
            return
        if r.status_code == 409 or "exist" in r.text.lower():
            raise _AlreadyExists()
        raise RuntimeError(f"create collection failed: {r.status_code} {r.text}")

    # ---- load (stage; build happens in optimize) ----
    def insert_embeddings(self, embeddings, metadata, labels_data=None, tenant_labels_data=None, **kwargs):
        metadata = _to_ints(metadata)
        s = self._sess()
        # "batch" is the only route that upserts inline; the other two stage.
        inline = self.load_path == "batch"
        payload_path = self.load_path in ("payload", "batch")
        if inline:
            url = self._url(self.collection_name, "points", "batch")
        else:
            url = self._url(self.collection_name, "points", "bulk")
        n = len(metadata)
        # The binary wire ships the vectors as raw f32 instead of base-10 text.
        # JSON encoding/decoding — not the index build — dominates load time on a
        # 768d corpus, so this is the difference between a load that is wire-bound
        # and one that is build-bound. Falls back permanently to JSON the first
        # time a server rejects the framing (pre-binary servers answer 400/404/415
        # and apply nothing, so re-sending the same chunk as JSON is safe).
        rows = _as_f32(embeddings)
        try:
            for start in range(0, n, INSERT_BATCH):
                end = min(start + INSERT_BATCH, n)
                ids = metadata[start:end]
                chunk = rows[start:end]
                if self._binary:
                    body = _binary_body(ids, chunk, upsert=inline, payloads=payload_path)
                    r = s.post(url, data=body, headers=_BINARY_HEADERS, timeout=600)
                    # Only a request made before ANY binary request has succeeded
                    # may fall back. Once one succeeds the server demonstrably
                    # speaks the framing, so a later 400 is a real error (a dim
                    # mismatch, an over-large chunk) and must surface instead of
                    # silently degrading the rest of the load to JSON.
                    if r.status_code in _BINARY_UNSUPPORTED and not self._binary_proven:
                        log.warning(
                            "Rostam server rejected the binary bulk wire (%s: %s); "
                            "falling back to JSON for the rest of the load",
                            r.status_code, r.text[:200],
                        )
                        self._binary = False
                    elif r.status_code != 200:
                        return start, RuntimeError(f"insert failed @{start}: {r.status_code} {r.text}")
                    else:
                        self._binary_proven = True
                        continue
                r = s.post(url, json=_json_body(ids, chunk, payload_path, inline), timeout=600)
                if r.status_code != 200:
                    return start, RuntimeError(f"insert failed @{start}: {r.status_code} {r.text}")
            return n, None
        except Exception as e:  # noqa: BLE001 — VDBBench expects (count, exc)
            return 0, e

    def optimize(self, data_size: int | None = None):
        # Build the HNSW index over everything staged so far. Blocks until built.
        # On the inline path every point was already indexed as it arrived, and
        # the build endpoint requires an empty index, so there is nothing to do.
        if self.load_path == "batch":
            log.info("Rostam load_path=batch: points indexed inline, no build step")
            return
        s = self._sess()
        url = self._url(self.collection_name, "points", "bulk", "build")
        r = s.post(url, json={}, timeout=24 * 3600)
        if r.status_code != 200:
            raise RuntimeError(f"bulk build failed: {r.status_code} {r.text}")

    # ---- query ----
    def prepare_filter(self, filters: Filter):
        if filters.type == FilterOp.NonFilter:
            self.query_filter = None
            return
        if filters.type == FilterOp.NumGE:
            if self.load_path == "bulk":
                # load_path="bulk" sends ids and vectors only, so there is no
                # payload to match on. Fail loud rather than silently timing an
                # unfiltered search and reporting it as a filtered one.
                raise RuntimeError(
                    "Rostam filter cases require load_path='payload' (or 'batch'); "
                    "load_path='bulk' stores no payload to filter on"
                )
            self.query_filter = {
                "op": "gte",
                "field": filters.int_field,
                "value": {"kind": "int", "int": int(filters.int_value)},
            }
            return
        raise RuntimeError(f"unsupported filter for Rostam: {filters}")

    def search_embedding(self, query, k: int = 100, *args, **kwargs):
        s = self._sess()
        url = self._url(self.collection_name, "points", "search")
        body = {"query": _to_list(query), "k": k}
        if self.query_filter is not None:
            body["filter"] = self.query_filter
        r = s.post(url, json=body, timeout=120)
        r.raise_for_status()
        return [int(h["id"]) for h in r.json().get("results", [])]


class _AlreadyExists(Exception):
    pass


# ---- binary bulk wire ("RVB1") ----
#
#   magic  b"RVB1"
#   flags  u32   bit0 payloads present, bit1 upsert
#   count  u32
#   dim    u32
#   rows   count x [ id u64 ][ dim x f32 ]
#   pays   count x [ len u32 ][ len bytes of JSON ]   (only when bit0)
#
# All big-endian, matching Rostam's internal op wire — the server reads the row
# region straight into the staging op with no per-float conversion, so the
# encoding cost here is one numpy byte-order cast rather than 768 float-to-text
# conversions per point.
_BULK_MAGIC = b"RVB1"
_BULK_FLAG_PAYLOADS = 1 << 0
_BULK_FLAG_UPSERT = 1 << 1
_BINARY_HEADERS = {"Content-Type": "application/octet-stream"}
# A server without the binary wire answers 400 (the JSON decoder chokes on the
# binary body), 415, or 404. All three apply nothing, so the chunk can be resent.
_BINARY_UNSUPPORTED = (400, 404, 415)


def _as_f32(embeddings):
    """Return embeddings as a contiguous (n, dim) float32 numpy array."""
    return np.ascontiguousarray(embeddings, dtype=np.float32)


def _binary_body(ids, rows, *, upsert: bool, payloads: bool) -> bytes:
    n, dim = rows.shape
    flags = 0
    if payloads:
        flags |= _BULK_FLAG_PAYLOADS
    if upsert:
        flags |= _BULK_FLAG_UPSERT
    out = [struct.pack(">4sIII", _BULK_MAGIC, flags, n, dim)]
    # One structured array lays out [id u64 BE][dim x f32 BE] per row exactly as
    # the wire wants it, so the whole chunk serializes in a single tobytes().
    rec = np.empty(n, dtype=np.dtype([("id", ">u8"), ("vec", ">f4", (dim,))]))
    rec["id"] = ids
    rec["vec"] = rows
    out.append(rec.tobytes())
    if payloads:
        for i in ids:
            # The id doubles as the filterable scalar: VDBBench's int filter cases
            # test `id >= value` against the same ids.
            blob = b'{"id":{"kind":"int","int":%d}}' % i
            out.append(struct.pack(">I", len(blob)))
            out.append(blob)
    return b"".join(out)


def _json_body(ids, rows, payloads: bool, upsert: bool) -> dict:
    """The original JSON body, used for pre-binary servers.

    ``upsert`` is a /points/batch field only — the staging route rejects it — so
    it is set independently of whether the points carry payloads.
    """
    vecs = rows.tolist()
    if payloads:
        points = [
            {"id": i, "vector": v, "metadata": {"id": {"kind": "int", "int": i}}}
            for i, v in zip(ids, vecs)
        ]
    else:
        points = [{"id": i, "vector": v} for i, v in zip(ids, vecs)]
    body = {"points": points}
    if upsert:
        body["upsert"] = True
    return body


def _to_list(row):
    if hasattr(row, "tolist"):  # numpy 1D
        return row.tolist()
    return list(row)


def _to_ints(xs):
    if hasattr(xs, "tolist"):
        xs = xs.tolist()
    return [int(x) for x in xs]
