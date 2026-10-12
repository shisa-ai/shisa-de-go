# Review Report

## Summary

The image read implementation is broadly aligned with the Python reference. The request shape, image preparation, option limits, fallback refusal, system-render detection, CLI wiring, and offline tests are covered.

I found two implementation/documentation issues and two test-coverage gaps.

## Findings

### MEDIUM: `Answer.SystemRender` is documented but not implemented
- Severity: MEDIUM
- Confidence: HIGH
- Location: `README.md:208`, `docs/PORT.md:45`, `client.go:50-90`, `client.go:1240-1260`
- Problem: The documentation says that `Answer.SystemRender` records whether the server rendered the system turn as expected. `Answer` has no `SystemRender` field. The value is stored only in `Decision.Raw[head]["system_render"]`. Code using the documented typed answer field will not compile, and callers inspecting `Decision.Answers` cannot access the value without parsing `Raw`.
- Recommendation: Either add and populate an exported `Answer.SystemRender` field, or change the documentation to state that the value is available only in `Decision.Raw[head]["system_render"]`. Keep the Python-compatible raw shape if that is intentional.

### LOW: Documentation says image probabilities are always uncalibrated
- Severity: LOW
- Confidence: HIGH
- Location: `README.md:190-201`, `docs/PORT.md:53-56`
- Problem: The documentation says image reads return uncalibrated probabilities and use “no calibration.” The implementation defaults image reads to raw probabilities, but an explicit `CallOptions.Calibrated` value of `true` enables calibration when the calibration record applies. This matches the Python client’s behavior.
- Recommendation: Change the wording to “uncalibrated by default” and document that callers can explicitly request calibration with `Calibrated: boolPointer(true)` or the equivalent public API.

### MEDIUM: An image read can mix tokenizer generations during `SetTokenizer`
- Severity: MEDIUM
- Confidence: HIGH
- Location: `readout.go:1002-1066`, `readout.go:1090-1110`, `readout.go:1220-1260`
- Problem: `ReadImage` resolves option slots with `CodeSlots`, resolves the answer-prefix token separately with `AnswerPrefixID`, and later calls `systemRender`, which obtains the tokenizer again. A concurrent `SetTokenizer` can occur between these operations. The read can then validate the server boundary with one tokenizer and compare system rendering with another. This conflicts with the readout’s stated guarantee that an in-flight read keeps one tokenizer and with the general concurrent-use contract.
- Recommendation: Capture one tokenizer and generation at the beginning of `ReadImage`, resolve the slots and answer-prefix ID against that tokenizer, and pass the same snapshot into system-render comparison. Do not cache derived state if the generation changed, as the text path already does.

### LOW: The parity fixture does not cover image boundary failures or enforce image-scenario coverage
- Severity: LOW
- Confidence: HIGH
- Location: `scripts/gen_readout_parity.py` image scenarios, `testdata/readout_parity.meta.json`, `readout_test.go:145-190`
- Problem: The replay fixture covers missing option letters, system-render differences, unverified rendering, and malformed top logprobs, but it does not record an image response with a missing or incorrect final boundary token. Those cases are covered by hand-written tests, but the Python-to-Go parity fixture does not verify them. The metadata check validates only the fixture’s self-reported digest, scenario count, and exchange count; a regenerated fixture that omitted boundary cases would still pass.
- Recommendation: Add recorded image scenarios for missing and incorrect answer-boundary tokens. Include named scenario or error-category counts in the metadata and assert them in `TestReadoutFixtureProvenance`.

## Things Checked

- `images.go` against `shisa_de/images.py`, including URL validation, path normalization, suffix handling, byte-signature detection, data URL construction, and error messages.
- `Readout.ReadImage` request fields, message ordering, option limits, top-logprob arithmetic, no-fallback behavior, response decoding, and boundary checks.
- `systemRender` and `ProbeChatRender`, including `string`, `differs`, and `unverified` outcomes.
- Client image preparation order, default calibration behavior, wide-choice refusal, one-read accounting, raw result shape, and metadata.
- CLI image flags, default top-logprob handling, image-only state behavior, and doctor probe output.
- Image and readout parity fixtures, fixture metadata checks, and focused image tests.
- `README.md`, `docs/PORT.md`, and `docs/READOUT-DE2.md` claims about image reads, calibration, option limits, and system rendering.

## Residual Risks

- The live image path remains unverified against a real vision-enabled vLLM endpoint.
- The boundary check relies on the server’s root-level `prompt_logprobs`; a server that reports incomplete prompt positions could still pass if its final reported position is the terminator.
- The documented system-render measurement does not establish whether the extra system-turn token changes model accuracy.

BEGIN_REVIEW_JSON
{
  "findings": [
    {
      "severity": "MEDIUM",
      "title": "`Answer.SystemRender` is documented but not implemented",
      "location": "README.md:208; client.go:50-90",
      "recommendation": "Add and populate the field, or document that system_render is available only in Decision.Raw."
    },
    {
      "severity": "LOW",
      "title": "Documentation says image probabilities are always uncalibrated",
      "location": "README.md:190-201",
      "recommendation": "Document raw probabilities as the default and explain the explicit calibration override."
    },
    {
      "severity": "MEDIUM",
      "title": "An image read can mix tokenizer generations during SetTokenizer",
      "location": "readout.go:1002-1066",
      "recommendation": "Use one tokenizer and generation snapshot throughout ReadImage and its system-render comparison."
    },
    {
      "severity": "LOW",
      "title": "Parity fixtures do not cover image boundary failures",
      "location": "scripts/gen_readout_parity.py; readout_test.go:145-190",
      "recommendation": "Record missing and incorrect image-boundary scenarios and assert their presence in fixture metadata."
    }
  ]
}
END_REVIEW_JSON
