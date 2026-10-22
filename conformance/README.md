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
