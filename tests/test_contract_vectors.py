"""The replay boundary's shared, versioned contract vectors."""

import json
from decimal import Decimal, InvalidOperation, localcontext
from pathlib import Path

import pytest

pytest.importorskip("google.protobuf")

from liquiditylab.protocol import (  # noqa: E402
    GO_ENV_ID,
    action_to_wire,
    decode_decimal,
    encode_decimal,
    seconds_to_ns,
)
from liquiditylab.replay import (  # noqa: E402
    OBSERVATION_FIELDS,
    OBSERVATION_SIZE,
    OBSERVATION_VERSION,
)
from liquiditylab.v1 import execution_pb2 as pb  # noqa: E402


def read_contract_vectors():
    vectors = json.loads(
        (Path(__file__).resolve().parents[1] / "conformance" / "contracts.json").read_text()
    )
    required = {
        "schema_version", "engine_version", "observation", "input_decimals",
        "output_decimals", "times", "legacy_float_times", "actions",
        "invalid_legacy_actions", "enums",
    }
    assert set(vectors) == required, "missing or unknown contract fixture family"
    assert vectors["schema_version"] == "phase1-contracts-v1"
    for family in ("input_decimals", "output_decimals", "times", "legacy_float_times", "actions"):
        cases = vectors[family]
        assert isinstance(cases, list) and cases, f"empty or invalid family: {family}"
        names = [case["name"] for case in cases]
        assert all(isinstance(name, str) and name for name in names)
        assert len(set(names)) == len(names), f"duplicate case name in {family}"
    assert {case["legacy"] for case in vectors["actions"]} == set(range(6))
    invalid_actions = vectors["invalid_legacy_actions"]
    assert isinstance(invalid_actions, list) and invalid_actions
    assert len(set(invalid_actions)) == len(invalid_actions)
    observation = vectors["observation"]
    assert observation["version"] and observation["field_names"]
    assert len(set(observation["field_names"])) == len(observation["field_names"])
    assert set(vectors["enums"]) == {"Action", "Side", "EventKind", "FeeCurrency"}
    for name, enum in vectors["enums"].items():
        assert enum["values"] and enum["rejected_wire"], f"empty enum family: {name}"
        assert len(set(enum["values"].values())) == len(enum["values"])
        assert len(set(enum["rejected_wire"])) == len(enum["rejected_wire"])
    return vectors


VECTORS = read_contract_vectors()


def test_contract_fixture_identity():
    assert VECTORS["schema_version"] == "phase1-contracts-v1"
    assert VECTORS["engine_version"] == GO_ENV_ID
    assert VECTORS["observation"]["version"] == OBSERVATION_VERSION
    assert VECTORS["observation"]["field_names"] == list(OBSERVATION_FIELDS)
    assert len(VECTORS["observation"]["field_names"]) == OBSERVATION_SIZE


@pytest.mark.parametrize("coefficient,scale", [("-0", 0), ("01", 0), ("10", 1)])
def test_noncanonical_output_regression(coefficient, scale):
    with pytest.raises(ValueError):
        decode_decimal(pb.Decimal(coefficient=coefficient, scale=scale))


@pytest.mark.parametrize("vector", VECTORS["input_decimals"], ids=lambda v: v["name"])
def test_input_decimal_vectors(vector):
    if vector["wire"] is None:
        # Python produces input money. Absence remains distinct on the generated
        # wire; the Go consumer tests required/optional admission for these cases.
        assert not pb.Amount().HasField("value")
        assert vector["valid"] is not vector["required"]
        return
    wire = pb.Decimal(**vector["wire"])
    if vector["valid"]:
        with localcontext() as context:
            context.prec = 2
            encoded = encode_decimal(vector["text"])
            assert encoded == wire
            assert encode_decimal(decode_decimal(wire)) == wire
    else:
        # The canonical output reader checks spelling; the input encoder checks
        # the narrower admission envelope. No malformed input can roundtrip.
        with pytest.raises((ValueError, InvalidOperation)):
            encode_decimal(decode_decimal(wire))


@pytest.mark.parametrize("vector", VECTORS["output_decimals"], ids=lambda v: v["name"])
def test_output_decimal_vectors(vector):
    wire = pb.Decimal(**vector["wire"])
    if vector["valid"]:
        with localcontext() as context:
            context.prec = 2
            assert decode_decimal(wire) == Decimal(vector["text"])
    else:
        with pytest.raises(ValueError):
            decode_decimal(wire)


@pytest.mark.parametrize("vector", VECTORS["times"], ids=lambda v: v["name"])
def test_time_vectors(vector):
    with localcontext() as context:
        context.prec = 2
        if vector["valid"]:
            assert seconds_to_ns(vector["seconds"]) == vector["nanoseconds"]
        else:
            with pytest.raises((ValueError, InvalidOperation)):
                seconds_to_ns(vector["seconds"])


@pytest.mark.parametrize("vector", VECTORS["legacy_float_times"])
def test_legacy_float_time_conversion(vector):
    assert seconds_to_ns(vector["value"]) == vector["nanoseconds"]


def test_action_mapping_vectors():
    for vector in VECTORS["actions"]:
        assert action_to_wire(vector["legacy"]) == vector["wire"]
        assert pb.Action.Name(vector["wire"]) == vector["name"]
    for action in VECTORS["invalid_legacy_actions"]:
        with pytest.raises(ValueError):
            action_to_wire(action)
    for action in (True, 1.0):
        with pytest.raises(ValueError):
            action_to_wire(action)


@pytest.mark.parametrize("name,vector", VECTORS["enums"].items())
def test_enum_vectors(name, vector):
    enum = getattr(pb, name)
    assert dict(enum.items()) == vector["values"]
    constructors = {
        "Action": (pb.EnvironmentAction, "action"),
        "Side": (pb.Plan, "side"),
        "EventKind": (pb.MarketEvent, "kind"),
        "FeeCurrency": (pb.FeeConfig, "currency"),
    }
    constructor, field = constructors[name]
    for integer in vector["rejected_wire"]:
        # Proto3 preserves future integer enum values. The Go RPC must reject
        # these values explicitly rather than relying on protobuf decoding.
        message = constructor(**{field: integer})
        decoded = constructor.FromString(message.SerializeToString())
        assert getattr(decoded, field) == integer
