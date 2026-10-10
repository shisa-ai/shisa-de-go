# Review Report

## Summary

Read-only review of the M4 surface of the Go port at `/home/morpheus/data/dev/shisa/shisa-de-go`: `policy.go`, `calibration.go`, `client.go`, `policy_test.go`, `calibration_test.go`, `client_test.go`, `client_parity_test.go`, `scripts/gen_client_parity.py`, `testdata/client_parity.json` and its `.meta.json`, plus the two edits to `readout.go` (`httpClient` nil guard) and `family.go` (`UnsupportedFamilyError` message). The Python repository at `/home/morpheus/data/dev/shisa/shisa-de` is the reference, and `docs/READOUT-DE2.md` is the contract.

No shell tool is available in this session, so `gofmt`/`go vet`/`go test`/`-race` are the scope's claims, not reproduced here; every statement below is against the inspected working tree.

The answer-producing path matches the reference on every point I could trace. The gate is read on the raw distribution before calibration; the option cap is `len(slots) > 26`; each answer is read under the stage name of the last component (`direct` / `repeat2` / `think`), which is exactly the key `Calibration.TemperatureFor` needs; `covered` implements the "a single-fit record leaves a post-thought answer raw" rule; the read after a thought is `prompt_ids + token_ids + [prefix_id]` with a fresh prefix id; `Answer.Value` vs `Answer.Label` split matches `value` vs `label`; the meta and usage key sets are the Python sets (minus the image-only `chat_system_render`); `ResolveOptions` layers, aliases, refusals and error ordering follow `resolve_options` case by case; and `Calibration` selection, applicability levels, `ID()` formatting and the two arithmetic helpers match `calibration.py`. The parity fixture is genuine (23 cases, 32 exchanges, every recorded response carrying the server's `system_fingerprint`), and the replay does fail on a request body the Python client never sent.

The findings are one MEDIUM value-type leak that the parity gate structurally cannot see, and six LOW items: an untested rule, two doc claims that overstate what the gate and the meta record, a documented regeneration path that does not work, a dead branch, and one exotic input where the two clients render different prompts.

## Findings

### MEDIUM: `*float64` values leak into caller-visible `any` maps where the Python client returns a number, and the gate cannot see it
- Severity: MEDIUM
- Confidence: HIGH
- Location: `client.go:147` (score `ToWire`), `client.go:171` (`ToDict`), `client.go:796` and `client.go:828`/`client.go:838` (`Classify` view entries), `client.go:802` (multi-label `Raw`), with `client_parity_test.go:457-464`
- Problem: each of these sites stores `answer.Confidence` (a `*float64`) or `answer.Noul` (a `*float64`) into a `map[string]any`, where the Python client returns a plain float: `Answer.to_wire()["confidence"]` for a score, `Answer.to_dict()["confidence"]`, `classify`'s `{"label":…,"confidence":…}` entry, and `decision.raw["topic:billing"]["noul"]` (`client.py` `to_wire`, `to_dict`, `classify`, `_run`). The consequence is caller-visible and silent: `fmt.Println(decision.Raw["topic:billing"]["noul"])` prints a pointer address, and `answer.ToWire()["confidence"].(float64)` panics for a score while the identical assertion succeeds for a choice, which dereferences two lines earlier at `client.go:155-156`. `PORT.md` states that the accessors and `ToWire` dereference "so a caller reads `answer.Value().(float64)` rather than a pointer", and none of these five sites is listed among the deliberate differences — by this repo's own rule (`AGENTS.md`: "A difference that is not in it is an accident, not a decision") they are accidents. Nothing in the tree catches them: `compareAnswer` checks the `Answer` struct field, not the maps, and `compareDecision` builds `gotAnswers` from `answer.ToDict()` at `client_parity_test.go:457-459` but only uses its length (`:461`) and its key list inside an error message (`:463`), so `to_dict()` is never actually compared — and even if it were, `jsonShape` marshals a `*float64` to the same number as a `float64`.
- Recommendation: dereference at each site, mapping a nil pointer to an untyped nil `any` so the key stays present as `null` (Python's `None`), which is what `ToDict`'s `thought_closed` and `value` already do; or state the pointer convention in `PORT.md` and pin the Go type in a test. Separately, either compare `to_dict()` against the fixture or delete the unused `gotAnswers` so the test does not read as a comparison it does not make.

### LOW: the "a record without per-read temperatures leaves a post-thought answer raw" rule has no test that fails without it
- Severity: LOW
- Confidence: HIGH
- Location: `client.go:989`, with `client_test.go:944-947`
- Problem: `covered := len(m.Calibration.Reads) != 0 || read.Thought == nil` is the only thing that stops a single-fit record's direct temperature from being applied to an answer read after a thought. `TestDecideUncalibratedWhenNoRecordCoversTheRead` is named and commented for exactly that rule, but the record it builds (`client_test.go:947-953`) sets **both** `Temperatures` and a non-empty `Reads` (`{"direct": …}`), so `covered` is true and the answer comes back raw only because `TemperatureFor("noul", "think")` misses the per-read table and returns 1.0. I traced every other `Calibration{…}` construction in the tree and the parity fixture (whose shipped record has `reads`): with `covered` deleted entirely, `m.CalibrationApplied && calibrated` still yields the same answer in every case, so the rule is unenforced by the suite — the same class of gap M3 round 3 raised about the tokenizer-generation guard.
- Recommendation: add a case whose record has `Temperatures` and no `Reads` at all, run it through a read that thinks, and assert `Calibrated == false` and `Temperature == 1.0`; then deleting `covered` fails.

### LOW: `PORT.md` claims the client gate compares `legend` field for field; it does not
- Severity: LOW
- Confidence: HIGH
- Location: `docs/PORT.md:415`, `client_parity_test.go:528-570` (`compareAnswer`), `testdata/client_parity.json`
- Problem: the sentence reads "the answer, its probabilities, confidence, level, legend, strategy and provenance are compared field for field". `legend` appears nowhere in the fixture (the generator records `{**answer.to_dict(), "level": answer.level}`, and `to_dict` has no `legend`), `answerFixture` has no `Legend` field, and `compareAnswer` never looks at `Answer.Legend`. A wrong legend text would pass the replay. This is the same defect class the previous rounds raised twice in this file: a verification claim the gate cannot back. `level` is compared, and it is derived by the same `pythonStr` path, so the practical exposure is narrow — but the claim is false as written.
- Recommendation: record `"legend": answer.legend` in the generator alongside `level` and compare it in `compareAnswer`, or drop `legend` from the claim.

### LOW: the documented way to regenerate the client fixture does not work
- Severity: LOW
- Confidence: HIGH
- Location: `scripts/gen_client_parity.py:19` (docstring), `:175` and `:187` (`os.environ["API_KEY"]`), `:315-316` (the guard and its message), `AGENTS.md:181-182`
- Problem: the script reads only a bare `API_KEY`. Its docstring says `SHISA_DE_API_KEY=... python scripts/gen_client_parity.py`, its own error message says "set API_KEY (or SHISA_DE_API_KEY) before recording", and `AGENTS.md` says "Regenerating the fixture needs `SHISA_API_KEY`". Running it as either document says it should be run exits with "set API_KEY (or SHISA_DE_API_KEY) before recording". The one generator whose fixture carries a live serving fingerprint is therefore the one whose documented invocation fails, which matters because the fixture's provenance is a claim a reader is asked to trust.
- Recommendation: read the key the way the Python client and the rest of the docs do (`SHISA_DE_API_KEY`, then `SHISA_API_KEY`) and fix the message; or change the docstring and `AGENTS.md` to `API_KEY=…` and drop the "or SHISA_DE_API_KEY" clause.

### LOW: the client fixture's `serving_fingerprint` is an endpoint label, not the serving shape
- Severity: LOW
- Confidence: MEDIUM
- Location: `testdata/client_parity.meta.json:4`, `docs/PORT.md:406-407`, `AGENTS.md:181-183`, `client_parity_test.go:153`
- Problem: the meta field named `serving_fingerprint` holds `"shisa-hosted-2026-10-10"`, an endpoint-and-date label; `AGENTS.md`'s Evidence Rules define the fingerprint as the serving shape and name the hosted endpoint's fingerprint on 2026-10-10 as `vllm-0.30.0-tp2-ca69195e`. That value is available: all 32 recorded responses in the same fixture carry `"system_fingerprint": "vllm-0.30.0-tp2-ca69195e"`. The test only asserts the field is non-empty, so a reader checking reproducibility gets a label that will not distinguish one serving build from another — which is the failure mode the Evidence Rules exist to prevent.
- Recommendation: put the served `system_fingerprint` in the field (the generator can take it from the first recorded response body) and keep the endpoint label in `endpoint`, which already holds it.

### LOW: `pythonStr`'s float branch is dead code, and its fallback is not Python's `str()`
- Severity: LOW
- Confidence: HIGH (dead branch) / MEDIUM (fallback)
- Location: `client.go:1210-1214`, `client.go:1219-1224`
- Problem: `case float64:` computes `typed == math.Trunc(typed) && math.Abs(typed) < 1e16` and returns `pythonFloatString(typed)` either way, so the condition and its comment carry no behaviour. The default branch returns `PythonJSON(value)`, which is not `str()` for containers: a level of `{"a": 1}` renders as `{"a": 1}` where Python gives `{'a': 1}`, and `["a"]` gives `["a"]` where Python gives `['a']`. The doc comment hedges ("for the value kinds a level can hold") and the parity fixture only uses string levels, but `Levels []any` accepts anything, and the divergence lands in `Answer.Level` and `Answer.Legend`.
- Recommendation: collapse the float branch to one return (or restore whatever the integral case was meant to do), and either restrict `pythonStr` to the scalar kinds a level is expected to hold and say so, or make the container case produce Python's `repr` spelling.

### LOW: the multi-label template substitution is not Python's `str.format`, so a template with braces renders differently
- Severity: LOW
- Confidence: LOW
- Location: `client.go:323-328`, against `client.py` `_MultiLabel.noul_for`
- Problem: Python calls `self.template.format(label=label)`, Go calls `strings.ReplaceAll(template, "{label}", label)`. For a template that contains `{{` the two disagree on the rendered question (`"Is {label} a {{thing}}?"` → Python `"Is billing a {thing}?"`, Go `"Is billing a {{thing}}?"`), and for a template with another placeholder Python raises `KeyError` out of `classify`/`decide` while Go appends the label and proceeds. The template is caller-supplied free text (`HeadSpec.Instructions`, Python's `prompt`), and the prompt is the answer, so this is a silent prompt divergence on legal input.
- Recommendation: either document the difference in `PORT.md` (with the note that Go does not implement `str.format`) or reject a template containing `{` other than `{label}`, which is closer to what the Python client does with one than to what Go does with it now.

## Things Checked

- **Read sequencing.** `ReadPolicy` against `read_policy`: the `direct` branch renders `Repeat: 1` with the contract limit and spends one request; the default renders `Repeat: 2`, gates on `maxProbability(first.Probabilities) < ThinkGate` (raw, pre-calibration), and stops for `repeat` or `len(slots) > ThinkOptionCap`; the thinking branch renders with `Thinking: true`, thinks with `ThinkBudget`, resolves the prefix id fresh, and reads `PromptIDs + TokenIDs + [prefixID]` through `ReadCodesAfterThought` (id form, copied). The thinking render passes `OptionLimit: limit` where Python passes its default 26 — no divergence, because that branch is only reachable when the question has at most 26 options, where `Payload` renders identically under either limit. `Strategy` strings (`direct`/`repeat2`/`repeat2-think`) and component stage names match.
- **Per-read temperature selection.** `answerFromRead` is called with `read.Components[len-1].Stage`, matching `read.components[-1][0]`; the shipped record's `reads` keys are exactly `direct`/`repeat2`/`think`; the fixture pins `temperature: 6.8` for the two `repeat2-think` cases, `1.89` for `repeat2`, `1.0` with `calibrated: false` for the explicitly uncalibrated case and for the non-fitted checkpoint. `TemperatureFor` maps `score` to the `choice` fit and an unnamed read to 1.0, as Python does.
- **`ResolveOptions`.** Family defaults, the three layers, `policy` replacing the inherited read, `reads`/`reasoning` modifying one part, the `single`+`reasoning` refusal, the disagreement error (including that only the parts actually given are listed), the `FamilyPolicies` check, gate/budget/overflow ranges, the `compound`-on-DE-2 refusal, and the alias precedence (`ReasoningProb` over `ThinkGate`, `ReasoningLen` over `ThinkBudget`, `Overflow` over `Compound`) all match `resolve_options`, including validation order. The unknown-family path differs in message only (Python raises `KeyError` where Go returns a `ReadoutError`).
- **Calibration.** `Temperatures` vs `Reads` selection, `Fitted` over both tables, `ID`'s sorted per-read spelling and Python float formatting (pinned by the fixture's `calibration_id` and `TestShippedCalibration`), the four applicability levels and both mismatch reasons, `Matches`, the embedded record byte-identical to the Python `data/calibration-de2.json`, and `TemperBinary`/`TemperDistribution`/`Confidence` arithmetic against hand-computed values and the 1e-12 floors.
- **Friendly layer.** `Classify`'s head shorthands and default questions (`What is the <head>?`, `Is {label} one of the <head>?`), the multi-label expansion, threshold filter and the three views, the `Answers` pop plus `Raw` split, `Decide` returning `Value()` where `Classify` returns `Label()`, the per-head provenance blocks, the call-level overrides not leaking into `m.Options`, and the probability/reasoning refusal before any request.
- **Wire shapes and key sets.** `Answer.ToWire`/`ToDict` key sets against `to_wire`/`to_dict` (including the conditional `confidence`, `missing_from_top`, `finalists`, `thought_closed`), `Decision.ToWire`'s model/answers/usage, and the meta/usage key sets (the meta's `calibrated` collapse rule, `calibration_by_head`/`strategy_by_head` computed before the multi-label pop — the fixture's `topic:*` keys confirm it).
- **The parity gate.** `parityTransport` fails the case on an unmatched body and checks method and path; `used != len(exchanges)` is asserted; `loadClientFixture` checks the sha256, endpoint/model, and both counts, and fails without a date and fingerprint; `buildParityModel` reproduces the generator's `declared_model`/`declared_tokenizer` handling, including the aliased-checkpoint and other-checkpoint cases. `compareDecision`/`compareAnswer` cover every field the fixture records except `legend` (finding above), and `TestReadPolicy*` covers what the live fixture cannot force.
- **The two edited files.** `Readout.httpClient` refuses a nil receiver and a nil client before `net/http` can panic, which closes the M3 round-2 LOW; `Close` is nil-safe; `UnsupportedFamilyError`'s message now names the fix, and both `NewReadout` and `NewDecisionModel` refuse a DE-1 resolution with it.
- **Readout-version discipline.** M4 adds layers above the readout and changes no rendering, request, code handling, policy or scoring semantics, so no `de2-codebook-v1` bump and no `calibrationCompatible` change is warranted; `docs/READOUT-DE2.md` is unmodified. Correct.
- **Doc claims spot-checked.** `PORT.md`'s M4 paragraphs on `HeadSpec`, `OptionOverrides`, `ReadPolicy`, `Health`, `Close`, warnings and the replay match the code, apart from the `legend` sentence; `README.md`'s status paragraph and API example are accurate; the `AGENTS.md` provenance bullet lists `Family`, `ReadoutVersion`, `Policy` and `Calibration` as answer fields, which `Answer` does not carry — but the Python guide's own sentence has the same shape, so I did not raise it as a finding.
- **Not re-reviewed.** `tokenizer/`, `pyjson.go`, `render.go`, `questions.go`, `endpoint.go` and the M3 readout internals, except where M4 calls them; the M3 findings' resolutions are as reported in that round's reports.

## Residual Risks

- No shell tool, so no build, vet, test or race run, and no `git` ancestry or working-tree status. Findings are against the working-tree files as read.
- `testdata/client_parity.json` is a 10 KB-line-per-case file I read in windows rather than end to end; I verified the case list, the exchange count (32 `system_fingerprint` occurrences), the first case in full, the score and multi-label blocks, and the `values`/`raw`/`meta` shapes for the cases I reasoned about, but not all 23 cases field by field.
- I could not execute the Python client, so the fixture's recorded answers are taken as the Python client's results, as its provenance claims; my comparison of the two implementations is by reading `shisa_de/client.py`, `policy.py` and `calibration.py`.
- The softmax comparison is a relative 1e-12 gate by design; a probability that landed within 1e-12 of a Python value but on the wrong side of the 0.7 gate would not be caught by the replay (the gate is compared through the recorded `strategy`, and both implementations compute it from the same raw logprobs, so this needs a genuine near-gate coincidence to matter).
- The `think` temperature rests on the record's own note (475 rows, 383 cut by the budget, cross-validated ECE 0.18); that is the record's limit, not this port's, and this port has produced no served-model measurements of its own.

BEGIN_REVIEW_JSON
{
  "findings": [
    {
      "severity": "MEDIUM",
      "title": "Pointer values leak into caller-visible any maps where Python returns a number",
      "location": "client.go:147, client.go:171, client.go:796, client.go:802, client.go:828, client.go:838; client_parity_test.go:457-464",
      "recommendation": "Dereference the pointer at each site (nil -> untyped nil so the key stays null), or document the pointer convention in PORT.md and pin the Go type; also compare ToDict in the parity gate or delete the unused gotAnswers."
    },
    {
      "severity": "LOW",
      "title": "The covered rule (single-fit record leaves a post-thought answer raw) has no test that fails without it",
      "location": "client.go:989; client_test.go:944-947",
      "recommendation": "Add a record with Temperatures and no Reads, run it through a read that thinks, and assert Calibrated=false and Temperature=1.0."
    },
    {
      "severity": "LOW",
      "title": "PORT.md claims the client gate compares legend field for field; it does not",
      "location": "docs/PORT.md:415; client_parity_test.go:528-570; testdata/client_parity.json",
      "recommendation": "Record answer.legend in the generator and compare it, or drop legend from the claim."
    },
    {
      "severity": "LOW",
      "title": "The documented way to regenerate the client fixture does not work",
      "location": "scripts/gen_client_parity.py:19,175,187,315-316; AGENTS.md:181-182",
      "recommendation": "Read SHISA_DE_API_KEY then SHISA_API_KEY as the docs say, or change the docstring, the error message and AGENTS.md to a bare API_KEY."
    },
    {
      "severity": "LOW",
      "title": "The client fixture's serving_fingerprint is an endpoint label, not the serving shape",
      "location": "testdata/client_parity.meta.json:4; docs/PORT.md:406-407; AGENTS.md:181-183",
      "recommendation": "Record the served system_fingerprint (vllm-0.30.0-tp2-ca69195e in all 32 recorded bodies) in that field."
    },
    {
      "severity": "LOW",
      "title": "pythonStr's float branch is dead code and its container fallback is not Python's str()",
      "location": "client.go:1210-1214, client.go:1219-1224",
      "recommendation": "Collapse the float branch to one return and either restrict the documented value kinds or reproduce Python's repr spelling for containers."
    },
    {
      "severity": "LOW",
      "title": "Multi-label template substitution is not Python's str.format",
      "location": "client.go:323-328",
      "recommendation": "Document the difference in PORT.md or reject a template containing braces other than {label}."
    }
  ]
}
END_REVIEW_JSON
