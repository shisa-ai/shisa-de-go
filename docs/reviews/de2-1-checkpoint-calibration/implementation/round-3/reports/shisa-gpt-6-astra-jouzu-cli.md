# Review Report

## Summary

Reviewed the accumulated checkpoint-calibration implementation in `/home/morpheus/data/dev/shisa/shisa-de-go`, including the round-3 fixes, relevant tests, documentation, and Python reference.

Found three MEDIUM issues and one LOW documentation issue. Missing relative paths can still select bundled records silently; the supplied-family-record warning misses the documented shared-tokenizer configuration; and conservative Unicode rejection can change which checkpoint wins when a name contains both release slugs.

The thinking-read mapping and `doctor` warning forwarding are correct by inspection. No files were modified or tests executed.

## Findings

### MEDIUM: Missing relative record paths can still resolve to bundled calibration
- Severity: MEDIUM
- Confidence: HIGH
- Location: calibration.go:614–658
- Problem: `looksLikeRecordPath` recognizes extensionless paths only when they have one of four prefixes or already exist as a non-directory. A missing relative path such as `fits/shisa-de-2.1/deployment` satisfies neither condition. `ResolveCalibration` then passes it through `ReleasedCheckpoint` and returns the bundled DE-2.1 record successfully instead of reporting the missing deployment fit. An existing directory with that spelling takes the same fallback. This preserves the silent wrong-record behavior the path fix is intended to prevent.
- Recommendation: Restrict the checkpoint-spec branch to explicitly supported checkpoint-name forms rather than arbitrary embedded slugs, and treat other path-shaped specs as files. Add regression cases for missing extensionless relative paths without `./`, and for directories containing a checkpoint slug. Preserve the documented canonical and quantized checkpoint spellings.

### MEDIUM: Shared tokenizer suppresses the supplied-family-record warning
- Severity: MEDIUM
- Confidence: HIGH
- Location: client.go:726–730; calibration.go:414–417
- Problem: Configure `Model: "shisa-ai/shisa-de-2.1"`, `Tokenizer: "shisa-ai/shisa-de-2"`, and supply the record returned by `CalibrationFor(FamilyDE2)`. This is the documented DE-2.1 tokenizer pairing combined with the mistaken family-level calibration selection the warning targets. `Applicability` reports `checkpoint` because the supplied DE-2 record matches the tokenizer. Consequently, the `level == ApplicabilityFamily` guard suppresses the family-record warning. The disagreement warning is also suppressed because a record was supplied. DE-2's temperatures are therefore applied to DE-2.1 without either warning, and `doctor` reports `match checkpoint` / `applied yes`.
- Recommendation: Determine this warning from the supplied bundled-family record and the model-first checkpoint selection, independently of the applicability level obtained from either name. Preserve explicit-record application semantics. Add a regression test with DE-2.1 as the model, DE-2 as the tokenizer, and the supplied DE-2 family record, including the resulting `doctor` warning.

### MEDIUM: Conservative Unicode boundary rejection can select a different checkpoint
- Severity: MEDIUM
- Confidence: HIGH
- Location: calibration.go:112–119, calibration.go:174–175
- Problem: The same boundary predicate is used before and after a slug, but Python's full lowercase expansion is asymmetric at those positions. U+0130 becomes `i` followed by U+0307. Immediately **before** a slug, Python sees the combining dot as the adjacent character and permits the match; Go tests `unicode.ToLower(U+0130) == 'i'` and rejects it.

  Rejecting that occurrence alone would be conservative, but continuing to a shorter checkpoint is not. For `İshisa-de-2.1/shisa-de-2`, Python selects DE-2.1. Go rejects the DE-2.1 occurrence and then selects DE-2 from the later slug. Automatic selection and applicability agree on DE-2, so its calibration is applied. This contradicts the documented implication that conservative rejection merely leaves the answer raw.
- Recommendation: Make the boundary check directional: account for the final character of the preceding rune's full lowercase expansion and the first character of the following rune's expansion. Alternatively, ensure an uncertain longer-checkpoint match cannot fall through to a different checkpoint. Add this combined Unicode/multiple-slug case to the regression table.

### LOW: Live-check documentation still offers the tokenizer as an unrestricted alias remedy
- Severity: LOW
- Confidence: HIGH
- Location: docs/PORT.md:1003–1008
- Problem: The live-gate section says to point the test at “the alias with `SHISA_DE_TOKENIZER` naming the checkpoint behind it,” without restricting that advice to aliases that name no released checkpoint. With `SHISA_DE_MODEL=shisa-ai/shisa-de-2` and `SHISA_DE_TOKENIZER=shisa-ai/shisa-de-2.1`, the test expects DE-2, the client applies DE-2's record, and the test can pass even when the endpoint actually serves DE-2.1. The corresponding comment in `live_test.go` correctly states the restriction, but this operational guidance does not.
- Recommendation: Add the same restriction to `PORT.md`. State that this test cannot validate changed weights behind another released checkpoint's id; use the actual checkpoint id for this check. Keep the explicit `Calibration` remedy for application configuration separate, since this live helper does not accept that override.

## Things Checked

- Model-first checkpoint selection, tokenizer fallback, family fallback, explicit-record application, and applicability reporting.
- Canonical ids, quantized names, local paths, unsupported versions, ASCII case handling, Unicode boundaries, and multiple-slug precedence.
- DE-2.1 record temperatures and provenance against the Python reference record.
- Warning guards, the supplied-record tests, and `doctor` stderr forwarding. `doctor` writes each library warning unchanged apart from its command prefix and newline.
- `repeat2-think` mapping to the `think` temperature table, matching the client's last-stage calibration lookup.
- README alias guidance in both locations, `PORT.md` adaptations, and both readout contracts.
- Regression coverage in `calibration_test.go`, `client_test.go`, `cmd/shisa-de/main_test.go`, and the relevant live test.

## Residual Risks

- This was static, read-only inspection. The validation results listed in the scope were not independently rerun.
- No shell tool was available, so the commit range and working-tree status could not be verified with Git. Findings refer to the files present during review.
- No DE-2.1 endpoint was queried. Response-level checks cannot establish which weights are behind a reused served-model id.
- The Unicode analysis identifies a concrete checkpoint-selection counterexample; it is not an exhaustive Unicode parity audit.

BEGIN_REVIEW_JSON
{
  "findings": [
    {
      "severity": "MEDIUM",
      "title": "Missing relative record paths can still resolve to bundled calibration",
      "location": "calibration.go:614-658",
      "recommendation": "Restrict checkpoint specs to supported name forms and reject missing relative record paths and directories instead of matching embedded checkpoint slugs."
    },
    {
      "severity": "MEDIUM",
      "title": "Shared tokenizer suppresses the supplied-family-record warning",
      "location": "client.go:726-730",
      "recommendation": "Detect a supplied bundled-family record conflicting with model-first checkpoint selection independently of tokenizer-derived applicability, and cover the shared-tokenizer configuration."
    },
    {
      "severity": "MEDIUM",
      "title": "Conservative Unicode boundary rejection can select a different checkpoint",
      "location": "calibration.go:112-119",
      "recommendation": "Use directional full-lowercase boundary semantics or prevent uncertain longer-checkpoint matches from falling through to another checkpoint; test İshisa-de-2.1/shisa-de-2."
    },
    {
      "severity": "LOW",
      "title": "Live-check documentation still offers the tokenizer as an unrestricted alias remedy",
      "location": "docs/PORT.md:1003-1008",
      "recommendation": "Restrict tokenizer-based alias guidance to ids that name no released checkpoint and document that the live test cannot validate changed weights behind a released-id alias."
    }
  ]
}
END_REVIEW_JSON
