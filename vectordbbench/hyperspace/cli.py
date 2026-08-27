# SPDX-License-Identifier: Apache-2.0
#
# `vectordbbench hyperspacehnsw --endpoint localhost:50051 --ef-construction 200 ...`
#
# Fair Euclidean-HNSW comparison for HyperspaceDB (see config.py / hyperspace.py).
from typing import Annotated, Unpack

import click
from pydantic import SecretStr

from vectordb_bench.backend.clients import DB
from vectordb_bench.cli.cli import (
    CommonTypedDict,
    cli,
    click_parameter_decorators_from_typed_dict,
    run,
)

DBTYPE = DB.Hyperspace


class HyperspaceTypedDict(CommonTypedDict):
    endpoint: Annotated[
        str,
        click.option("--endpoint", type=str, default="localhost:50051", help="HyperspaceDB gRPC endpoint"),
    ]
    ef_construction: Annotated[
        int,
        click.option("--ef-construction", type=int, default=200, help="HNSW efConstruction"),
    ]
    ef_search: Annotated[
        int,
        click.option(
            "--ef-search",
            type=int,
            default=64,
            help="HNSW efSearch (applied collection-wide via configure(); rebuild to change)",
        ),
    ]


@cli.command()
@click_parameter_decorators_from_typed_dict(HyperspaceTypedDict)
def HyperspaceHNSW(**parameters: Unpack[HyperspaceTypedDict]):
    from .config import HyperspaceConfig, HyperspaceHNSWConfig

    run(
        db=DBTYPE,
        db_config=HyperspaceConfig(endpoint=SecretStr(parameters["endpoint"])),
        db_case_config=HyperspaceHNSWConfig(
            ef_construction=parameters["ef_construction"],
            ef_search=parameters["ef_search"],
        ),
        **parameters,
    )
