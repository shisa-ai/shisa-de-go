# Review Report

## Summary

All round-1 findings are fixed in the working tree, and the fixes hold up under inspection. `servedRecord` now disables automatic calibration for unknown non-echoed IDs, resolves the neutral-alias case through `ReleasedCheckpoint` plus checkpoint-level `Applicability` (no tokenizer fallback, no readout bypass), and keeps supplied records authoritative with a per-answer diagnostic. `servedIdentity` reconciles blocks, fallbacks and the thought against the reads on either side and rejects conflicts before the final read is spent. Applied provenance survives `ToDict`, `Decision.Meta` (configured vs applied, uniform vs mixed), multi-label aggregation and the doctor probe, and the private lazy record cache is race-safe and isolated from the exported `Calibration`. The live suite now derives every calibration expectation independently (`live_calibration_test.go`), the parity gate hands both clients one record file and verifies its digest, and automatic selection has its own live gate. The docs are truthful: `verification.md` records the failed repeatability run and the rejected local-tokenizer configuration, and `PORT.md` marks the response ID as a server assertion and corrects the earlier rounded-value transcription.

What remains is five LOW findings, none of which breaks the round-1 invariants on any production constructor path: an echo-detection edge when a caller injects a `Readout` built for a different model id, a contradictory doctor probe flag for supplied records, a diagnostic warning that can carry an empty reason list, a silently swallowed record-load error in the private cache, and a latent gap in the live suite's independent expectation (it does not model the per-read coverage rule). Reviewed read-only; no shell tool is configured in this session, so the working tree was inspected directly with read/grep/find and no tests were run.

## Findings

### LOW: An echoing server is misread as alias evidence when an injected Readout names a different model
- Severity: LOW
- Confidence: MEDIUM
- Location: client.go:1435 (`servedRecord`), client.go:1464 (`noteCalibration`)
- Problem: `servedRecord` treats a response as echoed only when it equals `m.Model`, but the request the server echoes is the readout's model id. `NewDecisionModel` accepts an injected `config.Readout` without checking that its model matches `Model`, so a caller who builds a `Readout` for `shisa-ai/shisa-de-2` and a `DecisionModel` for `my-finetune` gets a plain echo of `shisa-ai/shisa-de-2` classified as a non-echoed identity: `reportProvenance` turns on, and the shipped DE-2 record is activated automatically even though the configured identity covers nothing. That violates the documented rule "missing or echoed IDs preserve the configured selection". The parity replay demonstrates the shape: `decide-other-checkpoint` (declared model and tokenizer `de2-candidate-lr5e5`, response echoing the readout's request id) needs the forced `Calibrated=false` override precisely because the echo activates the record. The CLI and all production constructors keep the two ids equal, so the divergence needs the library's injected-Readout configuration to occur.
- Recommendation: Treat a response equal to the readout's request model as an echo as well (`served == m.Model || served == m.Readout.model`), in both `servedRecord` and `noteCalibration`'s `reportProvenance`. If the current behaviour is deliberate, state in `docs/PORT.md` that an echo of the *readout's* id — not the configured `Model` — counts as alias evidence, and note that the replay's `decide-other-checkpoint` override compensates for it.

### LOW: The doctor probe reports "applicable true" for a supplied record that does not cover the response id
- Severity: LOW
- Confidence: HIGH
- Location: client.go:980-985; cmd/shisa-de/doctor.go:120-126
- Problem: In `Health`, `probe_calibration_applicable` is the second return of `servedRecord`, which for a supplied record is `m.CalibrationApplied` — the construction-time flag, not the record's applicability to the response identity. A caller who supplies DE-2's fitted record and probes an endpoint that reports `shisa-ai/shisa-de-2.1` gets `probe_calibration_applicable: true` printed as "applicable yes" on the very lines where `probe_calibration_warning` says "the supplied calibration is retained despite the response model". The flag and the warning contradict each other; for the automatic path the two meanings coincide, which is why only the supplied case shows the split.
- Recommendation: For a supplied record, report the record's `Applicability` against `answer.ServedModel` (as `noteCalibration` already computes), or rename the field to say "applied" rather than "applicable". Add a doctor probe test for the supplied-mismatch configuration.

### LOW: The supplied-record disagreement warning can end with an empty reason
- Severity: LOW
- Confidence: HIGH
- Location: client.go:1463-1475; calibration.go (`Applicability` returns `ApplicabilityUnfitted` with nil reasons)
- Problem: `noteCalibration`'s supplied branch fires whenever `level != ApplicabilityCheckpoint`, and joins the reasons with "; ". For an unfitted supplied record against a matching family and readout, `Applicability` returns `(ApplicabilityUnfitted, nil)`, so the warning reads "the supplied calibration is retained despite the response model: " with nothing after the colon. Nothing is being "retained" that could mislead in that case either — an unfitted record leaves the answer raw whether or not the response agrees.
- Recommendation: Skip the warning for `ApplicabilityUnfitted` (the answer already reports raw), or fall back to a reason string ("the record holds no fitted temperature") when `reasons` is empty.

### LOW: A shipped record that fails to parse silently disables response-based selection
- Severity: LOW
- Confidence: MEDIUM
- Location: client.go:1445-1451
- Problem: The private cache built inside `servedRecordsOnce.Do` drops any `LoadCalibration` error (`if record, err := LoadCalibration(entry.path); err == nil`). A shipped embedded record that fails to parse then makes every non-echoed response fall into the uncovered branch: answers go raw with only the generic "no compatible fitted calibration covers the response model" warning, and the actual cause — a broken record in the build — is invisible in every diagnostic surface. The failure direction is safe (never wrongly tempered), which is why this is LOW, but the cause is lost exactly when it is needed.
- Recommendation: Keep the first load error in the `Do` closure and include it in the answer warning or a `Health`/doctor field, so a broken shipped record reports itself rather than masquerading as an uncovered checkpoint.

### LOW: The live suite's independent expectation omits the per-read coverage rule
- Severity: LOW
- Confidence: HIGH
- Location: live_calibration_test.go:129-172 (`liveExpectedCalibration`, `liveAssertCalibration`); client.go:1288-1291
- Problem: `answerFromRead` leaves an answer read after a thought raw under a record without per-read fits (`covered := len(record.Reads) != 0 || read.Thought == nil`), and `TemperatureFor` returns 1.0 for a read a per-read record does not name. `liveExpectedCalibration` — presented as "the independent statement of the rule" — models neither: for a supplied record it sets `Applied = Fitted()` and `liveAssertCalibration` then expects `record.TemperatureFor(type, read)`, which for a legacy single-fit record on a `think` read is the direct fit, not 1.0. The live thinking test would then report a correct raw answer as a failure. Not reachable today: both shipped records carry `think` fits and the gate supplies only shipped records, so this is a latent disagreement between the helper and the client.
- Recommendation: Mirror the coverage rule in the expectation (a record without `Reads` contributes no fit for a `think` read), with a small offline test pairing a synthetic no-`Reads` record with `liveAssertCalibration` on a `repeat2-think` answer.

## Things Checked

- **Round-1 fix verification, item by item.** Unknown served IDs: `servedRecord` returns the configured record with `applied=false`, `noteCalibration` warns, `TestResponseCalibrationSelection("unknown")` asserts raw probability, empty applied record and a diagnostic. Neutral alias: the `served == m.Calibration.Model` short-circuit is gone; the neutral-alias cases (DE-2, DE-2.1, suffixed `-FP8`) pass through `ReleasedCheckpoint` and checkpoint-level `Applicability`. Readout compatibility: the served record is gated by `record.Applicability(served, m.ReadoutVersion, {Family: m.Family})` with no tokenizer fallback; the "incompatible" test doctors `ReadoutVersion` and asserts raw. Identity reconciliation: `servedIdentity.note` compares every block, every fallback (`letterLogprobFor` takes the read's identity) and the thought against both reads in `ReadPolicy`; conflicts fail before the final read is spent (asserted by request counts in the tests); a sole fallback or thought id propagates to the answer; the identity is per call (`TestReadoutKeepsNoIdentityBetweenCalls`).
- **Provenance surfaces.** `ToDict` adds `calibration`/`served_model`/`calibration_warning` only for non-echoed responses; `Decision.Meta` gains `configured_calibration`, `calibration_record_by_head`, `served_model_by_head` and `calibration_warnings` only when some head reported provenance, with `calibration` as the uniform applied record, `""` for all-raw and `nil` for mixed; multi-label heads keep per-head provenance in both `Raw` and `Meta` (computed in `run` before `Classify` deletes the expanded answers), and the mixed multi-label test asserts it. `ToWire` stays the Python TypeSafe shape.
- **Cache concurrency and isolation.** `servedRecordsOnce`/`servedRecords` are populated once from the embedded files via `sync.Once` (happens-before for later readers), read-only afterwards, and never written from the exported `Calibration`; `TestResponseCalibrationCacheIsPrivateAndConcurrent` exercises concurrent access and mutation isolation, and the scope records `-race` passing.
- **Replay exceptions vs independent coverage.** The `decide-other-checkpoint` raw override is commented at the call site, described in `docs/PORT.md`, and pairs with `compareDecision`'s explicit provenance assertions; automatic selection is pinned independently by `TestResponseCalibrationSelection`, `TestLiveCalibrationAgainstAServedId` and the live `TestLiveCalibrationMatchesTheServedCheckpoint`. The replay's provenance mirror can only fail loudly (a regression to "uniform = ID if any head calibrated" is separately pinned by the mixed-head test's `nil` assertion).
- **Live parity gate.** `liveParityRecord` resolves the configured identity's record, the recorder is handed it via `--calibration`, the meta records the file name and byte digest, and the Go side re-hashes the same path and compares file, digest, record id and fitted-on model; the recorder refuses an unusable record with exit 5 before spending requests (tested in `scripts/test_live_parity.py`), and there is no fallback to the recorder's own selection. All numeric, request and strategy gates are retained, the tolerance is unchanged at 1e-9, and the Python self-delta/unstable verdicts are reported as inconclusive rather than failures. The 2026-10-21 runs, including the failed 0.011 self-delta attempt and the rejected local-tokenizer configuration, are recorded in `docs/reviews/served-id-calibration/verification.md` and not counted as passes.
- **Docs and evidence.** README's alias paragraph, the local-server bullet ("needs `Calibration` if the response does not reveal the checkpoint"), AGENTS.md's summary lines and PORT.md's response-extension section all match the code; PORT.md distinguishes the server's identity assertion from weight verification, labels the pre-change 0.914502144 result as such, corrects the 0.961568822 transcription ("computed from the rounded raw value, not returned by the client") — the 1.98-tempered value reproduces from the recorded raw 0.998299278 — and the readout tables in README and `verification.md` agree with the shipped records. No Python client files are touched by this delta; the CLI replay excludes only the documented Python-only `strip_state_quotes: false` line and requires its recorded value.
- **Edge cases traced:** unknown `shisa-de-2.2`/`shisa-de-2.10`/training-arm ids name no checkpoint (the suffix boundary logic rejects them); a suffixed served id resolves through `ReleasedCheckpoint` and `Applicability`'s suffixed-repo rule; a supplied record naming a suffixed release produces no false warning; `Answer.Calibration` is empty whenever no temperature ≠ 1.0 was applied, which the live helper agrees with; a response whose family or readout disagrees is a mismatch and never applied automatically; `Health`'s probe requests raw probabilities and fails `ok` on a rejected (conflicting-identity) read.

## Residual Risks

- The whole feature trusts the response `model` field. A proxy that rewrites ids can move answers onto a record not fitted on the weights that answered; this is documented in README and PORT.md, and the failure is bounded to temperature scaling (argmax, chosen values and levels are unchanged), but nothing cross-checks the reported id against `/v1/models` or the request.
- `TestLiveParityRecordIdentifierMatchesPython` and the gate's shared-record design depend on the Python client's record identifier spelling staying identical; that is pinned offline where the Python checkout is present and skipped otherwise.
- I could not run `git`, `go test`, or the Python unit tests (no shell tool configured; the scope forbids broad runs), so the scope's reported validation results were taken as reported and everything above is from reading the working tree of `shisa-de-go`. The five findings are derived from code, not from executed failures; the injected-Readout echo finding in particular was confirmed against the parity fixture's `decide-other-checkpoint` shape rather than a live reproduction.

BEGIN_REVIEW_JSON
{
  "findings": [
    {
      "severity": "LOW",
      "title": "Echoing server misread as alias evidence when an injected Readout names a different model",
      "location": "client.go:1435",
      "recommendation": "Treat a response equal to the readout's request model as an echo in servedRecord and noteCalibration, or document that an echo of the readout's id counts as alias evidence"
    },
    {
      "severity": "LOW",
      "title": "Doctor probe reports applicable=true for a supplied record that does not cover the response id",
      "location": "client.go:980",
      "recommendation": "Report the record's applicability to answer.ServedModel for supplied records, or rename the flag to say applied rather than applicable"
    },
    {
      "severity": "LOW",
      "title": "Supplied-record disagreement warning can end with an empty reason",
      "location": "client.go:1463",
      "recommendation": "Skip the warning for an unfitted record, or supply a reason string when Applicability returns none"
    },
    {
      "severity": "LOW",
      "title": "A shipped record that fails to parse silently disables response-based selection",
      "location": "client.go:1445",
      "recommendation": "Keep the first LoadCalibration error and surface it in the answer warning or the Health report"
    },
    {
      "severity": "LOW",
      "title": "Live expectation helper omits the per-read coverage rule for records without per-read fits",
      "location": "live_calibration_test.go:129",
      "recommendation": "Model the covered rule in liveExpectedCalibration and add an offline test with a synthetic no-Reads record on a thinking read"
    }
  ]
}
END_REVIEW_JSON
