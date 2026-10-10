All primary files, the Python reference, PORT.md, the tests, the generator and the fixture have been reviewed. Writing the report.

# Review Report

## Summary

The M4 layer — `policy.go`, `calibration.go`, `client.go`, their tests, and the client parity replay — is a faithful port of `shisa_de/policy.py`, `shisa_de/calibration.py` and `shisa_de/client.py` under the DE-2-only scope. The core semantics all check out against the Python reference:

- `ReadPolicy` sequences direct / repeated / thinking reads exactly as `read_policy` does: the gate (0.7) reads the raw pre-calibration distribution, the option cap (26) is checked against the slot count, the close terminator is always written after a thought, and the answer is the final component's read. Strategy names (`direct`, `repeat2`, `repeat2-think`) and stage names (`direct`, `repeat2`, `think`) match.
- `ResolveOptions` layers overrides identically to `resolve_options`, including the aliased names (`reasoning_prob`/`think_gate`, `reasoning_len`/`think_budget`, `compound`/`overflow`), the `single`+`reasoning` refusal, the policy/part disagreement errors, and both-names-must-agree.
- Each calibration temperature is applied to the read its fit describes: `readName` is the final component's stage, `TemperatureFor` selects the per-read entry, and `covered` (`len(reads) != 0 || thought == nil`) keeps an answer read after a thought raw under a record that only fits a pre-thought read — matching Python line for line, and pinned by `TestDecideUncalibratedWhenNoRecordCoversTheRead`.
- `answerFromRead`, the `Answer`/`Decision` views, the multi-label threshold views, the meta/usage key sets, and the wire shapes match the Python client; the shipped record is byte-identical to the Python one and its ID string is pinned.
- The parity test can genuinely fail: request bodies are matched exactly against the recorded exchanges (a body the Python client never sent fails the case, and every recorded exchange must be used), and answers, values, raw and usage/meta are compared with a 1e-12 relative tolerance.

No HIGH/MEDIUM findings. The findings below are LOW: three undocumented divergences from the Python client (the port's own rule is that an unlisted difference is a bug), one silent leniency in record loading, and two places where PORT.md describes a check that does not exist as described.

## Findings

### LOW: Decide drops Python's label-set shorthand acceptance, and PORT.md does not record it
- Severity: LOW
- Confidence: HIGH
- Location: shisa-de-go/client.go:Classify/Decide (Decide signature), shisa-de-go/docs/PORT.md (HeadSpec paragraph)
- Problem: Python's `decide` accepts "a `Noul`, `Choice`, or `Score`, or to the label-set shorthand `classify` accepts" (`client.py: parsed = {head: _question_from_head(head, spec)[0] ...}`). Go's `Decide` takes only `map[string]Question`; `questionFromHead` and `multiLabelHead` are unexported, so an external caller cannot build a multi-label or shorthand question outside `Classify` at all. PORT.md's `HeadSpec` paragraph documents the redesign of `classify`'s shorthands but never says `Decide` no longer accepts them, and the Go `Decide` doc comment silently drops the clause. Under the port's stated rule (a difference not in PORT.md is a bug) this is unrecorded. It fails loudly at compile time for a porting caller, which is why it is LOW.
- Recommendation: Either record the difference in PORT.md's deliberate-differences section and in the `Decide` doc comment, or accept a `HeadSpec`-carrying form in `Decide` (e.g. let `HeadSpec.Question` be built by callers via an exported constructor).

### LOW: MaxWorkers of 0 means 8 workers, where Python's max_workers=0 means 1
- Severity: LOW
- Confidence: HIGH
- Location: shisa-de-go/client.go (NewDecisionModel, `if maxWorkers == 0 { maxWorkers = 8 }`), docs/PORT.md "Go's zero values cannot express two of the Python constructor arguments"
- Problem: Python is `max(1, int(max_workers))` with a default of 8, so an explicit `max_workers=0` yields serial execution (1 worker). Go's zero value cannot express "explicit 0", so `MaxWorkers: 0` yields 8 concurrent workers. Negative values agree (both clamp to 1). PORT.md documents exactly this class of zero-value divergence for `SystemPrompt` and `MaxLogprobs` but not for `MaxWorkers`, and the field comment ("Zero means 8") does not note that Python reads 0 as 1. A ported caller forcing serial execution gets concurrency instead.
- Recommendation: Add `MaxWorkers` to the PORT.md zero-value paragraph (alongside `MaxLogprobs`), stating that Go cannot express Python's explicit `0`/negative clamp.

### LOW: CalibrationFromJSON accepts records the Python loader rejects, silently defaulting provenance fields
- Severity: LOW
- Confidence: HIGH
- Location: shisa-de-go/calibration.go:CalibrationFromJSON
- Problem: Python's `calibration_from_dict` indexes `raw["temperatures"]`, `raw["serving_shape"]` and `raw["fitted_on"]` and raises `KeyError` when any is missing. Go requires only `model` and `readout_version`; a record without `temperatures` loads as an unfitted record (never applied, so low risk), but a record missing `serving_shape` or `fitted_on` loads with empty provenance and is still applied when the caller passes it explicitly — the record's ID then carries an empty serving shape. This repo's own evidence rules treat the provenance fields as the thing that decides whether a record applies; a truncated record should be refused rather than silently completed.
- Recommendation: Require `serving_shape` and `fitted_on` (and `temperatures` or `reads`) in `CalibrationFromJSON`, mirroring the Python loader, with an error naming the missing key.

### LOW: HeadSpec.MultiLabel with described options silently diverges from Python's reading of the same input
- Severity: LOW
- Confidence: MEDIUM
- Location: shisa-de-go/client.go:questionFromHead / multiLabelHead.noulFor
- Problem: In Python, `{"labels": {label: description, ...}, "multi_label": True}` reaches the `isinstance(labels, Mapping)` branch, which ignores `multi_label` and builds a single `Choice` over the described labels. In Go, `HeadSpec{Options: described, MultiLabel: true}` runs one noul per label, uses only the `Key`s and silently discards every `Description`. The two clients therefore send different wire shapes for the same declared intent, and the Go path drops caller input without an error or a doc note. The `HeadSpec` redesign is documented, but this combination's meaning is not.
- Recommendation: Either refuse `MultiLabel` with non-nil descriptions (or require nil descriptions explicitly), or document on `HeadSpec.MultiLabel` that descriptions are ignored in the multi-label path and that Python reads the equivalent input as a single Choice.

### LOW: PORT.md overstates the client replay: the legend is not compared, and no `CalibrationForRead` exists
- Severity: LOW
- Confidence: HIGH
- Location: shisa-de-go/docs/PORT.md (Client bullet, line ~385 "level, legend, strategy ... compared field for field"; line ~392 "`CalibrationForRead` returning nothing")
- Problem: Two inaccuracies in the verified-parity section. (1) `answerFixture` decodes no `legend` field, and the recorded Python `to_dict` does not carry one, so `Answer.Legend` (and the score answer's `Choice` position key) is not compared by the replay; it is only constrained by the offline stub tests. (2) The policy-and-calibration bullet credits a test of `CalibrationForRead`, an identifier that exists nowhere in the package; the tested function is `Calibration.TemperatureFor`. The replay gate itself is sound — this is a documentation mismatch, but PORT.md is the plan document this review is auditing against.
- Recommendation: Correct PORT.md: say the legend is pinned by the offline tests and the recorded request bodies rather than by the replay, and rename `CalibrationForRead` to `TemperatureFor`. Optionally record `legend` in the fixture's answer objects so the replay does compare it.

### LOW: parityTransport matches recorded exchanges by request body only, so duplicate bodies cannot replay
- Severity: LOW
- Confidence: MEDIUM
- Location: shisa-de-go/client_parity_test.go:newParityTransport (byBody map)
- Problem: `byBody` maps a canonical request body to a single exchange index; two recorded exchanges with identical request bodies (e.g. a future case asking the same question twice under different heads, or a retry the Python client actually re-sent verbatim) would collapse to the last index. The failure mode is a loud false failure (`used != len(exchanges)`), not a false pass, so the gate stays sound — but a future fixture generation could produce an unreplayable case that looks like a client bug.
- Recommendation: Keep a queue of indices per body (pop in order) instead of a single index, or add a generator-side assertion that no case records two identical request bodies.

## Things Checked

- `policy.go` against `shisa_de/policy.py`: gate (0.7, raw distribution), option cap 26 (27-option trip test), budget 1024, direct/repeat/repeat-think sequencing, the always-written close after a thought, `PolicyRead` accessors (`requests`, `prompt_tokens`, `logical_reads`, `thought_tokens`), `Raw(debug)` key set, and `ResolveOptions` for both families and every layer of overrides, including the aliased names, both-names-agree, NaN gate refusal, and every refusal message.
- `calibration.go` against `shisa_de/calibration.py`: `TemperBinary`/`TemperDistribution` arithmetic (1e-12 floors, T≤0 and T==1 passthrough), `Confidence` clipping, `Fitted`, `ID` float formatting (`6.8`, `6.0`), per-read `TemperatureFor` selection and the 1.0 fallback, the four applicability levels and their reasons (including unfitted-wins-over-family ordering and double-mismatch reasons), `Matches`, `ResolveCalibration`, and the shipped record's byte-identity with the Python `data/calibration-de2.json`.
- `client.go` against `shisa_de/client.py`: answer fields and the three pointer-for-None fields, `Value`/`Label` (Decide returns the score position where Classify returns the level), `ToWire` and `ToDict` key sets, `Decision` views and `ToWire` usage, `HeadSpec` shorthands and default questions, multi-label threshold and its three views (including the Answers/Raw split and the head:label meta keys), probability skipping the thinking read, calibration request-vs-applied distinction, `calibrated` meta aggregation, usage arithmetic, `Health` report shape and probe, warnings-as-strings, API key env order, and the DE-1 refusal at construction.
- The two modified shared files: `readout.go` nil-receiver/nil-client guard in `httpClient`, and `family.go`'s `UnsupportedFamilyError` message naming the fix (pinned by tests, including the zero-config case).
- `calibration_test.go`, `policy_test.go`, `client_test.go`: coverage matches PORT.md's claims — gate from both sides, cap from both sides, close written when the model did and did not close, per-read uncalibrated case, deterministic head order, bounded pool, threshold views, provenance block, zero-value client reporting.
- `client_parity_test.go` and `scripts/gen_client_parity.py`: the generator records exactly what the test replays (spec-driven cases, so the Go side cannot drift from the Python call), request-body-exact matching with full exchange coverage, meta/usage/raw/values compared with `base_url` and `wall_ms` excluded, fixture pinned by sha256 and counts; the fixture's 23 cases / 32 exchanges and its serving fingerprint (`shisa-hosted-2026-10-10`, 2026-10-10) match the meta file.
- DE-2 validation limits: Go validating every choice at `MaxCodes` is equivalent to Python's wide/not-wide split (`MAX_CODES` above 26, `MAX_OPTIONS` below); the thinking render passing `OptionLimit: limit` where Python leaves the 26 default is unreachable-different because of the option cap, and the recorded thinking exchanges pin the bytes.

## Residual Risks

- The client parity gate (and the render/tokenizer gates it builds on) skips silently when the 32 MB pinned `tokenizer.json` is not cached; `SHISA_DE_TOKENIZER_REQUIRE=1` turns the skip into a failure. CI that does not set it runs M4 with the strongest check disabled.
- The fixture was recorded against one serving fingerprint on 2026-10-10. Per the repo's evidence rules, logprobs move between serving shapes; the replay pins that fingerprint's answers, not the hosted endpoint's current behaviour. A live M6 suite is the thing that would catch drift, and it is a declared non-goal for this stage.
- `sameFloat` allows a relative 1e-12 (absolute 1e-12 floor), which is the right tolerance for reordered float sums but would not catch a divergence smaller than that; no such divergence is known.
- Python's `Decision` is a dict subclass, so `decision[head]` and the exported accessor surface differ structurally; `DecisionModel` also lacks exported `Overflow`/`Compound` accessors (Python properties), though `Options` is exported and `Health` reports `compound`.

BEGIN_REVIEW_JSON
{
  "findings": [
    {
      "severity": "LOW",
      "title": "Decide drops Python's label-set shorthand acceptance, not recorded in PORT.md",
      "location": "shisa-de-go/client.go:Decide",
      "recommendation": "Record the difference in PORT.md and the Decide doc comment, or accept HeadSpec-style specs in Decide."
    },
    {
      "severity": "LOW",
      "title": "MaxWorkers of 0 means 8 workers where Python's max_workers=0 means 1",
      "location": "shisa-de-go/client.go:NewDecisionModel",
      "recommendation": "Document MaxWorkers alongside MaxLogprobs in PORT.md's zero-value paragraph."
    },
    {
      "severity": "LOW",
      "title": "CalibrationFromJSON accepts records Python rejects, silently defaulting provenance fields",
      "location": "shisa-de-go/calibration.go:CalibrationFromJSON",
      "recommendation": "Require serving_shape, fitted_on and temperatures/reads like the Python loader, erroring on the missing key."
    },
    {
      "severity": "LOW",
      "title": "MultiLabel with described options silently diverges from Python and ignores the descriptions",
      "location": "shisa-de-go/client.go:questionFromHead",
      "recommendation": "Refuse MultiLabel with non-nil descriptions, or document that only the Keys are used."
    },
    {
      "severity": "LOW",
      "title": "PORT.md overstates the client replay: legend not compared; CalibrationForRead does not exist",
      "location": "shisa-de-go/docs/PORT.md:385-392",
      "recommendation": "Fix PORT.md: legend is pinned offline, and rename CalibrationForRead to TemperatureFor."
    },
    {
      "severity": "LOW",
      "title": "parityTransport collapses duplicate request bodies into one exchange",
      "location": "shisa-de-go/client_parity_test.go:newParityTransport",
      "recommendation": "Queue indices per body or assert the generator never records duplicate bodies."
    }
  ]
}
END_REVIEW_JSON
