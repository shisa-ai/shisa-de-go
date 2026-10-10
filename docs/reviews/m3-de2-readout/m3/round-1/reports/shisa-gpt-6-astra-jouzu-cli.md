# Review Report

## Summary

The normal DE-2 request shapes, candidate blocking, fallback accounting, thought truncation, and readout identities closely follow the Python reference. The replay tests make real request-body and result comparisons, but their probability comparison can accept NaN results.

Changes are needed before accepting M3: the readout does not enforce the DE-2-only restriction, ignores `LocalFilesOnly`, accepts null thought-token elements as token zero, and can panic on a null fallback entry. Tokenizer replacement also leaves stale validation state. Additional findings cover numerical guarantees, late capability downgrade, and replay coverage.

All locations below are relative to `/home/morpheus/data/dev/shisa/shisa-de-go`. This was a read-only source review; no files were modified and no commands or tests were executed.

## Findings

### HIGH: The public readout bypasses the DE-2-only family guard
- Severity: HIGH
- Confidence: HIGH
- Location: readout.go:323
- Problem: `NewReadout` validates only the endpoint and model strings. It never calls `RequireDE2`, and neither do the read methods. Consequently, `Model: "shisa-ai/shisa-de-1"` can be read through the DE-2 codebook contract. The shared tokenizer allows the boundary checks to pass, so they do not prevent this. The scope and `docs/PORT.md` explicitly require a model that resolves to DE-1 to fail; the existing family helper is not connected to the new executable readout.
- Recommendation: Resolve and validate the family when constructing the readout, before loading a tokenizer or sending requests. Add tests rejecting DE-1 identified through either the model or tokenizer source. If explicit family declarations are needed, expose and honor them consistently rather than leaving the guard to an optional caller action.

### MEDIUM: LocalFilesOnly does not prevent tokenizer downloads
- Severity: MEDIUM
- Confidence: HIGH
- Location: readout.go:385
- Problem: `ReadoutConfig.LocalFilesOnly` is stored in `r.localFilesOnly` but never read. Initialization always calls `tokenizer.Load`, whose `Resolve` calls `Fetch` on a cache miss. A caller explicitly requesting local-only operation can therefore trigger an external request and a tokenizer download. Python passes `local_files_only=True` to its loader.
- Recommendation: Use a local-file/cache-only resolution path when this option is enabled, returning an error on a cache miss without calling `Fetch`. Test both a cached success and an uncached failure with network access prohibited.

### MEDIUM: Null thought-token elements silently become token zero
- Severity: MEDIUM
- Confidence: HIGH
- Location: endpoint.go:69; readout.go:765–771
- Problem: Decoding `token_ids` directly into `[]int` does not validate that every JSON element is an integer: Go's JSON decoder accepts `null` into an integer element and leaves its zero value. For example, `{"choices":[{"token_ids":[null,101]}]}` becomes a thought containing token `0`, marked closed. Python explicitly rejects any element that is not an integer. The Go read can therefore continue invented tokens rather than the tokens the server returned.
- Recommendation: Validate individual token elements before converting them to integers, rejecting nulls as well as other non-integer values. Add response-shape tests for null elements, an empty array, a missing field, and a null field.

### MEDIUM: A null fallback entry causes a panic
- Severity: MEDIUM
- Confidence: HIGH
- Location: readout.go:721–722
- Problem: `PromptLogprobs` contains pointer-valued map entries. A response such as `{"choices":[{"prompt_logprobs":[null,{"65":null}]}]}` passes the single-entry check and then dereferences a nil `entry`. This is endpoint-controlled malformed input, not a caller programming error, and can terminate the calling goroutine or process instead of returning a readout error.
- Recommendation: Check the entry pointer before accessing its fields and return a `ReadoutError` identifying the malformed fallback. Add a regression test asserting that this response returns an error without panicking.

### MEDIUM: Replacing the tokenizer preserves stale slots and boundary approvals
- Severity: MEDIUM
- Confidence: HIGH
- Location: readout.go:372–375
- Problem: `SetTokenizer` replaces only `r.tokenizer`. Slots resolved from the previous tokenizer remain cached, and `boundaryChecked` continues to approve their codes. After resolving and checking a code with tokenizer A, installing tokenizer B with the same closing-token ID but different code IDs can send A's IDs while tokenizing the prompt with B. Equal prompt lengths do not detect that mismatch. Concurrent replacement additionally allows one operation to tokenize with one tokenizer and validate against another.
- Recommendation: Prefer making tokenizer injection initialization-only and reject replacement after initialization or use. If replacement must remain supported, invalidate all tokenizer-derived state and ensure an entire read uses one consistent tokenizer generation. Document the concurrency requirements for injected tokenizer implementations and test replacement behavior.

### MEDIUM: The replay probability comparison accepts NaN results
- Severity: MEDIUM
- Confidence: HIGH
- Location: readout_test.go:468–476
- Problem: `closeEnough` rejects a value only when `math.Abs(got[key]-value) > tolerance`. If `got[key]` is NaN, the comparison is false, so a readout returning NaN for every expected probability passes this part of the replay gate. The helper also does not check key presence, and its advertised relative tolerance is implemented as an absolute tolerance.
- Recommendation: Require every expected key to exist, reject non-finite values, and implement the stated tolerance explicitly. Add direct tests proving that NaN values, incorrect keys, and out-of-tolerance finite values fail. Include an exact gate-decision comparison for threshold-focused fixtures rather than relying only on numeric tolerance.

### LOW: Softmax's claim that rounding cannot change the gate is unsupported
- Severity: LOW
- Confidence: HIGH
- Location: readout.go:99–105, 129–133
- Problem: The implementation uses ordinary accumulation, whereas CPython 3.12+ uses compensated summation. A small denominator difference can put a probability on opposite sides of the strict `0.7` threshold when it is sufficiently close to that threshold. The replay's `1e-12` absolute tolerance cannot establish identical gate decisions. Also, the sorted order matches Python only when the supplied slots are in codebook order; Python preserves the caller's slot order, while the Go public read methods accept arbitrary slot slices.
- Recommendation: Remove the unconditional gate-equivalence claim. Match the reference's compensated accumulation where practical, add Python-derived cases around the gate, and document any remaining cross-runtime rounding limitation. Either preserve supplied slot order for normalization or explicitly require codebook order. Matching the existing reference more closely does not by itself require a new readout identity.

### LOW: A capability downgrade after the first block changes Python's result semantics
- Severity: LOW
- Confidence: HIGH
- Location: readout.go:576–580
- Problem: With candidate IDs disabled, Go processes `slots[start:]`, while Python's `read_codes` processes all `slots`. They differ if the first candidate block succeeds and a later block rejects `logprob_token_ids`, or another concurrent read disables the capability between blocks. After retrying without IDs, Python can replace earlier codes' values with those in the new top-k response; Go ignores those entries for earlier codes and may also perform fallbacks Python avoids. This behavioral difference is neither recorded in `docs/PORT.md` nor exercised by the generator, which rejects the first block.
- Recommendation: Add a replay scenario that accepts the first block and rejects the second. Then either match Python or explicitly document the Go behavior as an intentional correction and report the reference discrepancy upstream.

### LOW: The replay server does not verify the HTTP method or path
- Severity: LOW
- Confidence: HIGH
- Location: readout_test.go:340–369; scripts/gen_readout_parity.py:181–188
- Problem: The recorder saves request bodies but not request methods or paths. The replay server responds on every route and compares only the JSON body. Changing the implementation from `/v1/completions` to an incorrect path would leave the replay passing, despite breaking endpoint compatibility.
- Recommendation: Record and compare the method and path, or assert `POST /v1/completions` in the replay server. Add separate narrow coverage for `GET /v1/models` and bearer-header propagation.

## Things Checked

- Compared `readout.go`, `endpoint.go`, and the rendering addition against `shisa_de/readout.py` and both readout contract pages.
- Confirmed the standard code request includes `max_tokens: 1`, `temperature: 0`, `logprobs`, `logprob_token_ids`, and `return_tokens_as_token_ids`, with candidate blocks capped at 128.
- Confirmed ID-key lookup precedes decoded-text lookup, including Python's distinction between an absent ID key and a present null value.
- Checked successful-request accounting, fallback ranks, missing-code order, accumulated prompt-token counts, and debug top-logprob collection.
- Confirmed text fallbacks append code text and post-thought fallbacks append the token ID.
- Confirmed thinking requests use `return_token_ids` and `stop_token_ids`, preserve generated IDs, and truncate at the first closing token.
- Checked prompt-token mismatch rejection and tolerance of omitted or zero usage.
- Checked mutex access to the tokenizer reference, slot cache, boundary cache, and candidate-ID capability. Ordinary reads with a fixed built-in tokenizer have no evident unprotected mutation; tokenizer replacement is the exception described above.
- Confirmed scaffold constants, readout versions, and calibration compatibility entries match Python.
- Confirmed non-empty custom system prompts mirror Python and thinking renders always use `ThinkSystem`. Adding this existing reference capability does not change the default contract or require a version bump.
- Reviewed the fixture generator and replay assertions rather than the generated fixture bytes. Request-body comparisons and most result-field comparisons can genuinely fail; the probability comparison has the defect above.
- Confirmed the documented multi-entry fallback rejection and string-valued sampled result. Additional differences include Go zero-value defaulting and stricter typed response decoding; the three highlighted differences in `docs/PORT.md` are not an exhaustive description.

## Residual Risks

- The scope reports successful build, vet, and test runs. Those results were not independently reproduced in this review.
- No shell tool was available, so commit ancestry, the exact diff, and working-tree status were not verified. Findings are against the inspected working-tree files.
- No live requests were made, as required. Served-model behavior and quality remain outside this review.
- No race-detector run or numerical threshold experiment was performed. The concurrency and floating-point findings are based on source inspection.
- Policy, calibration application, and typed-answer selection are later milestones. In particular, those layers must preserve presentation-order tie breaking rather than iterate the unordered result maps.

BEGIN_REVIEW_JSON
{
  "findings": [
    {
      "severity": "HIGH",
      "title": "The public readout bypasses the DE-2-only family guard",
      "location": "readout.go:323",
      "recommendation": "Enforce family resolution and DE-1 rejection in readout construction, with model-source and tokenizer-source regression tests."
    },
    {
      "severity": "MEDIUM",
      "title": "LocalFilesOnly does not prevent tokenizer downloads",
      "location": "readout.go:385",
      "recommendation": "Use local-file/cache-only resolution when requested and test that an uncached tokenizer fails without fetching."
    },
    {
      "severity": "MEDIUM",
      "title": "Null thought-token elements silently become token zero",
      "location": "endpoint.go:69; readout.go:765-771",
      "recommendation": "Validate every token_ids element before integer conversion and reject null elements."
    },
    {
      "severity": "MEDIUM",
      "title": "A null fallback entry causes a panic",
      "location": "readout.go:721-722",
      "recommendation": "Check for nil fallback entries and return a ReadoutError, with a no-panic regression test."
    },
    {
      "severity": "MEDIUM",
      "title": "Replacing the tokenizer preserves stale slots and boundary approvals",
      "location": "readout.go:372-375",
      "recommendation": "Make tokenizer injection initialization-only or invalidate derived state and keep each read on one tokenizer generation."
    },
    {
      "severity": "MEDIUM",
      "title": "The replay probability comparison accepts NaN results",
      "location": "readout_test.go:468-476",
      "recommendation": "Check key presence and finiteness, implement the intended tolerance, and test that invalid distributions fail."
    },
    {
      "severity": "LOW",
      "title": "Softmax's claim that rounding cannot change the gate is unsupported",
      "location": "readout.go:99-105,129-133",
      "recommendation": "Remove the unconditional guarantee, use reference-compatible accumulation, and add threshold-focused parity cases."
    },
    {
      "severity": "LOW",
      "title": "A capability downgrade after the first block changes Python's result semantics",
      "location": "readout.go:576-580",
      "recommendation": "Add a late-downgrade replay and either match Python's all-slot processing or document and justify the divergence."
    },
    {
      "severity": "LOW",
      "title": "The replay server does not verify the HTTP method or path",
      "location": "readout_test.go:340-369; scripts/gen_readout_parity.py:181-188",
      "recommendation": "Assert or record request methods and paths, and add narrow model-listing and authorization-header coverage."
    }
  ]
}
END_REVIEW_JSON
