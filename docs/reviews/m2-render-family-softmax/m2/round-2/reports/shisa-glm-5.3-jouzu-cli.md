# Review Report

## Summary

Commit a76057a (on top of the round-1 revision 12a1a29) closes the round-1 findings in the M2 Go port, and the fixes are mostly correct. The repeated-key decode now matches `json.loads` in both the surviving value and its position; `isJSONNumberLiteral` is exactly JSON's number grammar and `appendJSONNumber` reproduces CPython's `json.loads`+`json.dumps` bytes for the edge cases, including `-0`, out-of-range exponents (both signs) and integers beyond 2^64, which are written verbatim exactly as Python's arbitrary-precision ints would be; `isPythonSpace` is Python's `str.strip()` set; the softmax accumulation order matches the Python client's option-order sum; and the fixture provenance check is sound. One gap remains in the UTF-8 rejection the commit set out to complete: `json.RawMessage` still bypasses it, because `json.Valid` does not check UTF-8. Two smaller documentation/strictness asymmetries follow from the same area.

## Findings

### MEDIUM: json.RawMessage can still write invalid UTF-8 into the prompt, bypassing the new rejection
- Severity: MEDIUM
- Confidence: HIGH
- Location: pyjson.go:185 (`case json.RawMessage:`)
- Problem: `appendPythonString` now rejects strings that are not valid UTF-8, and `appendPythonObject` propagates that error for `Object` and map keys, so every string-valued path is covered — except `json.RawMessage`. That case only runs `json.Valid`, and Go's JSON scanner checks syntax only (structure, quotes, control characters below 0x20); it accepts arbitrary bytes ≥ 0x80 inside strings, which is why `encoding/json` substitutes U+FFFD rather than failing when it decodes one. So `PythonJSON(Object{{"evidence", json.RawMessage(`{"t":"a\xffb"}`)}})` passes the validity check and writes the invalid bytes verbatim into the user turn. Python cannot produce or hold those bytes (`json.loads` on bytes raises `UnicodeDecodeError`), so this is exactly the "bytes neither the caller nor Python produced" case the fix comment describes. A plausible caller path exists: decoding a server or webhook payload into a `json.RawMessage` field preserves the raw bytes, and the type is documented as accepted and inserted verbatim.
- Recommendation: Before writing a `json.RawMessage`, require `utf8.Valid(typed)` and return the same error `appendPythonString` returns when it fails (or decode and re-encode it through the validated string path). Add a case to `TestPythonJSONRejectsWhatItCannotRender` for a `json.RawMessage` with an invalid byte in a string.

### LOW: DecodeJSON substitutes U+FFFD for invalid UTF-8 in input strings, where Python's json.loads raises
- Severity: LOW
- Confidence: HIGH
- Location: pyjson.go:79 (`DecodeJSON`)
- Problem: The `DecodeJSON` doc lists exactly one place it is stricter than `json.loads` (bare NaN/Infinity) and is otherwise presented as matching Python. It is also silently *looser* in one: Go's decoder replaces invalid UTF-8 inside string tokens with the replacement rune, where CPython raises `UnicodeDecodeError`. So `DecodeJSON([]byte(`{"t":"a\xffb"}`))` succeeds and yields a string containing U+FFFD, which `PythonJSON` then happily writes into the prompt — valid UTF-8, but bytes Python would never have produced for that input. The new output-side rejection cannot catch it, because the value has already been laundered into a valid Go string.
- Recommendation: Either document this asymmetry next to the NaN/Infinity note in the `DecodeJSON` doc comment, or pre-validate the input with `utf8.Valid(data)` (Python's strictness) and return an error. A test pinning the chosen behavior would fix it in either direction.

### LOW: PORT.md's "Verified parity" header attributes the offline gates to the hosted endpoint
- Severity: LOW
- Confidence: MEDIUM
- Location: docs/PORT.md:105
- Problem: The section header says the parity was "Checked on 2026-10-10 against `shisa-ai/shisa-de-2` on the hosted endpoint and the Python client at `shisa-de` `0.4.0`", and it covers the tokenizer, render and JSON bullets. Those gates were produced offline — the scope states no requests were made to a served model, and the fixtures come from the pinned snapshot and the Python client. Under this repo's own evidence rules (a number says when and against what it was measured), a reader could take the 33 renders and 1,056 tokenizer strings as endpoint-verified when the endpoint contributed at most the serving fingerprint and the identity bullets.
- Recommendation: Scope the "hosted endpoint" claim to the bullets that actually used the endpoint, and say of the render/JSON/tokenizer gates that they were generated offline from the pinned snapshot and the Python client.

## Things Checked

- **Repeated keys (pyjson.go decodeValue).** Last value replaces in place at the first occurrence's position; three-occurrence and nested cases reduce to the same rule. `Object.Get` returns the first member, which is the surviving one, so `Get` agrees with what `PythonJSON` writes. Matches `json.loads`+dict ordering; pinned by `TestDecodeJSONKeepsTheLastOfARepeatedKey`.
- **Number grammar (isJSONNumberLiteral).** Walked the implementation against `-?(0|[1-9][0-9]*)(\.[0-9]+)?([eE][+-]?[0-9]+)?`: leading zeros, bare `-`, `+`, trailing `.`/`e`/`e+`, `01`, `.5` and empty text are all rejected; `0`, `-0`, `0.5`, `1E5` accepted. Matches JSON's grammar exactly.
- **appendJSONNumber vs CPython.** Integer literals (any length, past 2^64) are written verbatim, which is what Python's arbitrary-precision int repr does; `-0` normalizes to `0` as Python's int dump does. Float-path literals: `1e400`/`-1e400` reach the ErrRange branch with the value already ±Inf (Go applies the sign after the range error, so `-1e-400` yields −0.0), and `appendPythonFloat` writes `Infinity`/`-Infinity`/`0.0`/`-0.0` where CPython dumps the same. The scope's recorded CPython cross-checks cover these.
- **UTF-8 rejection coverage.** Every string-writing path was traced: `case string`, `reflect.String`, `Object` keys, map keys (via the members slice) all validate; `json.Number` is ASCII-only by grammar; integers, bools and floats emit ASCII. The one uncovered path is the `json.RawMessage` case (MEDIUM above).
- **Softmax order.** The codebook is pinned by `TestCodebookMatchesTheAudit` to be A–Z then lexical pairs (GZ skipped), which is exactly the (len, lexicographic) order, so sorting any subset — including the non-prefix subsets a fallback read can produce — yields the codebook's relative order for every n from 2 to 256. The Python client sums `weights.values()` in slots order (readout.py rebuilds `logprobs` in option order at line 584 before `softmax`), so peak, total and each division accumulate in the same order and are bit-reproducible given the same inputs.
- **isPythonSpace.** Go's `unicode.IsSpace` is Unicode's White_Space set; Python's `str.strip()` set is Zs ∪ bidi WS/B/S, which adds exactly U+001C–U+001F. The union matches Python's set with nothing extra (U+200B is in neither). `RenderOption` and `validateQuestion` both use it, matching `render_option` and `Question.validate`.
- **Non-positive option limit.** `validateQuestion`, `Payload` and `Render` all default `optionLimit <= 0` to `MaxOptions` before comparing, so a 27-option question with a zero limit fails naming 26 in all three, and a 26-option one passes. The Go zero-value defaulting is a documented divergence from Python, which has no defaulting.
- **Fixture provenance.** The test hashes the checked-in `render_parity.json` bytes and compares against the meta the generator computed over the bytes it wrote; the meta's model/revision are compared against `tokenizer.PinnedRepo`/`PinnedSnapshot`, and the generator's own `MODEL`/`REVISION` constants match those pins, so a regeneration from another snapshot fails the test. Empty render or serialization lists are now fatal in `loadRenderFixture`. The 216-token pin is correctly re-attributed to the fixture rather than the readout page.
- **Docs and comments.** `family.go` and `render.go` changes are comment-only as claimed; the empty-declared-string divergence and the `bos_token`-is-tokenizer-config note are accurate. `readout.go`'s inert DE-1 calibration entry matches the Python source of truth. `docs/PORT.md` and `AGENTS.md` accurately record the M1/M2 attribution and the JSON difference, apart from the header nit above.
- Round-1 finding fixes were verified against the working tree, not just the diff excerpts.

## Residual Risks

- `Softmax` still returns an unordered map; carrying code order to the caller is documented as M3's job.
- Go's `math.Exp` and CPython's libm `exp` can differ by an ulp on some inputs, so bit-identical parity with the Python client is guaranteed for the arithmetic order, not necessarily for every `exp` result. Inherent to running in a different runtime; nothing in this commit can change it.
- Go's Unicode tables and the Python runtime's `unicodedata` track Unicode versions independently; a future Unicode release that changes the Zs/White_Space sets could desync `isPythonSpace` from `str.strip()` until both are updated. The unit test pins today's set, which limits the drift to a test failure.
- `DecodeJSON` remains stricter than Python on bare NaN/Infinity literals — documented as a deliberate asymmetry rather than fixed.
- The fixture sha256 is computed over the body string the generator writes rather than by reading the file back, so regenerating on a platform whose `write_text` newline translation differs would make the provenance test fail loudly (a safe direction: false failure, not silent drift).
- No live validation was possible in this review (read-only, no served-model requests); the render and JSON conclusions rest on the checked-in fixtures and code reading.

BEGIN_REVIEW_JSON
{
  "findings": [
    {
      "severity": "MEDIUM",
      "title": "json.RawMessage can still write invalid UTF-8 into the prompt",
      "location": "pyjson.go:185",
      "recommendation": "Require utf8.Valid on json.RawMessage before writing it verbatim (same error as appendPythonString), and add a rejection test for a RawMessage with invalid UTF-8 in a string."
    },
    {
      "severity": "LOW",
      "title": "DecodeJSON substitutes U+FFFD for invalid UTF-8 input where json.loads raises",
      "location": "pyjson.go:79",
      "recommendation": "Pre-validate the input with utf8.Valid (Python's strictness) or document this asymmetry beside the NaN/Infinity note, and pin the chosen behavior with a test."
    },
    {
      "severity": "LOW",
      "title": "PORT.md 'Verified parity' header attributes offline gates to the hosted endpoint",
      "location": "docs/PORT.md:105",
      "recommendation": "Scope the hosted-endpoint claim to the bullets that used the endpoint and state that the tokenizer, render and JSON gates were produced offline from the pinned snapshot and the Python client."
    }
  ]
}
END_REVIEW_JSON
