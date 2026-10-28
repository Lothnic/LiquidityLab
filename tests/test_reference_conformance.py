"""Record intended v1 arithmetic/time differences without rewriting the oracle."""

import json
import runpy
from decimal import ROUND_UP, Context, localcontext
from pathlib import Path

import pytest

from liquiditylab.protocol import seconds_to_ns

ROOT = Path(__file__).resolve().parents[1]
TIES = json.loads((ROOT / "conformance/time-compatibility.json").read_text())


@pytest.mark.parametrize("case", TIES["cases"], ids=lambda value: value["name"])
def test_legacy_float_timer_compatibility(case):
    assert TIES["schema"] == "legacy-float-timer-compatibility-v1"
    assert TIES["v1_equal_time_order"] == ["receipt", "arrival", "cancel"]
    start, latency, receipt = (case[name] for name in
                               ("start_seconds", "latency_seconds", "receipt_seconds"))
    timer, frame = float(start) + float(latency), float(receipt)
    relation = "before" if timer < frame else "after" if timer > frame else "equal"
    assert relation == case["legacy_arrival_relation"]
    assert seconds_to_ns(start) + seconds_to_ns(latency) == seconds_to_ns(receipt)
    assert case["v1_arrival_relation"] == "equal"


def test_reference_vectors_ignore_ambient_decimal_context():
    module = runpy.run_path(str(ROOT / "conformance/regenerate.py"))
    expected = json.loads((ROOT / "conformance/golden.json").read_text())
    with localcontext(Context(prec=2, rounding=ROUND_UP)):
        assert module["build_vectors"]() == expected
