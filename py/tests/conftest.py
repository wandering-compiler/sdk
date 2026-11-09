import os
from pathlib import Path

import pytest

from w17_business.descriptors import default_pool

HERE = Path(__file__).resolve().parent
CONFORMANCE = HERE / "conformance"
DESCRIPTORS = CONFORMANCE / "testdata" / "descriptors.binpb"
MANIFEST = CONFORMANCE / "service" / "w17-business.json"


@pytest.fixture(scope="session")
def pool():
    p = default_pool()
    p.add_set_file(DESCRIPTORS)
    return p


@pytest.fixture
def msg(pool):
    def get(name):
        return pool.message("conformance.v1." + name)

    return get


@pytest.fixture
def chdir_tmp(tmp_path):
    old = os.getcwd()
    os.chdir(tmp_path)
    try:
        yield tmp_path
    finally:
        os.chdir(old)
