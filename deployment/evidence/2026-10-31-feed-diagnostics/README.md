# Source failure diagnostics — 31 October 2026

The existing local container later required account recovery while HTTP stayed
available. These saved logs and durable journal boundaries establish an
interruption after the earlier ready snapshot. They do not establish its cause.
The current parent was already cancelled; no new trading was performed.
Container UTC timestamps fall on 30 October; the operator date is 31 October in
Asia/Kolkata. The last durable receipt is not the source failure onset time.

Runtime CLI commit `9788269` preserves internal error identity while exposing
allowlisted messages, the UTC source-return observation time and whether
invalidation was confirmed. The controlled regression first failed for missing
diagnostics, then passed with real randomized PostgreSQL fixtures. An injected
ownership takeover proves that diagnostic path, not the historical cause.

The [full race record](../../../conformance/results/20261031-source-diagnostics.json)
retains 1,190 passing test/subtest events and the compressed raw event log. The diagnostic build was then observed through 480 seconds from startup with a
cancelled parent and synthetic 2 Hz source. Every account probe was ready, receipt
959 was retained, and no failure line appeared. This bounded observation cannot
establish a reliability rate, complete a soak or resolve the historical cause.
