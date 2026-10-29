# Cross-runtime conformance vectors

`golden.json` contains deterministic outputs from the Python v0 reference. Run
`uv run python conformance/regenerate.py` from the `liquiditylab` directory to
recreate it after intentionally changing the reference semantics.

Market event times and report timestamps use integer nanoseconds. All monetary,
price, and quantity values use base-10 strings. `expected_report` preserves the
Python report fields and normalizes time-bearing fields to `*_ns`; a `null`
terminal objective means the episode has no valid economic score.

Cases include a fully executed buy, a known partial sell, a cancel/fill race
with a frozen residual midpoint and a later feed gap, and a feed gap that
invalidates an active episode. The Python engine's six action integers are
listed explicitly in the fixture metadata.

A fifth case adds replacement finality: a minimum-resting-age refusal, a
cancel request, a partial fill while cancellation is pending, a refused
replacement before acknowledgement, and a replacement capped to the remaining
parent quantity after known finality. `internal/replay/replacement_test.go`
also checks each intermediate state against hand-calculated values; both
languages' shared golden checks cover its final economics and observations.
The original four cases remain unchanged.


`contracts.json` is the hand-maintained `phase1-contracts-v1` boundary fixture.
Both `internal/replayrpc/contract_vectors_test.go` and
`tests/test_contract_vectors.py` read it directly. It freezes the existing
`LiquidityLab-SpotExecution-v1` environment, `compact-v1` observation version and
ordered fields, enum names/numbers, and policy action 0..5 to wire action 1..6.
Unspecified and future integer enum values survive protobuf decoding and must
be rejected by RPC admission.

Decimal vectors distinguish canonical input money (128 coefficient digits,
scale 36) from derived output money (512 digits, scale 144). Go validates inputs
and checks its output encoder against internal derived values. Python checks its
input encoder and canonical output reader, including independence from ambient
Decimal precision. Missing required/optional decimal admission belongs to the
Go consumer; Python verifies that generated messages preserve absence. Invalid
output vectors are exercised by Python because Go does not consume output money.

Time vectors cover ties to even, fine decimal/scientific precision, signed zero,
subnanosecond values, and the exact signed-int64 boundary before rounding.
Malformed whitespace, underscores, non-ASCII digits, missing digits, and repeated
exponents fail the shared ASCII decimal/scientific grammar in `docs/protocol.md`.
Both fixture loaders require all case families to be nonempty and each family's
case names to be unique, so removing a family cannot silently remove coverage. The
time import envelope is 128 coefficient digits with decimal exponent -128..128,
independent of monetary limits. Correcting Go v1's former monetary scale-36
restriction intentionally admits matching fine-precision time values. Legacy
Python float seconds continue through
`Decimal(str(value))`; `legacy_float_times` records that adapter behavior.
Economic cancellation, settlement, and frozen-residual outcomes remain in
`golden.json` and the existing replay tests.

Run the complete contract gate from the repository root:

```sh
uv run --locked --extra dev --extra go --extra codegen --extra rl python scripts/check_conformance.py
```

Pass `--report /fresh/path.json` to write a machine-readable report. The command
checks generated-binding drift, regenerates the Python golden vectors in memory
and checks them for drift, runs the Go contract/replay/RPC race checks, builds its
own daemon, and runs the full Python suite with no skips. Install the pinned Go
protobuf plugins described in `docs/protocol.md`, or supply `PROTOC_GEN_GO` and
`PROTOC_GEN_GO_GRPC`. Change the shared fixture only when the intended contract
changes; its fixture schema version does not replace the environment or
observation compatibility versions.

`time-compatibility.json` records the intended changes in float-timer ties;
both languages execute those examples. The Python economic golden generator
owns a 512-digit, half-even Decimal context, and a regression runs it beneath a
hostile two-digit caller context without changing the golden output. See
`go-runtime.md` for the arithmetic and v0/v1 compatibility limits.
