"""The public CRC32 boundary never narrows or truncates malformed values."""

import copy
import json
from decimal import Decimal
from pathlib import Path

import pytest

from liquiditylab.data.kraken import BookError, KrakenBook

FIXTURE = json.loads((Path(__file__).resolve().parents[1] /
                      "conformance/kraken-checksums.json").read_text())


@pytest.mark.parametrize("case", FIXTURE["cases"], ids=lambda case: case["name"])
def test_shared_crc32_wire_cases(case):
    assert FIXTURE["schema"] == "kraken-crc32-v1"
    names = [item["name"] for item in FIXTURE["cases"]]
    assert names and all(names) and len(names) == len(set(names))
    payload = copy.deepcopy(FIXTURE["book"])
    payload["checksum"] = json.loads(case["checksum_json"], parse_float=Decimal)
    book = KrakenBook()
    if case["valid"]:
        book.apply(payload, snapshot=True)
        assert book.valid
    else:
        with pytest.raises(BookError, match="checksum"):
            book.apply(payload, snapshot=True)
        assert not book.valid
