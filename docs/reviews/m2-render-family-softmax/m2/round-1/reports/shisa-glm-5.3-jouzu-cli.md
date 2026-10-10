# Review Report

## Summary

Reviewed the M2 surface of the Go port at commit 12a1a29 (`render.go`, `pyjson.go`, `questions.go`, `family.go`, `readout.go`, `scripts/gen_render_parity.py`, `render_test.go`) against the Python reference in `shisa-de/shisa_de` and `docs/READOUT-DE2.md`.

The load-bearing claims hold. `DirectSystem`, `ThinkSystem`, `InputRepeat` and `AnswerPrefix` are byte-identical to the Python constants and to the readout page, and the template assembly in `Render` reproduces the page's complete prompt, including the `\n` after each turn tag, the `<|think|>\n` position, and the empty thought block for the direct read. Every render shape the policy uses (direct, repeated, thinking) is covered by the parity corpus, which was produced by running the real Python client — the corpus constrains the implementation rather than being shaped by it. `PythonJSON` matches `json.dumps(ensure_ascii=False)` on the separator, escaping and float-repr rules, including the `repr`/exponent switch at exactly Python's `decpt <= -4 || decpt > 16` boundary; the fixed-point fixtures replay Python-produced bytes. The family slug matcher is behaviorally equivalent to the Python regex on every case in the scope question and on the further boundary cases I traced (`de1de2`, `dde1`, `d-e-1`, `de--1`); the resolution order and the DE-2-only refusal are right. The softmax arithmetic matches the Python one, including the all-`-Inf` NaN case. `readout.go`'s identity machinery has the same argument direction and unknown-version behavior as `calibration_compatible`.

One tooling limit: `testdata/render_parity.json` is a single 375 KB line, and my read/grep tools could not sample its contents (line-length limit). I verified the corpus by reading the generator, whose case list sums to the claimed 33 renders, and by the recorded validation runs (`go test ./...` passing the 33 fixtures). I could not independently re-derive the fixture bytes.

Findings: one MEDIUM (duplicate object keys diverge from `json.loads` and can silently change prompt bytes), and six LOW (softmax map-order nondeterminism, silent U+FFFD substitution for invalid UTF-8, three `json.Number` decode divergences, three documentation-accuracy items).

## Findings

### MEDIUM: DecodeJSON keeps duplicate object keys; json.loads keeps the last
- Severity: MEDIUM
- Confidence: HIGH
- Location: pyjson.go:76-97 (`decodeValue`, object case); pyjson.go:59-66 (`Object.Get`)
- Problem: `decodeValue` appends a `Member` for every key occurrence, so `DecodeJSON('{"a": 1, "a": 2}')` returns an `Object` with two `a` members. Python's `json.loads` keeps the last value. Two downstream behaviors diverge: `PythonJSON` writes `{"a": 1, "a": 2}` where Python writes `{"a": 2}`, so a state JSON blob with a duplicate key renders a different user turn — and therefore different prompt bytes — than the Python client would send, silently. And `Object.Get` returns the first occurrence where Python's dict holds the last, so the two halves of the package disagree with each other. The module's documented contract is "parses JSON the way Python's json.loads does", and this is the one input class where that claim is false.
- Recommendation: In `decodeValue`, when the key already exists, either replace the existing member's value (Python semantics) or reject the document with an error naming the duplicate key. Replacement requires `Get` to stay consistent (a linear replace keeps first position, which matches Python: dict preserves first-insertion position with the last value). If a deliberate keep-both is preferred, document it in `Object`'s comment as a divergence from `json.loads` — but silent divergence in the layer whose job is byte parity is the outcome to avoid.

### LOW: Softmax accumulates the total in randomized map order, so probabilities are not bit-reproducible
- Severity: LOW
- Confidence: HIGH
- Location: readout.go:90-104 (`Softmax`)
- Problem: `total += weight` iterates the input map, whose order is randomized per run in Go. The float sum therefore varies in the last ulp between calls with identical logprobs, and from the Python client, whose `sum(weights.values())` runs in deterministic option order (the Python readout rebuilds `result.logprobs` in slot order before normalizing). The effect is ~1 ulp on each probability; it cannot practically flip the 0.7 gate, but it defeats exact reproducibility of reported probabilities, and `TestSoftmaxDoesNotDependOnMapOrder` papers over it with a 1e-15 tolerance rather than eliminating it.
- Recommendation: Accumulate in a deterministic order: sort the keys once and iterate that order for both weights and total. Cheap, and makes Go output bit-comparable to Python's for the same logprobs.

### LOW: PythonJSON silently replaces invalid UTF-8 with U+FFFD instead of failing
- Severity: LOW
- Confidence: HIGH
- Location: pyjson.go:244-268 (`appendPythonString`)
- Problem: `utf8.DecodeRuneInString` returns U+FFFD for invalid bytes, and the writer emits it, so a state string containing invalid UTF-8 (for example, file bytes cast to `string` in the wrong encoding) is silently rewritten into the prompt. Python cannot reach this state — a Python `str` is always valid, and encoding such bytes would raise before a prompt existed — so there is no Python counterpart to match; the substitution produces prompt bytes that are neither the caller's input nor an error.
- Recommendation: Scan for invalid UTF-8 (for example with `utf8.ValidString`) and return an error naming the position. If the substitution is kept deliberately, document it in `PythonJSON`'s comment.

### LOW: Three json.Number edge cases diverge from Python's parser/dumper
- Severity: LOW
- Confidence: HIGH
- Location: pyjson.go:199-215 (`appendJSONNumber`), pyjson.go:41-56 (`DecodeJSON`)
- Problem: Three literals where the Go path differs from `json.loads` + `json.dumps`:
  1. `json.Number("-0")` is written back as `-0` (no `.`, `e`, `E`), but Python parses `-0` as the integer `0` and dumps `0`.
  2. `json.Number("1e400")`: `strconv.ParseFloat` returns a range error, so `PythonJSON` fails; Python parses `1e400` as `inf` and dumps `Infinity`.
  3. `DecodeJSON("NaN")` (and `Infinity`, `-Infinity`) is rejected by `encoding/json`, but Python's `json.loads` accepts these literals by default — and `PythonJSON` itself writes them, so the pair is asymmetric where Python's is round-trip symmetric.
  None of these appear in the parity corpus, so a regression here would pass the gate. All three are pathological for evidence states; item 1 is the only one that silently produces wrong bytes rather than an error.
- Recommendation: Normalize integer-form `json.Number` text to Python's int semantics (at minimum `-0` → `0`). For 2 and 3, either match Python or keep the stricter behavior and document it in `PythonJSON`/`DecodeJSON`'s comments; erroring is defensible, an unmentioned asymmetry is not. Add all three to `pyjson_test.go`.

### LOW: PORT.md attributes the M2 files to M1 and the planned rows to M2-or-later
- Severity: LOW
- Confidence: HIGH
- Location: docs/PORT.md:31 ("M1 has landed the codebook, the tokenizer, the renderer, family resolution, the question types and the Python-JSON writer"; "Rows marked planned are M2 or later")
- Problem: The renderer, family resolution, question types and Python-JSON writer are M2 (this review's scope, commit 12a1a29), not M1; the rows marked planned are M3 through M6. The sentence is wrong in both directions.
- Recommendation: Rewrite as "M1 landed the codebook and the tokenizer; M2 landed the renderer, family resolution, the question types and the Python-JSON writer; rows marked planned are M3 or later."

### LOW: AGENTS.md presents the M3–M5 surfaces as existing code
- Severity: LOW
- Confidence: HIGH
- Location: shisa-de-go/AGENTS.md:42-49 ("Project Overview": "readout.go — the raw reads: render the repeated prompt, request every option's code by token id, ... generate a bounded thought"; "policy.go — sequences the reads"; "client.go — the friendly layer") and the Summary bullet "Every answer records its provenance (Family, ReadoutVersion, Policy, ...)"
- Problem: `readout.go` currently holds the scaffold constants, readout identities and `Softmax` only — no requests, no boundary checks, no thought generation; `policy.go` and `client.go` do not exist; there is no `Answer` type. The Key Files table in the same file marks policy, calibration, client and CLI as "Planned (M3)" and following, so the file contradicts itself, and an agent following the Overview would expect request code to review against.
- Recommendation: Mark the Project Overview rows as the target layout the same way the Key Files table does, and qualify the provenance bullet as planned.

### LOW: render_test.go attributes the 216-token jp-choice pin to the readout page, which says nothing about it
- Severity: HIGH that the attribution is wrong; the check itself is sound
- Severity: LOW
- Confidence: HIGH
- Location: render_test.go:153-163 (`TestRenderMatchesTheReadoutPage`)
- Problem: The test's comment says it "pins the three prompts the readout page names", and the failure message says "the readout page says %d". `docs/READOUT-DE2.md` section 2 names two token counts: 238 and 137. I searched both repositories' docs and source: 216 appears nowhere outside the fixture and this test. The 216 pin is a valid fixture regression check, but its stated provenance is false, and this project's evidence rules treat a number without its source as a defect.
- Recommendation: Either cite 216's actual source (the recorded fixture, generated on a named date and revision) or drop the page attribution for that entry and keep only 238 and 137 as page-pinned.

## Things Checked

- **Scaffold constants**: `DirectSystem`, `ThinkSystem`, `InputRepeat`, `AnswerPrefix` compared character by character against `shisa_de/readout.py` and `docs/READOUT-DE2.md` sections 2 and 4. Identical. The thinking line is the direct line with only the reasoning sentence replaced, in both.
- **Renderer template**: walked the assembly for the direct, repeated and thinking shapes against the page's complete prompt sample: `<bos>`, `<|turn>system\n`, system content, `<turn|>\n`, `<|turn>user\n`, user, `<turn|>\n`, `<|turn>model\n`, then `<|channel>thought\n<channel|>` for the direct/repeated read and nothing for the thinking read; `<|think|>\n` at the top of the system turn for thinking. `Repeat <= 1` and negative `Repeat` write the user turn once, matching Python's `if repeat > 1`; `Repeat > 2` joins the same way Python's `INPUT_REPEAT.join([user] * repeat)` does. The policy only ever renders thinking with `repeat=1` (`policy.py:263` uses the default), so every shape the readout needs is covered by the corpus; thinking with `repeat=2` is producible by both and used by neither.
- **PythonJSON against `json.dumps(ensure_ascii=False)`**: `", "`/`": "` separators; `\"`, `\\`, `\b\f\n\r\t` short forms; other control characters as lowercase `\u00xx`; non-ASCII, `<>&`, `/`, DEL and U+2028/2029 unescaped; `NaN`/`Infinity`/`-Infinity`; `-0.0`; the repr/exponent switch — `1e15` fixed, `1e16` exponential, `1e-4` fixed, `1e-5` exponential, point <= -4 || point > 16 matching CPython's decpt rule exactly; two-digit exponents; integers written exactly at any width (int64, uint64, and `json.Number` passthrough for values beyond 2^64, where float64 would lose digits); nil/bool/empty containers; depth cap 200. The fixed-point fixtures replay Python-produced bytes through `DecodeJSON` → `PythonJSON`.
- **DecodeJSON against `json.loads`**: int-or-float kept apart (`UseNumber`), key order preserved via `Object` (justified: key order reaches the prompt), exponent/decimal integers become floats and re-render as Python floats, surrogate-pair escapes handled by `encoding/json`, leading/trailing whitespace accepted, trailing non-whitespace rejected. `Object` as a slice of pairs survives review for the ordering reason; `Payload`/`DecodeJSON` build fresh objects, and the shared slices (`Codebook`, `CodesFor` above 26) are documented read-only.
- **Option-to-code assignment**: `Payload` → `Options()` → `CodesFor(len(options))` → `letter` = code i, matching `readout._payload`. `Choice.Options` preserves the caller's slice order and applies `RenderOption` (`nil` and blank-string descriptions replaced by the key — identical to `render_option`); `Score` keys by position and does not substitute blank levels, matching Python; `Noul` fixed to yes/no. `Validate` bounds come from the contract limit passed in, defaulting to 26, not the renderer's default; a limit above 256 still fails in `CodesFor`, same as `codes_for`.
- **Family resolution**: `hasFamilySlug` traced against every id in the review question plus `de1de2`, `dde1`, `d-e-1`, `de--1`, `de 2`, `2`, and empty — all agree with the Python regex `(?<![a-z0-9])de-?N(?![a-z0-9])` after lowercasing (boundary bytes of multibyte runes are never ASCII alphanumerics, so the byte-level checks are safe). Resolution order declared → model id → tokenizer → assumed matches the documented one; `SourceAssumed` is a distinct value so an assumption cannot be reported as a detection; `RequireDE2` returns `*UnsupportedFamilyError` rather than falling back. `NormalizeFamily("de--2")` and `"de 2"` yield `de2`, `"2"` and `""` error — same as Python.
- **Softmax**: max subtraction, underflow to zero for `-Inf` alongside a finite peak, all-`-Inf` producing NaN exactly as Python does, single key yielding probability 1, empty input erroring (matches the Python contract), and non-code keys left to the caller (also matching Python).
- **Identity machinery**: `CalibrationCompatibleWith(fitted, current)` has the same argument direction as `calibration_compatible`; an unknown `current` cannot match anything except by equality (nil map lookup in Go, `.get(current, ())` in Python — same). The DE-2 empty compatibility list is the intended statement and matches the Python table; the DE-1 entry mirrors the shared source of truth and is inert in a port that refuses DE-1 models at `RequireDE2`.
- **Parity corpus**: generated by running the Python client's real `_payload`/`render`/tokenizer, not the Go code, so it is not shaped to the implementation; the generator's case list sums to 33 renders covering both template shapes, the empty-description rule, non-string descriptions, 17 evidence forms, widths 2/26/27/77/256, and long evidence. The token check is id-exact (compares every id, not just the count), so it adds the Go tokenizer's agreement with `transformers` on full prompts and pins the page's 238/137 — beyond the byte comparison. `TestRenderParity` runs without any tokenizer; only `TestRenderTokenCounts` skips when `tokenizer.json` is absent, with `SHISA_DE_TOKENIZER_REQUIRE=1` available to turn the skip into a failure — the right split for the byte half of the gate.
- **Docs claims**: PORT.md "No Jinja" and "Python's JSON, not Go's" paragraphs are accurate and match the code; PORT.md's "Verified parity" section does not overclaim on M2 (it names one render case; the 33-case gate is not claimed there). The M1/M2 attribution and the AGENTS.md overview are the inaccurate claims (findings above).

## Residual Risks

- The fixture file `testdata/render_parity.json` is one 375 KB line my tools could not sample; my confirmation of its contents is the generator's case list plus the recorded passing runs, not independent inspection.
- Shortest-round-trip float formatting: `appendPythonFloat` builds on Go's `strconv` shortest form and CPython's `repr` uses its own dtoa; I verified the format boundaries and a spread of values but not all 2^64 doubles. If the two shortest-repr implementations ever disagree on a tie case, the divergence would be one digit in a float state value, and the corpus would not catch it.
- Unicode case-fold oddities in slugs: Python's `re.IGNORECASE` matches `ſ` (U+017F) and the Kelvin sign against `[a-z]` in lookarounds; Go lowercases first. For ids like `ſde-1` the two can disagree (Go names DE-1, Python names nothing). No real checkpoint id looks like this; noting it because the scope asks for exactness in this matcher.
- `ResolveFamily` treats `declared == ""` as "not declared"; Python's `resolve_family` treats `family=""` as an invalid declaration and raises. Given Go's string type this is a reasonable adaptation, but a caller passing an empty family string gets a silent assumption in Go where Python raises.
- A Go `map` passed as evidence has its keys sorted; Python serializes a dict in insertion order, so the same evidence built as a Go map produces different prompt bytes than the Python caller's dict. `Object` exists for order-sensitive evidence and the doc comment says so, but the M5 `Classify`/`Decide` surface must take this into account or wide map evidence will diverge from Python.
- The parity corpus does not cover `NaN`/`±Inf` state values, duplicate keys, or `-0`, so regressions in those areas pass the gate (findings above cover the current behavior).
- The `DirectSystem`/`ThinkSystem` constants are duplicated between the two repositories by design; the Python repo is the source of truth, and a change on one side that misses the other changes answers silently. The fixtures would catch it on the next regeneration, not before.

BEGIN_REVIEW_JSON
{
  "findings": [
    {
      "severity": "MEDIUM",
      "title": "DecodeJSON keeps duplicate object keys where json.loads keeps the last",
      "location": "pyjson.go:76",
      "recommendation": "Replace or reject duplicate keys in decodeValue so the rendered user turn cannot silently diverge from Python's bytes; keep Object.Get consistent."
    },
    {
      "severity": "LOW",
      "title": "Softmax accumulates in randomized map order, so probabilities are not bit-reproducible",
      "location": "readout.go:90",
      "recommendation": "Iterate sorted keys when accumulating the total so identical logprobs produce identical probabilities."
    },
    {
      "severity": "LOW",
      "title": "PythonJSON silently replaces invalid UTF-8 with U+FFFD",
      "location": "pyjson.go:244",
      "recommendation": "Return an error on invalid UTF-8 in a string value, or document the substitution."
    },
    {
      "severity": "LOW",
      "title": "json.Number edge divergences: -0, out-of-range exponents, NaN/Infinity literals",
      "location": "pyjson.go:199",
      "recommendation": "Normalize -0 to 0 for integer-form numbers; document or match Python for 1e400 and NaN/Infinity literals; add tests."
    },
    {
      "severity": "LOW",
      "title": "PORT.md attributes the M2 files to M1 and the planned rows to M2 or later",
      "location": "docs/PORT.md:31",
      "recommendation": "Correct the stage attribution: M1 the codebook and tokenizer, M2 renderer/family/questions/pyjson, planned rows M3+."
    },
    {
      "severity": "LOW",
      "title": "AGENTS.md Project Overview describes M3-M5 code as present",
      "location": "shisa-de-go/AGENTS.md:42",
      "recommendation": "Mark the readout/policy/client overview rows as planned, consistent with the Key Files table."
    },
    {
      "severity": "LOW",
      "title": "Test pins 216 tokens for jp-choice-repeat2 to a readout page that never says 216",
      "location": "render_test.go:153",
      "recommendation": "Cite the fixture as the source of the 216 pin or drop the page attribution."
    }
  ]
}
END_REVIEW_JSON
