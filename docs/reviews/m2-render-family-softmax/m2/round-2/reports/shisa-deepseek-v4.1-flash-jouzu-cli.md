# Review Report

## Summary

I reviewed the Go port at HEAD (`e0a4baf`, fix commit `a76057a`) against the M2 scope: the pinned DE-2 renderer, the Python-compatible JSON writer/reader, question types and option limits, family resolution, and softmax. I could not run the suite (read-only tools, no shell), so I verified each round-1 fix by reading the code, the Go standard library it relies on, and the Python reference.

All the round-1 fixes are present and, on the questions the scope asks, correct:

- Repeated-key decoding keeps the last value at the first occurrence's position, which is what `json.loads` and a Python dict do, and `Object.Get` then agrees with what `PythonJSON` writes.
- `isJSONNumberLiteral` is exactly JSON's number grammar (`-?(0|[1-9][0-9]*)(\.[0-9]+)?([eE][+-]?[0-9]+)?`), and Go's own decoder rejects leading zeros and bare `.`/trailing `e` too, so the gate is belt-and-braces rather than load-bearing. `-0` → `0`, `1e400` → `Infinity`, `-1e400` → `-Infinity`, `1e-400` → `0.0`, and integers beyond 2^64 written verbatim all match CPython's `json.loads`+`json.dumps`.
- `isPythonSpace` is Python's `str.strip()` set: Go's `unicode.IsSpace` is a strict subset, and the added U+001C–U+001F are exactly the code points Python's bidi class B/S add.
- The softmax sort by `(len, lexicographic)` equals `CodesFor(n)` for every n in 2..256, because the codebook is 26 letters followed by 2-letter codes in lexical order (GZ skipped) and `CodesFor` returns a prefix.
- `validateQuestion`'s non-positive default and the name in its error message agree with `Payload` and `Render`, which both resolve the limit before validating.
- The fixture provenance check is sound in the way it can be: the digest covers the checked-in bytes, and the generator's `REVISION` reaches `AutoTokenizer` as `revision`, so it does pin the snapshot the prompts came from.

The new strictness rejects nothing the readout itself produces; the only behavior change for inputs the existing gates cover is the decoder's key deduplication, which moves the port toward `json.loads`, not away from it.

I found one MEDIUM and seven LOW findings. The MEDIUM is the mirror image of the round-1 duplicate-key fix: the decoder now deduplicates, but the writer still emits a repeated key when a caller hands it a hand-built `Object`, which Python's `json.dumps` can never produce.

## Findings

### MEDIUM: `PythonJSON` writes a repeated object key, which Python's `json.dumps` cannot produce
- Severity: MEDIUM
- Confidence: HIGH
- Location: pyjson.go:245 (`appendPythonObject`)
- Problem: `appendPythonObject` writes every member it is given. `DecodeJSON` was fixed to keep the last value at the first occurrence's position, but a caller-built `Object` is not deduplicated, so `PythonJSON(Object{{"a", 1}, {"a", 2}})` returns `{"a": 1, "a": 2}` while `PythonJSON(DecodeJSON([]byte(`{"a": 1, "a": 2}`)))` returns `{"a": 2}`. Python's `json.dumps` iterates a dict and can never emit a repeated key, so the encoder's output has no Python counterpart — the same class of divergence the decoder fix was for, on the writing side. It is reachable through the public API: `Payload(state, question, 0)` with an `Object` state renders that document into the prompt. `Object` is a `[]Member`, so duplicates are easy to build by accident from a list of pairs. The package doc's rule ("Anything else is an error naming the type rather than a guess at its JSON form") is not applied here.
- Recommendation: reject a repeated key in `appendPythonObject` with an error naming the key (a `map[string]struct{}` alongside the loop costs one allocation and keeps the O(n) walk), or state in the `Object` doc that a repeated key is the caller's responsibility and is written verbatim. A test for the writer path should accompany whichever choice.

### LOW: The prompt-token and code-boundary checks cannot detect a render divergence
- Severity: LOW
- Confidence: HIGH
- Location: docs/PORT.md:45-46, render.go:33-35
- Problem: PORT.md says of the hand-written renderer, "The `usage.prompt_tokens` check and the code-boundary check are what catch a divergence", and render.go repeats it ("the guard is indirect: every read checks that the prompt ends on `AnswerPrefix`, and the first read checks the server's prompt-token count against this client's"). Both checks compare the client's and the server's tokenization of the *same bytes the client produced*: the completions request carries the prompt text, the server counts what it received, and the boundary check compares `encode(AnswerPrefix + code)` against the client's own `encode`. A render that disagrees with the checkpoint's Jinja template (for instance a wrong `<bos>` literal) is consistent on both sides, so neither check fires. The real guard is `TestRenderParity` (33 byte-identical prompts against the Python client), which the same page describes correctly under "Verified parity".
- Recommendation: say what each guard actually catches — a client/server tokenizer mismatch — and name the render fixture as the guard for the render itself. This matters for M3, where resolving `<bos>` from the tokenizer is the check that would catch a wrong literal.

### LOW: `Softmax`'s total cannot be bit-identical to the Python client's on CPython 3.12+
- Severity: LOW
- Confidence: MEDIUM
- Location: readout.go:88-92
- Problem: The comment says the fixed order "also matches its arithmetic for code keys". The order does match: the Python DE-2 read rebuilds `logprobs` in option order before `softmax`, so it sums in codebook order. But CPython 3.12 changed `sum()` to Neumaier-compensated summation for floats ("higher accuracy and better commutativity", gh-100425), and the Python package allows `>=3.10`; the machine here has Python 3.14. Go's naive left-to-right accumulation therefore differs from `sum(weights.values())` in the last bits for some inputs, so the port's probabilities are not reproducible against the Python client's on a modern interpreter. The impact is a relative difference around 1e-16, which cannot change an argmax or a decision, and no test or doc number depends on it.
- Recommendation: soften the claim to what is true — the order matches, so the result is deterministic and as close to the Python client's as a non-compensated sum can be — and note the version dependence. Implementing Neumaier summation would match 3.12+ but move away from 3.10/3.11, so documenting is the better fix.

### LOW: UTF-8 rejection is not complete — `DecodeJSON` and `json.RawMessage` can still put U+FFFD or invalid bytes into the output
- Severity: LOW
- Confidence: HIGH
- Location: pyjson.go:79 (`DecodeJSON`), pyjson.go:185 (`json.RawMessage`), pyjson.go:348 (`appendPythonString`)
- Problem: Two paths bypass the new check. (1) `encoding/json`'s decoder coerces invalid UTF-8 and lone surrogate escapes to U+FFFD (`decode.go`: "Coerce to well-formed UTF-8"), so `PythonJSON(DecodeJSON([]byte("\"\xff\"")))` writes a replacement character where `json.loads` on those bytes raises `UnicodeDecodeError` — the exact substitution `appendPythonString` now refuses. (2) `json.Valid` does not check UTF-8 inside strings (`/usr/local/go/src/encoding/json/scanner.go:339` only rejects bytes below 0x20), so a `json.RawMessage` carrying invalid UTF-8 passes the gate and is written verbatim. The `RawMessage` behavior is documented as "inserted verbatim", but the package doc's "a string that is not valid UTF-8 ... has no Python bytes to match" reads as a general invariant, and `DecodeJSON`'s comment lists only the NaN/Infinity asymmetry.
- Recommendation: document both paths in the `DecodeJSON` and `RawMessage` comments, and consider `utf8.ValidString` on the decoded value in `DecodeJSON` (or a note that the caller must check it). No change is needed for the readout's own payloads, which are built from Go strings.

### LOW: `decodeValue`'s repeated-key replacement is quadratic in the member count
- Severity: LOW
- Confidence: HIGH
- Location: pyjson.go:127-136
- Problem: Each new key scans the members accumulated so far, so decoding an object with n distinct keys costs n(n-1)/2 comparisons. The readout's own payloads are tiny (three top-level keys, two per option), so nothing in M2 is affected, but `DecodeJSON` is exported and a caller decoding a large JSON document (tens of thousands of keys) pays seconds to minutes where `json.loads` is linear. The M2 changes made this loop load-bearing (it is the fix for the repeated-key finding) rather than incidental.
- Recommendation: keep a `map[string]int` from key to position while decoding the object and update in place, which is O(n) and preserves the last-value/first-position semantics exactly.

### LOW: `[N]byte` is rendered as an array of numbers where `[]byte` is an error
- Severity: LOW
- Confidence: HIGH
- Location: pyjson.go:207-215
- Problem: The `reflect.Slice` case rejects a byte slice with "pass a string or a base64 string", but the check is guarded by `Kind() == reflect.Slice`, so a byte *array* (`[32]byte` from `sha256.Sum256`, `[16]byte` from a UUID, a fixed-size header) falls through to the array path and renders as `[1, 2, 3, ...]`. Python's `json.dumps` raises `TypeError` for `bytes`, so this is a silent divergence of the same kind the byte-slice error exists to prevent, and the doc's "anything else is an error naming the type rather than a guess at its JSON form" does not hold for it.
- Recommendation: extend the byte-element check to arrays (`reflect.Array` with `Elem().Kind() == reflect.Uint8`), with the same error text.

### LOW: README's status paragraph is stale after M1 and M2
- Severity: LOW
- Confidence: HIGH
- Location: README.md:10-12
- Problem: "The repository currently holds the pinned codebook and the specification. The tokenizer, readout, policy, calibration, client and CLI are being ported in that order; nothing is callable yet." M1 landed the tokenizer and M2 landed the renderer, the question types, family resolution and the Python-JSON writer; `tokenizer`, `Render`, `Payload`, `PythonJSON`, `DecodeJSON` and `Softmax` are exported and usable. PORT.md and AGENTS.md were corrected in `a76057a`, so the three documents now disagree about the same state.
- Recommendation: update the paragraph to name M1/M2 as landed and list the remaining milestones, as AGENTS.md's project overview now does.

### LOW: AGENTS.md lists a live suite that does not exist, and the command passes silently
- Severity: LOW
- Confidence: MEDIUM
- Location: AGENTS.md:141
- Problem: The verification table gives `SHISA_DE_LIVE=1 go test ./... -run Live` as the "Live suite, hosted DE-2" command, but there is no live test in the tree (`codebook_test.go`, `family_test.go`, `pyjson_test.go`, `render_test.go`, `tokenizer/tokenizer_test.go`). `-run Live` matches nothing, so the command exits 0 and reports success. The project-overview paragraph marks planned items explicitly; this row is not marked, so it reads as an available check.
- Recommendation: mark the row planned (as the Key Files table does), or add a live test that the name matches.

## Things Checked

- `pyjson.go`: `DecodeJSON`'s repeated-key semantics against CPython's dict behavior (last value, first position) and against `Object.Get`; the trailing-data check (`decoder.Token()` vs `io.EOF`); `isJSONNumberLiteral` against JSON's grammar, including `-0`, `-01`, `00`, `1.`, `.5`, `1e`, `1e+`, `1E+5`, `1_000`, non-ASCII digits and trailing junk; `appendJSONNumber`'s integer path (verbatim, `-0`→`0`, arbitrary length), float path (`ParseFloat` then Python repr), and the `ErrRange` branch for `1e400`, `-1e400`, `1e-400`; `appendPythonFloat`'s exponent thresholds (`point <= -4 || point > 16`), two-digit exponent padding, integral `.0` and `-0.0` against CPython's `repr` rules; every path that writes text (`appendPythonString`, `appendPythonFloat`, `appendJSONNumber`, the `\u%04x` escape, `RawMessage`); the depth bound and the `[]byte`/non-string-map-key/struct rejections; `go vet`-visible control flow.
- Go standard library behavior the port depends on: `encoding/json`'s scanner (leading zeros rejected via `stateEndValue`; no UTF-8 validation in `stateInString`, so `json.Valid` accepts invalid UTF-8) and its decoder's "Coerce to well-formed UTF-8" replacement of invalid bytes and lone surrogates.
- `questions.go`: `isPythonSpace` against Python's `str.strip()` set (general category Zs plus bidi WS/B/S = Go's `White_Space` plus U+001C–U+001F); `trimPythonSpace` on both ends; `RenderOption` against `render_option` (`None`, `str`, blank); `validateQuestion`'s non-positive default and message against `Payload` and `Render`; `Score` not substituting its key.
- `readout.go`: the softmax key order against `CodesFor(n)` for n = 2..256 via the codebook's structure (`TestCodebookMatchesTheAudit`) and the prefix property (`TestCodesForIsAPrefixOfTheCodebook`); peak, weights and division all iterating the same sorted slice; NaN/±Inf inputs; the Python client's `softmax` and its `result.logprobs = {slot.letter: ... for slot in slots}` rebuild that fixes codebook order; `sum()`'s Neumaier behavior on CPython 3.12+.
- `render.go` / `family.go`: the template fragments and message shapes against `Readout.render` (`repeat > 1`, `INPUT_REPEAT.join`, thinking marker, no answer prefix when thinking); the payload keys and order against `_payload`; `ResolveFamily`'s empty-declaration reading against Python's `resolve_family` (`family is not None`), including that `NormalizeFamily("")` still errors.
- Fixtures and provenance: `scripts/gen_render_parity.py` (MODEL/REVISION constants, `tokenizer_revision` reaching `AutoTokenizer` as `revision`, sha256 over the same bytes written to the file, counts); `render_test.go`'s `TestRenderFixtureProvenance`, `loadRenderFixture`'s non-empty serialization check, and the 216-token re-attribution (the readout page carries 238 and 137 in section 2 and no 216); `testdata/render_parity.meta.json` against the script's fields.
- `docs/PORT.md` and `AGENTS.md`: the M1/M2 attribution, the "Python's JSON, not Go's" difference list, the render/JSON parity claims and counts, and the verification table against the files in the tree.
- Limits of this review: I could not run `go test`, `gofmt`, or `go vet`, so I did not independently confirm the pass claims in the scope, and I could not recompute the fixture's sha256. `testdata/render_parity.json` is a single 375 KB line, which the `read` and `grep` tools will not return, so I checked the fixture only through the test and generator logic.

## Residual Risks

- The provenance test cross-checks the meta's model/revision against `tokenizer.PinnedRepo`/`PinnedSnapshot`, but the meta is written from the generator's own constants, so a mis-pin at generation time would not be detected by the test; only a fixture edited after generation is caught. The recorded `transformers`/`tokenizers` versions are parsed and never checked, and `TestRenderTokenCounts` skips when no tokenizer.json is present, so a normal `go test ./...` does not re-verify the recorded token ids.
- `scripts/gen_render_parity.py` writes the fixture with `write_text(..., encoding="utf-8")`, whose default newline handling translates `\n` to `os.linesep`; on Windows the file bytes would no longer hash to the recorded `sha256` and the Go test would fail confusingly. A `newline=""` would make it deterministic.
- A `json.Number` with more than 4,300 digits is written verbatim here, where CPython 3.11+ raises `ValueError` on both `json.loads` and `json.dumps` (`sys.set_int_max_str_digits`). Not reachable from `DecodeJSON`, since Go's decoder accepts such a literal.
- A typed nil in an option description (`(*string)(nil)`) is not `nil` to `RenderOption`'s `description == nil` check, so it renders as `null` rather than falling back to the key; Python has no equivalent type, and the fixture's blank-description case does not cover it.
- Softmax still returns an unordered map, so M3 must carry the option order itself; the doc says so, and no caller can currently get the order from the result.

BEGIN_REVIEW_JSON
{
  "findings": [
    {
      "severity": "MEDIUM",
      "title": "PythonJSON writes a repeated object key that json.dumps cannot produce",
      "location": "pyjson.go:245",
      "recommendation": "Reject a repeated key in appendPythonObject (or document that an Object must not repeat one), and test the writer path."
    },
    {
      "severity": "LOW",
      "title": "Prompt-token and boundary checks cannot catch a render divergence",
      "location": "docs/PORT.md:45, render.go:33",
      "recommendation": "State that these checks catch a client/server tokenizer mismatch, and name the render fixture as the guard for the render itself."
    },
    {
      "severity": "LOW",
      "title": "Softmax total is not bit-identical to CPython 3.12+ sum()",
      "location": "readout.go:88",
      "recommendation": "Soften the comment to claim the matching order and Go-side determinism, and note that CPython 3.12+ uses compensated summation."
    },
    {
      "severity": "LOW",
      "title": "UTF-8 rejection bypassed by DecodeJSON coercion and json.RawMessage",
      "location": "pyjson.go:79, pyjson.go:185",
      "recommendation": "Document that DecodeJSON coerces invalid UTF-8 and lone surrogates to U+FFFD and that RawMessage is written verbatim; consider validating decoded strings."
    },
    {
      "severity": "LOW",
      "title": "decodeValue's repeated-key replacement is O(n^2)",
      "location": "pyjson.go:127",
      "recommendation": "Track key to position in a map while decoding the object to keep the scan linear."
    },
    {
      "severity": "LOW",
      "title": "[N]byte renders as an array of numbers where []byte is an error",
      "location": "pyjson.go:207",
      "recommendation": "Apply the byte-element rejection to arrays as well as slices."
    },
    {
      "severity": "LOW",
      "title": "README status paragraph is stale after M1 and M2",
      "location": "README.md:10",
      "recommendation": "Name M1/M2 as landed and list the remaining milestones, matching AGENTS.md."
    },
    {
      "severity": "LOW",
      "title": "AGENTS.md lists a live suite that does not exist and passes silently",
      "location": "AGENTS.md:141",
      "recommendation": "Mark the row planned, as the Key Files table does, or add a matching live test."
    }
  ]
}
END_REVIEW_JSON
