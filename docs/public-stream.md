# Incremental public event stream

`internal/marketdata` exposes the existing Kraken normalization rules as a
bounded serial component:

```go
normalizer := marketdata.NewNormalizer()
events, err := normalizer.Accept(receipt)
```

`Accept(Receipt) ([]Event, error)` retains one checksum book, its pinned depth,
receipt/session counters, symbol, first verified snapshot time, and last receipt
time. It retains no recording or event history and starts no goroutines.
Call it serially in source receipt order. Each returned slice belongs to that
receipt; the caller decides whether to keep it. Raw frames are limited to
4,000,000 bytes, session IDs to 256 bytes, and gap reasons to 4,096 bytes. The
persistent book retains only the configured Kraken subscription depth.

`Normalize(path)` now scans the raw JSONL file through this same normalizer.
Whole-file normalization still returns a collected event slice and rejects a
recording with no checksum-verified snapshot. A running capture never reopens
or rereads its growing output to produce events.

Receipt time remains causal integer nanoseconds relative to the first verified
book snapshot. Source decimal text, exponent rendering, level diffs/deletions,
subscription-depth truncation, CRC validation, source timestamps, gap behavior,
and historical trade-snapshot filtering use the existing normalization rules.
Before the first valid snapshot, no event is emitted. Missing/wrong schema,
noncontiguous receipt index, changed/missing session, and changed/invalid depth
invalidate the book and emit a gap once an origin exists. Bad CRC invalidates
the chain; trade flow stays suppressed until a verified fresh snapshot.

Malformed raw JSON, noncausal time, invalid numeric input and invalid trade
payloads return an error and invalidate the book. Events from that failed
receipt are discarded. A streaming consumer must stop or deliver an explicit
gap and obtain a fresh snapshot before resuming. File normalization continues
to reject the entire malformed recording. Existing missing-trade-ID validation
is preserved; this change does not manufacture venue execution IDs.

The additive `Event.ReceiptSessionID string`, JSON `receipt_session_id`, is the
pinned raw recording session identity. Existing Go `Event` readers and Python
normalized-event dictionary readers accept the additive field. Existing event
financial values and identities are unchanged:

- `ReceiptIndex` identifies each normalized event, with consecutive indices.
- `SourceReceiptIndex` identifies the raw receipt. Several trades in one raw
  frame share this source index but have separate normalized event indices.
- `ReceiptSessionID` scopes receipt-based identities across capture restarts.
- `TradeID` remains the supplied venue trade identity when present.

For paper actor ingress, preserve the session ID and normalized event index
when calling `Actor.MarketFromSession`; using the raw index for each trade in a
multi-trade frame would collide. A market snapshot revision is a different
identity from the capture session.

## Opt-in capture delivery

```go
cfg.OnEvent = func(ctx context.Context, event marketdata.Event) error {
    return consume(ctx, event)
}
manifest, err := marketdata.Capture(ctx, cfg)
```

`CaptureConfig.OnEvent func(context.Context, Event) error` is optional. Each raw
receipt is flushed and `fsync` completes before its normalized events are
forwarded through the incremental normalizer. Explicit recording gaps follow
the same order. Callbacks run synchronously in event order; they provide
backpressure without queues, per-event goroutines, repeated database polling,
or growing-file scans. No callback is configured implicitly. `Recorder` remains
a raw writer without authority or consumer side effects.

The callback context has the earlier caller/capture-duration deadline. A
consumer must honor it and may impose a shorter operation deadline. Capture
cannot forcibly interrupt a callback that ignores context; adding a goroutine
would leave an unbounded or unresolved consumer operation behind. The daemon
owns downstream readiness and recovery policy.

A callback or normalization error stops capture. It does not continue reading
as a valid source. Capture marks its final book invalid, writes and syncs an
`event_delivery_failed` terminal gap, attempts delivery of that gap once, and
writes an `incomplete` manifest plus an invalid `capture_end` marker. The
manifest uses a generic consumer-failure description; the returned error
preserves the original cause for `errors.Is`. A consumer must treat its own
error or the returned capture error as a stop signal even if it cannot accept
the best-effort terminal gap. Storage failure can prevent writing the gap or
manifest; returned errors and invalid in-memory status remain authoritative.

A raw receipt can contain several events. If a consumer accepts the first and
rejects a later event, the raw frame remains recorded completely while the
remaining callbacks stop. The terminal gap may therefore skip event indices
that were normalized from that frame but not delivered. Recovery must use
stable source identities and reconciliation; capture does not retry rejected
consumer effects or silently reconnect after consumer failure.

Transport reconnects, CRC failures and subscription failures retain their
existing recorded-gap behavior. Capture still has independent wall-time,
frame-count, disk-byte and read-frame limits. The public socket subscribes only
to unauthenticated market data; no private exchange credentials or order
transport are introduced here.

## Evidence

`go test -race ./internal/marketdata` uses synthetic frames and package-private
fake sockets. Checks cover streaming/file equivalence, frozen normalized
snapshot bytes (excluding the additive session field), multi-trade identities,
CRC invalidation/resnapshot, historical trade snapshots, numeric/frame limits,
source persistence before callbacks, callback failure and terminal manifest,
context expiry, and the unchanged callback-free recorder path. No real network
capture, venue availability, feed throughput, or operational capacity is claimed.
