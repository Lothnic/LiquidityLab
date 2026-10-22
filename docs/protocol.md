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
512-digit, scale-144 envelope. Money never passes through a Python/Go float.
Python conversion uses Decimal tuples, independent of ambient Decimal context.

Relative time is signed integer nanoseconds. Legacy written decimal seconds
import with nearest nanosecond, ties to even, without changing receipt order.
Reject negative, nonfinite, overflowing, or unsupported input precision.

Python policy actions 0..5 map explicitly to wire actions 1..6. Wire zero is
unspecified and rejected. Optional configuration fields distinguish an explicit
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
