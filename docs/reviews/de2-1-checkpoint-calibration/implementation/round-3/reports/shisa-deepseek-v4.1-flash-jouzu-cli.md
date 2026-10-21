# Review Report

## Summary

Reviewed the Go port working tree at `/home/morpheus/data/dev/shisa/shisa-de-go` (HEAD `f648d39`, the scope's target) against the round-3 scope. The round-2 fixes are present: the README's local-server bullet now qualifies the tokenizer remedy and points a released-id alias at `Calibration`; the disagreement warning is gated on no supplied record and its wording is conditional; the supplied-record warning is restricted to the family's own record; `doctor` writes `DecisionModel.Warnings` to stderr; the boundary decodes and lowercases the adjacent rune; `looksLikeRecordPath` treats rooted paths as paths; and the live check maps `repeat2-think` to the record's `think` key.

The rune-aware boundary is a subset of Python's rule for every adjacency I could construct, and the live check now uses the same read name the client passes to `TemperatureFor`. Three MEDIUM findings remain: a relative, non-`.json` spec whose directory contains a checkpoint slug still resolves to the bundled record instead of failing; the custom-fit test never enters the branch the round-2 fix changed, so a regression of that fix would pass; and PORT.md tells an operator the live check covers a released-id alias through `SHISA_DE_TOKENIZER`, which it does not. Two LOW items are comment and documentation accuracy.

## Findings

### MEDIUM: A missing relative record path that contains a checkpoint slug resolves to the bundled record

- Severity: MEDIUM
- Confidence: HIGH
- Location: `calibration.go:645-660` (`looksLikeRecordPath`), `calibration_test.go:795-805`
- Problem: `ResolveCalibration` loads a path before matching a name only when the spec ends in `.json`, starts with `/`, `./`, `../` or `~`, or is an existing regular file. A relative spec that misses all three falls through to `ReleasedCheckpoint`, whose matcher is deliberately permissive. `ResolveCalibration("fits/shisa-de-2.1/deployment")` therefore returns the bundled DE-2.1 record with `ok=true` rather than reporting the missing file, and `ResolveCalibration("deployment/shisa-de-2/fit")` returns DE-2's. This is reachable from the command line (`--calibration` passes the string straight through, `cmd/shisa-de/main.go:169`) and from the library. The test table that claims to cover missing paths uses `<tmp>/not-here.json`, `<tmp>/deployment`, `/fits/shisa-de-2.1/deployment`, `./fits/shisa-de-2.1/deployment` and `~/fits/shisa-de-2.1/deployment` — all rooted or `.json` — so the relative spelling is untested and the suite passes with the gap present. This is the same wrong-record class round 2 fixed for `/fits/shisa-de-2.1/deployment.json`, just with the extension removed.
- Recommendation: Treat a spec containing a path separator as a path unless its last element names a released checkpoint (which keeps `shisa-ai/shisa-de-2.1` and `shisa-ai/shisa-de-2.1-FP8-dynamic` working), or otherwise add a rule that closes the relative case. Add the relative spelling to `TestResolveCalibrationLoadsAPathBeforeMatchingAName` so it fails instead of returning a record. Do not change `ReleasedCheckpoint` itself: served model ids such as `/models/shisa-de-2.1` must keep matching.

### MEDIUM: `TestACustomFitIsNotSecondGuessed` does not exercise the branch the fix changed

- Severity: MEDIUM
- Confidence: HIGH
- Location: `client_test.go:1958-1982`
- Problem: The test builds a custom record with `Model = "my-de2-finetune"` and serves it under the same id. `Applicability` therefore returns `checkpoint`, so `NewDecisionModel`'s `if supplied && level == ApplicabilityFamily` block is never entered. The pre-fix guard (family level plus `own.Model != supplied.Model`) would also have skipped the warning for this configuration, so the test passes with the round-2 defect restored. The round-2 defect was a custom fit on an *uncovered* checkpoint whose served name differs from the record's model, which is exactly the case the test does not construct. The scope's claim that this test "pins the difference" is not true as written.
- Recommendation: Serve the custom record under a different, uncovered id (for example record `Model = "my-de2-finetune"`, served `Model = "my-endpoint"`), which makes the level `family`. Under the old guard `own.Model` (`shisa-ai/shisa-de-2`) differs from the supplied model and the warning fires; under the new guard the supplied model differs from the family record's model and it does not. Assert no warning and that the record is still applied.

### MEDIUM: PORT.md says the live check can see a released-id alias through `SHISA_DE_TOKENIZER`

- Severity: MEDIUM
- Confidence: HIGH
- Location: `docs/PORT.md:1003-1008`
- Problem: The paragraph says the check "cannot see through an alias, so it is pointed at the checkpoint's own id, or at the alias with `SHISA_DE_TOKENIZER` naming the checkpoint behind it." `TestLiveCalibrationMatchesTheServedCheckpoint` only consults `SHISA_DE_TOKENIZER` when `ReleasedCheckpoint(liveModelID())` is empty (`live_test.go:1315-1318`). For the alias that matters — `SHISA_DE_MODEL=shisa-ai/shisa-de-2` serving DE-2.1, `SHISA_DE_TOKENIZER=shisa-ai/shisa-de-2.1` — the expectation is DE-2's record, the client applies DE-2's record, and the test passes with the wrong fit applied. The test's own docstring states the condition correctly ("When the id names no released checkpoint, set `SHISA_DE_TOKENIZER` ..."); PORT.md drops it and so overstates the check's coverage of the one case the feature exists for.
- Recommendation: Add the condition to the PORT.md sentence, as the test docstring has it, and say that a released-id alias still passes with the id's record applied. Alternatively, make the test prefer `SHISA_DE_TOKENIZER` when it is set and names a different released checkpoint, but that would make the test assert something the client does not do; the documentation fix matches the code.

### LOW: The `asciiLower` comment block is attached to `prevRuneStart`, and two folding claims are imprecise

- Severity: LOW
- Confidence: HIGH
- Location: `calibration.go:121-139`, `calibration.go:154-158`, `calibration_test.go:653-656`
- Problem: The `prevRuneStart` paragraph was appended to the `asciiLower` doc block, so `asciiLower` now has no comment of its own and `prevRuneStart`'s doc begins "asciiLower folds A-Z to a-z". `continuesCheckpointName`'s first line still says "reports whether b would make ..." although the parameter is `at int`. The comment then says Python lowercases U+0130 to `i` "so both continue a name there"; that is true when U+0130 follows the slug (Python sees the `i`), but when it precedes it Python's lowered string ends in U+0307, which is a boundary there and a continuation here — the conservative direction, but not what the sentence says. The test comment says Python's `str.lower()` maps U+017F to `s`; that is `casefold()`'s mapping, and `str.lower()` leaves U+017F unchanged (the next line's "U+017F is not `s` under `str.lower()` either" is the correct statement).
- Recommendation: Split the two comment blocks and place each above its function, restore the `at` parameter name in `continuesCheckpointName`'s first line, qualify the U+0130 claim to the following position, and either drop the U+017F example or label it as `casefold()`.

### LOW: The README does not say `doctor` now prints the warnings to stderr

- Severity: LOW
- Confidence: HIGH
- Location: `README.md:340-344`, `README.md:555`
- Problem: The calibration section says "`shisa-de doctor` prints the record it chose and the checkpoint it was fitted on, so the answer is visible before it is trusted", and the command table says doctor checks "whether the calibration belongs to the served model". After this change, the one signal the report fields cannot carry — a model id and tokenizer naming different checkpoints, or a supplied family record on a checkpoint with its own — is written to stderr, and the README never says so. A user following the alias guidance and running doctor has to notice stderr on their own; the Python client's equivalent channel is also stderr, so this is a documentation gap rather than a behavior defect.
- Recommendation: Add one sentence to the calibration section (or the doctor row) saying that `doctor` writes `DecisionModel.Warnings` to stderr, and that the report's own fields cannot show a tokenizer/model disagreement.

## Things Checked

- Round-2 HIGH: the README's local-server bullet now limits the tokenizer's checkpoint naming to aliases that are not released ids and points a released-id alias at `Calibration`; the calibration section's alias example and the fine-tune paragraph agree with `CalibrationForCheckpoint`'s model-then-tokenizer order.
- Warning gating and wording in `client.go`: the disagreement warning requires `!supplied` and both names to resolve to released checkpoints; the not-applied warning requires `Fitted() && !CalibrationApplied`, which is false for any supplied fitted record; the family-record warning requires `supplied && level == ApplicabilityFamily`, the supplied record's model to equal `CalibrationFor(family).Model`, and the served name to have a record of its own. `TestADisagreeingTokenizerIsReported` covers the alias direction, the correct DE-2.1 + DE-2-tokenizer pairing, an agreeing tokenizer, and the supplied-record suppression.
- Doctor stderr: `runDoctor` prints each warning as `shisa-de doctor: <message>` before either output form, stdout stays pure for `--json`, and `TestDoctorReportsADisagreeingTokenizerOnStderr` checks the disagreement line, the unchanged `match checkpoint` / `applied yes` fields, and silence for an agreeing tokenizer. The CLI parity fixture's doctor cases use `--model shisa-ai/shisa-de-2` with no tokenizer and record empty stderr, so the new lines do not break `TestCLIParity`; the fixture's cases that would warn in Python are not recorded.
- Boundary rule: `ReleasedCheckpoint` folds ASCII only, tests the adjacent rune through `prevRuneStart`/`utf8.DecodeRuneInString`, and lowercases it with `unicode.ToLower`. I checked U+0130 in both positions, U+212A, U+017F, U+FFFD, invalid UTF-8, multi-byte boundaries, both-slugs-present ordering, and `shisa-de-2.10`/`shisa-de-21`. Python's only multi-character full lowercase is U+0130 (`i` + U+0307), so the only divergence is Go rejecting a match Python makes (leading U+0130); the port never matches a name Python rejects.
- `Applicability`: the served names use `ReleasedCheckpoint` or `asciiLower`, the record's own model uses the same rule, and the record-side normalisation is a deliberate divergence from Python (`record.model.lower()`) that only changes the level for a caller-written record. The client still applies a supplied record regardless of level, so the divergence changes `Health` reporting, not which fit is applied.
- `ResolveCalibration`: order is path, released checkpoint, family, file; `.json`, rooted paths and existing regular files are paths; the checkpoint spellings, family spellings, `auto`, a DE-1 name, and `shisa-ai/shisa-de-2.2` behave as the tests assert. The relative-path gap is finding 1.
- Live check: `liveCalibrationRead` maps `repeat2-think` to `think` and passes `repeat2`/`direct` through, matching the stage name `client.go` passes to `TemperatureFor`; the test asserts the answer's own `Temperature`, requires a per-read table, and cross-checks the other shipped record. The shipped DE-2 and DE-2.1 temperatures differ for all three reads and both question types, so the cross-check cannot fail spuriously.
- Data and README: the DE-2/DE-2.1 temperatures in `data/calibration-de2.json`, `data/calibration-de2.1.json` and the README table agree (2.69/1.81, 2.76/1.89, 6.8/6.8 and 1.98/1.53, 2.11/1.64, 5.23/5.23), and `temperatures` repeats `repeat2` in both records.
- Python reference: `calibration.py`'s `released_checkpoint`, `applicability` and `resolve_calibration`, and `client.py`'s warning text and `calibration_applied` rule, were read for comparison. The two Go-only warnings are correctly recorded in PORT.md, and the not-applied warning is Python's sentence with the Go field names.
- PORT.md: the ASCII-fold, adjacent-rune, tokenizer-not-weights, path-first, `Applicability`-normalisation, warnings-to-stderr and calibration test-inventory paragraphs match the code, apart from finding 3.

## Residual Risks

- No shell tool is available in this session, so `gofmt -l .`, `go vet ./...`, `go test ./... -count=1` and `go test -race ./...` were not re-run; the scope's validation results are taken as claimed. Every statement above is from reading the working tree.
- No live DE-2.1 endpoint was available, so the live mapping is checked against the client's code path and the shipped records, not a run. The alias case remains undetectable from a response by construction; finding 3 is about the documentation of that limit, not the limit itself.
- The disagreement warning still fires on the documented correct DE-2.1 configuration (`Model: "shisa-ai/shisa-de-2.1"`, `Tokenizer: "shisa-ai/shisa-de-2"`). It says nothing false, its wording is conditional, and suppressing that direction would also hide the reverse alias, so I did not report it as a defect.
- `ResolveCalibration` still lets an existing regular file named like a family spelling (`de2`) shadow the family route. That is the documented file-wins rule and the file must be a valid record to load; noted rather than reported.
- The relative-path gap (finding 1) is the one place I found where a caller can ask for a record file and receive a different bundled record with `ok=true`. If it is left as-is, it should at least be tested and documented, because the current test table reads as if all missing paths fail.

BEGIN_REVIEW_JSON
{
  "findings": [
    {
      "severity": "MEDIUM",
      "title": "A missing relative record path containing a checkpoint slug resolves to the bundled record",
      "location": "shisa-de-go/calibration.go:645",
      "recommendation": "Treat a spec with a path separator as a path unless its last element names a released checkpoint, and add the relative spelling to the missing-path test table."
    },
    {
      "severity": "MEDIUM",
      "title": "TestACustomFitIsNotSecondGuessed does not enter the branch the round-2 fix changed",
      "location": "shisa-de-go/client_test.go:1958",
      "recommendation": "Serve the custom record under a different uncovered model id so Applicability returns family, which fails under the old guard and passes under the new one."
    },
    {
      "severity": "MEDIUM",
      "title": "PORT.md claims the live check sees a released-id alias through SHISA_DE_TOKENIZER",
      "location": "shisa-de-go/docs/PORT.md:1003",
      "recommendation": "Add the condition that the tokenizer route applies only when the model id names no released checkpoint, as the test docstring says."
    },
    {
      "severity": "LOW",
      "title": "asciiLower comment is attached to prevRuneStart and two folding claims are imprecise",
      "location": "shisa-de-go/calibration.go:121",
      "recommendation": "Split the comment blocks, restore the at parameter name, qualify the U+0130 claim to the following position, and label U+017F as casefold rather than lower."
    },
    {
      "severity": "LOW",
      "title": "README does not say doctor writes the warnings to stderr",
      "location": "shisa-de-go/README.md:340",
      "recommendation": "Add one sentence that doctor prints DecisionModel.Warnings to stderr and that the report fields cannot show a tokenizer/model disagreement."
    }
  ]
}
END_REVIEW_JSON
