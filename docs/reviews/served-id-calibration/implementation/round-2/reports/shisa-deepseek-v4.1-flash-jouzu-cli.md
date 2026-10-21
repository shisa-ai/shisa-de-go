# Review Report

## Summary

The feature is implemented as the scope describes: a non-echoed response ID selects a shipped record only when that record applies at checkpoint level, uncovered IDs fall back to raw probabilities, a supplied record stays authoritative with a per-answer diagnostic, and applied-versus-configured provenance survives `Answer`, `ToDict`, `Decision.Meta`, multi-label expansion and the doctor probe. Identity reconciliation covers blocks, fallbacks and the thinking stages, and the private record cache is concurrency-safe and isolated from the exported `Calibration`.

One correctness gap remains in the definition of "echoed". `servedRecord` and `noteCalibration` compare the response ID with `DecisionModel.Model`, the name the caller declared, not with the ID the request actually sent (`Readout.model`). Those differ whenever a caller supplies `Readout`, which is a documented configuration and the one the repo's own parity replay uses. In that configuration an echoed response is treated as a server assertion, so a record the configuration withheld can be applied, or an applied record can be withheld, and the port diverges from the Python client in both directions.

Three smaller items: an empty diagnostic on one supplied-record path, a stale live-gate measurement in the two canonical docs, and a field comment that still describes the pre-change provenance.

## Findings

### MEDIUM: Echo detection compares the response ID with the declared model, not the ID the request sent
- Severity: MEDIUM
- Confidence: HIGH
- Location: `client.go:1436`, `client.go:1465`
- Problem: `servedRecord` treats `served == m.Model` as an echo and preserves configuration; anything else is read as an assertion. `m.Model` is `DecisionModelConfig.Model`, but the request carries `Readout.model`. `NewDecisionModel` keeps a supplied `Readout` as-is (`client.go:795`), so the two can differ, and then an echo of the request is classified as an assertion. Both directions are wrong against `docs/PORT.md:415` ("Missing or echoed IDs preserve configuration") and against the Python client, which never consults the response ID for record selection:
  - *Activates a withheld record.* `DecisionModel{Model: "de2-v4-lr5e5-e3-s7", Tokenizer: "neutral", Readout: model "shisa-ai/shisa-de-2"}` configures the DE-2 family record with `CalibrationApplied=false` (uncovered model and tokenizer). The server echoes `"model": "shisa-ai/shisa-de-2"`, which is exactly the request's model; `served != m.Model`, `ReleasedCheckpoint` resolves it, `Applicability` returns `ApplicabilityCheckpoint`, and the answer is tempered with a record the configuration withheld — and because the level is checkpoint, `CalibrationWarning` stays empty. The same client with `Model: "neutral"` returns raw for the identical request and response. This is the configuration in `testdata/client_parity.json:7872-7874`, and its exchange is a literal echo (`request.model` and `response.model` are both `shisa-ai/shisa-de-2` at lines 7881 and 7910).
  - *Withholds an applied record.* `DecisionModel{Model: "shisa-ai/shisa-de-2", Readout: model "de2-test"}` — the shape every `newPolicyReadout` test builds (`policy_test.go:128`) — has `CalibrationApplied=true`. A server that echoes `de2-test` makes `ReleasedCheckpoint` return empty, so `servedRecord` returns `(m.Calibration, false)`: the answer is raw, `reportProvenance` is true, and the warning asserts "no compatible fitted calibration covers the response model", which is false — the response named nothing the caller did not already know.
  The replay's own assertion shows the classification is wrong: `client_parity_test.go:599` requires `actual.ServedModel == model.Readout.model` for the case it labels non-echoed, so the fixture records an echo of the request.
- Recommendation: treat the request's model as an echo too: `if served == "" || served == m.Model || m.calibrationSupplied || (m.Readout != nil && served == m.Readout.model)`, and use the same test for `reportProvenance` in `noteCalibration` so both agree. This keeps the parity replay passing — the aliased case still resolves to the DE-2 record through its tokenizer, so the applied ID is unchanged, and the conditional extension at `client_parity_test.go:624` simply stops firing — and it makes `PORT.md:415` true. Add two subtests where the response equals `readout.model` and differs from `Model`: one with a released request ID (expect the configured selection, no provenance extension), one with an unreleased request ID (expect the configured record to stay applied). No current test covers either direction.

### LOW: A supplied record with no fitted temperature produces a diagnostic with an empty reason
- Severity: LOW
- Confidence: HIGH
- Location: `client.go:1469-1473`
- Problem: `Applicability` returns `(ApplicabilityUnfitted, nil)` when `Calibration.Fitted()` is false (`calibration.go:396-398`), with no reasons. For a supplied record, `noteCalibration` takes the `level != ApplicabilityCheckpoint` branch and builds `"the supplied calibration is retained despite the response model: " + strings.Join(nil, "; ")`, so `Answer.CalibrationWarning` and `Decision.Meta["calibration_warnings"]` end in a dangling colon with nothing after it. Reachable with `Calibration: &Calibration{}`, the only model-level way to force raw answers, and untested: `TestResponseCalibrationExplicitDisagreement` supplies a fitted record. In the same configuration `Health` reports `calibration_match: true` and `ok: true` with `calibration_applied: false`, so `doctor` exits zero on a record that tempers nothing.
- Recommendation: when `len(reasons) == 0`, say why in words — for example "the supplied record holds no fitted temperature, so probabilities are raw" — the phrase `live_calibration_test.go:104` already uses. Consider whether `Health` should treat `ApplicabilityUnfitted` as `calibration_match: false`; the answer is raw either way, but "ok" currently implies the record is doing something.

### LOW: The canonical docs still quote the pre-shared-record gate measurement
- Severity: LOW
- Confidence: HIGH
- Location: `docs/PORT.md:1020-1027`, `AGENTS.md:336-338`
- Problem: Both present the M6 gate's agreement as "2.2e-19 up to 5.6e-16 ... against `vllm-0.30.0-tp2-ca69195e` on 2026-10-12", and `PORT.md:1026` concludes "The tolerance is six orders of magnitude above that and not more". The gate now supplies one explicit record to both clients, so both sides apply the same temperature and the range includes the tempering arithmetic: `docs/reviews/served-id-calibration/verification.md:26-27` records 1.22e-15 (alias) and 2.55e-15 (canonical) against `vllm-0.30.0-tp2-c2e7e64c` on 2026-10-21. A reader who runs the gate today sees a range about 4.5x the documented maximum and about 5.6 orders below the tolerance, not six. The shared record is described in `PORT.md:437-439`, but the measurement paragraph still reads as the current gate's.
- Recommendation: keep the 2026-10-12 range, labelled as the pre-change run on raw answers, and add the 2026-10-21 shared-record range with its fingerprint; restate the tolerance margin as "five orders of magnitude" or drop the multiplier. Make the same edit to the `AGENTS.md` evidence bullet so the two do not disagree.

### LOW: The `Decision.Meta` field comment and the `doctor --probe` table row still describe the pre-change provenance
- Severity: LOW
- Confidence: HIGH
- Location: `client.go:297-299`, `README.md:572`
- Problem: `Decision.Meta` is documented as "the provenance: model, family, readout version, policy and calibration", but `Meta["calibration"]` is now the uniform *applied* record, empty when every head is raw and `nil` when they differ, with `configured_calibration` beside it and the per-head maps appearing only for non-echoed responses. A godoc reader has no way to tell which record the key names. `README.md:572` lists `doctor --probe` as reporting "how the server renders the image path" without the response-ID and applicability report added at `doctor.go:120-127`, though `README.md:353-355` does describe it.
- Recommendation: extend the field comment by one sentence naming the applied record and pointing at `configured_calibration`; add the probe's identity report to the command-table row.

## Things Checked

- Read the round-1 disposition and all three round-1 reports, then verified each fix in the tree: uncovered IDs now return raw (`client.go:1441-1444`); a neutral alias activates a released record through the record lookup rather than the early return; family and readout compatibility are enforced through `Applicability` at checkpoint level; provenance reaches `ToDict` (`client.go:271-277`), `Decision.Meta` (`client.go:1479-1512`), expanded multi-label heads and the doctor probe; live gates pass one explicit record to both clients.
- Read `servedRecord`, `noteCalibration`, `addCalibrationProvenance`, `answerFromRead` and the `run()` call order. `applicable` is resolved before `answerFromRead`, and `answerFromRead` reports `Calibrated=false` when the selected record has no entry for this read, so a record with no `repeat2` fit yields a raw answer rather than a misleading record ID.
- Checked the private cache: `sync.Once`, one parse per shipped record, no write to `m.Calibration`, and the mutation-isolation test covers the exported-record hazard. No value copy of `DecisionModel` exists in the tree, so the embedded `sync.Once` cannot be copied.
- Traced identity reconciliation in `readCodes` (answer block plus per-slot fallback requests), `ReadCodesAfterThought`, the policy's thinking stage (`policy.go:446-480`), and the image path; conflicting checkpoints fail the read and the first reported spelling reaches `Answer.ServedModel`.
- Read the replayed alias exchange in `testdata/client_parity.json` and `compareDecision`'s conditional extension, and confirmed the two replay cases still pass under the corrected echo rule.
- Compared `README.md`, `docs/PORT.md` and `AGENTS.md` claims against `verification.md` and the code: alias guidance, canonical-ID evidence, probe fields, and the `strip_state_quotes: false` exclusion are accurate; the gate measurement is the exception.
- Did not run the build, vet or test suites; the scope reports them as already run and this review is read-only.

## Residual Risks

- A response ID is a server assertion. A proxy that rewrites `model` defeats automatic selection, and `docs/PORT.md:414-415` says so; nothing here narrows that.
- The live gates were not rerun. The 2026-10-21 numbers are taken from `verification.md`, including the documented initial alias failure (Python self-delta 0.011) and the corrected tokenizer configuration.
- `Readout.letterLogprob` (`readout.go:955`) is now used only by `readout_test.go:706`; production fallbacks go through `letterLogprobFor` with an identity. It is a small maintenance cost, not a defect.
- The test helper `newPolicyReadout` names the readout `de2-test` while every served-record test declares a different model, so all of those tests exercise the request/declaration mismatch described in the MEDIUM finding without asserting anything about it.

BEGIN_REVIEW_JSON
{
  "findings": [
    {
      "severity": "MEDIUM",
      "title": "Echo detection compares the response ID with the declared model, not the request model",
      "location": "client.go:1436, client.go:1465",
      "recommendation": "Also treat served == Readout.model as an echo in servedRecord and noteCalibration, and add tests where the response echoes the request ID while differing from DecisionModel.Model."
    },
    {
      "severity": "LOW",
      "title": "Supplied unfitted record yields a warning with an empty reason",
      "location": "client.go:1469-1473",
      "recommendation": "Fall back to an explicit phrase when Applicability returns no reasons, and consider reporting ApplicabilityUnfitted as a calibration mismatch in Health."
    },
    {
      "severity": "LOW",
      "title": "Live-gate measurement in PORT.md and AGENTS.md predates the shared-record gate",
      "location": "docs/PORT.md:1020-1027, AGENTS.md:336-338",
      "recommendation": "Label the 2026-10-12 range as the pre-change raw-answer run, add the 2026-10-21 shared-record range with fingerprint c2e7e64c, and restate the tolerance margin."
    },
    {
      "severity": "LOW",
      "title": "Meta field comment and doctor --probe table row describe the pre-change provenance",
      "location": "client.go:297-299, README.md:572",
      "recommendation": "Name the applied record and configured_calibration in the field comment, and add the probe identity report to the command table row."
    }
  ]
}
END_REVIEW_JSON
