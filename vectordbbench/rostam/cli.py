# SPDX-License-Identifier: Apache-2.0
# Copyright 2026 RostamLabs
#
# `vectordbbench rostamhnsw --url ... --m 16 --ef-construction 200 ...`
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

DBTYPE = DB.Rostam


class RostamTypedDict(CommonTypedDict):
    url: Annotated[
        str,
        click.option("--url", type=str, default="http://127.0.0.1:8080", help="Rostam HTTP API url"),
    ]
    m: Annotated[
        int,
        click.option("--m", type=int, default=16, help="HNSW index parameter m"),
    ]
    ef_construction: Annotated[
        int,
        click.option("--ef-construction", type=int, default=200, help="HNSW index parameter efConstruction"),
    ]
    ef_search: Annotated[
        int,
        click.option(
            "--ef-search",
            type=int,
            default=64,
            help="HNSW efSearch (collection-level in Rostam; rebuild to change)",
        ),
    ]
    quant: Annotated[
        str,
        click.option("--quant", type=str, default="", help="quantization: ''|sq8|bq1|pq|sq|prq"),
    ]


@cli.command()
@click_parameter_decorators_from_typed_dict(RostamTypedDict)
def RostamHNSW(**parameters: Unpack[RostamTypedDict]):
    from .config import RostamConfig, RostamHNSWConfig

    run(
        db=DBTYPE,
        db_config=RostamConfig(url=SecretStr(parameters["url"])),
        db_case_config=RostamHNSWConfig(
            m=parameters["m"],
            ef_construction=parameters["ef_construction"],
            ef_search=parameters["ef_search"],
            quant=parameters["quant"],
        ),
        **parameters,
    )
