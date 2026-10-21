# Review Report

## Summary

The per-checkpoint records and ordinary DE-2/DE-2.1 selection paths match the Python implementation. However, the documented tokenizer override does not work for the deployment scenario in scope: `Model: "shisa-ai/shisa-de-2"` still selects and applies DE-2’s record even when `Tokenizer` explicitly names DE-2.1.

Additional issues affect explicit calibration-file resolution, the new live test, Unicode name-matching parity, and measurement attribution.

Reviewed the working tree in `/home/morpheus/data/dev/shisa/shisa-de-go`; its HEAD reference is `ecc90ff2abb73bd38653f6681423bea8f68dd272`. No files were modified and no tests or builds were run.

## Findings

### HIGH: The documented tokenizer override silently applies the wrong checkpoint’s calibration
- Severity: HIGH
- Confidence: HIGH
- Location: `calibration.go:516–530`; `client.go:688–703`; `README.md:310–326`
- Problem: `CalibrationForCheckpoint` returns the first recognized record, checking the model before the tokenizer. The README’s example supplies `Model: "shisa-ai/shisa-de-2"` and `Tokenizer: "shisa-ai/shisa-de-2.1"`, but this selects DE-2’s record. `Applicability` then recognizes the model as DE-2, returns `checkpoint`, and the constructor applies the record without warning. This reproduces exactly the wrong-fit condition the example claims to prevent. The Python implementation has the same behavior; matching it does not make this escape hatch functional. Existing tests cover an unrecognized `"alias"`, not an alias that is another released checkpoint’s ID.
- Recommendation: Immediately replace the ineffective example with the working explicit `Calibration` override and state that tokenizer fallback only works when the model ID does not identify a released checkpoint. Detect conflicting recognized model/tokenizer identities and require an explicit calibration or checkpoint assertion rather than silently reporting a checkpoint match. Add regression coverage for both conflict directions and coordinate the behavior with Python.

### MEDIUM: A calibration-file path containing a checkpoint slug is ignored
- Severity: MEDIUM
- Confidence: HIGH
- Location: `calibration.go:539–546`
- Problem: `ResolveCalibration` runs the permissive served-name matcher before attempting file loading. For example, `/fits/shisa-de-2.1/custom.json` contains a valid checkpoint token, so the function returns the embedded DE-2.1 record without opening `custom.json`. The same happens if the requested file does not exist. A caller explicitly selecting a deployment-specific fit can therefore receive a different temperature set without an error. This behavior is also present in Python.
- Recommendation: Separate checkpoint selectors from filesystem paths. Recognize exact supported checkpoint spellings as selectors, and load explicit paths as files without substring-based fallback. Test an existing custom record and a missing file beneath a directory named `shisa-de-2.1`; neither should resolve to the bundled record.

### MEDIUM: The new live test neither contacts the endpoint nor handles tokenizer-resolved aliases correctly
- Severity: MEDIUM
- Confidence: HIGH
- Location: `live_test.go:1294–1328`
- Problem: `liveModel` constructs a lazy client, and this test never makes a request. It can pass with an unreachable endpoint and cannot establish anything about the checkpoint actually served. Its expected checkpoint comes only from `liveModelID()`, ignoring `SHISA_DE_TOKENIZER`. Consequently, a supported configuration such as `SHISA_DE_MODEL=alias` with `SHISA_DE_TOKENIZER=shisa-ai/shisa-de-2.1` correctly applies calibration in the client but fails the test’s “names no released checkpoint” branch. Conversely, the recognized DE-2 alias from the first finding silently passes with DE-2’s record. Comparing `TemperatureFor` with the same record’s `Reads` table also does not verify temperatures on returned answers.
- Recommendation: Move constructor-only selection assertions into offline tests and include tokenizer fallback and conflicting identities. If retaining a deployment test, perform a real read, check the answer’s calibration identity and temperature against an independently specified expected checkpoint, and record the serving fingerprint. State explicitly that this verifies the operator’s checkpoint assertion, not hidden weight identity.

### LOW: Unicode lowercasing makes the matcher differ from Python
- Severity: LOW
- Confidence: HIGH
- Location: `calibration.go:74`; `calibration_test.go:488–518`
- Problem: The ASCII boundary checks match Python’s regex, but the preceding lowercase conversion does not match Python for every Unicode input. Go’s `strings.ToLower` maps `İ` (U+0130, capital I with dot) to `i`; Python’s `str.lower()` maps it to `i` followed by U+0307. Thus `ReleasedCheckpoint("SHİSA-DE-2.1")` recognizes DE-2.1 in Go, while Python does not. With an explicitly declared DE-2 family, this can change calibration from unapplied to applied. The current table does not exercise this case.
- Recommendation: Match Python’s lowercase semantics for checkpoint recognition, or explicitly document and conservatively handle the divergence. Add cases for U+0130 inside and adjacent to the slug, ordinary multibyte boundaries, and repeated slug occurrences.

### LOW: The DE-2.1 measurements are attributed to the Go client
- Severity: LOW
- Confidence: HIGH
- Location: `docs/READOUT-DE2.md:400–404`
- Problem: The new section says the measurements were collected “through this client with the same two scripts.” In the Go repository this attributes the measurements to the Go client, but the scope identifies the Python client as the collector and fitter and states that no Go live run against DE-2.1 occurred. The collection and fitting scripts named later are also Python-repository scripts. The subsequent “not reproduced here” qualification refers specifically to the research repository’s comparison and does not clearly qualify the calibration table.
- Recommendation: Attribute the measurements and scripts explicitly to the Python reference implementation and state that the Go port embeds its record without independently reproducing the DE-2.1 measurements. Preserve the recorded provenance in the copied JSON.

## Things Checked

- Read the review prompt and scope before inspecting the Go implementation.
- Inspected all primary files named in the scope, the CLI help renderer, relevant constructor code, and both readout contracts.
- Compared Go selection and applicability logic with `shisa_de/calibration.py`.
- Compared the displayed DE-2.1 record contents with the Python record; temperatures and provenance agree. Byte identity was not independently checked.
- Traced exact IDs, quantized names, local directories, unknown versions, training arms, short strings, repeated occurrences, ASCII boundaries, and Unicode lowercasing.
- Confirmed model-first selection matches Python, while conflicting model/tokenizer identities receive no warning.
- Confirmed an explicitly supplied `Calibration` remains a working override.
- Checked the new client-selection tests, tempering test, and live-test assertions.
- Confirmed the README now explicitly lists quote-delimiter normalization as absent. The broad “everything else” API-parity claim was not independently established through a complete public-API inventory.
- Found no reason in this change to bump `de2-codebook-v1`: checkpoint-specific calibration records do not change the shared readout contract.

## Residual Risks

A client cannot infer hidden weights reliably from a reused model ID or a shared tokenizer. Documentation plus an explicit calibration assertion can be sufficient for that information limit; a new override field is not strictly necessary because `Calibration` already supplies one. However, the documentation must identify the override that actually works, and detectable model/tokenizer conflicts should not silently become a claimed checkpoint match.

For the planned hosted migration, an endpoint-specific warning or deployment configuration requiring an explicit checkpoint assertion would provide stronger protection than a general README warning. Globally redefining `shisa-ai/shisa-de-2` as DE-2.1 would instead break legitimate DE-2 deployments.

The scope’s reported formatting, vet, test, and race-test results were not rerun. No shell tool was available, so commit-diff and working-tree cleanliness checks were not performed. Review conclusions are based on static inspection, not live endpoint evidence.

BEGIN_REVIEW_JSON
{
  "findings": [
    {
      "severity": "HIGH",
      "title": "The documented tokenizer override silently applies the wrong checkpoint's calibration",
      "location": "calibration.go:516-530; README.md:310-326",
      "recommendation": "Document the working explicit Calibration override, detect conflicting checkpoint identities, and add regression coverage for recognized aliases."
    },
    {
      "severity": "MEDIUM",
      "title": "A calibration-file path containing a checkpoint slug is ignored",
      "location": "calibration.go:539-546",
      "recommendation": "Separate exact checkpoint selectors from filesystem paths and test existing and missing files beneath checkpoint-named directories."
    },
    {
      "severity": "MEDIUM",
      "title": "The new live test neither contacts the endpoint nor handles tokenizer-resolved aliases correctly",
      "location": "live_test.go:1294-1328",
      "recommendation": "Move selection assertions offline and make deployment verification perform a real read against an independently declared expected checkpoint."
    },
    {
      "severity": "LOW",
      "title": "Unicode lowercasing makes the matcher differ from Python",
      "location": "calibration.go:74",
      "recommendation": "Match Python lowercase semantics or conservatively document the divergence, with Unicode and repeated-occurrence regression cases."
    },
    {
      "severity": "LOW",
      "title": "The DE-2.1 measurements are attributed to the Go client",
      "location": "docs/READOUT-DE2.md:400-404",
      "recommendation": "Attribute collection and fitting to the Python reference and state that the Go port has not reproduced the measurements."
    }
  ]
}
END_REVIEW_JSON
