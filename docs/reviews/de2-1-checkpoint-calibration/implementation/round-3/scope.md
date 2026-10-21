# Review Scope: de2.1-checkpoint-calibration

- Mode: implementation
- Stage: implementation
- Target: commits ecc90ff..f648d39 (HEAD), against bb503d4/v0.1.0

## Summary

Round 3 of the review of per-checkpoint calibration selection in the Go port of the Shisa DE-2 client. Round 2 found one HIGH and five MEDIUM; commit d8a6a42 fixes them.

HIGH: README's local-server bullet still offered `Tokenizer` as the alias remedy, contradicting the calibration section. It now says the tokenizer names the checkpoint only when the alias is not itself a released checkpoint id, and that a released-id alias needs `Calibration`.

The three warning defects:
- The disagreement warning fired even when the caller supplied a record, where it said "the model id chose the record" about a record the caller chose. It is now gated on no supplied record, and its wording is conditional ("if the endpoint serves X behind that id, pass Calibration for it"), because DE-2.1 is served with DE-2's tokenizer and that correct configuration triggers it too. PORT.md records that the pairing is expected and the warning informational there.
- The supplied-record warning fired for any supplied record at family level, including a caller's own fit on an uncovered checkpoint, and told them to discard it. It now fires only when the supplied record is that family's record, and `TestACustomFitIsNotSecondGuessed` pins the difference.
- The warnings never reached the command line. `doctor` now writes them to stderr, which is the stream `warnings.warn` uses in the Python client, and `TestDoctorReportsADisagreeingTokenizerOnStderr` checks the line and that an agreeing tokenizer says nothing. The report's own fields cannot carry the signal: for the alias case they read `match checkpoint` / `applied yes`, the same as a server really serving DE-2.

The Unicode and path defects:
- The boundary test looked at one byte, so `shisa-de-2.1` followed by U+0130 or preceded by U+212A matched here and not in Python, which lowercases the adjacent rune before applying `[a-z0-9.]`. The boundary now decodes the adjacent rune and lowercases it, and the comment, PORT.md and the test table say what the rule is. `Applicability`'s raw-name comparison folds ASCII on both sides for the same reason.
- `looksLikeRecordPath` now treats a rooted path (`/`, `./`, `../`, `~`) as a path even without a `.json` suffix, so `/fits/shisa-de-2.1/deployment` is an error rather than the bundled record. Tests cover five missing-path spellings.
- The live check keyed the calibration table by `Answer.Strategy`, which is `repeat2-think` for a thinking answer while the record keys that read as `think`, so a correct thinking answer failed. A test-local `liveCalibrationRead` maps it, matching the last-stage name the client passes to `TemperatureFor`.

PORT.md now records the `Applicability` adaptation (Python compares the record's raw lowercased model), the two Go-only warnings, the rune-aware boundary and an updated calibration test inventory.

Known non-goals: no DE-1 record, no `strip_state_quotes`, no readout version bump, and no live run against a DE-2.1 endpoint. The alias case remains undetectable from a response; the port reports what it can see and documents what it cannot.

## Primary files / regions

- `README.md` — reason: The corrected alias documentation in both places it appears
- `client.go` — reason: Gated and reworded warnings
- `calibration.go` — reason: Rune-aware boundary, ASCII fold in Applicability, rooted-path rule
- `cmd/shisa-de/doctor.go` — reason: Warnings to stderr
- `cmd/shisa-de/main_test.go` — reason: The doctor stderr test
- `calibration_test.go` — reason: Unicode counterexamples, missing-path spellings
- `client_test.go` — reason: The supplied-record cases and the custom-fit case
- `live_test.go` — reason: The thinking-read mapping
- `docs/PORT.md` — reason: Recorded adaptations, warnings and test inventory

## Review questions

- Does any documentation still describe a remedy that does not work, in particular anywhere the tokenizer is offered as the way to name the weights behind an alias?
- Are the two warnings now false or unhelpful on any documented configuration, and does the doctor stderr line match the library's warnings exactly?
- Is the rune-aware boundary now a strict subset of Python's rule, or does a counterexample remain?
- Can ResolveCalibration still turn a spec into the wrong record, for a path or a name?
- Does the live check fail for the reason it claims, including on a thinking answer, and can it pass while the wrong record is applied?
- Is docs/PORT.md now accurate about every adaptation this change makes, including the ones that diverge from Python?

## Validation already run

- gofmt -l . (clean)
- go vet ./... (clean)
- go test ./... -count=1 (all packages pass)
- go test -race ./... -count=1 (all packages pass)
- Live suite not run: needs a DE-2.1 endpoint
