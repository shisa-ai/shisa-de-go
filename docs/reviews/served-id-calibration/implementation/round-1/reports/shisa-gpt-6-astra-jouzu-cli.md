# Review Report

## Summary

The implementation handles the main DE-2 → DE-2.1 alias case without mutating shared client configuration. Explicitly supplied records remain authoritative, and both text and image answers capture the response model.

However, record selection has two incorrect edge cases, multi-request reads can discard contradictory model evidence, and the applied record is lost from existing provenance surfaces. The live checks also retain expectations that conflict with the new behavior.

Reviewed the working tree in `/home/morpheus/data/dev/shisa/shisa-de-go`. Its branch reference points to `de33223df779249827c54dcf7cfde9d904349caa`. No files were modified and no tests or builds were run.

## Findings

### HIGH: An unknown served checkpoint retains the configured checkpoint’s calibration
- Severity: HIGH
- Confidence: HIGH
- Location: `client.go:1418–1424`
- Problem: When a request configured for `shisa-ai/shisa-de-2` returns a different, uncovered model such as `shisa-ai/shisa-de-2.2`, `ReleasedCheckpoint(served)` returns empty and `servedRecord` returns the DE-2 record with its existing `applied=true`. The answer is therefore calibrated using DE-2’s temperatures despite response evidence naming another checkpoint. This contradicts the rule that uncovered checkpoints receive raw probabilities.
- Recommendation: For an automatically selected record, distinguish an absent or echoed model from a different, unrecognized model. Leave the latter uncalibrated and report why. Preserve the explicit-record override. Add coverage for a released request ID returning an uncovered served ID.

### MEDIUM: A neutral alias reporting DE-2 never activates its matching record
- Severity: MEDIUM
- Confidence: HIGH
- Location: `client.go:1415–1420`
- Problem: A client configured with a neutral alias and a neutral tokenizer source holds the family’s DE-2 record but has `CalibrationApplied=false`. If the response then identifies `shisa-ai/shisa-de-2`, the early equality check against `m.Calibration.Model` returns that unchanged false flag. Thus response evidence activates calibration for DE-2.1 but not DE-2, solely because DE-2 happened to be the initial family record. The normalized-checkpoint equality branch has the same problem.
- Recommendation: Once a non-echoed response identifies a released checkpoint, evaluate whether its record applies even when it is the record already held. Test neutral aliases returning each released checkpoint, including normalized or suffixed names.

### HIGH: Multi-request reads ignore conflicting model IDs and all fallback IDs
- Severity: HIGH
- Confidence: HIGH
- Location: `readout.go:765–770`, `readout.go:855–860`, `readout.go:895–934`
- Problem: `noteServed` keeps the first nonempty ID and silently ignores later IDs. A wide read can therefore combine one block from DE-2 with another from DE-2.1, then report and calibrate the combined distribution as DE-2. Naming the same alias in both requests does not establish that the gateway served the same checkpoint. Additionally, `letterLogprob` discards `response.Model`, so fallback requests cannot supply a missing ID or reveal a conflicting checkpoint.
- Recommendation: Carry model identity back from fallback requests and validate identity across every response contributing logprobs. Reject contradictory checkpoint identities rather than normalizing and calibrating a mixed distribution. Add tests for consistent blocks, conflicting blocks, fallback-only identity, and conflicting fallback identity.

### MEDIUM: Applied calibration provenance disappears outside the Answer struct
- Severity: MEDIUM
- Confidence: HIGH
- Location: `client.go:241–267`, `client.go:1012–1023`, `client.go:1344`
- Problem: After an alias switches records, `Decision.Meta["calibration"]` still identifies the configured record rather than the applied record. `Answer.ToDict()` omits both new fields, so serialization loses the only correction to that misleading metadata. Multi-label `Classify` is worse: it deletes the individual answers and replaces their raw entries with only `noul` and `requests`, leaving no returned record of the served checkpoint or applied calibration for those answers.
- Recommendation: Preserve per-head served IDs and applied record IDs in a durable decision-level provenance map, including expanded multi-label heads. Clearly distinguish configured calibration from applied calibration. Expose the new fields through an appropriate serialization surface and test alias cases independently of Python parity fixtures.

### MEDIUM: Existing live gates assert the old calibration behavior
- Severity: MEDIUM
- Confidence: HIGH
- Location: `live_test.go:1018–1022`, `live_test.go:1355–1357`, `live_test.go:457–577`
- Problem: `TestLiveCalibrationMatchesTheServedCheckpoint` derives the expected answer temperature from `model.Calibration`, which intentionally remains DE-2 when the answer switches to DE-2.1. Against the endpoint behavior documented in the scope, the answer correctly uses DE-2.1 while this test expects DE-2. The thinking test makes the same assumption. The live Python parity gate still compares calibrated answers without accounting for the explicitly documented served-ID difference.
- Recommendation: Update the live calibration checks to validate answer provenance against the reported checkpoint while checking configured state separately. Keep the cross-language gate meaningful by comparing raw reads or explicitly selecting the same record in both clients, and retain a separate test of automatic alias selection. Correct the claim in `docs/PORT.md:427–429` that the existing calibration test passes with the old record applied.

### LOW: Automatic record switching bypasses readout compatibility
- Severity: LOW
- Confidence: HIGH
- Location: `client.go:1422–1426`
- Problem: The replacement record is checked for its model and whether it is fitted, but not through `Calibration.Applicability` or `CalibrationCompatibleWith`. Both bundled records currently match `de2-codebook-v1`, so this does not cause a mismatch today. However, after an incompatible readout change, constructor selection would correctly disable an older record while this alias path could load another old record and unconditionally return `true`.
- Recommendation: Apply the same family and readout compatibility checks used during construction before enabling a response-selected record. Add a focused incompatible-readout regression test so a future version change cannot bypass this safeguard.

### LOW: Documentation retains contradictory alias guidance and overstates identity evidence
- Severity: LOW
- Confidence: HIGH
- Location: `README.md:435–441`; `docs/PORT.md:403–429`
- Problem: The README still says a released-ID alias “needs `Calibration`,” without the new qualification that this is necessary when the response does not reveal the checkpoint. PORT describes automatic detection and then describes the default ID as still applying the old fit. It also states that identical logprobs under one tokenizer establish identical weights; agreement on one question does not prove weight identity. The returned canonical ID is additional server-reported evidence, not independent verification of the weights.
- Recommendation: Reconcile the remaining guidance with automatic selection and its explicit-record exception. Label the old calibration result as pre-change behavior. Describe canonical IDs as server assertions and equal observed probabilities as supporting evidence, not proof of weight identity.

## Things Checked

- Inspected all seven primary files named in the scope, plus calibration selection, policy handling, bundled record identities, live checks, and the relevant readout contracts.
- Verified that both response structs decode `model`, text code reads capture it, and `ReadImage` copies it into `CodeRead`.
- Traced both answer-producing call sites through `servedRecord` and `answerFromRead`.
- Confirmed that record switching does not mutate `DecisionModel.Calibration`.
- Confirmed that explicit caller-supplied calibration remains authoritative, consistent with the existing caller-assertion contract.
- Confirmed that calibration still uses the final stage’s read name and that bundled records have no image entry, leaving image answers raw.
- Reviewed the three new tests. The switching test detects removal of automatic switching. The supplied-record and echoed-ID tests correctly test preservation behavior, but would still pass if switching alone were reverted; they are not independent positive tests of the new selection logic.
- Checked multi-block, fallback, multi-label, serialization, compatibility, and live-test interactions through static inspection.

## Residual Risks

- No shell execution tool was available, so I could not inspect `git diff` or independently verify working-tree cleanliness. The commit reference was checked by reading Git metadata.
- The scope reports passing offline, race, and vet checks; those results were not rerun here. Live-test failures described above are inferred from their assertions and the documented endpoint behavior, not from a new live run.
- A response model ID cannot authenticate served weights. A proxy that rewrites IDs can still cause incorrect automatic selection; this trust boundary should be documented.
- Preserving an explicitly supplied record is appropriate, but a disagreement with the response is not surfaced as a diagnostic. Callers currently need access to the answer fields to notice it.

BEGIN_REVIEW_JSON
{
  "findings": [
    {
      "severity": "HIGH",
      "title": "Unknown served checkpoints retain the configured calibration",
      "location": "client.go:1418-1424",
      "recommendation": "Leave automatically selected calibration unapplied when a different served ID names an uncovered checkpoint, and add regression coverage."
    },
    {
      "severity": "MEDIUM",
      "title": "Neutral aliases reporting DE-2 do not activate its matching record",
      "location": "client.go:1415-1420",
      "recommendation": "Reevaluate applicability when response evidence identifies the already-held record instead of preserving the original unapplied flag."
    },
    {
      "severity": "HIGH",
      "title": "Multi-request reads discard conflicting and fallback model identities",
      "location": "readout.go:765-770",
      "recommendation": "Collect model identities from every contributing block and fallback response, and reject conflicting checkpoint identities."
    },
    {
      "severity": "MEDIUM",
      "title": "Applied calibration provenance is lost in metadata and serialization",
      "location": "client.go:1344",
      "recommendation": "Preserve per-head applied record and served model provenance through serialization and multi-label aggregation."
    },
    {
      "severity": "MEDIUM",
      "title": "Live gates retain pre-change calibration expectations",
      "location": "live_test.go:1355-1357",
      "recommendation": "Validate response-selected calibration separately from configured state and align the cross-language parity comparison on raw reads or an explicit shared record."
    },
    {
      "severity": "LOW",
      "title": "Automatic switching bypasses readout compatibility checks",
      "location": "client.go:1422-1426",
      "recommendation": "Require family and readout applicability before automatically applying a replacement record."
    },
    {
      "severity": "LOW",
      "title": "Alias documentation is inconsistent and overstates weight identity evidence",
      "location": "README.md:435-441; docs/PORT.md:403-429",
      "recommendation": "Reconcile alias guidance with automatic selection, label pre-change results, and distinguish server-reported identity from verified weights."
    }
  ]
}
END_REVIEW_JSON
