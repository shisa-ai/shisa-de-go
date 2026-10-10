# Review Scope: m5-command-line

- Mode: implementation
- Stage: m5
- Target: The M5 command line after the round-1 fixes

## Summary

Round 2 of M5 (the `shisa-de` command line). Re-review the full M5 scope after the round-1 fix set: the command line against `shisa_de/cli.py`, the `json.dumps` options in `pyjson.go`, the Python-compatibility exports (`PythonStr`, `PythonRepr`, `TrimPythonSpace`, `MaxLogprobs`, `PythonJSONWith`), and the documentation of every deliberate difference. The round-1 reports are in the repository at docs/reviews/m5-command-line/m5/round-1/.

## Primary files / regions

- `cmd/shisa-de/main.go` — lines/regions: the parser, the set-aware choice flags, the family match, the dispatch, familyNames, contains — reason: The round-1 flag-strictness and help fixes
- `cmd/shisa-de/explain.go` — lines/regions: all — reason: The read order, the gate spelling, the null logprob, the Python whitespace
- `cmd/shisa-de/doctor.go` — lines/regions: all — reason: orderedSlots deleted; reportOrder unchanged
- `cmd/shisa-de/state.go` — lines/regions: stateFrom and labelsFrom — reason: The dropped error return and TrimPythonSpace
- `cmd/shisa-de/flags.go` — lines/regions: commonFlags — reason: The --compound help line and the gate default
- `cmd/shisa-de/main_test.go` — lines/regions: the whole round-1 fix set and the new tests — reason: The new test cases and the stub knob
- `client.go` — lines/regions: family_note, the warning, Health's slots — reason: pythonQuote and the Object slot summary
- `calibration.go` — lines/regions: the three Applicability reasons — reason: pythonQuote
- `questions.go` — lines/regions: TrimPythonSpace — reason: The new export
- `client_test.go` — lines/regions: TestHealthOffline — reason: The Object slot summary assertion
- `docs/PORT.md` — lines/regions: the flag-grammar and null-logprob paragraphs; the Verified parity paragraph — reason: The recorded differences
- `AGENTS.md` — lines/regions: the cmd/shisa-de key-file entry — reason: TrimPythonSpace in the compatibility surface
- `docs/reviews/m5-command-line/m5/round-1/NOTE.md` — lines/regions: all new — reason: The round-1 consolidation

## Review questions

- Does the round-1 fix set actually close each finding it claims to, and did any fix introduce a new divergence from `shisa_de/cli.py`?
- Is `Health`'s `slots` now a `shisade.Object` the right shape, and does `doctor --json` still print exactly what `json.dumps(report, indent=2)` prints for it?
- Is `explain`'s print order now Python's, and does `TestFailedRead` really prove the read comes after sections 1-2?
- Is the set-aware handling of `--family`/`--policy`/`--reads`/`--calibration` correct, and does `--family` matching `argparse`'s `choices` exactly break any legitimate spelling?
- Are `TestReportOrderCoversHealth` and `TestDoctorNotOKExitsOne` load-bearing, or do they pass for the wrong reason?
- Is the `%q` to `pythonQuote` change complete across the strings that reach the command line, and does `pythonQuote` equal Python's `!r` on the model ids in play?
- Is `TrimPythonSpace` the right export, and is it applied everywhere the command line strips text a Python counterpart strips?
- Does PORT.md now describe every remaining deliberate difference, including the ones this round recorded rather than fixed?

## Known non-goals

- DE-1: this port implements DE-2 only by design; docs/READOUT-DE2.md is the contract
- The image path (shisa_de/images.py) and the chat-render probe are not ported, so --image/--image-top-logprobs and doctor's `chat system` line are absent
- Live tests behind SHISA_DE_LIVE are M6
- `Decision.Values` holding `map[string]any` is a recorded output-key-order difference, not a defect to fix here
- The Python client resolves the tokenizer source in `Readout.__init__` and this port resolves it lazily in `EnsureTokenizer`; that difference predates M5 and is recorded in PORT.md
- `doctor --probe` cannot print Python's `chat system` line because Health has no chat-render probe

## Validation already run

- gofmt -l . (clean)
- go vet ./...
- go test ./... -count=1
- go test ./... -count=1 -race
- diff of `doctor --model shisa-ai/shisa-de-2` against the Python command line on the hosted endpoint: IDENTICAL
- diff of `doctor --model shisa-ai/shisa-de-2 --json`: IDENTICAL
- diff of `ask --labels billing,outage`: IDENTICAL
- diff of `explain --labels billing,outage`: IDENTICAL
- diff of `doctor --probe`: differs only by the documented `chat system` line
- the assumed-family `note` line compared against Python's `!r` spelling: identical

## Diff summary / excerpts

Round 2 of M5. Round 1 ran ds4 and glm (astra and the config-default gpt-5.5 both returned "[Error: Our servers are currently overloaded]"; astra is one of the three requested reviewers and did not execute, which is why this round re-runs). Round 1 found 0 HIGH, 0 CRITICAL, 1 MEDIUM, 14 LOW. Everything below is the round-1 fix set, over the same scope as round 1.

Library changes:
- client.go: `family_note` and the assumed-family warning now use `pythonQuote` (Python's `!r`) instead of `%q`; the not-applied warning too. `Health` now stores `report["slots"]` as a `shisade.Object` (`count`, then the codebook's first two codes) instead of a `map[string]any`, so `doctor --json` keeps Python's order by construction; the CLI's `orderedSlots` sort is deleted.
- calibration.go: the three `Applicability` reason strings use `pythonQuote`.
- questions.go: `TrimPythonSpace` is exported (Python's `str.strip()`, which strips U+001C-U+001F and `strings.TrimSpace` does not).
- client_test.go: `TestHealthOffline` asserts the `Object` shape and the count/code order.

Command-line changes:
- explain.go: the read now happens after sections 1 and 2 are printed, which is Python's order, so a failed read leaves the prompt and the slots on stdout. The think gate prints via `PythonStr` (so an integral gate is `1.0`, not `1`). A null logprob prints `None`; `topLogprob` carries a `null` field. `apiKeyFromEnv` trims with `TrimPythonSpace`.
- main.go: `--family`, `--policy`, `--reads` and `--calibration` are registered through `optionalStringValue` and record that they were given; an explicitly empty one is a usage error (exit 2) rather than the default. `--family` is an exact `de1`/`de2` match, not `NormalizeFamily`. The bare word `help` is no longer accepted (argparse has no such subcommand; it is now an unknown command, exit 2). `optionalString` is gone; `familyNames` is new; `contains` is generic over `~string` because the family list is `[]shisade.Family`.
- flags.go: `--compound / --no-compound` is listed in the hand-written help; the `--reasoning-prob` default is spelled with `PythonStr`.
- state.go: `stateFrom` no longer returns an always-nil error; `stateFrom` and `labelsFrom` trim with `TrimPythonSpace`; a comment that named a nonexistent `defaultState` is fixed.
- ask.go: drops the dead error branch.
- main_test.go: `cliServer` gained a `completionStatus` knob; `installCLIModel` now calls a factored `stubModel`; new tests `TestDoctorNotOKExitsOne`, `TestReportOrderCoversHealth`, `TestChoiceFlagsRejectAnEmptyValue`, `TestFailedRead`, `TestStateFromTrimsPythonsWhitespace`, `TestDoctorQuotesLikePython`, `TestExplainPrintsTheGateLikePython`; exact-quoting assertions in `TestDoctorText`; the `null` flag in `TestSortedTopLogprobs`; the `help` case in `TestRunDispatch`; `TestExplainReportsAnUnreadableQuestion`'s comment corrected.

Docs: PORT.md records the flag layer's remaining `argparse` grammar differences (no prefix abbreviation, `--reasoning=true` accepted, `--timeout 0` read as the library default) and `explain`'s null-logprob and tie-break handling, and the "Verified parity" paragraph now says what the offline suite covers instead of overstating the manual diff. AGENTS.md's key-file entry lists `TrimPythonSpace` as part of the Python-compatibility surface.
