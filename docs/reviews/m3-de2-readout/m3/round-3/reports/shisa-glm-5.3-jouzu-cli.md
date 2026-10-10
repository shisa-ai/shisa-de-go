All primary files, the Python reference, the generator, the fixture test, and the supporting packages have been reviewed against the working tree (HEAD `b34c8e4`, one docs commit after `6b35ee2`). Read-only tools only; the validation listed in the scope was not re-run here.

# Review Report

## Summary

Reviewed the Go port at `/home/morpheus/data/dev/shisa/shisa-de-go` (M3 range `853305b..HEAD`, fix commit `6b35ee2`) against the Python reference at `/home/morpheus/data/dev/shisa/shisa-de` and its `docs/READOUT-DE2.md` contract. The round-2 fixes are correctly in place: the exported `CheckCodeBoundary` no longer self-deadlocks (the full exported-method sweep found no remaining wrapper-calls-locking-helper), the approval cache is keyed by code and token id, the tokenizer-generation counter gates every cache read and write, `ReadoutHTTPError` is nil-embed safe, and the zero-value `Readout` reports itself from every request-spending entry point. The replay gate compares what it claims and the `codes-late-downgrade` scenario remains discriminating. No HIGH or MEDIUM findings this round; the six findings are LOW — four documentation accuracy/completeness gaps in `docs/PORT.md` and two small code/test gaps.

## Findings

### LOW: PORT.md's Softmax paragraph overstates the match for the codebook-ordered prefix
- Severity: LOW
- Confidence: HIGH
- Location: docs/PORT.md, "Softmax sums in a fixed key order" paragraph
- Problem: The page says the sorted sum "matches Python for the codebook-ordered prefix the readout always passes", then confines the divergence to "Python's arithmetic in general". On CPython 3.12+ `sum()` is Neumaier-compensated, so even a codebook-ordered prefix is not bit-identical to Go's plain left-to-right loop. The `Softmax` doc comment in `readout.go` states this correctly ("It is not bit-identical on CPython 3.12+"); the PORT.md wording implies the prefix case is exact and only other orders differ.
- Recommendation: Align the PORT.md sentence with the `readout.go` comment: the port sums in the same order Python uses for a codebook-ordered prefix, but the result is still a relative ~1e-16 from Python's on CPython 3.12+.

### LOW: PORT.md mis-describes the Python client on a null `prompt_logprobs` entry value
- Severity: LOW
- Confidence: HIGH
- Location: docs/PORT.md, "stricter about three things" list, first bullet
- Problem: "A null entry at that position is an error too, where the Python client reads `None` through and reports the code as unreturned." For the shape the Go test pins (`prompt_logprobs: [null, {"65": null}]`), Python's `_letter_logprob` does `entry.get("logprob")` on a `None` entry and raises `AttributeError` — an unhandled crash, not "reports the code as unreturned". The shape that Python does report as unreturned (a null last position, `[null, null]`) behaves identically in this port, so there is no divergence to document there.
- Recommendation: Fix the sentence: a null last position is handled the same way by both clients (the code is reported as unreturned); a null entry value crashes the Python client with `AttributeError` where this port returns a `ReadoutError`.

### LOW: PORT.md's difference list is incomplete after the round-2 fixes
- Severity: LOW
- Confidence: MEDIUM
- Location: docs/PORT.md, "Deliberate differences" / "stricter" list
- Problem: The page's introduction claims it records "every place the Go client deliberately behaves differently", but two round-2 changes and one pre-existing divergence are unlisted:
  1. The boundary-approval cache is keyed by (code, token id) and re-checks when the id differs, where Python's `_boundary_checked` is a set keyed by letter alone — a hand-built `Slot` with a foreign token id passes Python's `check_code_boundary` and fails this port's.
  2. `list_models` error taxonomy: Python's `list_models` calls `raise_for_status()` (an `httpx.HTTPStatusError`, outside the `ReadoutError` hierarchy) and lets `response.json()` errors escape uncaught; this port returns `ReadoutHTTPError`/`ReadoutError` for the same responses.
  3. `SetTokenizer`'s discard-and-invalidate semantics (slots, approvals, generation counter) have no Python counterpart — worth a sentence since it is Go-only API surface with deliberate semantics.
- Recommendation: Add the three entries to the difference/stricter lists, or note explicitly that they are Go-only additions with no Python counterpart.

### LOW: `ReadCodesAfterThought` aliases the caller's token-id slice; Python copies it
- Severity: LOW
- Confidence: HIGH
- Location: readout.go (`idPrompt`, `renderedPrompt.tokenIDs`)
- Problem: `idPrompt(promptIDs)` stores the caller's slice without copying (only `nil` is replaced), so the whole read — every block's `prompt` field and the prompt-token count — reads the caller's buffer live. Python's `read_codes` does `list(prompt)` for the id form, so a Python read is immune to the caller mutating the list mid-read; a Go caller that reuses the buffer while a read is in flight changes the prompts the later blocks send. Nothing mutates the slice inside the package, so this is a defensive-copy gap, not a corruption bug.
- Recommendation: Copy the ids in `idPrompt` (or in `ReadCodesAfterThought`), matching Python's `list(prompt)`.

### LOW: the stale-generation branch of `checkCodeBoundaryFor` has no test
- Severity: LOW
- Confidence: MEDIUM
- Location: readout.go (`checkCodeBoundaryFor`, the `current == false` path); readout_test.go
- Problem: `TestSetTokenizerForgetsBoundaryApprovals` pins the half where `SetTokenizer` clears the approvals, but the half the round-2 fix actually added — a read that captured an older generation re-checks its codes and caches nothing — is reachable only through a genuine race and is exercised by no test. A regression that goes back to caching unconditionally (deleting the `current` guard on the write) would pass the whole suite, including `-race`; the race detector cannot see a logical cache-poisoning that involves no data race.
- Recommendation: Add an internal test (the tests are in-package): call `r.tokenizerSnapshot()`, then `r.SetTokenizer(...)`, then `r.checkCodeBoundaryFor(oldTok, oldGeneration, ...)` and assert that no approval was written — e.g. by following with a foreign-token-id slot for a code that would have been cached, which must still be re-checked. This is the exported behaviour with the weakest coverage; everything else exported on `Readout` has a direct or fixture test.

### LOW: `Readout.Render` cannot explicitly request `DirectSystem` per call when the readout carries a custom line
- Severity: LOW
- Confidence: MEDIUM
- Location: readout.go (`Readout.Render`)
- Problem: `RenderOptions.SystemPrompt` documents "Empty means DirectSystem" at package level, but `Readout.Render` replaces an empty value with `r.systemPrompt`. Through the method, `""` therefore means "the readout's configured line" for a custom-line readout, so a caller who reads the package documentation and passes `RenderOptions{}` expecting `DirectSystem` silently gets the configured line instead. There is no parity violation — Python's `render` has no per-call override at all — but the same field means two different things through the function and the method.
- Recommendation: Either document the override on `Readout.Render` ("empty means the readout's own line, and the package function is the only way to force `DirectSystem`"), or add an explicit way to request the default line.

## Things Checked

- **Deadlock sweep (scope question 1).** Every exported method on `Readout` was traced for the wrapper-calls-locking-helper class: `Slot`, `CodeSlots`, `AnswerPrefixID` take `r.mu` and call only `...Locked`/lock-free helpers; `CheckCodeBoundary` uses `tokenizerSnapshot` (lock and release) then `checkCodeBoundaryFor` (own lock); `readCodes` holds no lock across its requests; `Think` uses `ensureTokenizer` then only lock-free helpers; `ListModels`/`Render`/`ResolvedFamily` take no lock; `usesCandidateIDs`/`disableCandidateIDs` are self-contained. Every `...Locked` helper is called only with the lock held; `answerPrefixIDFor`, `checkPromptTokens`, `firstChoice`, `letterLogprob`, `httpClient` are lock-free and touch only immutable fields. The class is closed at present; each locking wrapper also has a calling test (`CheckCodeBoundary`, `Slot`, `CodeSlots`, `AnswerPrefixID`, `ReadCodes`, `Think`, `ListModels`, `SetTokenizer`), which is what makes a future regression loud.
- **Approval-cache interleavings (scope question 2).** `checkCodeBoundaryFor` computes `current` once under `r.mu` and holds the lock across the whole loop, so no `SetTokenizer` can change the generation mid-check. A read that lost the race re-checks every unapproved code against the tokenizer it captured and writes nothing; a current read's approvals are (code, id) pairs invalidated by the next swap. Stale slots handed to `ReadCodes` after a swap fail the per-code check against the new tokenizer rather than sending the old ids. No path was found that sends an id the current tokenizer never produced, skips a check it should run, or leaves an approval a later read trusts.
- **`ReadoutHTTPError` constructions (scope question 3).** `ReadoutHTTPError{}` as a value does not implement `error` (pointer receivers), so outside the package only pointer constructions exist: `&ReadoutHTTPError{}` and `&ReadoutHTTPError{Status: 503}` report a status-only message and unwrap to an untyped nil, so `errors.As` cannot return a nil `*ReadoutError`; the embed-set construction behaves as before. A nil `*ReadoutHTTPError` does not panic. The `logprob_token_ids` sniff still works: `doJSON` always builds the error with its embed, `errors.As` matches the type directly, and the 400/422 + detail-substring test matches Python's.
- **Zero-value `Readout` (scope question 3).** Every request-spending entry point reports an error: `ListModels`/`ReadCodes` via `httpClient` (tested), and `Slot`/`CodeSlots`/`AnswerPrefixID`/`CheckCodeBoundary`/`Think`/`ReadCodesAfterThought` via `ensureTokenizerLocked`, because `ResolveWith("")` refuses an empty source before the nil slot map could be written. `Render` succeeds (it needs neither endpoint nor tokenizer, and an empty configured line falls through to `DirectSystem`) and `ResolvedFamily` returns a zero family — neither panics.
- **Replay gate (scope question 4).** Method, path, the two client-set headers, JSON-decoded request bodies, all `CodeRead` fields (logprobs exactly, probabilities at 1e-12 relative with the NaN/Inf-rejecting `closeEnough`, requests, prompt tokens, missing codes, ranks, sampled, debug prompt and top-logprobs), all `Thought` fields, error class (including the "not also an HTTP error" check for `ReadoutError`), and request counts in both directions (server-side overrun check plus the after-step `replay.at` count) are all compared and all able to fail. `codes-late-downgrade` is still discriminating: the stub's `topk_score` differs from its candidate-id scores, so both the request count and the replaced logprob values pin the re-read-everything behaviour, and `codes-topk-rejected`'s second step pins the capability flag persisting across reads.
- **PORT.md claims (scope question 5).** The `token_ids` "both clients refuse" paragraph, the field-level decoding paragraph, the codebook provenance, and the header-recording paragraph were checked against the Python source; the Softmax paragraph and the null-fallback-entry sentence are the two that mis-state Python (findings 1 and 2), and the difference list is missing the round-2 divergences (finding 3). The Softmax sort claim itself holds: the codebook is A–Z then lexicographic pairs, so shortest-then-lexicographic equals codebook order for every prefix.
- **`Softmax` sum order:** verified the sorted order equals the codebook's own order for any codebook prefix, and that the readout always passes a codebook-ordered map (`readCodes` rebuilds `Logprobs` in option order before calling).
- **`hub.go` local-only policy:** `ResolveWith("")` errors, `LocalFilesOnly` yields `ErrNotCached` after the file and cache checks, `Lookup` never substitutes a different revision, `checkSource` blocks path traversal, and `TestReadoutLocalFilesOnly` pins the sentinel.
- **Parity spot-checks against Python `readout.py`:** the mid-read downgrade retry (whole-list re-read, `Sampled` only at `start == 0`), the id/text fallback key lookup including a present-but-null id key, the request-not-counted-on-rejection rule, `codes_for(0)`, the empty-slots read failing in `Softmax` the same way Python's `softmax({})` does, and `Think`'s first-occurrence trim of the closing token.

## Residual Risks

- A cold readout's first read holds `r.mu` across the tokenizer load, which can be a network fetch with a 10-minute timeout that is not context-cancelable; every other lock-taking call on that instance stalls behind it. This mirrors Python's shape (its tokenizer load holds a global init lock), so it is parity, not a regression — but it is worth knowing before M4 wires cancellation into the read paths.
- `checkCodeBoundaryFor` encodes `AnswerPrefix + code` for every unapproved code while holding `r.mu`; a 256-option first read serializes other reads behind those encodings. One-time per code per tokenizer generation.
- The fixture gate compares error messages only against short regex patterns and error class, not wording; a wording change in either client passes. That is the recorded intent, but it means message drift is invisible to the gate.
- The gate's probability comparison is 1e-12 relative, not exact, for the documented arithmetic reason; a change that moved a probability by less than that would pass. No measured value sits that close to the 0.7 gate (per the readout page's measurements, which are not this repo's).
- The `Readout` type doc says "every method" on a zero value returns an error; `Render` and `ResolvedFamily` in fact succeed (harmlessly — no request, no tokenizer), and `ResolvedFamily` reports a zero `Family`. A one-line doc narrowing to "every method that reads or spends a request" would make the claim true.
- No live validation was done and none is claimed: everything here is offline, fixture- or stub-based, per the scope's non-goals.

BEGIN_REVIEW_JSON
{
  "findings": [
    {
      "severity": "LOW",
      "title": "PORT.md Softmax paragraph overstates the codebook-prefix match",
      "location": "docs/PORT.md:Softmax paragraph",
      "recommendation": "State, as readout.go's Softmax comment does, that even a codebook-ordered prefix is not bit-identical on CPython 3.12+ (Neumaier-compensated sum); the port matches the sum order, not the arithmetic."
    },
    {
      "severity": "LOW",
      "title": "PORT.md mis-describes Python on a null prompt_logprobs entry value",
      "location": "docs/PORT.md:stricter list, first bullet",
      "recommendation": "Correct the sentence: a null last position is handled identically by both clients; a null entry value crashes the Python client with AttributeError where this port returns a ReadoutError."
    },
    {
      "severity": "LOW",
      "title": "PORT.md difference list incomplete after round-2 changes",
      "location": "docs/PORT.md:Deliberate differences",
      "recommendation": "Add the (code, token id)-keyed approval cache, the list_models error taxonomy divergence (httpx.HTTPStatusError vs ReadoutHTTPError), and SetTokenizer's invalidation semantics."
    },
    {
      "severity": "LOW",
      "title": "ReadCodesAfterThought aliases the caller's token-id slice",
      "location": "readout.go:idPrompt",
      "recommendation": "Copy the ids on entry, matching Python's list(prompt)."
    },
    {
      "severity": "LOW",
      "title": "Stale-generation branch of checkCodeBoundaryFor untested",
      "location": "readout.go:checkCodeBoundaryFor",
      "recommendation": "Add an in-package test that snapshots the tokenizer, swaps it, calls checkCodeBoundaryFor with the old generation, and asserts nothing was cached."
    },
    {
      "severity": "LOW",
      "title": "Readout.Render reinterprets an empty per-call SystemPrompt",
      "location": "readout.go:Readout.Render",
      "recommendation": "Document that empty means the readout's own line through the method, or provide an explicit way to request DirectSystem."
    }
  ]
}
END_REVIEW_JSON
