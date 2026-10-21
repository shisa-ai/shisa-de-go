# Review Report

## Summary

The round-2 change does the core job. The DE-2.1 record is embedded, `CalibrationForCheckpoint` selects by checkpoint with the model id first, `Applicability` normalises the record's own model, `ResolveCalibration` loads a path before a name, and the offline tests pin the selection table, the per-read temperatures and the record provenance. The DE-2.1 section of the readout page matches the Python page.

I found one HIGH documentation defect left from round 1, two MEDIUM defects in the new warnings that can recommend the wrong fit on documented correct configurations, a MEDIUM bug in the new live test's thinking path, and four LOW issues.

## Findings

### HIGH: The local-server bullet still documents the tokenizer as the alias remedy

- Severity: HIGH
- Confidence: HIGH
- Location: `README.md:431-433`, contradicting `README.md:310-316`; the round-1 recommendation covered both the alias paragraph and this bullet.
- Problem: The bullet says: "If the server uses an alias for the checkpoint, pass the alias as `Model` and the real checkpoint as `Tokenizer`. The tokenizer name also tells the client which family the alias is, so a DE-2 alias keeps the DE-2 calibration." For `Model: "shisa-ai/shisa-de-2"` (an alias for DE-2.1) and `Tokenizer: "shisa-ai/shisa-de-2.1"`, `ReleasedCheckpoint(model)` is DE-2, so `CalibrationForCheckpoint` returns DE-2's record; the tokenizer is consulted only when the model id names no released checkpoint. The bullet leads a self-hosting user to the over-correction the change exists to prevent. The conflict warning tells a library caller what happened, but the bullet is false and sits where a local-server user looks first.
- Recommendation: Rewrite the bullet to match 310-316: the tokenizer names the family, and the checkpoint only when the model id names none; a released-id alias needs `Calibration`. Add the documented alias case to a doc check or test so the two sections cannot drift again.

### MEDIUM: The supplied-record warning fires for custom fits and for the documented alias remedy

- Severity: MEDIUM
- Confidence: HIGH
- Location: `client.go:719-724`; `client_test.go:748-767`; `README.md:323-337`; `README.md:352-366`; `docs/PORT.md:147-149`.
- Problem: The guard is `supplied && level == ApplicabilityFamily` followed by `own.Model != supplied.Model`. That catches any supplied record at family level, not just a family spelling. The README's custom-fit example (`Model: "my-de2-finetune"`, `Calibration: &record` from `my.json`) is at family level when the record's model is not the served name, so it warns: "the record passed is fitted on X and my-de2-finetune names shisa-ai/shisa-de-2, which has its own record; pass that record to calibrate it" — telling the caller to discard the fit they vouched for. The documented alias remedy (supplied DE-2.1 record, `Model: "shisa-ai/shisa-de-2"`) is also at family level and warns "pass that record", which is DE-2's. PORT.md says the warning is for "a supplied record [that] is the family's"; the code does not check that.
- Recommendation: Warn only when the supplied record is the family's record, for example `supplied.Model == familyRecord.Model` from `CalibrationFor(family)`. Add tests for the custom-fit and alias-supplied cases expecting no such warning, and keep `TestAFamilyRecordOnACheckpointWithItsOwnRecordIsReported` passing.

### MEDIUM: The conflict warning is false with a supplied record and advises the wrong fit on the correct DE-2.1 + DE-2 tokenizer configuration

- Severity: MEDIUM
- Confidence: HIGH
- Location: `client.go:705-714`; `client_test.go:1822-1858`; `README.md:310-316`.
- Problem: The warning is emitted regardless of `supplied`. When the caller follows the alias remedy (`Model: "shisa-ai/shisa-de-2"`, `Tokenizer: "shisa-ai/shisa-de-2.1"`, supplied DE-2.1 record), it says "the model id chose the record. Pass Calibration to apply the other checkpoint's fit", but the supplied record is applied and the model id did not choose it. On the documented correct serving shape (`Model: "shisa-ai/shisa-de-2.1"`, `Tokenizer: "shisa-ai/shisa-de-2"`, no supplied record), it warns and advises applying DE-2's fit to DE-2.1. The comment above already says the reverse disagreement is a correct configuration. The test table also omits the documented alias case, and its `warned != applied` invariant would fail if the reverse configuration were added (applied true, warned true).
- Recommendation: Gate the warning on `!supplied`, and make the advice neutral: the model id chose X's record; if the tokenizer names the weights actually served, pass `Calibration` for Y. Suppress it for the known DE-2.1 + DE-2 tokenizer pairing, or state that the pairing is expected. Add the reverse case and replace the XOR with an assertion on the specific warning.

### MEDIUM: The new live test fails on any thinking answer

- Severity: MEDIUM
- Confidence: HIGH
- Location: `live_test.go:1342` and `live_test.go:1356`; contrast `live_test.go:1022`.
- Problem: `answer.Strategy` is `"repeat2-think"` for a thinking read, but `TemperatureFor` keys the record by `"think"`. It returns 1.0 while the answer's temperature is 5.23 and `Calibrated` is true, so a correct answer that thinks fails both the temperature assertion and the "other record shares this temperature" loop. `TestLiveDE2ThinksWhenTheRepeatedReadIsUnsure` already uses `TemperatureFor("choice", "think")`, which is the intended key.
- Recommendation: Map the strategy to the read name (`"repeat2-think"` to `"think"`, otherwise the strategy) or derive the last stage from the raw components. Add a think-path case to this test so the mapping is pinned.

### LOW: The ASCII-fold subset claim is false; U+0130 and U+212A next to the slug match here and not in Python

- Severity: LOW
- Confidence: HIGH
- Location: `calibration.go:123-148`; `docs/PORT.md:131-139`; `calibration_test.go:650-670`; `calibration.go:378` and `:384` for the `Applicability` fallback.
- Problem: `asciiLower` leaves U+0130 and U+212A as multi-byte runes, and `continuesCheckpointName` treats a byte at or above 0x80 as a boundary. `"shisa-de-2.1İ"` (U+0130 after the slug) matches here; Python's `lower()` turns it into `i` plus U+0307, and the lookahead sees `i`, so Python does not match. `"Kshisa-de-2.1"` (U+212A) matches here; Python's lower gives `k`, and the lookbehind fails. The port is therefore not a subset, contrary to the comment and PORT.md. The U+017F comment also confuses `lower()` with `casefold()`: Python's lower leaves U+017F, so neither client matches it inside the slug. The `Applicability` fallback uses `strings.ToLower`, so a supplied record whose model contains U+0130 can also match a served name here and not in Python.
- Recommendation: Correct the claim and the test comment, and add `{"shisa-de-2.1\u0130", ""}` and `{"\u212Ashisa-de-2.1", ""}`. To make the fold a strict subset, decode the adjacent rune and treat it as continuing when `unicode.ToLower` of it is an ASCII `[a-z0-9.]`; otherwise document the divergence. Use the same rule in `Applicability`'s fallback.

### LOW: A missing no-extension path containing a checkpoint slug still resolves to the bundled record

- Severity: LOW
- Confidence: HIGH
- Location: `calibration.go:582-625`, `calibration.go:617-625`; `docs/PORT.md:152-157`.
- Problem: `looksLikeRecordPath` is true only for a `.json` suffix or an existing regular file. `/fits/shisa-de-2.1/deployment` (missing), `./deployment/shisa-de-2.1` (missing) and `~/fits/shisa-de-2.1/deployment` (missing) are not paths, so `ReleasedCheckpoint` matches the slug and the bundled DE-2.1 record is returned. This is the silent fallback the path-first rule was meant to remove, for a path without `.json`. The function's comment and PORT.md's "a misspelt path is an error" overclaim.
- Recommendation: Treat a leading `/`, `./`, `../` or `~/` as a path even without `.json` and return an error if it is missing, or state in PORT.md that only `.json` and existing files are paths. Add a test for a missing absolute path containing the slug.

### LOW: PORT.md does not record two divergences, and its test inventory is stale

- Severity: LOW
- Confidence: HIGH
- Location: `docs/PORT.md:122-129`, `docs/PORT.md:141-150`, `docs/PORT.md:565-572`, `docs/PORT.md:843-851`.
- Problem: (a) `Applicability` now normalises the record's own model through `ReleasedCheckpoint`; Python compares `record.model.lower()` raw, so a record fitted on `shisa-ai/shisa-de-2.1-FP8-dynamic` reaches checkpoint level here and family level there. PORT.md presents the normalisation as a rule both sides use. (b) The two Go-only warnings (conflict, supplied-family) are not in "Warnings are recorded, not raised", which says the port collects Python's two. (c) The calibration test inventory still says "the shipped record's temperatures and provenance" and omits the name table, the per-record provenance loop, the path-before-name test and the family-record-on-a-checkpoint-with-its-own-record test.
- Recommendation: Add (a) and (b) to PORT.md, and update the inventory to name the current tests.

### LOW: The new warnings never reach the CLI

- Severity: LOW
- Confidence: HIGH
- Location: `cmd/shisa-de` (`main.go`, `doctor.go`) has no reference to `DecisionModel.Warnings`; `docs/PORT.md:565-572`.
- Problem: The detection mechanism is `DecisionModel.Warnings`, but `doctor` prints only `Health` and `ask` prints the decision. A CLI user who passes `--model shisa-ai/shisa-de-2 --tokenizer shisa-ai/shisa-de-2.1` sees `calibration_model: shisa-ai/shisa-de-2`, `match checkpoint`, `applied yes` and no disagreement. That is the alias configuration the change is about. PORT.md's "Nothing is printed" is a deliberate difference, but it leaves the CLI without the signal the library adds.
- Recommendation: Print `model.Warnings` to stderr in `doctor` (and optionally `ask`), or add them to the doctor report. If the silence is deliberate, say in the README that the CLI does not surface them and library callers must read `Warnings`.

## Things Checked

- The round-2 prompt, scope and manifest under the Python repository's `docs/reviews/de2-1-checkpoint-calibration/implementation/round-2/`, and the `shisa-de-go` working tree at `12faffa` (main).
- `ReleasedCheckpoint`: exact ids, suffix and local-directory names, the `[a-z0-9.]` boundary cases (`shisa-de-2.10`, `shisa-de-2.2`, `shisa-de-21`), 2.1-before-2 precedence, uppercase ASCII, U+0130 inside the slug, U+017F, Japanese adjacency, and the U+0130/U+212A adjacent counterexamples.
- `CalibrationForCheckpoint`: model id first, tokenizer only when the model id names no release, family guard, quantized and local names.
- `Applicability`: checkpoint, family and mismatch levels; the record-side normalisation; reasons; the tokenizer fallback; `Fitted` and `TemperatureFor` for records with and without `reads`.
- `ResolveCalibration`: `auto`, path with `.json`, existing path, checkpoint id, family spelling, missing `.json`, missing no-extension path containing a slug, and the error text.
- Client warnings: the not-applied warning, the conflict warning and the supplied-family warning, traced through the README alias remedy, the README custom-fit example, the correct DE-2.1 + DE-2 tokenizer configuration, and the `TestDecisionModelCalibration` and `TestAFamilyRecordOnACheckpointWithItsOwnRecordIsReported` cases.
- Live test: the model-id-then-tokenizer expectation, the non-empty `Reads` requirement, the per-read table, the other-record temperature loop, the think-path mismatch, and the existing think test's `"think"` key.
- Records: DE-2 and DE-2.1 temperatures against the README table and the DE-2.1 section of the readout page; per-read keys `direct`, `repeat2`, `think`; provenance and withheld entries.
- Readout version: `de2-codebook-v1` is unchanged, `calibrationCompatible` is unchanged, and no answer-producing contract changed, so no version bump is required.
- Docs: README calibration, alias, local-server and custom-fit sections; `AGENTS.md`; `docs/PORT.md` calibration, warnings and test-inventory sections; the DE-2.1 section of `docs/READOUT-DE2.md` compared with the Python page.
- CLI: `--calibration` and `--model` help, `doctor` output, and the absence of `Warnings` printing.
- This was a read-only review without a shell. I did not run `gofmt`, `go vet`, `go test`, `go test -race` or `git diff`; the scope's validation record is the evidence for the passing runs.

## Residual Risks

- The DE-2.1 section of `docs/READOUT-DE2.md` still says the record is "chosen when the model ID (or the tokenizer name) names it", without the model-id-first caveat. The page is byte-identical to Python's and is the spec, so the caveat has to be fixed upstream rather than in the port alone.
- The hosted `shisa-ai/shisa-de-1` id is an alias for DE-2. A caller who declares `Family: "de2"` but does not pass `Calibration` gets the family record held and raw answers, because no DE-1 record ships. The warning says so, but the README's DE-1 section does not.
- The live test still passes in the alias configuration (model id and tokenizer naming different checkpoints) by design: it verifies the client's selection rule, not the weights the server actually loaded. It could fail loudly on a disagreement instead of passing.
- The `warned != applied` invariant in `TestTheClientAppliesTheRecordOfTheCheckpointItServes` no longer holds for the correct reverse configuration. Adding that case will fail until the conflict warning is fixed.
- The `--calibration <family>` spelling applies DE-2's fit to a DE-2.1 endpoint. The family spelling is now documented, and the supplied-family warning fires, but the CLI does not print it.

BEGIN_REVIEW_JSON
{
  "findings": [
    {
      "severity": "HIGH",
      "title": "The local-server bullet still documents the tokenizer as the alias remedy",
      "location": "README.md:431-433, contradicting README.md:310-316",
      "recommendation": "Rewrite the bullet to match 310-316: the tokenizer names the family and, only when the model id names no released checkpoint, the checkpoint; a released-id alias needs Calibration. Add the documented alias case to a doc check or test."
    },
    {
      "severity": "MEDIUM",
      "title": "The supplied-record warning fires for custom fits and for the documented alias remedy",
      "location": "client.go:719-724; client_test.go:748-767; README.md:323-337; README.md:352-366; docs/PORT.md:147-149",
      "recommendation": "Warn only when the supplied record is the family's record, for example supplied.Model == familyRecord.Model. Add tests for the custom-fit and alias-supplied cases expecting no such warning."
    },
    {
      "severity": "MEDIUM",
      "title": "The conflict warning is false with a supplied record and advises the wrong fit on the correct DE-2.1 + DE-2 tokenizer configuration",
      "location": "client.go:705-714; client_test.go:1822-1858; README.md:310-316",
      "recommendation": "Gate on !supplied, make the advice neutral or suppress the known DE-2.1 + DE-2 pairing, add the reverse case, and replace the warned != applied invariant with an assertion on the specific warning."
    },
    {
      "severity": "MEDIUM",
      "title": "The new live test fails on any thinking answer",
      "location": "live_test.go:1342 and live_test.go:1356; contrast live_test.go:1022",
      "recommendation": "Map the strategy to the read name (repeat2-think to think) or derive the last stage from the raw components, and add a think-path case."
    },
    {
      "severity": "LOW",
      "title": "The ASCII-fold subset claim is false; U+0130 and U+212A next to the slug match here and not in Python",
      "location": "calibration.go:123-148; docs/PORT.md:131-139; calibration_test.go:650-670; calibration.go:378 and :384",
      "recommendation": "Correct the claim and the U+017F comment, add the adjacent U+0130 and U+212A cases, and either make the boundary test rune-aware or document the divergence. Apply the same rule in Applicability's fallback."
    },
    {
      "severity": "LOW",
      "title": "A missing no-extension path containing a checkpoint slug still resolves to the bundled record",
      "location": "calibration.go:582-625 and :617-625; docs/PORT.md:152-157",
      "recommendation": "Treat a leading /, ./, ../ or ~/ as a path even without .json and error if missing, or state the limit in PORT.md. Add a test for a missing absolute path containing the slug."
    },
    {
      "severity": "LOW",
      "title": "PORT.md does not record two divergences, and its test inventory is stale",
      "location": "docs/PORT.md:122-129, :141-150, :565-572, :843-851",
      "recommendation": "Record Applicability's record-side normalisation and the two Go-only warnings, and update the calibration test inventory to name the current tests."
    },
    {
      "severity": "LOW",
      "title": "The new warnings never reach the CLI",
      "location": "cmd/shisa-de (main.go, doctor.go) has no reference to DecisionModel.Warnings; docs/PORT.md:565-572",
      "recommendation": "Print model.Warnings to stderr in doctor and optionally ask, or add them to the doctor report; if the silence is deliberate, document that the CLI drops them."
    }
  ]
}
END_REVIEW_JSON
