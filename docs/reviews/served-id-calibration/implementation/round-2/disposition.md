# Served-ID calibration: round 2 disposition

Reviewed candidate: `a8b2b29`. Zero HIGH/CRITICAL findings and no uncovered
reviewer execution failures. Astra used its configured
`openai-codex-gpt-6-astra-jouzu-cli` fallback after the Shisa primary failed;
ds4 and glm used their primary configurations. No third round is required by
the review policy after fixing these MEDIUM/LOW findings.

| Finding | Fix and verification |
| --- | --- |
| Echo comparison ignores an injected readout's request ID | Share an echo predicate across selection and provenance; recognize both declared and actual request IDs. Regression tests cover activating a withheld record and disabling an applied record. Remove the round-1 replay overrides: the complete historical Python comparison now passes unchanged. |
| Probe conflates explicit authorization with applicability | Independently call `Calibration.Applicability` against the response identity without tokenizer fallback. Test explicit DE-2 calibration on a DE-2.1 probe: retained record, applicability false, diagnostic present. |
| Unfitted explicit record yields an empty diagnostic | Emit a complete unfitted-record message. Keep Health success semantics: raw answers are supported, and an unfitted record is not a transport or read failure. |
| Private cache discards record load failures | Retain per-checkpoint load errors and expose the cause in response diagnostics; regression simulates a failed embedded record load. |
| Live expectation omits legacy post-thought coverage | A record without per-read fits contributes no temperature to a thinking read; synthetic legacy-record regression covers this. |
| Digest and record use separate file reads | Parse the captured bytes with the existing Python dictionary loader and hash that same buffer. Real-loader test forbids reopening the file. No Python repository edits. |
| Invalid temperatures can reach the probe | Validate every temperature table/value before requests. Null, nonnumeric, boolean and nonfinite values become exit 5 with no network calls or output. |
| Canonical docs and field comments are stale | Distinguish historical and shared-record measurements, applied/configured metadata, response-ID probe diagnostics and actual-request echoes. |

Checks after fixes: build, vet, full Go offline suite and race suite pass;
82 recorder tests pass. Live outcomes are recorded in `../../verification.md`.
