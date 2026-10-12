# Review Report

## Summary

Scope reviewed: m7-image-live-coverage, commits ff24434 and b2c0866, in `/home/morpheus/data/dev/shisa/shisa-de-go` (the scope's file names resolve there, not in the Python repository the prompt sits in). I reviewed the vision probe and `api_url` in `scripts/live_parity.py` against the Python client's own endpoint normalization, the gate's image question and skip path in `live_test.go`, `liveSystemRenderProblem` and its table test, the question-set tests in `scripts/test_live_parity.py`, the two image cases in `scripts/gen_client_parity.py` and `client_parity_test.go`, `TestPortDocFixtureCounts` in `readme_test.go`, and the doc claims in `docs/PORT.md`, `AGENTS.md` and `README.md`.

The core work is sound. The `api_url` fix is complete: it reproduces `DecisionModel.__init__`'s normalization exactly (`rstrip("/")`, then one `/v1` strip, `client.py:321-323`) and is table-tested for the four base shapes. The gate's skip cannot be caused by a Go-side request bug — the Go side skips purely on the recorder's measured `meta.Images`. The client parity fixture's regeneration is the right call and its provenance is honest: one recorded date (2026-10-12), one fingerprint, a digest, and the test additionally checks the response bodies' own `created` dates and fingerprints against the provenance. The doc-count test's "reads nothing" failure is the correct behaviour and fails loudly rather than silently. I found two medium findings — the probe reports every failure class as "does not serve images", and the live-gate paragraph in `docs/PORT.md` still describes the nine-question set — and two low ones.

## Findings

### MEDIUM: the vision probe reports rate limits, auth failures and transport errors as "the endpoint does not serve images"
- Severity: MEDIUM
- Confidence: MEDIUM
- Location: scripts/live_parity.py:139-176 (serves_images), live_test.go:534 (the skip)
- Problem: `serves_images()` returns `(False, reason)` for every non-200 and every transport exception, and the Go gate then skips the image case with `t.Skipf("the endpoint does not serve images: %s", meta.ImagesReason)`. A 429 (rate limit — plausible on the hosted endpoint), a 401/403, a 503, a timeout, or a transient network error therefore silently removes the gate's image coverage while the run passes, framed as a property of the deployment. `AGENTS.md` says "a skip here is a statement about the deployment"; a 429 is not. The reason string does carry the status and body, so a reader can diagnose it, but nothing fails and nothing distinguishes "cannot" from "would not right now". Relatedly, the probe is a third hand-rolled request shape, sparser than the real read (no `logprobs`/`top_logprobs`/`prompt_logprobs`/`chat_template_kwargs`), so a server that rejects the probe's shape but accepts the read's would also be a false negative — harder to construct with vLLM, but the URL bug this milestone fixed was exactly this class.
- Recommendation: classify the failure. Only a capability rejection (a 4xx/501 whose body the server wrote) should read as "does not serve images"; auth, rate-limit, 5xx and transport failures should make the recorder exit non-zero (or record a distinct reason the Go test refuses to turn into a skip). Consider building the probe from the client's own machinery — a one-option `Readout.read_image` with the error caught — so the probe's shape cannot drift from the read's.

### MEDIUM: `docs/PORT.md`'s live-gate paragraph still enumerates the nine-question set and cites only the 2026-10-10 measurement
- Severity: MEDIUM
- Confidence: HIGH
- Location: docs/PORT.md:851-864 (the Live gate bullet), AGENTS.md:296-298
- Problem: The bullet enumerates the fixed set as "a noul, a three-option choice, a choice with unnamed options, an ordered score, a 27-option choice, a 256-option choice, the same choice read under `direct`, the same score asked for the one-read probability view, and a choice whose gate is 1.0 so it thinks" — nine questions, no image — and states "over nine questions against `vllm-0.30.0-tp2-ca69195e` on 2026-10-10". The set now holds ten, including `image-color`, and the milestone's validation run measured all ten (deltas 2.2e-19 to 5.6e-16). The same bullet was edited in this milestone (its last paragraphs name `image-color` and the Client bullet above was updated), so the enumeration and the measurement were left stale rather than deliberately preserved. `AGENTS.md:296-298` carries the same 2026-10-10 nine-question measurement as "the port's own live measurements". The ten-question measurement this milestone produced is recorded in no durable document, which is the exact drift class the milestone's doc-count work exists to prevent — and the image gate claim in `AGENTS.md:309` has no date or fingerprint beside it, against the repository's own evidence rules.
- Recommendation: Update the enumeration to ten, and either refresh the measurement sentence (ten questions, new date, the 2.2e-19–5.6e-16 range, same fingerprint) or state the new measurement beside the old one. Do the same for the `AGENTS.md` sentence, and give the `image-color` coverage claim its date and fingerprint.

### LOW: `liveSystemRenderProblem`'s empty-string sentinel conflates "not an image question" with "no verdict recorded"
- Severity: LOW
- Confidence: MEDIUM
- Location: live_test.go:575-598 (liveSystemRenderProblem), live_test.go:273-278 (SystemRender field), scripts/live_parity.py:236-237 (the recording), live_test.go:496 (the unstable message)
- Problem: `want == ""` means both "this was a text question" and "the recording carried no verdict". The recorder writes `recorded["system_render"] = result.raw[head].get("system_render")` for an image question; if the Python side ever wrote `null` or omitted the key, Go's decoder leaves `SystemRender` as `""` and the comparison silently passes for an image question — the mirror of the case the table test pins (a recorded verdict against an answer carrying none). Today this is unreachable: Python's `read_image` always sets `system_render` from `_system_render`, which always returns one of three strings, and Go's `systemRenderFor` likewise never returns `""` on a successful read. But the gate knows which questions are image questions from `live_questions.json` and could pass that explicitly instead of relying on the recording's key. Two smaller wording items in the same area: a `system_render` flip between the recorder's passes lands in `meta.Unstable`, whose message says the passes "disagree on the chosen value", which a render flip is not; and the flip has no effect on `self_delta`, so only the `Unstable` check catches it.
- Recommendation: Pass "is an image question" from the set (`question.Image != ""`) into the comparison rather than inferring it from the recorded string, or have the recorder write a presence marker. Reword the unstable message to "disagree on the chosen value or the system render".

### LOW: the doc-count test guards four fixtures; the tokenizer, pyjson, repr and images-file counts in the same prose are unguarded
- Severity: LOW
- Confidence: HIGH
- Location: readme_test.go:319-363 (portFixtureCounts / TestPortDocFixtureCounts)
- Problem: `TestPortDocFixtureCounts` reads the readout, client, images and render counts out of `docs/PORT.md` and compares them with `testdata/*.meta.json`. The Verified-parity section states several more numbers whose sources are metadata files the same test could read: the tokenizer's "1,056 strings, 48,392 token ids" and "83 decode outputs" (`tokenizer_parity.meta.json` records `strings: 1056`, `ids: 48392`, `decode_cases: 83`), pyjson's "264 more", the repr fixture's "32,509 sampled code points", and the images fixture's "files: 14". These can drift exactly as the readout's 22/62 did — the stale readout paragraph is what this test was built to prevent, and the tokenizer paragraph is the next-most-likely regeneration to move. The four chosen rows are also not marked as a deliberate subset anywhere. The regexes themselves are sound: each is anchored well enough for the current prose, and a reword that stops matching fails loudly as "this check reads nothing" rather than passing, which is the right failure direction.
- Recommendation: Add rows for `tokenizer_parity.meta.json` (strings, ids, decode_cases) and the other counted claims, or state in the table's comment which prose numbers are deliberately unguarded and why.

## Things Checked

- **The probe's URL fix.** `api_url` reproduces the client's own normalization — `DecisionModel` does `rstrip("/")` then one `/v1` strip (`client.py:321-323`), `Readout` re-rstrips, and the relative-path join appends — for the default base, a base ending in `/v1`, a trailing slash, and `/v1/`. All four are table-tested in `ApiUrlCase`, and `test_the_gate_and_the_recorder_read_the_same_variables` pins the shared environment resolution. The fix is complete for the shapes the client accepts.
- **The probe's request shape.** The probe's messages match `Readout._messages` exactly (system as a string, user as parts with `image_url` before the text); only the sampling fields differ from the real read. A 200-with-error-body false positive surfaces loudly: the recorder's transport `SystemExit`s on non-200 and a failing Python image read aborts the recording, so the Go test fails rather than skipping.
- **The gate's skip path.** The Go side skips only on `meta.Images`, which only the recorder's probe sets; a Go-side image request bug cannot produce a skip. The `asked` count, the `ImagesReason` presence/absence invariants, the model and endpoint identity checks, the digest, and the fingerprint comparison across all gate clients (including the per-gate model's own recorder) are all in place. The skip cannot be reached by a Go port defect.
- **`liveSystemRenderProblem`.** The table test covers the mismatch, the missing key, a non-string value and a nil raw; Go's `systemRenderFor` always returns a verdict on a successful image read, and `imageRaw` writes the key whenever the field is non-empty, so the "answer carries none" direction is caught. The remaining gap is the recording-side "" sentinel (the LOW above).
- **Image selection under overrides.** In both clients, a non-empty image selects the image branch before the policy is consulted: Go's `runOne` checks `imageURL != ""` first (`client.go:1143-1152`), and Python's `_run` does the same. `liveQuestion.call()` puts `Image` on `CallOptions`, and `TestLiveQuestionSetHasAnImageCase` pins that. A future image question carrying `policy` or `probability` would take the image read in both clients identically; `probability=true` with an image is not refused in either.
- **The client parity fixture's regeneration.** Regenerating all 26 cases in one session is the honest call: the checkpoint is non-deterministic across sessions (documented in `AGENTS.md`), so keeping 24 old recordings under a fresh date would have mixed two sessions into one provenance. The meta carries one date (2026-10-12), one fingerprint, one tokenizer revision, a sha256; `fixtureProblems` additionally verifies the responses' own `system_fingerprint` and `created` dates against the provenance, and `TestFixtureIntegrityChecksCanFail` shows each check can fail. The two image cases cover both entry points, the `described` head type, debug shapes (`prompt` null, `top_logprobs`, `messages`) and the whole `Raw` block against a real exchange.
- **`TestPortDocFixtureCounts`.** All four patterns match the current prose and the current meta files (38/77, 26/37, 55, 35), and the meta field names line up (`images_parity.meta.json` records `cases`). The "reads nothing" `t.Fatalf` is the right behaviour per `AGENTS.md`'s stated design.
- **Doc corrections.** `README.md` carries no stale image claims (the stale-phrase guard in `TestReadmeNamesTheDeliberateAbsences` enforces it); `live_test.go`'s header describes the image question and the skip; `AGENTS.md`'s evidence rules now state the image read is live-covered and that skips are measured, with the M7 `Answer.SystemRender` history recorded. The stale statements are the live-gate paragraph and the measurement sentence (the MEDIUM above).
- **Recorder logic.** `drift` covers every consecutive pair, keeps the worst delta, and folds a `system_render` flip into `unstable`; exit 3 is classified separately from other failures and drives the inconclusive verdict rather than a recording failure.

## Residual Risks

- I ran nothing: this was a read-only review, and the scope's validation (gofmt, vet, both Go suites, the Python recorder tests, the live ten-question run, the 26-case replay) is taken from the scope file, not reproduced. The live deltas and the fixture's replay pass are the author's measurements.
- `serves_images` itself has no offline test (it needs `httpx` and a socket); only its payload and URL are tested. A false negative from a shape-rejecting server would first surface as a skip with the server's answer in the reason, not as a failure.
- The pre-existing 24 cases' regenerated numbers were not compared against the old recordings beyond the fingerprint, which the scope already lists as a non-goal; the replay passing is the check that matters.
- The image probe is sent even when the question set carries no image question, spending one request; a future set without image questions would pay for a probe it does not use.

BEGIN_REVIEW_JSON
{
  "findings": [
    {
      "severity": "MEDIUM",
      "title": "vision probe reports rate limits, auth failures and transport errors as \"does not serve images\"",
      "location": "scripts/live_parity.py:139-176",
      "recommendation": "Only a capability rejection should read as \"does not serve images\"; make auth, rate-limit, 5xx and transport probe failures fail the recorder (or a reason the Go test refuses to skip on), and build the probe from the client's own request machinery."
    },
    {
      "severity": "MEDIUM",
      "title": "docs/PORT.md live-gate paragraph still enumerates the nine-question set and the 2026-10-10 measurement",
      "location": "docs/PORT.md:851-864",
      "recommendation": "Update the enumeration to ten questions and record the ten-question measurement (date, fingerprint, delta range) here and in AGENTS.md's live-measurement sentence."
    },
    {
      "severity": "LOW",
      "title": "liveSystemRenderProblem's empty-string sentinel conflates \"not an image question\" with \"no verdict recorded\"",
      "location": "live_test.go:575-598",
      "recommendation": "Pass \"is an image question\" from the question set instead of inferring it from the recorded string, and reword the unstable message to cover a system-render flip."
    },
    {
      "severity": "LOW",
      "title": "doc-count test guards four fixtures while the tokenizer, pyjson, repr and images-file counts in the same prose are unguarded",
      "location": "readme_test.go:319-363",
      "recommendation": "Add rows for tokenizer_parity.meta.json and the other counted claims, or state the deliberate exclusions in the table's comment."
    }
  ]
}
END_REVIEW_JSON
