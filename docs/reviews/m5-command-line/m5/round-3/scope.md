# Review Scope: m5-command-line

- Mode: implementation
- Stage: m5
- Target: The M5 command line after the round-2 fixes

## Summary

Round 3 of M5 (the `shisa-de` command line), the last round before maxCycles. Re-review the full M5 scope after the round-2 fix set: the command line against `shisa_de/cli.py`, the readout's error messages against `readout.py`, the `json.dumps` options in `pyjson.go`, the Python-compatibility exports, and the documentation of every deliberate difference. Rounds 1 and 2 are in the repository at docs/reviews/m5-command-line/m5/.

## Primary files / regions

- `readout.go` — lines/regions: the four boundary/slot/fallback messages — reason: The pythonQuote sweep and the DE-2 rewording comments
- `client.go` — lines/regions: APIKeyFromEnv — reason: TrimPythonSpace on the bearer token
- `readout_test.go` — lines/regions: TestBoundaryMessagesQuoteLikePython — reason: The whole-sentence assertions
- `client_test.go` — lines/regions: TestAPIKeyFromEnv — reason: The U+001C strip case
- `cmd/shisa-de/main_test.go` — lines/regions: the family_note guard and the --state '' case — reason: The two round-2 test gaps
- `docs/PORT.md` — lines/regions: the four new paragraphs — reason: The recorded differences
- `docs/reviews/m5-command-line/m5/round-1/NOTE.md` — lines/regions: all — reason: The round-1 consolidation
- `docs/reviews/m5-command-line/m5/round-2/NOTE.md` — lines/regions: all — reason: The round-2 consolidation

## Review questions

- Does the round-2 fix set close each finding it claims to, and did any of it introduce a new divergence from `shisa_de/cli.py` or from `readout.py`?
- Is the `%q` to `pythonQuote` change now complete across every message that reaches the command line and is otherwise word-for-word Python's, and is the set of messages deliberately left as Go's own the right one?
- Does `TestBoundaryMessagesQuoteLikePython` pin the real sentences, and does `TestAPIKeyFromEnv` pin the Python strip?
- Is `docs/PORT.md` now complete: does it record every remaining deliberate difference, including the four this round added?
- Is there anything in M5 that would change a served answer, a readout version, or the calibration record?
- Is the M5 milestone complete against the plan in docs/PORT.md, or is something in the command-line scope still missing before M6?

## Known non-goals

- DE-1: this port implements DE-2 only by design; docs/READOUT-DE2.md is the contract
- The image path (shisa_de/images.py) and the chat-render probe are not ported, so --image/--image-top-logprobs and doctor's `chat system` line are absent
- Live tests behind SHISA_DE_LIVE are M6
- `Decision.Values` holding `map[string]any` is a recorded output-key-order difference
- The Python client resolves the tokenizer source in `Readout.__init__` and this port resolves it lazily in `EnsureTokenizer`; recorded in PORT.md
- `Object` having no `MarshalJSON` is recorded in PORT.md, not a defect to fix in this milestone
- The usage/help/error text is this port's own wording, recorded in PORT.md

## Validation already run

- gofmt -l . (clean)
- go vet ./...
- go test ./... -count=1
- go test ./... -count=1 -race
- diff of `doctor --model shisa-ai/shisa-de-2` against the Python command line on the hosted endpoint: IDENTICAL
- diff of `doctor --model shisa-ai/shisa-de-2 --json`: IDENTICAL
- diff of `ask --model shisa-ai/shisa-de-2 --labels billing,outage`: IDENTICAL
- diff of `explain --model shisa-ai/shisa-de-2 --labels billing,outage`: IDENTICAL
- diff of `doctor --probe`: differs only by the documented `chat system` line

## Diff summary / excerpts

Round 3 of M5 (last round; maxCycles is 3). Round 1 ran ds4 and glm (astra failed with a provider overload error). Round 2 ran ds4 and glm again, 0 HIGH, 0 CRITICAL, 9 LOW; astra failed with the same overload error both times. This round re-runs the three requested reviewers over the full updated scope.

The round-2 fix set:
- readout.go: the three boundary/token-count messages and the code-slot messages now use `pythonQuote` (Python's `!r`) instead of `%q`. They are word-for-word the Python client's text and reach `doctor`'s `boundary_check`/`read_probe` fields in both output forms and `explain`'s stderr. Two Go-only messages (the tokenizer-load wrapper, the two `prompt_logprobs` fallback strictness checks) keep `%q` because they have no Python counterpart.
- client.go: `APIKeyFromEnv` strips with `TrimPythonSpace` rather than `strings.TrimSpace`, because that value is the bearer token and Python's `_api_key_from_env` uses `str.strip()`.
- cmd/shisa-de/main_test.go: `TestReportOrderCoversHealth`'s `family_note` guard compared a three-element argv to two and never ran; fixed. `TestAskState` now pins `--state ''` reaching the prompt as `"evidence": ""` rather than the built-in example.
- readout_test.go: new `TestBoundaryMessagesQuoteLikePython` asserts the whole terminator and per-code sentence.
- client_test.go: `TestAPIKeyFromEnv` pins the Python strip on U+001C-U+001F.
- docs/PORT.md records four things round 2 found unrecorded: `Health`'s `slots` is a `shisade.Object` (a public shape change; `json.Marshal` writes an array of members for it); the usage, `--help` and error text is this port's own layout and wording, with only the flag set, exit statuses and streams matching `argparse`; the library's `ResolveCalibration("")` means "auto" where Python raises on `""` (the CLI refuses an explicit `""` first); and the DE-2 readout rewords the messages it shares with DE-1 (letter/letter slots to code/codebook).

`Object` was deliberately left without a `MarshalJSON`, which round 2 offered as an alternative to recording the shape: adding one would change what `encoding/json` does to every `Object` in the library, and `PythonJSON` already writes the object Python writes.
