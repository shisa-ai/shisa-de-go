# Review Scope: served-id-calibration

- Mode: implementation
- Stage: implementation
- Target: /home/morpheus/data/dev/shisa/shisa-de-go at a8b2b29; full delta 954adba..a8b2b29

## Summary

Review the full served-ID calibration feature in Go repo at a8b2b29 against 954adba, including de33223 and all round-1 fixes. Requests may use aliases; response model is trusted server evidence for choosing a checkpoint record, not weight verification. Unknown non-echoed IDs disable automatic calibration. Missing/echoed IDs retain configured selection. Known response checkpoints require applicability at checkpoint level. Explicit records remain authoritative with diagnostics. Blocks/fallback/thought IDs are reconciled per answer; conflicts reject read. Non-echoed answers serialize applied identity and record; metadata retains configured record separately, uniform applied record or nil for mixed, and per-head provenance including multi-label heads. Health probe exposes response identity/applicability. Live gates compare both clients under identical explicit file/digest, independent tests assert automatic selection. Production alias/canonical gates pass after an initial server-repeatability failure and an unsupported local-directory test configuration. No Python changes.

## Primary files / regions

- `/home/morpheus/data/dev/shisa/shisa-de-go/client.go` — lines/regions: servedRecord, noteCalibration, addCalibrationProvenance, Answer serialization and Health — reason: Calibration safety and provenance
- `/home/morpheus/data/dev/shisa/shisa-de-go/readout.go` — lines/regions: servedIdentity, fallback reconciliation, Thought.ServedModel, policy reconciliation — reason: Reject mixed checkpoint responses
- `/home/morpheus/data/dev/shisa/shisa-de-go/policy.go` — lines/regions: thinking read identity reconciliation — reason: Thought and final read identity
- `/home/morpheus/data/dev/shisa/shisa-de-go/endpoint.go` — lines/regions: response model fields — reason: Decode asserted identity
- `/home/morpheus/data/dev/shisa/shisa-de-go/served_calibration_test.go` — lines/regions: new regression tests — reason: Unknown, neutral, incompatible, explicit, image, multi-label, concurrent cache
- `/home/morpheus/data/dev/shisa/shisa-de-go/served_identity_test.go` — lines/regions: new tests — reason: Blocks, fallback, thought consistency
- `/home/morpheus/data/dev/shisa/shisa-de-go/client_parity_test.go` — lines/regions: legacy alias replay and provenance assertions — reason: Check explicit parity exceptions
- `/home/morpheus/data/dev/shisa/shisa-de-go/live_test.go` — lines/regions: shared calibration, automatic selection gates — reason: Live verification
- `/home/morpheus/data/dev/shisa/shisa-de-go/live_calibration_test.go` — lines/regions: new expectations and tests — reason: Independent calibration expectations
- `/home/morpheus/data/dev/shisa/shisa-de-go/scripts/live_parity.py` — lines/regions: record loading/digest, tokenizer provenance — reason: Shared record and reproducibility
- `/home/morpheus/data/dev/shisa/shisa-de-go/scripts/test_live_parity.py` — lines/regions: record/source tests — reason: 74 offline recorder tests
- `/home/morpheus/data/dev/shisa/shisa-de-go/cmd/shisa-de/doctor.go` — lines/regions: probe report fields — reason: Probe diagnostics
- `/home/morpheus/data/dev/shisa/shisa-de-go/README.md` — lines/regions: alias and provenance sections — reason: User guidance
- `/home/morpheus/data/dev/shisa/shisa-de-go/docs/PORT.md` — lines/regions: response extension, historical evidence, live gate — reason: Document deviations and measurements

## Review questions

- Have all round-1 findings been fixed across unknown/neutral aliases, explicit records, family/readout compatibility, image reads and mixed-response identities?
- Does applied calibration provenance survive ToDict, decision metadata, multi-label aggregation and doctor probes without claiming configured fits actually tempered different answers?
- Are lazy private records safe under concurrency and isolated from exported mutable Calibration?
- Do replay exceptions test a documented legacy comparison while independent tests prove automatic selection, rather than hiding regressions?
- Does live parity supply identical explicit record bytes and retain every numeric/request/strategy gate, while separate live tests check automatic response selection?
- Are measured docs truthful, including failed initial attempts, server identity assertions and tokenizer source provenance?

## Known non-goals

- No Python client edits
- No DE-1 implementation
- No default model change
- No inference that response IDs authenticate weights
- No weakened numerical tolerances or ignored alias discrepancies

## Validation already run

- go build ./... passes
- go vet ./... passes
- go test ./... -count=1 passes all three packages
- go test -race ./... -count=1 passes all three packages
- python3 -m unittest discover -s scripts -p 'test_*.py': 74 pass
- CLI fixture regenerated; CLI replay passes with explicit Python-only strip_state_quotes:false exclusion
- Production 2026-10-21 https://api.shisa.ai/openai fingerprint vllm-0.30.0-tp2-c2e7e64c: automatic calibration and thinking pass for alias and canonical DE-2.1
- Ten-case shared-record live parity passes for both IDs, Python self-delta 0, largest cross-client deltas 1.22e-15 alias / 2.55e-15 canonical, tolerance unchanged 1e-9
- Initial alias run failed Python self-delta 0.011; documented, not counted as pass; unsupported local tokenizer directory test configuration corrected to hub source

## Diff summary / excerpts

954adba..a8b2b29: response Model fields; per-answer compatible served-record selection, private lazy record cache, unknown response IDs raw, explicit overrides diagnosed; identity consistency across code blocks, fallback and thought; conditional serialized/per-head provenance, multi-label retention, Health/doctor probe; shared-record live parity and independent selection expectations; recorder digest/source provenance and tests; documentation corrections. Round-1 disposition and verification are committed under docs/reviews/served-id-calibration.

## Extra context

Review repository /home/morpheus/data/dev/shisa/shisa-de-go, NOT the parent cwd Python repository. User explicitly forbids changes to Python client; its team handles its own changes. Prior topic de2-1-checkpoint-calibration already exhausted 3 rounds; this is served-id-calibration round 2 of 3 after round 1 had HIGH findings. Reports may be generated under parent cwd; inspect the absolute Go target. Read round-1 reports/disposition in Go repository. No changes to prompt, request shapes, calibration data or readout version. Assess whether error guards/record applicability fixes preserve contract. Do not treat same single-query distribution as proof of weights.
