"""Checks for source precision, checksum recovery, and causal normalization."""

import json
import zlib
from decimal import Decimal

import pytest

from liquiditylab.data.kraken import BookError, KrakenBook, normalize_recording


def fixture_snapshot():
    # Kraken's published checksum example; numeric facts preserve wire precision.
    bids = [
        ("45283.5", "0.10000000"), ("45283.4", "1.54582015"),
        ("45282.1", "0.10000000"), ("45281.0", "0.10000000"),
        ("45280.3", "1.54592586"), ("45279.0", "0.07990000"),
        ("45277.6", "0.03310103"), ("45277.5", "0.30000000"),
        ("45277.3", "1.54602737"), ("45276.6", "0.15445238"),
    ]
    asks = [
        ("45285.2", "0.00100000"), ("45286.4", "1.54571953"),
        ("45286.6", "1.54571109"), ("45289.6", "1.54560911"),
        ("45290.2", "0.15890660"), ("45291.8", "1.54553491"),
        ("45294.7", "0.04454749"), ("45296.1", "0.35380000"),
        ("45297.5", "0.09945542"), ("45299.5", "0.18772827"),
    ]
    return {"symbol": "BTC/USD", "bids": [{"price": p, "qty": q} for p, q in bids],
            "asks": [{"price": p, "qty": q} for p, q in asks], "checksum": 3310070434}


def simple_snapshot():
    # Independent checksum input: asks then bids, decimal points/leading zeros removed.
    checksum = zlib.crc32(b"1020250010003000") & 0xFFFFFFFF
    return {"symbol": "BTC/USD", "bids": [{"price": "100.0", "qty": "0.3000"}],
            "asks": [{"price": "102.0", "qty": "0.2500"}], "checksum": checksum}


def write_rows(path, rows):
    path.write_text("".join(json.dumps(row) + "\n" for row in rows))
    return path


def envelope(payload, elapsed=2, index=0, message_type="snapshot", channel="book"):
    return {"elapsed": elapsed, "receipt_index": index, "depth": 10,
            "raw": json.dumps({"channel": channel, "type": message_type, "data": [payload]})}


def test_published_checksum_preserves_trailing_decimal_precision():
    book = KrakenBook()
    book.apply(fixture_snapshot(), snapshot=True)
    assert book.valid
    assert book.checksum() == 3310070434
    assert book.bids[Decimal("45283.5")] == ("45283.5", "0.10000000")


def test_corrupt_book_requires_a_new_snapshot():
    book = KrakenBook()
    book.apply(fixture_snapshot(), snapshot=True)
    with pytest.raises(BookError, match="checksum"):
        book.apply({"bids": [], "asks": [], "checksum": 0}, snapshot=False)
    assert not book.valid
    with pytest.raises(BookError, match="snapshot"):
        book.apply(fixture_snapshot(), snapshot=False)
    book.apply(fixture_snapshot(), snapshot=True)
    assert book.valid


def test_wire_binary_floats_are_rejected():
    payload = simple_snapshot()
    payload["bids"][0]["price"] = 100.0
    with pytest.raises(BookError, match="parse_float"):
        KrakenBook().apply(payload, snapshot=True)


def test_normalizer_drops_historical_trade_snapshot_and_preserves_receipt_order(tmp_path):
    trade = {"symbol": "BTC/USD", "price": "101.0", "qty": "0.0200",
             "side": "buy", "trade_id": 7, "timestamp": "2026-10-23T00:00:00Z"}
    rows = [envelope(simple_snapshot(), 10),
            envelope(trade, 11, 1, "snapshot", "trade"),
            envelope(trade, 12, 2, "update", "trade")]
    events = normalize_recording(write_rows(tmp_path / "raw.jsonl", rows))
    assert [e["kind"] for e in events] == ["snapshot", "trade"]
    assert [e["time"] for e in events] == [0, 2]
    assert events[1]["quantity"] == "0.0200"
    assert events[1]["source_receipt_index"] == 2


def test_gap_prevents_trades_until_resnapshot(tmp_path):
    trade = {"symbol": "BTC/USD", "price": "101.0", "qty": "0.02",
             "side": "sell", "trade_id": 8}
    rows = [envelope(simple_snapshot(), 10),
            {"elapsed": 11, "kind": "gap", "reason": "disconnect"},
            envelope(trade, 12, 2, "update", "trade"),
            envelope(simple_snapshot(), 13, 3),
            envelope(trade, 14, 4, "update", "trade")]
    events = normalize_recording(write_rows(tmp_path / "raw.jsonl", rows))
    assert [e["kind"] for e in events] == ["snapshot", "gap", "snapshot", "trade"]
    assert not events[1]["valid"]


def test_receipt_time_regression_cannot_be_sorted_away(tmp_path):
    rows = [envelope(simple_snapshot(), 10), envelope(simple_snapshot(), 9, 1)]
    with pytest.raises(ValueError, match="Noncausal"):
        normalize_recording(write_rows(tmp_path / "raw.jsonl", rows))


def test_depth_truncation_emits_deletions_for_normalized_book(tmp_path):
    initial = fixture_snapshot()
    book = KrakenBook()
    book.apply(initial, snapshot=True)
    # New best bid pushes the tenth bid out without a venue zero-quantity message.
    expected = KrakenBook()
    expected.bids = dict(book.bids)
    expected.asks = dict(book.asks)
    expected.bids.pop(Decimal("45276.6"))
    expected.bids[Decimal("45284.0")] = ("45284.0", "0.10000000")
    update = {"symbol": "BTC/USD", "bids": [{"price": "45284.0", "qty": "0.10000000"}],
              "asks": [], "checksum": expected.checksum()}
    events = normalize_recording(write_rows(tmp_path / "raw.jsonl", [
        envelope(initial, 10), envelope(update, 11, 1, "update")]))
    assert ["45276.6", "0"] in events[1]["bids"]
    assert ["45284.0", "0.10000000"] in events[1]["bids"]
