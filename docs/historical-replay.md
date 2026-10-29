# Offline historical execution comparison

The CLI compares a supplied reference plan with up to seven alternatives on one
verified public recording. Every result, including the reference, is a simulated
estimate. This workflow runs without PostgreSQL, an account actor or a venue
connection. It reuses the Go replay engine, exact ledger, compiler and baseline
policies.

## Run the retained example

Build the CLI and follow [the archive runbook](market-archive.md) to import the
retained Kraken capture into an operator-controlled archive root. Copy the
returned dataset ID, then run:

```sh
go build -o build/liquiditylab ./cmd/liquiditylab
build/liquiditylab replay compare \
  --archive /absolute/path/to/liquiditylab-market-archive \
  --id <id-from-import-output> \
  --spec examples/replay/retained-short-compare.json \
  --output /absolute/path/to/new-comparison.json
```

The output parent directory must already exist. The destination must be new.
The example explicitly supplies assumed instrument rules, synthetic quote fees,
latencies, message/resting limits, participation, initial balances and plans.
These are demonstration assumptions, not verified current venue/account terms.
Its one-second horizon fits the short retained capture; it is not a policy
evaluation dataset. Both supported policies are deterministic baselines:
`scheduled` and `liquidity-aware`.

## Specification

The JSON schema identifier is `liquiditylab.historical-compare.spec.v1`. Unknown
fields, null/missing required assumptions, trailing JSON values, unsupported
policies/models and invalid exact amounts are rejected. All fields shown in the
[example](../examples/replay/retained-short-compare.json) are required. Monetary
values are decimal strings; time fields are integer nanoseconds. There are no
implicit rule, balance, fee or simulation defaults in this command.

The spec contains shared `rules`, `fees`, `simulation` and
`valuation_reference`, one `reference`, and one to seven `alternatives`. Each
scenario has a unique label, complete plan and baseline policy. Only
`conservative-v0` fill assumptions and `deadline-midpoint` comparison valuation
are currently supported. A different limit or child size can be explored with
the other objective fields held constant.

## Source selection and coverage

`marketarchive.Load` rechecks the original raw/source-manifest hashes,
normalization and every stored artifact before replay. Version 1 starts at the
first verified snapshot, normalized to receipt time zero. Arbitrary offsets are
not supported. The selected interval ends at the maximum of every scenario's
deadline plus settlement allowance, with overflow checks. All events at that
endpoint are included in original receipt order.

The sealed `capture_end` establishes coverage, including a quiet tail after the
last market event. The tape records and hashes this explicit coverage; no
terminal trade or book event is invented. Ordinary `replay.NewTape` callers
retain their existing last-event coverage behavior and dataset hashes.

The entire selected interval must have valid, uncrossed market data. Source or
checksum gaps within it, including at either endpoint, reject the comparison.
Gaps strictly earlier than the first snapshot's elapsed time may precede a
recovered origin; their count is reported and their details remain in the archive
manifest. Gaps at the same elapsed time as the origin are conservatively
rejected. A later gap beyond the selected endpoint does not invalidate the
earlier interval. `continuous=true` alone never substitutes for these checks or
for sufficient deadline/settlement coverage.

## Reading the report

The report retains the archive ID and payload hashes, normalizer version,
selected source receipt bounds, coverage-aware tape hash, resolved spec/hash,
engine/compiler/baseline/environment/observation versions and available Go build
identity. Unknown build metadata remains `unknown`; it is not a promotion or
execution authorization.

Each scenario includes exact fills, quote/base fees, balances, completion,
residual quantity, termination/truncation and the engine's original terminal
valuation. Additional comparison fields use the immutable public-book midpoint
at that plan's deadline. This valuation is independent of the simulated fill
time and simulated book depletion. It does not replace the engine's terminal
report.

An alternative receives a cost delta only if both estimates terminate with valid
settlement and have the same symbol, side, target quantity, deadline, initial
balances and comparison valuation. Other results have `comparable=false`,
explicit reasons and a null delta. The delta is:

```text
estimated_cost_reduction_quote = reference marked cost - alternative marked cost
```

A positive value means lower estimated marked cost under those assumptions.
Inspect completion and residual quantity alongside cost. Float rewards and the
miss-penalty objective are separate diagnostics. This delta is not realized
savings or evidence that one policy is better on unseen recordings.

`source_session_binding` is explicitly null. A supplied reference is not a
reconstruction of the trader's observed execution. Matching filenames, symbols
or declared UTC timestamps do not establish such a binding.

## Bounds, cancellation and output

- Spec: 64 KiB; scenarios: eight total.
- Selected tape: 20,000 events, 1,000,000 supplied levels, 2,048 levels per event
  and in the accumulated book.
- Execution: 10,000 decisions per scenario; 20,000 decisions/orders/fills across
  the comparison. Cancellation is checked during selection and between steps.
- Reports: 4 MiB per scenario and 16 MiB for the complete JSON artifact.

Exceeding a bound returns an error; execution is not silently shortened.
Unresolved simulated settlement is retained as an invalid economic result with
no comparable delta. Invalid source data or invalid input fails the command.

Spec input is a bounded regular file opened without following the final symlink
on Linux. Intermediate paths and the output directory are operator trusted.
Output is staged privately, fully written and synced, then published exclusively
with a same-directory hard link and a directory sync. Existing files/symlinks
are never overwritten. Cancellation before publication removes the staging
file. A failure after publication can leave a complete file present without
confirmed directory durability; inspect that file and the command error rather
than assuming success or overwriting it.

## Executed local checkpoint — 29 October 2026

An ordinary review build ran the retained example against archive
`2a409892cbf06e56141fd23bc5bbf33bba4920400f753740e0b0020e2cb7e80a`.
Both estimates filled 0.001 BTC with net quote change -80.860398 USD and marked
comparison cost 0.161448 USD; their estimated cost difference was exactly zero.
Independent rational arithmetic reconstructed fills, fees, balances, residuals,
the common valuation and delta, and verified the resolved-spec hash. This is
functional evidence from a review build, not a clean performance or policy
quality result.

The full historical trader workflow still requires durable paper-session/source
binding, replay job persistence and authorization, original-execution comparison,
trader UI/export integration, and immutable policy evaluation/promotion. This
offline command supplies the verified baseline comparison path for that work.
