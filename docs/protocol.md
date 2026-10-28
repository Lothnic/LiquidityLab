# Go/Python protocol v1

The source of truth is `proto/liquiditylab/v1/execution.proto`. Generated bindings
are checked in under `gen/go/liquiditylab/v1` and `src/liquiditylab/v1`. The browser
API is a separate JSON contract. The policy-service messages specify a future
boundary; checked-in messages do not establish a running inference service.

## Reproduce bindings

Use the repository's pinned Go release and locked Python dependencies:

```sh
uv sync --extra go --extra codegen --extra dev
go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.12
go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@v1.6.2
uv run --locked --extra codegen python scripts/generate_protocol.py
uv run --locked --extra codegen python scripts/generate_protocol.py --check
```

The Go plugins must be on PATH. Alternatively set `PROTOC_GEN_GO` and
`PROTOC_GEN_GO_GRPC` to their executable paths. The generator checks plugin
versions. `grpcio-tools==1.84.1` provides the pinned compiler, so a system `protoc`
installation is unnecessary. Generated Python requires grpcio >=1.84.1 and
protobuf >=7.35.1; install the `go` extra for clients.

## Exact quantities and time

Money is a signed integer coefficient string plus a nonnegative decimal scale.
Inputs have at most 128 coefficient digits and scale 36. Use canonical values:
no plus sign, leading zeros, negative zero, or removable fractional trailing
zeros. Zero is coefficient `0`, scale `0`. Outputs may use the core's wider
512-digit, scale-144 envelope and must use the same canonical spelling. The
Python output reader rejects noncanonical spellings before constructing a
Decimal. Money never passes through a Python/Go float.
Python conversion uses Decimal tuples, independent of ambient Decimal context.

Relative time is signed integer nanoseconds. Legacy written decimal seconds
import with nearest nanosecond, ties to even, without changing receipt order.
Written seconds use ASCII decimal/scientific syntax:
`[+-]?(?:[0-9]+(?:\.[0-9]*)?|\.[0-9]+)(?:[eE][+-]?[0-9]+)?`.
Signs, leading zeros, `.5`, and `1.` are accepted. Whitespace, underscores,
non-ASCII digits, missing digits, and repeated exponents are rejected before
Python constructs a Decimal. Reject negative, nonfinite, overflowing, or
unsupported input precision. The
seconds importer permits at most 128 coefficient digits and a decimal exponent
in [-128,128] before rounding; the exponent includes the written exponent minus
the number of fractional places. Leading coefficient zeros do not count toward
precision; trailing zeros do. This time envelope is independent of monetary
scale 36. Inputs greater than `9223372036.854775807` seconds fail even when
rounding would otherwise produce a representable nanosecond value. Signed zero
imports as zero. Python retains legacy numeric configuration seconds by applying
`Decimal(str(value))`, including existing float seconds; this does not permit
float money. The Go v1 seconds importer now applies this time precision envelope
instead of the monetary scale-36 limit; this is an intentional correction to
match the Python boundary for fine precision. Python's broader Decimal text
parser is restricted to the shared written-seconds grammar at this boundary;
legacy numeric seconds still use their existing `str(value)` conversion.

Python policy actions 0..5 map explicitly to wire actions 1..6. Wire zero is
unspecified and rejected. Unknown future integer values of Action, Side,
EventKind, and FeeCurrency also fail RPC admission; proto3 itself preserves
unknown enum integers. Optional configuration fields distinguish an explicit
zero latency/penalty from an omitted field/default.

## Replay identity and failures

Create uploads one episode per batch. The server returns a process instance ID,
opaque batch/environment handles, engine/observation versions, and next sequence.
Reset/step/close use a stable request ID and an ordered batch sequence. A retry
must reuse the complete original request. Cached results are replayed; evicted
or conflicting identities fail rather than executing again. Unknown handles
after a server restart do not create replacement environments.

The client retries an unavailable/timed-out RPC once with the same identity.
An unresolved outcome aborts that batch and the training trajectory. A deadline
is not proof that the server did not mutate state. Once admitted mutation starts,
the server completes/caches the bounded operation for safe retry. Limits apply
to batches, tapes, environments, histories, messages, work, and response caches.
These are local offline handles; they grant no exchange capabilities.

Do not reuse removed field numbers or change enum meanings. Contract changes
that alter action/observation/economic behavior require a new environment or
policy compatibility version and explicit reevaluation.


## Shared conformance gate

`conformance/contracts.json` freezes canonical input/output decimals, exact
seconds import, observation version and ordered fields, and enum/action
mappings. Both Go and Python tests read the same versioned fixture. Economic
reference vectors remain in `conformance/golden.json`. Run both contracts and
all required local checks with:

```sh
uv run --locked --extra dev --extra go --extra codegen --extra rl python scripts/check_conformance.py
```

An optional `--report /fresh/path.json` records results and fixture hashes. The
command also checks binding generation and golden drift, runs Go race checks,
builds its own daemon, and requires the full Python suite to finish without
skips. See `conformance/README.md` for fixture ownership and the pinned plugin
requirements above.
