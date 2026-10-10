# Review Report

## Summary

I reviewed M3 (the DE-2 readout: `readout.go`, `endpoint.go`, the `render.go` system-prompt addition, `readout_test.go`, `scripts/gen_readout_parity.py`, and the `docs/PORT.md` claims) against the Python reference at `/home/morpheus/data/dev/shisa/shisa-de` (`shisa_de/readout.py`, `0.4.0`), read-only.

The readout itself is a faithful port. I compared `read_codes`, `think`, `_letter_logprob`, `_check_prompt_tokens` and `check_code_boundary` against their Go counterparts field by field on the wire (every request key, every response key read, every result field) and found no request, response or result field that differs in a way that changes an answer. The readout identities and `calibrationCompatible` match Python exactly, and the added `SystemPrompt` option does not change any answer-producing contract (the system line is already a Python constructor parameter, the default render is unchanged, and both renders are pinned byte-for-byte). The replay gate is genuinely able to fail: it compares request bodies after JSON decoding and every result field, it counts requests in both directions, and its stub tokenizer is rebuilt from the fixture's word list so a tokenizer divergence surfaces as a wrong prompt-token count.

Findings are one MEDIUM (the error taxonomy diverges from Python's class hierarchy and `PORT.md` implies it does not) and seven LOW items: one real fidelity divergence, one stale count in a doc comment, one gate-coverage gap, and four smaller documentation/coercion mismatches.

## Findings

### MEDIUM: `ReadoutHTTPError` is not catchable as a `ReadoutError`, unlike the Python class it replaces
- Severity: MEDIUM
- Confidence: HIGH
- Location: `endpoint.go:18-24`, `errors.go:3-21`, `docs/PORT.md:104-107`
- Problem: In Python, `class ReadoutHTTPError(ReadoutError)` (`shisa_de/readout.py:153`), so `except ReadoutError` catches an HTTP failure. The reference client depends on that: `DecisionModel.health()` catches `(httpx.HTTPError, ReadoutError)` and `(ReadoutError, QuestionError)` around calls that can return HTTP errors (`client.py:491`, `510`, `517`), and `tests/test_offline.py:1633` asserts `pytest.raises(ReadoutError)` for a handler that answers HTTP 503. The Go `ReadoutHTTPError` is a sibling struct with no embedded `ReadoutError` and no `Unwrap`, so `errors.As(err, &readoutErr)` misses it. `errors.go:3` says callers distinguish the errors "in the same places the Python client catches `ReadoutError` and `QuestionError`", and `PORT.md:104` says `ReadoutError`/`QuestionError` "replace the Python exception classes" — both read as if the catch sites map across, and they do not. Nothing in the tree catches `ReadoutError` today except the future M4/M5 code, so this is cheap to fix now and a trap later.
- Recommendation: embed `ReadoutError` in `ReadoutHTTPError` (or return a `*ReadoutError` from `Unwrap`), which keeps `errors.As(err, &httpError)` working for the `logprob_token_ids` sniff in `readout.go:604`, and say so in `PORT.md`'s "Errors instead of exceptions" paragraph. If the flat struct is deliberate, state the difference there instead.

### LOW: A mid-read `logprob_token_ids` downgrade re-reads only the remainder, where Python re-reads every code
- Severity: LOW
- Confidence: HIGH
- Location: `readout.go:576-581` against `shisa_de/readout.py:544`
- Problem: Python computes `block = slots[start:start + CANDIDATE_ID_LIMIT] if self._candidate_ids else slots` — with the capability off it uses `slots`, the whole list. Go computes `block := slots[start:]`, so after a downgrade that happens on a later block (>128 options, first block accepted, second rejected with 400/422) Python retries the whole option list in one request and re-applies that response's top-k to the codes it already read, while Go retries only the remainder. The retry request bodies are identical (the field is absent either way), so the gate cannot see this: `codes-topk-rejected` only rejects the first block, and the divergence needs a server that accepts the first block and rejects the second. The observable difference requires a server whose top-k logprob for a token differs from its `logprob_token_ids` logprob for the same token, which the readout page measured at 0.0.
- Recommendation: match Python (`block = slots` when the capability is off), or add it to `PORT.md`'s deliberate-difference list as a stricter/lighter retry.

### LOW: `render.go` says `TestRenderParity` compares 33 rendered prompts; the fixture holds 35
- Severity: LOW
- Confidence: HIGH
- Location: `render.go:39`, against `testdata/render_parity.meta.json` (`"renders": 35`) and `docs/PORT.md:52`
- Problem: `TestRenderParity` iterates every entry of `fixture.Renders`, and `gen_render_parity.py` emits 35 cases (`spam-*`, the evidence shapes, widths, `wide-256-direct`, and the two `custom-system-prompt*` cases M3 added). `PORT.md` was updated to 35; the comment in `render.go` still says 33. The number is now stale in exactly the file whose M3 change added the two cases.
- Recommendation: change 33 to 35 in `render.go:39`.

### LOW: Go's zero values cannot express two Python constructor arguments, and `PORT.md` does not record it
- Severity: LOW
- Confidence: HIGH
- Location: `readout.go:279-282`, `render.go:17-20`, `docs/PORT.md:44-49`
- Problem: `ReadoutConfig.SystemPrompt: ""` renders `DirectSystem`, so the empty system line that `Readout(system_prompt="")` produces in Python cannot be requested; `ReadoutConfig.MaxLogprobs: 0` sends `logprobs: 20` where Python's `max_logprobs=0` sends `logprobs: 0` (which makes every code fall back). Both mappings are documented on the fields, neither is in `PORT.md`'s list of places the port deliberately differs.
- Recommendation: add both to `PORT.md` as Go zero-value conventions (they are sensible), or expose a way to pass Python's explicit values.

### LOW: The replay gate compares request bodies only, so the path, method and headers are unchecked
- Severity: LOW
- Confidence: HIGH
- Location: `readout_test.go:340-366`; `scripts/gen_readout_parity.py` `Recorder._handle` (records `json.loads(request.content)` only)
- Problem: `replayServer.ServeHTTP` never looks at `r.URL.Path`, `r.Method` or `r.Header`, and the fixture records only bodies. A Go readout that posted to the wrong path, used the wrong method, or dropped the `Content-Type`/`Authorization` headers would pass all 57 exchanges. Nothing else in the tree checks them: there is no live test until M6 and no `doctor` yet, so the bearer token is currently unverified anywhere.
- Recommendation: record the path and method per exchange and compare them, and assert that `Authorization` is present exactly when `APIKey` is set.

### LOW: Gate details that cannot fail or are not exercised
- Severity: LOW
- Confidence: HIGH
- Location: `readout_test.go:67` and `:313`; `scripts/gen_readout_parity.py:283` and `:343`
- Problem: three smaller gaps. (1) `replayStepError.Type` is decoded but only used in a failure message, so Python's error class is never asserted against the Go error's class. (2) The three thinking scenarios and `error-no-token-ids` record any exception Python raises with the hardcoded pattern `return_token_ids`, so a fixture generated from an unexpected Python failure would carry a `match` that does not describe it (it would fail the Go test, not pass it, but the recorded `type`/`message` would mislead). (3) The stub always sets `choices[0].text`, so the claimed `Sampled` string-vs-`None` difference (`PORT.md:118-121`) has no scenario, and the 8 MB cap has no test at all — two of the three claimed tolerances are unpinned.
- Recommendation: assert `want.Type` against the Go error type (mapping `ReadoutError`/`ReadoutHTTPError`), make the generator require the error it expected instead of a fixed pattern, and add a scenario whose choice omits `text`.

### LOW: Field-level decoding is stricter than Python's coercion, and unrecorded
- Severity: LOW
- Confidence: MEDIUM
- Location: `endpoint.go:39-76`, `readout.go:203-215`
- Problem: Python coerces where Go's typed unmarshal fails. `int((data.get("usage") or {}).get("prompt_tokens") or 0)` accepts `"238"` or `238.5`; `entry.get("rank")` accepts `30.0`; `choice.get("text")` accepts a non-string; `[entry.get("id") for entry in ...]` yields `None` for a model entry with no `id` where Go yields `""`. None of these is reachable from vLLM, and `PORT.md:126-129` covers only the top-level JSON value, not the fields.
- Recommendation: add one sentence to that paragraph naming the field-level strictness, or leave it and accept the stricter behaviour knowingly.

### LOW: `closeEnough`'s comment says relative; the comparison is absolute
- Severity: LOW
- Confidence: HIGH
- Location: `readout_test.go:267-269`, `:468-479`
- Problem: the comment says the probabilities "compare to a relative 1e-12 rather than exactly", but `math.Abs(got[key]-value) > tolerance` is an absolute test, so a probability of 1e-9 is only checked to a relative 1e-3. The tolerance is right for this data; the comment is wrong, and a future reader could rely on it.
- Recommendation: fix the comment, or make the comparison relative (`> tolerance*math.Abs(value)` with an absolute floor).

## Things Checked

- **Wire fidelity, request side.** Every key of the code read (`model`, `prompt`, `max_tokens: 1`, `temperature: 0`, `logprobs`, `logprob_token_ids`, `return_tokens_as_token_ids`), the fallback (`prompt_logprobs: 0`, and `prompt + code` as text or as `prompt + [token_id]` for an id prompt), and the thinking read (`stop_token_ids`, `return_token_ids`, `max_tokens: budget`) match `shisa_de/readout.py`. `omitempty` on `*int`/`*bool` fields does not drop `prompt_logprobs: 0` or `logprobs: 20` because the pointers are non-nil; `LogprobTokenIDs` cannot be empty when the field is set, since an empty slot list skips the loop. Request key *order* differs from Python's dict order and is irrelevant (the gate compares after JSON decoding).
- **Wire fidelity, response and result side.** `CodeRead` carries every field `LetterRead` carries for this path (`logprobs`, `probabilities`, `requests`, `prompt_tokens`, `missing_from_top`, `ranks`, `sampled`, `prompt`, `top_logprobs`); `LetterRead.elapsed_ms`/`messages`/`system_render` are image-path only. The `top.get(f"token_id:{id}", top.get(text))` default-vs-missing semantics are reproduced exactly (a present-but-null id key does not fall through), `top_logprobs` null/empty handling matches `((choice.get("logprobs") or {}).get("top_logprobs") or [{}])[0] or {}`, `prompt_logprobs` empty/`{}`/`null` handling matches `(… or [None])[-1]` plus `if not entries`, `closed`/truncation matches `close_id in token_ids` and `token_ids.index(close_id)`, and the option-order rebuild of `logprobs` before softmax matches.
- **The three claimed differences are the ones I found in behaviour, plus the low findings above.** The multi-entry `prompt_logprobs` refusal (`readout.go:683-689`, pinned by `TestReadoutRejectsAnAmbiguousFallback`), the `Sampled` string, and the 8 MB cap are all real, as are the two the following `PORT.md` paragraph records (non-object JSON, missing `choices`).
- **The gate can fail.** Request bodies are compared with `reflect.DeepEqual` after decoding into `any`, extra and missing requests are both detected (`replayServer` errors on an unexpected request; the step loop compares `replay.at` to the recorded count), all nine `readResult` fields and all five `thoughtResult` fields are compared, the debug prompt and the constructed read-after-thought ids are compared, and the stub tokenizer is rebuilt from the fixture's words with the same longest-match-first rule as the generator's regex (`sorted(..., key=len, reverse=True)`; equal-length ties cannot both match at one position), so a divergence shows up as a wrong prompt-token count. The `error-*` steps require a `*ReadoutError` and a regex match, and `error-boundary` requires zero requests. I found no comparison that cannot fail and no scenario that would pass with the readout broken.
- **The fixture is the Python client's.** The generator drives the real `shisa_de` from `~/data/dev/shisa/shisa-de` through an `httpx.MockTransport`, records `read_codes`/`think` results with `read_record`/`thought_record`, and asserts the stub's `logprob_token_ids` block size; the meta records the Python revision `d172d497…`, `de2-codebook-v1`, the limits, 19 scenarios, 57 exchanges and the fixture digest.
- **Concurrency.** Every mutable field (`tokenizer`, `slots`, `boundaryChecked`, `candidateIDs`) is written and read under `mu`; `baseURL`, `model`, `systemPrompt`, `maxLogprobs`, `client` and `headers` are immutable after `NewReadout` (the shared `http.Header` is only read, and `*http.Client` is concurrency-safe). No nested locking: `ensureTokenizer` and `CheckCodeBoundary` take and release the mutex before the next call, and the `*Locked` helpers never re-lock. A failed load is not cached, so it is retried, matching Python.
- **Readout identities.** `DE1ReadoutVersion`/`DE2ReadoutVersion`, `ReadoutVersionFor`, `calibrationCompatible` and `CalibrationCompatibleWith` match `READOUT_VERSIONS`, `CALIBRATION_COMPATIBLE` and `calibration_compatible` including the DE-1 predecessors and the empty DE-2 tuple, and the missing-key case agrees (`get(current, ())` vs a nil slice).
- **`SystemPrompt` needs no version bump.** The system line was already `Readout(system_prompt=…)`; the default render is unchanged, `Thinking` still forces `ThinkSystem` on both sides, and `TestRenderParity` pins the default and caller-supplied renders for both shapes byte-for-byte.
- **Softmax.** The sum order (shortest key, then lexicographic) equals the codebook order, which `TestCodebookMatchesTheAudit` pins against the audit the page describes; Python sums in the same order because `read_codes` rebuilds `logprobs` in option order before normalizing. The residual difference is CPython 3.12+ Neumaier compensation, as the comment says, and it cannot move an argmax or the 0.7 gate.
- **Boundary and prompt-token checks.** Both halves match Python, including the per-code cache (per process, not per prompt) and the tolerance of a server that reports no `prompt_tokens`.

## Residual Risks

- **No measurement.** Nothing here was checked against a served model; the gate's tokenizer is a stub, so it cannot see a divergence between the Go render/tokenizer and the checkpoint. `TestRenderParity`/`TestRenderTokenCounts` cover that separately, and the token-count test skips when no `tokenizer.json` is present.
- **The 8 MB cap's rationale is narrower than stated.** `endpoint.go:96-99` justifies the cap with "the largest thing a server sends back here is one position of top-logprobs", but the `prompt_logprobs: 0` fallback returns an entry per prompt token (`docs/READOUT.md`'s sample shows the last-position entry for a 514-token prompt). A long prompt on a server without `logprob_token_ids` could exceed 8 MB and fail as "did not return a JSON object". Unmeasured; the cap itself is fine.
- **The concurrency claim rests on inspection.** The validation list does not include `go test -race`, and no test exercises concurrent reads of one `Readout`.
- **Softmax's order depends on the shipped file.** If the codebook's pair order ever stopped being lexicographic, the sort would still be deterministic but no longer the option order, and the comment's "the codebook's own order" would be wrong. `TestCodebookMatchesTheAudit` is what holds that today.
- **`readout_parity.meta.json`'s `python_repo` is recorded but never asserted**, so a fixture generated from a different Python revision would still pass the provenance test as long as its own digest and counts are self-consistent.

BEGIN_REVIEW_JSON
{
  "findings": [
    {
      "severity": "MEDIUM",
      "title": "ReadoutHTTPError is not a ReadoutError, unlike the Python class it replaces",
      "location": "endpoint.go:18-24, errors.go:3-21, docs/PORT.md:104-107",
      "recommendation": "Embed ReadoutError in ReadoutHTTPError (or return a *ReadoutError from Unwrap) so errors.As finds both, and record the taxonomy in PORT.md; errors.As(err, &httpError) in readout.go:604 keeps working."
    },
    {
      "severity": "LOW",
      "title": "Mid-read logprob_token_ids downgrade retries only the remainder, Python retries every code",
      "location": "readout.go:576-581 vs shisa_de/readout.py:544",
      "recommendation": "Use the whole slot list when the capability is off, or list this as a deliberate difference in PORT.md."
    },
    {
      "severity": "LOW",
      "title": "render.go says TestRenderParity compares 33 prompts; the fixture holds 35",
      "location": "render.go:39",
      "recommendation": "Change 33 to 35, matching testdata/render_parity.meta.json and docs/PORT.md:52."
    },
    {
      "severity": "LOW",
      "title": "Zero values cannot express Python's empty system line or max_logprobs=0",
      "location": "readout.go:279-282, render.go:17-20, docs/PORT.md:44-49",
      "recommendation": "Record both zero-value mappings in PORT.md's difference list, or expose a way to pass the explicit values."
    },
    {
      "severity": "LOW",
      "title": "Replay gate compares request bodies only; path, method and headers are unchecked",
      "location": "readout_test.go:340-366, scripts/gen_readout_parity.py (Recorder._handle)",
      "recommendation": "Record and compare the request path and method, and assert Authorization is present exactly when an API key is configured."
    },
    {
      "severity": "LOW",
      "title": "Gate gaps: error type never asserted, hardcoded think error match, Sampled and 8 MB differences unpinned",
      "location": "readout_test.go:67,313; scripts/gen_readout_parity.py:283,343",
      "recommendation": "Assert the recorded error type, require the generator's expected error instead of a fixed pattern, and add a scenario whose choice omits text."
    },
    {
      "severity": "LOW",
      "title": "Field-level decoding is stricter than Python's coercion and is unrecorded",
      "location": "endpoint.go:39-76, readout.go:203-215, docs/PORT.md:126-129",
      "recommendation": "Name the field-level strictness (prompt_tokens, rank, text, model id) in the same PORT.md paragraph that covers the top-level JSON value."
    },
    {
      "severity": "LOW",
      "title": "closeEnough's comment claims a relative tolerance; the comparison is absolute",
      "location": "readout_test.go:267-269,468-479",
      "recommendation": "Fix the comment or make the tolerance relative with an absolute floor."
    }
  ]
}
END_REVIEW_JSON
