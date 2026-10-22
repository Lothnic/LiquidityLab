"""Read-only Kraken L2 recording with exact wire decimals and CRC validation.

There are deliberately no authenticated clients or order submission methods here.
"""

from __future__ import annotations

import asyncio
import hashlib
import json
import math
import time
import uuid
import zlib
from datetime import UTC, datetime
from decimal import Decimal, InvalidOperation
from pathlib import Path
from typing import Any

PUBLIC_URL = "wss://ws.kraken.com/v2"
SCHEMA = "liquiditylab.kraken.raw.v1"


class BookError(ValueError):
    """The observed book cannot be treated as synchronized."""


def _number(value: Any) -> str:
    """Keep the wire's decimal precision, including trailing zeros."""
    if isinstance(value, float):
        raise BookError("Decode wire floats with parse_float=Decimal")
    try:
        number = Decimal(str(value))
    except InvalidOperation as exc:
        raise BookError("Invalid book number") from exc
    if not number.is_finite():
        raise BookError("Non-finite book number")
    return format(number, "f")


class KrakenBook:
    """A bounded L2 book; checksum errors invalidate the entire state."""

    def __init__(self, depth: int = 10):
        if depth not in (10, 25, 100, 500, 1000):
            raise ValueError("Kraken depth must be 10, 25, 100, 500, or 1000")
        self.depth = depth
        self.bids: dict[Decimal, tuple[str, str]] = {}
        self.asks: dict[Decimal, tuple[str, str]] = {}
        self.valid = False

    def levels(self, side: str) -> list[list[str]]:
        book = self.bids if side == "bids" else self.asks
        return [list(book[p]) for p in sorted(book, reverse=side == "bids")]

    def checksum(self) -> int:
        pieces = []
        for side in ("asks", "bids"):
            for price, quantity in self.levels(side)[:10]:
                pieces.extend(v.replace(".", "").lstrip("0") for v in (price, quantity))
        return zlib.crc32("".join(pieces).encode("ascii")) & 0xFFFFFFFF

    def apply(self, payload: dict[str, Any], *, snapshot: bool) -> None:
        if not snapshot and not self.valid:
            raise BookError("Cannot apply updates before a valid snapshot")
        if snapshot:
            self.bids.clear()
            self.asks.clear()
        self.valid = False
        for side in ("bids", "asks"):
            book = self.bids if side == "bids" else self.asks
            for level in payload.get(side, []):
                price, quantity = _number(level["price"]), _number(level["qty"])
                p, q = Decimal(price), Decimal(quantity)
                if p <= 0 or q < 0:
                    raise BookError("Book price must be positive and quantity nonnegative")
                if q == 0:
                    book.pop(p, None)
                else:
                    book[p] = (price, quantity)
            keep = set(sorted(book, reverse=side == "bids")[: self.depth])
            for p in list(book):
                if p not in keep:
                    del book[p]
        if not self.bids or not self.asks or max(self.bids) >= min(self.asks):
            raise BookError("Book is empty or crossed")
        if "checksum" not in payload or self.checksum() != int(payload["checksum"]):
            raise BookError("Kraken L2 checksum mismatch or missing checksum")
        self.valid = True


async def capture(symbol: str, output: Path, duration: float, depth: int = 10) -> dict:
    """Record public book/trade messages for a bounded duration into JSONL.

    Missing or corrupt state is recorded as a gap and requires a new snapshot.
    The raw payload stays a string so JSON serialization cannot change decimals.
    """
    from websockets.asyncio.client import connect

    if not math.isfinite(duration) or duration <= 0:
        raise ValueError("duration must be finite and positive")
    if not symbol or "/" not in symbol:
        raise ValueError("symbol must be a Kraken v2 pair such as BTC/USD")
    KrakenBook(depth)  # Validate before opening a file or a connection.
    output = Path(output)
    output.parent.mkdir(parents=True, exist_ok=True)
    started = time.monotonic()
    session = str(uuid.uuid4())
    stats = {"schema": SCHEMA, "symbol": symbol, "depth": depth, "session_id": session,
             "messages": 0, "book_messages": 0, "gaps": 0, "connections": 0,
             "requested_duration_seconds": duration, "output": str(output)}
    receipt_index = 0

    with output.open("x", encoding="utf-8") as stream:
        def record(**fields: Any) -> None:
            nonlocal receipt_index
            row = {"schema": SCHEMA, "session_id": session, "depth": depth,
                   "receipt_index": receipt_index, "elapsed": time.monotonic() - started,
                   "received_at": datetime.now(UTC).isoformat(), **fields}
            stream.write(json.dumps(row, separators=(",", ":")) + "\n")
            stream.flush()
            receipt_index += 1

        while time.monotonic() - started < duration:
            book = KrakenBook(depth)
            remaining = duration - (time.monotonic() - started)
            try:
                async with connect(PUBLIC_URL, open_timeout=min(10, remaining),
                                   ping_interval=20, ping_timeout=20, max_size=4_000_000) as ws:
                    stats["connections"] += 1
                    for channel in ("book", "trade"):
                        params = {"channel": channel, "symbol": [symbol], "snapshot": True}
                        if channel == "book":
                            params["depth"] = depth
                        await ws.send(json.dumps({"method": "subscribe", "params": params}))
                    while (remaining := duration - (time.monotonic() - started)) > 0:
                        try:
                            raw = await asyncio.wait_for(ws.recv(), timeout=remaining)
                        except TimeoutError:
                            break
                        if isinstance(raw, bytes):
                            raw = raw.decode("utf-8")
                        message = json.loads(raw, parse_float=Decimal)
                        if message.get("method") == "subscribe" and message.get("success") is False:
                            record(raw=raw, valid=False)
                            raise BookError(f"Subscription rejected: {message.get('error')}")
                        if message.get("channel") == "book":
                            try:
                                for item in message.get("data", []):
                                    if item.get("symbol") == symbol:
                                        book.apply(item, snapshot=message.get("type") == "snapshot")
                                        stats["book_messages"] += 1
                            except BookError:
                                record(raw=raw, valid=False)
                                stats["messages"] += 1
                                raise
                        record(raw=raw, valid=book.valid)
                        stats["messages"] += 1
            except asyncio.CancelledError:
                record(kind="gap", reason="capture_cancelled", valid=False)
                raise
            except (OSError, TimeoutError, ValueError, RuntimeError) as exc:
                record(kind="gap", reason=f"{type(exc).__name__}: {exc}", valid=False)
                stats["gaps"] += 1
            except Exception as exc:
                # WebSocket closure classes vary by installed library version.
                from websockets.exceptions import WebSocketException
                if not isinstance(exc, WebSocketException):
                    raise
                record(kind="gap", reason=f"{type(exc).__name__}: {exc}", valid=False)
                stats["gaps"] += 1
            remaining = duration - (time.monotonic() - started)
            if remaining > 0:
                record(kind="gap", reason="connection_ended_resnapshot_required", valid=False)
                stats["gaps"] += 1
                await asyncio.sleep(min(1, remaining))
        record(kind="capture_end", valid=book.valid if stats["connections"] else False)

    stats["duration_seconds"] = time.monotonic() - started
    stats["sha256"] = hashlib.sha256(output.read_bytes()).hexdigest()
    stats["status"] = "recorded" if stats["book_messages"] else "no_valid_book"
    output.with_suffix(output.suffix + ".manifest.json").write_text(
        json.dumps(stats, indent=2) + "\n", encoding="utf-8")
    return stats


def normalize_recording(path: Path) -> list[dict]:
    """Validate raw capture and return causal replay events in receipt order."""
    events: list[dict] = []
    book: KrakenBook | None = None
    origin: float | None = None
    previous_elapsed = -1.0
    symbol: str | None = None

    def emit(kind: str, elapsed: float, row: dict, **payload: Any) -> None:
        if origin is None:
            return
        timestamp = elapsed - origin
        events.append({"kind": kind, "time": timestamp, "type": kind, "ts": timestamp,
                       "receipt_index": len(events),
                       "source_receipt_index": row.get("receipt_index"),
                       "symbol": symbol, "valid": kind != "gap", **payload})

    with Path(path).open(encoding="utf-8") as stream:
        for line_number, line in enumerate(stream, start=1):
            if not line.strip():
                continue
            row = json.loads(line)
            elapsed = float(row["elapsed"])
            if not math.isfinite(elapsed) or elapsed < previous_elapsed:
                raise ValueError(f"Noncausal receipt time at line {line_number}")
            previous_elapsed = elapsed
            if row.get("kind") == "gap":
                if book:
                    book.valid = False
                emit("gap", elapsed, row, reason=row.get("reason", "capture_gap"))
                continue
            if "raw" not in row:
                continue
            message = json.loads(row["raw"], parse_float=Decimal)
            channel = message.get("channel")
            if channel == "book":
                for item in message.get("data", []):
                    snapshot = message.get("type") == "snapshot"
                    if symbol is None:
                        symbol = item["symbol"]
                    if item["symbol"] != symbol:
                        raise ValueError("A replay recording must contain one symbol")
                    if book is None:
                        book = KrakenBook(int(row.get("depth", 10)))
                    previous = {side: dict(getattr(book, side)) for side in ("bids", "asks")}
                    try:
                        book.apply(item, snapshot=snapshot)
                    except BookError as exc:
                        emit("gap", elapsed, row, reason=str(exc))
                        continue
                    if origin is None:
                        origin = elapsed
                    levels: dict[str, list[list[str]]] = {}
                    for side in ("bids", "asks"):
                        if snapshot:
                            levels[side] = book.levels(side)
                        else:
                            current = getattr(book, side)
                            changed = [list(value) for price, value in current.items()
                                       if previous[side].get(price) != value]
                            removed = [[value[0], "0"] for price, value in previous[side].items()
                                       if price not in current]
                            levels[side] = changed + removed
                    emit("snapshot" if snapshot else "book", elapsed, row,
                         bids=levels["bids"], asks=levels["asks"],
                         checksum=int(item["checksum"]), exchange_timestamp=item.get("timestamp"))
            elif channel == "trade" and book is not None and book.valid:
                # Subscription snapshots contain historical trades, not fresh flow.
                if message.get("type") == "snapshot":
                    continue
                for item in message.get("data", []):
                    if item.get("symbol") != symbol:
                        continue
                    price, quantity = _number(item["price"]), _number(item["qty"])
                    if Decimal(price) <= 0 or Decimal(quantity) <= 0 or item["side"] not in ("buy", "sell"):
                        raise ValueError("Invalid trade event")
                    emit("trade", elapsed, row, price=price, quantity=quantity,
                         side=item["side"], trade_id=str(item["trade_id"]),
                         exchange_timestamp=item.get("timestamp"))
    if not events or origin is None:
        raise ValueError("Recording contains no valid checksum-verified book snapshot")
    return events
