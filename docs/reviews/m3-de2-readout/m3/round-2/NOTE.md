# Round 2 coverage and disposition

Reviewers: `astra`, `ds4`, `glm`. Two produced usable reports.

`astra` failed again, in the same way as M2 round 3: its report file holds
only `[Error: Our servers are currently overloaded. Please try again
later.]`, and the CLI exited 0, so the harness recorded the round as having
no execution errors. `astra` has now produced no review for M2 round 3 or
for either M3 round. `glm` ran via its fallback candidate
(`glm-zai-glm-5.3-jouzu-cli`) after its primary timed out; the fallback
produced a full report.

`glm` found the round's only HIGH, and it was a regression from the round-1
fix: `CheckCodeBoundary` took the readout mutex and then called a helper
that took it again, so every call to the exported method hung while holding
the lock. Nothing in the tree called it, which is why a green suite and
`go test -race ./...` both missed it.

`ds4` found one MEDIUM: the boundary-approval cache was keyed by code
alone, so a caller-supplied `Slot` with an approved code and a foreign
token id skipped the per-code check. `glm` found the same cache's other
hole independently — an in-flight read could write approvals for the
tokenizer it had captured into the cache `SetTokenizer` had just cleared.

Both are addressed in `6b35ee2`, together with the LOW findings: the
nil-embed panic and typed-nil `errors.As` in `ReadoutHTTPError`, the
zero-value `Readout` panic, the unasserted `think_system`, the response
cap asserted as a lower bound, the two inaccurate `docs/PORT.md` claims
(the `token_ids` bullet and the unclaimed `Softmax` sum order), the
unverified request headers, and the missing direct tests for `closeEnough`
and the exported `Slot`/`Render`/`AnswerPrefixID`/`CheckCodeBoundary`.
