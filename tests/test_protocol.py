from decimal import Decimal, localcontext

import pytest

pytest.importorskip("google.protobuf")

from liquiditylab.contracts import ReplayConfig  # noqa: E402
from liquiditylab.protocol import (  # noqa: E402
    action_to_wire,
    decode_decimal,
    encode_decimal,
    episode_spec,
    seconds_to_ns,
)
from liquiditylab.replay import synthetic_events  # noqa: E402


def test_decimal_encoding_is_exact_independent_of_context():
    value = Decimal("123456789012345678901234567890.123456")
    with localcontext() as context:
        context.prec = 3
        encoded = encode_decimal(value)
        assert encoded.coefficient == "123456789012345678901234567890123456"
        assert decode_decimal(encoded) == value
    assert encode_decimal("0.1000").coefficient == "1"
    assert encode_decimal("0.1000").scale == 1
    assert encode_decimal("-0.00").coefficient == "0"
    assert encode_decimal("-0.00").scale == 0


@pytest.mark.parametrize("value", [0.1, True, "NaN", "Infinity", "1e-37", "1e129"])
def test_unsupported_money_rejected(value):
    with pytest.raises((ValueError, TypeError)):
        encode_decimal(value)


def test_nanosecond_import_and_action_mapping():
    assert seconds_to_ns("0.0000000005") == 0
    assert seconds_to_ns("0.0000000015") == 2
    assert seconds_to_ns("9223372036.854775807") == 2**63 - 1
    for value in ("-1", "NaN", "9223372036.854775808"):
        with pytest.raises(ValueError):
            seconds_to_ns(value)
    assert [action_to_wire(i) for i in range(6)] == list(range(1, 7))
    for value in (-1, 6, True, 1.0):
        with pytest.raises(ValueError):
            action_to_wire(value)


def test_explicit_zero_config_and_default_validity():
    spec = episode_spec(synthetic_events(), config=ReplayConfig(
        submission_latency=0, cancel_latency=0, lambda_miss=0))
    assert spec.config.HasField("submission_latency_ns")
    assert spec.config.submission_latency_ns == 0
    assert spec.config.HasField("lambda_miss")
    assert spec.events[1].HasField("valid") and spec.events[1].valid
    assert spec.events[0].bids[0].price.coefficient == "9999"
    assert spec.events[0].bids[0].price.scale == 2
