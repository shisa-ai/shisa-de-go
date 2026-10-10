# Review Report

## Summary

Reviewed the Go port at `/home/morpheus/data/dev/shisa/shisa-de-go` (range `853305b..HEAD`, fix commit `29049db`) against the Python reference at `/home/morpheus/data/dev/shisa/shisa-de` and its `docs/READOUT-DE2.md` contract.

Most of `29049db` lands correctly. I verified each round-1 fix against the Python source: the `RequireDE2` guard is complete and its resolution order matches `resolve_family`; the mid-read downgrade (`block = slots` when the capability is off) is an exact match to Python's `slots[start:start + CANDIDATE_ID_LIMIT] if self._candidate_ids else slots`, including `start` accounting, request counting, and the `Sampled`-at-start-0 rule; `TokenIDList` rejects every non-integer shape Go's decoder would coerce (null, fractional, string, bool, nested, out-of-range); the local-only policy never reaches `Fetch`; the `ReadoutHTTPError` taxonomy works with `errors.As` in both directions without disturbing the `logprob_token_ids` sniff; and the `codes-late-downgrade` scenario genuinely distinguishes the two behaviours (its top-k scores differ from its candidate-id scores, so a client that re-read only the remaining block fails either the request-body comparison or the logprobs comparison).

The round-1 fix for `SetTokenizer` introduced one regression: the exported `CheckCodeBoundary` now self-deadlocks, because `checkCodeBoundaryFor` acquired its own lock in the same commit that left the exported wrapper holding it. No test covers the exported method, which is why the suite still passes. A second, smaller hole remains in the same fix: an in-flight read can write boundary approvals validated against the old tokenizer into the just-cleared cache, which later reads under the new tokenizer then trust.

## Findings

### HIGH: `CheckCodeBoundary` self-deadlocks on the readout mutex
- Severity: HIGH
- Confidence: HIGH
- Location: readout.go:530-545
- Problem: `CheckCodeBoundary` (readout.go:531) takes `r.mu.Lock()`, then calls `checkCodeBoundaryFor` (readout.go:544), which takes `r.mu.Lock()` again. `sync.Mutex` is not reentrant, so the second `Lock` blocks forever while the first is held. Every call to the exported method hangs, and because it holds `mu` indefinitely it also wedges every other read on that `Readout`. This is a regression from `29049db` item 7: before the fix, the boundary helper relied on the caller's lock; the fix gave the helper its own lock (needed when called from `readCodes`, which does not hold it) but left the exported wrapper locking too. The comment on `checkCodeBoundaryFor` — "which the caller already holds" — is only true of the `readCodes` call site. No test calls `CheckCodeBoundary`, which is how the deadlock escaped `go test -race ./...`.
- Recommendation: Drop the lock from the exported wrapper: `CheckCodeBoundary` should call `ensureTokenizer` (which locks and releases) and then `checkCodeBoundaryFor`, exactly like `readCodes` does. Add a unit test that calls `CheckCodeBoundary` on a stubbed readout — it currently hangs, so any test at all catches this.

### MEDIUM: An in-flight read can poison the boundary-approval cache for a swapped-in tokenizer
- Severity: MEDIUM
- Confidence: HIGH
- Location: readout.go:541-567 (write at 567), against `SetTokenizer` at readout.go:413-419
- Problem: `SetTokenizer` clears `slots` and `boundaryChecked` under `mu`, and a read in flight keeps the tokenizer it captured — but when that read reaches `checkCodeBoundaryFor`, it re-checks each code against its *old* tokenizer and writes `r.boundaryChecked[slot.Code] = true` under the lock, after the clear. The cache now holds approvals validated against tokenizer A while `r.tokenizer` is tokenizer B. A subsequent read under B finds `boundaryChecked[code]` true (readout.go:557) and skips the per-code check, so a code whose boundary moves under B — exactly the tokenizer-mismatch case the check exists to catch (the message says "the tokenizer does not match the served model") — passes unvalidated. This defeats the fix's own documented invariant ("keeping them would let a read tokenize the prompt with one and validate the codes against another"). The prompt-end half is safe (it re-checks the terminator against the current read's tokenizer every time); only the cached per-code half is affected. Reachable only through a `SetTokenizer` concurrent with a read, which limits severity — but `SetTokenizer` is an exported method and `TestReadoutConcurrentReads` exercises the concurrency claim without covering this interleaving.
- Recommendation: In `checkCodeBoundaryFor`, only trust and only write `boundaryChecked` when `tok` is still the readout's current tokenizer (compare `tok` against `r.tokenizer` under the lock). A read holding a replaced tokenizer then re-checks its codes without caching, and later reads under the new tokenizer find no stale approvals.

### LOW: A caller-constructed `ReadoutHTTPError` with a nil embedded error panics in `Error()`
- Severity: LOW
- Confidence: HIGH
- Location: endpoint.go:34-46
- Problem: `ReadoutHTTPError` embeds `*ReadoutError`, so `Error()` is promoted from the embedded pointer. The only construction site in the package (endpoint.go:200) always sets it, so no package error path panics — I checked every return path in `endpoint.go` and `readout.go`. But the struct is exported with exported fields, and `var e ReadoutHTTPError; e.Error()` (or `&ReadoutHTTPError{Status: 500}`) panics on the nil pointer. The scope asked specifically whether such a path exists; inside the package there is none, outside there is.
- Recommendation: Define an explicit `Error()` on `ReadoutHTTPError` that falls back to a status-only message when the embedded error is nil, or document that the embedded error must be non-nil.

### LOW: The provenance gate never checks the recorded `think_system`
- Severity: LOW
- Confidence: HIGH
- Location: readout_test.go:97-103 (`readoutMeta`), scripts/gen_readout_parity.py (`meta` in `main`)
- Problem: The generator writes `"think_system": THINK_SYSTEM` into `testdata/readout_parity.meta.json`, but the Go `readoutMeta` struct does not decode it and no assertion compares it to the port's `ThinkSystem` constant. Every other recorded identity — readout version, limits, counts, digest — is asserted. A fixture recorded against a Python client whose thinking system line diverged from this port's `ThinkSystem` would pass the provenance check; the render-parity fixture covers the line, but the readout meta claims to pin this fixture's provenance and does not.
- Recommendation: Add `ThinkSystem string \`json:"think_system"\`` to `readoutMeta` and assert `meta.ThinkSystem == ThinkSystem` in `TestReadoutFixtureProvenance`.

### LOW: `TestReadoutResponseCap` does not pin the cap it claims to
- Severity: LOW
- Confidence: HIGH
- Location: readout_test.go:857
- Problem: The test's comment says it pins "the size of the real cap", but the assertion is `maxResponseBytes < 16<<20`, a lower bound a third of the actual value. A regression that shrinks the cap from 64 MB to 17 MB passes. The other provenance checks in this file assert exact values.
- Recommendation: Assert `maxResponseBytes == 64<<20` (or assert equality with the documented value), keeping the comment's rationale for the size.

### LOW: `docs/PORT.md`'s field-level decoding claim omits the logprob values
- Severity: LOW
- Confidence: MEDIUM
- Location: docs/PORT.md, "Field-level decoding is typed where Python coerces"
- Problem: The paragraph lists `usage.prompt_tokens`, `rank`, `choices[0].text` and a model entry's `id`. It does not mention that the `top_logprobs` and `prompt_logprobs` values are decoded into `*float64` / `*promptLogprobEntry`, so a value that is a string or array fails the response at decode time, where the Python client stores it (`result.logprobs[slot.letter] = value` accepts any JSON value) and only crashes later inside `math.exp`. The claim "stricter about four things" is therefore not the complete list of strictnesses, and the scope asks whether any actual difference is unclaimed. This one is.
- Recommendation: Add the logprob-map values to the field list in that paragraph.

## Things Checked

- **DE-2 guard.** All request-spending methods (`ReadCodes`, `ReadCodesAfterThought`, `Think`, `ListModels`, the fallback) are on `*Readout`, constructible only through `NewReadout` (all fields unexported), which calls `RequireDE2` before any load or request. Resolution order matches `resolve_family` exactly: declaration, model-id slug, tokenizer slug, assumed. `TestNewReadoutRefusesDE1` covers the model id, a bare id, the tokenizer source, a declaration, and a declaration overriding a DE-2 id; an unknown declaration string produces a normalize error (Python raises `ValueError` there too, so that is parity, though the Go error is neither `*ReadoutError` nor `*UnsupportedFamilyError`).
- **Downgrade parity.** Compared `readCodes` line by line with `read_codes`: block selection, `start += len(block)`, the 400/422 + detail sniff, request counting (a rejected request is not counted), `Sampled` at `start == 0`, the debug top-logprobs merge, and the option-order rebuild before softmax. All match, including the id-key-present-but-null vs text-key fallback lookup (`top.get(f"token_id:{id}", top.get(text))`).
- **`codes-late-downgrade` discrimination.** The scenario's server honours the first 128-candidate request, rejects the second, and answers the top-k with `topk_score` values that differ from the candidate-id scores at `top_k=256`. A client that re-read only the remaining block both sends a request body that differs from the recorded Python retry (the replay server compares bodies after JSON decode) and derives different logprobs. The gate fails either way.
- **`TokenIDList` strictness.** `null`, `1.5`, `"101"`, `true`, nested values and out-of-int-range integers all fail `json.Unmarshal(item, &id)` or the nil check. `token_ids: []` remains valid (matches Python's list check) and `token_ids: null`/absent is the missing-field error, matching Python's `isinstance(token_ids, list)` gate. Python additionally rejects `True` via its explicit `isinstance(t, bool)` guard, so no coercion gap exists on either side.
- **`LocalFilesOnly`.** Traced `ReadoutConfig.LocalFilesOnly` → `ensureTokenizerLocked` → `LoadWith` → `ResolveWith`: a readable file still loads (matching `from_pretrained` on a local path), the hub cache is checked via `Lookup`, and `Fetch` is reachable only when the policy is off. `TestReadoutLocalFilesOnly` proves the path with the `ErrNotCached` sentinel.
- **Error taxonomy.** `errors.As` finds `*ReadoutHTTPError` directly and `*ReadoutError` through `Unwrap`; a `*ReadoutError` wrapping an HTTP error resolves both ways too. The `readCodes` sniff uses `errors.As(err, &httpError)` on errors returned by `doJSONLimit`, which returns `*ReadoutHTTPError` un-wrapped for status ≥ 400, so the sniff is intact. Every construction site sets the embedded error.
- **Fallback semantics.** `letterLogprob` matches `_letter_logprob`: a whole-null last position yields the generic "returned nothing" error on both sides; a `{"key": null}` value yields a clean `ReadoutError` in Go where Python would crash on `None.get` (covered by PORT.md's null-entry claim); more than one entry errors in Go where Python takes the first — claimed as a deliberate strictness and pinned by `TestReadoutRejectsAnAmbiguousFallback`.
- **`closeEnough`.** Key presence is required in both maps (via the length check plus the `!ok` check), NaN and ±Inf on either side fail, and the tolerance `abs(got-want) > tol * max(abs(want), 1e-9)` is a correct relative comparison with a near-zero floor. A value of NaN everywhere cannot pass.
- **Replay gate.** The server compares method, path, and the JSON-decoded body (`reflect.DeepEqual` on `any` values, so number formatting cannot diverge); the step end-count check catches over- and under-sending; `requireReadoutError` asserts the recorded Python class and refuses a `ReadoutError` that is secretly an HTTP error; the generator re-raises an unexpected Python exception and `SystemExit`s on a message mismatch; a scenario whose expected error does not materialize records a result that `verifyRead` still fully compares. The after-thought step independently rebuilds the id list from the thought and compares it to the fixture. No comparison that cannot fail remains, apart from the two LOW items above.
- **`SetTokenizer` trace.** `CodeSlots` resolves the whole list under one lock; `readCodes` and `Think` capture the tokenizer once and use it for prefix resolution, encoding, and the token count, so no single read mixes tokenizers. The remaining hole is the approval-cache poisoning in the MEDIUM finding. The slot cache has no equivalent hole: entries are always written under the lock with the then-current tokenizer, and the swap clears them.
- **PORT.md claims.** The DE-2-only rationale, the four strictnesses, the zero-value conventions (verified `SystemPrompt`/`MaxLogprobs` defaulting), the error-taxonomy paragraph, the downgrade note, and the `LocalFilesOnly` note all match the code. The one incomplete claim is the LOW finding above.
- **Response cap.** `maxResponseBytes` is 64 MB; the rationale (the `prompt_logprobs: 0` fallback returns one entry per prompt token) is now correct, and `doJSONLimit` lets the test exercise truncation without a 64 MB socket.

## Residual Risks

- The deadlocked `CheckCodeBoundary` has zero test coverage; when it is fixed, the added test should also run under `-race`.
- The concurrency guarantees rest on `TestReadoutConcurrentReads` and the race detector. The `SetTokenizer`-during-a-read interleaving is untested (and currently has the cache-poisoning hole); Python has no `SetTokenizer`, so no parity scenario can cover it — the port owns this invariant.
- No live validation exists in this milestone (by scope), so all parity claims are against the stub's vLLM-shaped responses; a served checkpoint that shapes `top_logprobs` differently (for example, keying by text despite `return_tokens_as_token_ids`) is covered only by the recorded `codes-text-keys` scenario.
- The `error-no-token-ids` generator branch raises `SystemExit` if Python ever accepts a tokenless thinking response, so a future Python change silently hard-stops the generator rather than producing a stale fixture — intended, but worth knowing when regenerating.

BEGIN_REVIEW_JSON
{
  "findings": [
    {
      "severity": "HIGH",
      "title": "CheckCodeBoundary self-deadlocks on the readout mutex",
      "location": "readout.go:530-545",
      "recommendation": "Remove the lock from the exported CheckCodeBoundary wrapper (use ensureTokenizer then checkCodeBoundaryFor, as readCodes does) and add a test that calls CheckCodeBoundary."
    },
    {
      "severity": "MEDIUM",
      "title": "In-flight read poisons the boundary-approval cache for a swapped-in tokenizer",
      "location": "readout.go:541-567",
      "recommendation": "In checkCodeBoundaryFor, trust and write boundaryChecked only when tok is still the readout's current tokenizer, so a stale-tokenizer read cannot leave approvals a new-tokenizer read will skip."
    },
    {
      "severity": "LOW",
      "title": "Caller-constructed ReadoutHTTPError with nil embedded error panics in Error()",
      "location": "endpoint.go:34-46",
      "recommendation": "Give ReadoutHTTPError an Error() that falls back to a status-only message when the embedded *ReadoutError is nil."
    },
    {
      "severity": "LOW",
      "title": "Provenance gate never checks the recorded think_system",
      "location": "readout_test.go:97-103",
      "recommendation": "Decode think_system in readoutMeta and assert it equals ThinkSystem in TestReadoutFixtureProvenance."
    },
    {
      "severity": "LOW",
      "title": "TestReadoutResponseCap does not pin the 64 MB cap it claims to",
      "location": "readout_test.go:857",
      "recommendation": "Assert maxResponseBytes equals the documented value instead of a 16 MB lower bound."
    },
    {
      "severity": "LOW",
      "title": "PORT.md field-level decoding claim omits the logprob map values",
      "location": "docs/PORT.md",
      "recommendation": "Add top_logprobs and prompt_logprobs values (JSON number or null only) to the field list in the field-level decoding paragraph."
    }
  ]
}
END_REVIEW_JSON
