# Review Report

## Summary

Reviewed the scoped implementation in `/home/morpheus/data/dev/shisa/shisa-de-go`, where the referenced files and commits reside. Found two live-gate validation gaps and one incomplete documentation-count check. No files were modified.

## Findings

### MEDIUM: Probe failures can incorrectly disable image coverage
- Severity: MEDIUM
- Confidence: HIGH
- Location: scripts/live_parity.py:170-175
- Problem: Every exception and every non-200 response becomes `images=False`. A transient timeout, HTTP 429/503, malformed probe request, or another incorrect URL therefore drops the image question. If subsequent text requests succeed, the gate passes with “the endpoint does not serve images,” although that conclusion was never established. The `api_url` correction fixes the identified URL construction bug, but not this failure classification. No offline test exercises `serves_images()` responses or exceptions.
- Recommendation: Skip only for an explicit, recognized unsupported-image response. Treat transport errors and unclassified HTTP failures as unsuccessful capability checks that fail the run. Add mocked tests covering supported images, explicit unsupported images, malformed requests, wrong paths, rate limits, server errors, and transport exceptions.

### MEDIUM: Missing reference render verdict silently disables comparison
- Severity: MEDIUM
- Confidence: HIGH
- Location: live_test.go:582-584; scripts/live_parity.py:297-298
- Problem: An empty expected verdict is treated as proof that the question is text-only. However, the recorder uses `.get("system_render")` for image questions, so a missing reference field becomes JSON `null`, which decodes into the Go string’s empty value. Both Python passes can consistently omit it, pass drift validation, and bypass comparison even when Go returns `"differs"`. The table test catches a missing Go verdict but not this reverse mismatch.
- Recommendation: Determine whether a verdict is required from `question.Image`, not from the recorded value. Require image recordings to carry `string`, `differs`, or `unverified`; reject absent, null, and empty verdicts. Add tests for those cases.

### LOW: The render fixture count remains unchecked in its parity paragraph
- Severity: LOW
- Confidence: HIGH
- Location: readme_test.go:349-352
- Problem: The render pattern checks “compares 35 rendered prompts” in the earlier renderer discussion, but not “**Render.** All 35 fixture renders” under Verified parity. A regeneration can update the metadata and first paragraph while leaving the second count stale, and `TestPortDocFixtureCounts` still passes. This retains the documentation drift the new test is intended to prevent.
- Recommendation: Check both render-count claims, or keep the numeric count in one place and remove its duplicate.

## Things Checked

- `api_url` matches the Python client’s trailing-slash and `/v1` normalization.
- `liveQuestion.call()` forwards image, policy, and probability settings. The client selects the image read before the text-policy branch.
- Both new client fixture cases carry real chat exchanges. The debug case records `prompt: null`, `top_logprobs`, messages, and `system_render`; replay compares the complete `Raw` block.
- Regenerating all cases is consistent with one recording date and fingerprint. Metadata records 26 cases and 37 exchanges; integrity checks compare the digest, counts, fingerprint, and response dates.
- Failing when a documentation pattern stops matching is appropriate; it prevents silently checking nothing.
- The reviewed documentation no longer asserts that the hosted endpoint is text-only or that image coverage is absent.

## Residual Risks

- This was static, read-only inspection. No tests, builds, live requests, or digest computations were executed.
- Validation results supplied in the scope were not independently reproduced.
- No shell tool was available to inspect commit diffs or working-tree status.

BEGIN_REVIEW_JSON
{
  "findings": [
    {
      "severity": "MEDIUM",
      "title": "Probe failures can incorrectly disable image coverage",
      "location": "scripts/live_parity.py:170-175",
      "recommendation": "Skip only for explicit unsupported-image responses; fail on unclassified probe errors and test the classifications offline."
    },
    {
      "severity": "MEDIUM",
      "title": "Missing reference render verdict silently disables comparison",
      "location": "live_test.go:582-584",
      "recommendation": "Require a valid recorded system_render for every image question and test absent, null, and empty verdicts."
    },
    {
      "severity": "LOW",
      "title": "The render fixture count remains unchecked in its parity paragraph",
      "location": "readme_test.go:349-352",
      "recommendation": "Check both documented render counts or remove the duplicate numeric claim."
    }
  ]
}
END_REVIEW_JSON
