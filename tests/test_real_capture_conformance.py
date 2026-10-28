"""Retained real public data agrees with the versioned Go normalizer golden."""

import hashlib
import json
from decimal import Decimal
from pathlib import Path

from liquiditylab.data.kraken import normalize_recording


def test_verified_real_public_capture_matches_go_normalization():
    directory = Path(__file__).resolve().parents[1] / "examples/recordings/kraken-btcusd-20261028"
    raw = directory / "public-capture.jsonl"
    manifest = json.loads((directory / "public-capture.manifest.json").read_text())
    assert hashlib.sha256(raw.read_bytes()).hexdigest() == manifest["sha256"]
    assert raw.stat().st_size == manifest["bytes"]
    python_events = normalize_recording(raw)
    go_events = [json.loads(line) for line in
                 (directory / "normalized-events.jsonl").read_text().splitlines()]
    assert len(python_events) == len(go_events) == 45
    fields = (
        "kind", "type", "receipt_index", "source_receipt_index", "symbol", "valid",
        "checksum", "exchange_timestamp", "price", "quantity", "side", "trade_id",
    )
    for python_event, go_event in zip(python_events, go_events, strict=True):
        for field in fields:
            assert python_event.get(field) == go_event.get(field), field
        for side in ("bids", "asks"):
            python_levels = {Decimal(p): (p, q) for p, q in python_event.get(side, [])}
            go_levels = {Decimal(p): (p, q) for p, q in go_event.get(side, [])}
            assert python_levels == go_levels
        # The reference API retains float seconds; Go's v1 source uses exact ns.
        assert abs(round(python_event["time"] * 1_000_000_000) - go_event["time_ns"]) <= 1
