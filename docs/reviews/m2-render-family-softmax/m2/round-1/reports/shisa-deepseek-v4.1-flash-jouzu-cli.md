# Review Report

## Summary

I reviewed the M2 commit (`12a1a29`) of the Go port in `/home/morpheus/data/dev/shisa/shisa-de-go` against the Python reference in `/home/morpheus/data/dev/shisa/shisa-de`: `render.go`, `pyjson.go`, `questions.go`, `family.go`, `readout.go`, `scripts/gen_render_parity.py` and the tests.

The central claim holds. I checked the pinned renderer line by line against the actual template the Python client runs — `shisa-ai/shisa-de-2` snapshot `8f21734b…` carries `chat_template.jinja` and no `chat_template` in `tokenizer_config.json`, so `apply_chat_template` uses that file — and the assembled bytes match for the direct read (Repeat 1 and 2), the thinking read (Repeat 1), and thinking with Repeat 2. The `\n` placement, the position of `<|think|>\n` inside the system turn, the `<|channel>thought\n<channel|>` suffix and its absence when thinking are all correct; the template's `| trim` on the system and user content is a no-op for these strings. `PythonJSON` matches `json.dumps(…, ensure_ascii=False)` on the separators, the escaping set, and the fixed/scientific boundary at `decpt <= -4 || decpt > 16`, including the `Py_DTSF_ADD_DOT_0` rule. `family.go` reproduces `family.py`'s `de-?[12]` slug semantics, including `de-13`, `de1x`, `my-de_1-finetune` and the both-families case. `questions.go`'s option order and empty-description rule match `questions.py`.

No finding reaches HIGH; nothing I found produces a wrong prompt for the shapes the readout uses. The findings are about a test that can pass vacuously, a JSON divergence from `json.loads`, and several smaller fidelity gaps.

## Findings

### MEDIUM: The Python-JSON fixed-point test can pass with zero cases
- Severity: MEDIUM
- Confidence: MEDIUM
- Location: `pyjson_test.go:118-120`, `render_test.go:55-64`
- Problem: `loadRenderFixture` fails the run when `len(fixture.Renders) == 0`, but `TestPythonJSONFixedPoints` iterates `fixture.Serialization` with no equivalent guard. If the checked-in `testdata/render_parity.json` does not contain a `serialization` array — an older generator, a hand-trimmed fixture, or a key typo — the test passes with zero cases and the Python-JSON parity claim is unverified while still reporting green. I could not settle which case applies: `testdata/render_parity.json` is a single 375 KB line and neither `read` (line exceeds the 50 KB limit) nor `grep` (which reads `tokenizer_parity.json` but returns "No matches found" for this file) could open it, so I could not confirm the array is present. The 33-render count in the scope matches the generator's case list exactly, which is weak evidence the fixture is current.
- Recommendation: add the same non-empty guard for `fixture.Serialization` (and for the specific `floats`/`strings` groups if you want the corpus pinned), and note in `render_test.go` that the fixture must be regenerated with `scripts/gen_render_parity.py` whenever the Python client's renderer changes.

### MEDIUM: `DecodeJSON` diverges from `json.loads` on duplicate object keys
- Severity: MEDIUM
- Confidence: HIGH
- Location: `pyjson.go:100-119`, `pyjson.go:78-85`
- Problem: `decodeValue` appends every member, so `{"a": 1, "a": 2}` decodes to an `Object` with two members and `PythonJSON` re-emits `{"a": 1, "a": 2}`. Python's `json.loads` keeps the last value at the first key's position, so `json.dumps(json.loads(x))` gives `{"a": 2}`. `Object.Get` compounds this by returning the first match where a Python dict returns the last. Any caller who decodes evidence or a payload with `DecodeJSON` before rendering gets different prompt bytes from the Python client on such input. No fixture or unit test covers a duplicate key, so the parity corpus would not catch it.
- Recommendation: decide and document the intended behaviour. Either make `decodeValue` replace an existing key's value in place (matching `json.loads`), or document that `DecodeJSON` preserves duplicates and is therefore not a `json.loads` equivalent for that input, and add a case to `TestDecodeJSONKeepsIntsAndFloatsApartAndKeyOrder`.

### LOW: The render fixture has no pinned provenance
- Severity: LOW
- Confidence: HIGH
- Location: `scripts/gen_render_parity.py:147`, `testdata/render_parity.json`
- Problem: The generator builds `Readout(base_url="http://unused.invalid", model="shisa-ai/shisa-de-2")` with no revision, so `ensure_tokenizer()` resolves whatever snapshot the hub serves at generation time — both the recorded token ids and the recorded prompt bytes (which come from that snapshot's `chat_template.jinja`) depend on it. Unlike the M1 fixture, there is no `render_parity.meta.json` recording model, revision, library versions or a sha256, so a regenerated or edited fixture is undetectable and the test cannot tell which tokenizer produced the ids. The provenance exists only as prose in the `render_test.go` header comment. PORT.md records "the tokenizer fixture fails if it disagrees with its meta file"; the render fixture has no equivalent.
- Recommendation: pass `tokenizer_revision="8f21734b27b3462471c4662d5638e52195b005a3"` in the generator, write a `testdata/render_parity.meta.json` with the counts and a sha256 of the fixture, and check it from `loadRenderFixture`.

### LOW: `Softmax` accumulates in Go map order, so the result is order-dependent in the last bits
- Severity: LOW
- Confidence: HIGH
- Location: `readout.go:96-99`, `pyjson_test.go:200-214`
- Problem: `total += weight` walks the map, and Go randomizes map iteration per iteration, so the same call can return probabilities differing in the last bits between runs and from Python, which sums in dict insertion order. `TestSoftmaxDoesNotDependOnMapOrder` names this invariant but only asserts agreement to `1e-15` over three keys, so it documents the tolerance rather than proving invariance. The answer is an argmax, so the practical risk is a near-tie flipping; the contract is that the bytes and the numbers are reproducible.
- Recommendation: accumulate over a deterministic order (iterate a sorted key slice, or accept the logprobs as an ordered `[]struct{Key string; Value float64}`), and strengthen the test to require exact equality.

### LOW: `strings.TrimSpace` is not Python's `str.strip()` for U+001C–U+001F
- Severity: LOW
- Confidence: HIGH
- Location: `questions.go:43-51`, `questions.go:53-58`
- Problem: The empty-description rule and the blank-instruction check are contract behaviour ("a label with no description renders as the label"), and `render_option`/`validate` in Python use `description.strip()` / `str(...).strip()`. Python's `str.isspace()` is true for U+001C–U+001F (file, group, record and unit separators); Go's `unicode.IsSpace` is false for them. A description or instruction consisting only of those characters is blank to Python and non-blank to this port, so the rendered prompt differs. The parity fixtures cover `""`, `None`, `"   "` and control characters inside a value, but not a separator-only description.
- Recommendation: replace `strings.TrimSpace` with a small Python-compatible predicate (`unicode.IsSpace(r) || (r >= 0x1c && r <= 0x1f)`) used by both `RenderOption` and `validateQuestion`, and add a fixture case for `"\x1c"`.

### LOW: Non-canonical and out-of-range `json.Number` texts are not canonicalized the way Python's parser would
- Severity: LOW
- Confidence: MEDIUM
- Location: `pyjson.go:238-254`
- Problem: `appendJSONNumber` writes any text without `.eE` verbatim, so `DecodeJSON([]byte("-0"))` → `json.Number("-0")` → `"-0"`, where `json.loads("-0")` gives the int `0` and `json.dumps` gives `"0"`. In the other direction, `DecodeJSON([]byte("1e400"))` succeeds (the scanner accepts it, `UseNumber` keeps the text) and then `PythonJSON` fails with a `ParseFloat` range error, where Python's `json.loads` gives `inf` and `json.dumps` gives `"Infinity"`. Neither input is covered by a fixture, so the corpus would not catch it.
- Recommendation: canonicalize the integer branch (strip a leading `-` from `-0`, reject non-canonical digit runs) and treat `strconv.ErrRange` as ±Infinity to match Python, or document that `DecodeJSON` refuses the second class and add both as test cases.

### LOW: Invalid UTF-8 and lone surrogates are silently replaced instead of rejected
- Severity: LOW
- Confidence: MEDIUM
- Location: `pyjson.go:257-282`, `pyjson.go:87-100`
- Problem: `appendPythonString` runs `utf8.DecodeRuneInString` and writes `U+FFFD` for each invalid byte, so a caller-supplied Go string holding invalid UTF-8 (or a CESU-8 surrogate) renders a prompt containing replacement characters. Python cannot represent that state: `json.dumps('\ud800', ensure_ascii=False)` returns a `str` that raises `UnicodeEncodeError` when the prompt is encoded, so the Python client fails loudly where this port substitutes silently. `DecodeJSON` likewise turns a `\ud800` escape into `U+FFFD` where `json.loads` keeps the lone surrogate. The renderer's whole purpose is byte-identity, so a silent substitution is the failure mode it is meant to avoid.
- Recommendation: reject strings that are not valid UTF-8 in `appendPythonString` (return an error naming the offset), or document the substitution explicitly in the `PythonJSON` doc comment so a caller knows the bytes are not Python's.

### LOW: `Softmax` returns a map, discarding the option order the Python client preserves
- Severity: LOW
- Confidence: MEDIUM
- Location: `readout.go:82`
- Problem: Python's `readout.py` rebuilds `result.logprobs` in option order (`{slot.letter: … for slot in slots}`) before `softmax`, so `probabilities` and `result.raw[head]["components"]` carry the option order that `to_wire()`, `explain` output and the accounting table in `docs/READOUT-DE2.md` rely on. A `map[string]float64` cannot carry that order, so M3 has to reconstruct it from the slot list at every call site. This is a design choice rather than a bug, but it moves an invariant out of the type that owns it.
- Recommendation: keep the map for lookup but have `Softmax` (or its caller) also return the order it was given, or accept an ordered slice of code/logprob pairs and return an ordered slice of probabilities.

### LOW: DE-1 readout identity is carried in a port that refuses DE-1
- Severity: LOW
- Confidence: MEDIUM
- Location: `readout.go:36-63`
- Problem: `ReadoutVersionFor(FamilyDE1)` returns `de1-letter-slots-v3` and `calibrationCompatible` lists `de1-letter-slots-v1`/`-v2`, in a package whose `RequireDE2` refuses DE-1 outright. A caller can therefore ask "which readout version does this family use" or "is this calibration compatible" and get a confident answer about a contract this client cannot produce. The table is faithful to the Python one, which is a defensible reason to keep it, but nothing in the file says the DE-1 entries are unreachable here.
- Recommendation: either state in the `readout.go` comment that the DE-1 entries exist only to keep the identity table faithful and are unreachable because no DE-1 read is implemented, or drop them and let a future DE-1 milestone add them with its read.

### LOW: `Question.Validate` does not default a zero limit the way `Payload` does
- Severity: LOW
- Confidence: HIGH
- Location: `questions.go:53-73`, `render.go:45-48`, `family.go:150-153`
- Problem: `Payload` treats `optionLimit <= 0` as `MaxOptions`, but the exported `Validate(limit int)` methods do not: `NewNoul("Is it?").Validate(0)` fails with "offers 2 answers … supports at most 0 per prompt", which no caller of the readout can hit but a direct caller can. Relatedly, `ResolveFamily` treats `declared == ""` as "not declared" and falls through to the model id, where Python's `resolve_family(family="")` calls `normalize_family("")` and raises. The second is the better Go idiom; the first is an inconsistency between two exported entry points to the same rule.
- Recommendation: have `validateQuestion` default `optionLimit <= 0` to `MaxOptions` (or reject a non-positive limit with a clear message), and document that an empty `declared` string means "not declared" where Python raises.

### LOW: PORT.md misattributes M2's work to M1 and does not record the render/JSON parity checks
- Severity: LOW
- Confidence: HIGH
- Location: `docs/PORT.md:30-32`, `docs/PORT.md:86-117`
- Problem: "M1 has landed the codebook, the tokenizer, the renderer, family resolution, the question types and the Python-JSON writer. Rows marked planned are M2 or later." is wrong in both directions: the renderer, family resolution, question types and JSON writer are M2 (`12a1a29`), and the rows marked planned are M3–M6, not M2. The Verified-parity section then records one render ("the section 2 spam question … 238 tokens") and nothing at all about the JSON writer, although the file mapping lists `json.dumps`/`json.loads` → `pyjson.go` and there is a checked-in fixed-point fixture. There is also no "Python's JSON, not Go's" paragraph among the deliberate differences, so the reason `pyjson.go` exists at all is documented only in a code comment.
- Recommendation: correct the stage attribution, and add verified-parity entries for the 33-render gate and the JSON fixed-point gate, saying whether each was checked against the fixture and when.

### LOW: The renderer pins the literal `<bos>`, which comes from `tokenizer_config.json`, not the template
- Severity: LOW
- Confidence: MEDIUM
- Location: `render.go:32`, `render.go:98`
- Problem: `templateBOS = "<bos>"` reproduces `{{- bos_token -}}` for the pinned checkpoints (`shisa-ai/shisa-de-2` and `google/gemma-4-26B-A4B-it` both declare `"bos_token": "<bos>"`), but `bos_token` is a tokenizer-config value, not part of the template. A checkpoint with the same Gemma 4 template and a different `bos_token` renders a prompt that differs in its first token, and the `render.go` comment about the pin mentions only the template. The documented guard (prompt-token check, boundary check) is M3 and does not exist yet.
- Recommendation: say in the `render.go` comment that the pin includes `bos_token` as well as the template, and let the M3 read resolve `bos_token` from the tokenizer or check it against the literal.

## Things Checked

- **Renderer vs the template.** Read `chat_template.jinja` at `shisa-ai/shisa-de-2@8f21734b` (the file `apply_chat_template` uses, since `tokenizer_config.json` has no `chat_template`). Walked the setup block, the message loop and the `add_generation_prompt` block for `enable_thinking` false and true: `<bos><|turn>system\n` + optional `<|think|>\n` + system + `<turn|>\n` + `<|turn>user\n` + user + `<turn|>\n` + `<|turn>model\n` + optional `<|channel>thought\n<channel|>`. Matches `Render` exactly, including that the thinking shape ends at `<|turn>model\n` with no answer prefix. Confirmed the `| trim` on both contents is a no-op for `DirectSystem`, `ThinkSystem` and the JSON payload, and that `Repeat` 1, 2, >2, 0 and negative follow the same `repeat > 1` rule as `readout.py:render`.
- **Scaffold constants.** `DirectSystem`, `ThinkSystem`, `InputRepeat` and `AnswerPrefix` are byte-identical to `shisa_de/readout.py` (`DIRECT_SYSTEM`, `THINK_SYSTEM`, `INPUT_REPEAT`, `ANSWER_PREFIX`); `ThinkSystem` is the direct line with only the reasoning sentence replaced.
- **Option-to-code assignment.** `Payload` builds `evidence`, `criterion`, `options` in that order and `letter` = code *i* for option *i*, matching `_payload`'s `zip(codes_for(len(options)), options)`. `Noul`/`Score`/`Choice` option orders match `questions.py`; `RenderOption` is applied only for `Choice`, as in Python; `Validate` bounds are the caller's `optionLimit` (defaulting to `MaxOptions`) and `CodesFor` enforces `MaxCodes`, so the 256-code limit is not the renderer's default.
- **`PythonJSON` fidelity.** Separators `, ` and `: `; escaping of `"`, `\`, `\b\f\n\r\t`, `\u00xx` below 0x20; non-ASCII, `<`, `>`, `&`, `/`, DEL and U+2028/9 left raw; `NaN`/`Infinity`/`-Infinity`; negative zero; the float fixed/scientific boundary at `decpt <= -4 || decpt > 16` and the two-digit exponent; `int64`/`uint64`; nil; bools; empty containers; `json.RawMessage` verbatim; `[]byte` and non-string map keys rejected (Python would stringify an int key — a divergence in the loud direction). `map` key sorting is a documented Go-only behaviour that cannot arise in Python.
- **`DecodeJSON`.** Integer/float separation via `UseNumber`, key order preserved in `Object`, `null`/scalars/arrays/objects, leading and trailing whitespace, trailing-data rejection, surrogate-pair escapes combined, very large integers kept verbatim. Divergences recorded above.
- **`family.go`.** `hasFamilySlug` against `shisa-ai/shisa-de-1`, `de1`, `de-1`, `DE_2` (no marker, as in Python), `de1-cont-lr1e5`, `de-13`, `de1x`, `xde1`, `my-de_1-finetune`, `de2-vs-de1-ablation`, `de-2`, a tokenizer path containing `de-1`, and an id naming both; resolution order declared → model id → tokenizer → assumed, with `SourceAssumed` always reported; `RequireDE2` returning `*UnsupportedFamilyError` rather than falling back. Matches `DE1_SLUG`/`DE2_SLUG` and `resolve_family`.
- **`readout.go` identity.** `ReadoutVersionFor` and `calibrationCompatible` match `READOUT_VERSIONS` and `CALIBRATION_COMPATIBLE`; `CalibrationCompatibleWith(fitted, current)` has the same argument order and the same "unknown current matches only itself" behaviour as `calibration_compatible`.
- **Tests.** `TestRenderParity` always runs (it needs no tokenizer), so the byte half of the gate is not skippable; `TestRenderTokenCounts` skips without a tokenizer.json and `SHISA_DE_TOKENIZER_REQUIRE=1` turns that into a failure; `TestRenderMatchesTheReadoutPage` pins 238/137 against the page. `TestRenderShape`, `TestRenderRefusesWhatItCannotRender`, `TestRenderDefaultOptionLimit`, `TestQuestionValidation`, `TestQuestionOptions` cover the shape and refusal paths. Noted that the fixture's `state` values are all decoded into `Object`, so the Go-native map path is covered only by hand-written expectations, and that Repeat > 2 has no test or fixture.

## Residual Risks

- **I could not read `testdata/render_parity.json`** (single 375 KB line; `read` refuses it and `grep` does not open it). Everything I say about the fixture's contents is inferred from `scripts/gen_render_parity.py`, `render_test.go` and the 33-case count. If the fixture lacks the `serialization` array, the JSON parity claim is currently unverified.
- **Float formatting is asserted, not proven against Python at runtime.** The formatter's rule matches CPython's `format_float_short` and `Py_DTSF_ADD_DOT_0` as I read them, and the fixtures' float group (if present) is Python-derived, but I could not execute either implementation here. A shortest-repr tie broken differently by Go's `strconv` and CPython's `_Py_dg_dtoa` would be caught only by a fixture value that happens to be such a tie.
- **The renderer's guards are M3.** Today nothing at runtime compares the pinned render against the server: the prompt-token check and the code-boundary check that `render.go` and PORT.md name as the safety net do not exist in this commit. A template change at the endpoint is silent until M3 lands.
- **`Codebook`/`CodesFor` return the shared codebook slice** (`codes[:count]`), documented as "must not be modified" but not enforced; a caller can corrupt the codebook for the process. Out of scope as M0 code, but `Payload` depends on it.
- **Wire shapes are not ported.** `questions.py`'s `to_wire()` has no counterpart, and neither does `_messages`/`read_image`, so `PORT.md`'s file mapping is a target rather than a description for those rows.
- `README.md` still says the repository holds "the pinned codebook and the specification", which has been stale since M1; it is a high-conflict file and M2 did not touch it.

BEGIN_REVIEW_JSON
{
  "findings": [
    {
      "severity": "MEDIUM",
      "title": "Python-JSON fixed-point test can pass with zero cases",
      "location": "pyjson_test.go:118",
      "recommendation": "Guard fixture.Serialization for non-empty like fixture.Renders, and state that the fixture must be regenerated with scripts/gen_render_parity.py."
    },
    {
      "severity": "MEDIUM",
      "title": "DecodeJSON keeps duplicate object keys where json.loads keeps the last",
      "location": "pyjson.go:112",
      "recommendation": "Replace an existing key's value in place, or document the divergence and add a duplicate-key test case."
    },
    {
      "severity": "LOW",
      "title": "Render fixture has no pinned provenance or sha256",
      "location": "scripts/gen_render_parity.py:147",
      "recommendation": "Pin tokenizer_revision, write a render_parity.meta.json with counts and a sha256, and verify it from the test."
    },
    {
      "severity": "LOW",
      "title": "Softmax sums in randomized map order",
      "location": "readout.go:97",
      "recommendation": "Accumulate over a deterministic order and tighten TestSoftmaxDoesNotDependOnMapOrder to exact equality."
    },
    {
      "severity": "LOW",
      "title": "TrimSpace does not match Python str.strip for U+001C-U+001F",
      "location": "questions.go:43",
      "recommendation": "Use a Python-compatible whitespace predicate in RenderOption and validateQuestion, and add a separator-only fixture case."
    },
    {
      "severity": "LOW",
      "title": "json.Number texts are not canonicalized like json.loads",
      "location": "pyjson.go:238",
      "recommendation": "Canonicalize -0 and non-canonical integers, and treat strconv.ErrRange as +/-Infinity, or document the refusals."
    },
    {
      "severity": "LOW",
      "title": "Invalid UTF-8 and lone surrogates are silently replaced with U+FFFD",
      "location": "pyjson.go:280",
      "recommendation": "Reject strings that are not valid UTF-8, or document the substitution, since Python raises instead."
    },
    {
      "severity": "LOW",
      "title": "Softmax returns a map and loses option order",
      "location": "readout.go:82",
      "recommendation": "Return the order alongside the map, or accept and return ordered code/logprob pairs."
    },
    {
      "severity": "LOW",
      "title": "DE-1 readout identity is reachable in a DE-2-only port",
      "location": "readout.go:44",
      "recommendation": "State that the DE-1 entries are unreachable and kept for table fidelity, or drop them until a DE-1 read exists."
    },
    {
      "severity": "LOW",
      "title": "Question.Validate does not default a zero limit as Payload does",
      "location": "questions.go:53",
      "recommendation": "Default a non-positive optionLimit to MaxOptions in validateQuestion, and document empty declared family as not declared."
    },
    {
      "severity": "LOW",
      "title": "PORT.md misattributes M2 work to M1 and omits render/JSON parity",
      "location": "docs/PORT.md:30",
      "recommendation": "Fix the stage attribution and add verified-parity entries for the 33-render gate and the JSON fixed-point gate."
    },
    {
      "severity": "LOW",
      "title": "Renderer pins literal <bos>, which comes from tokenizer_config",
      "location": "render.go:32",
      "recommendation": "Document that the pin includes bos_token, and resolve or check it in the M3 read."
    }
  ]
}
END_REVIEW_JSON
