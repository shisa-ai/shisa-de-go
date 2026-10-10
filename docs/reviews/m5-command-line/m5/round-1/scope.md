# Review Scope: m5-command-line

- Mode: implementation
- Stage: m5
- Target: The M5 command line (cmd/shisa-de/) and the Python-JSON options it needs

## Summary

M5 of the Go port: the `shisa-de` command line, and the library surface it needed. Review the new `cmd/shisa-de/` package against its reference `shisa_de/cli.py`, the `json.dumps` options added to `pyjson.go` (with their generated fixture), the four newly exported identifiers, and the documentation of every deliberate difference.

## Primary files / regions

- `cmd/shisa-de/main.go` — lines/regions: all new — reason: The parser, the shared flags, the `newModel` seam and the dispatch
- `cmd/shisa-de/flags.go` — lines/regions: all new — reason: The option shapes `flag` lacks: optionalBool with --no-, optionalFloat, optionalInt, optionalStringValue
- `cmd/shisa-de/state.go` — lines/regions: all new — reason: stateFrom, labelsFrom, defaultQuestion, optionsFromLabels
- `cmd/shisa-de/doctor.go` — lines/regions: all new — reason: The report text, the ordered JSON form and the exit status
- `cmd/shisa-de/ask.go` — lines/regions: all new — reason: The one-line classification and its JSON output
- `cmd/shisa-de/explain.go` — lines/regions: all new — reason: The walkthrough, bestCode, sortedTopLogprobs, apiKeyFromEnv, shlexQuote
- `cmd/shisa-de/main_test.go` — lines/regions: all new — reason: The stub endpoint, the stub tokenizer, and every command-line test
- `pyjson.go` — lines/regions: PythonJSON, PythonJSONWith, PythonJSONOptions, appendPythonJSON, appendPythonObject, appendPythonString, appendPythonItemSeparator, appendPythonIndent — reason: The json.dumps options and the indented writer
- `pyjson_test.go` — lines/regions: TestPythonJSONDumpsOptions, TestPythonJSONIndentIsNotGoIndent — reason: The fixture replay and the negative comparator
- `client.go` — lines/regions: PythonStr, PythonRepr — reason: The two exports the command line prints with
- `readout.go` — lines/regions: MaxLogprobs — reason: The accessor the curl body needs
- `scripts/gen_pyjson_dumps.py` — lines/regions: all — reason: The generator for the json.dumps fixture
- `docs/PORT.md` — lines/regions: the .gitignore anchor; the command-line sections — reason: Every deliberate difference and the M5 status
- `README.md` — lines/regions: the command-line section — reason: The user-facing CLI documentation

## Review questions

- Does `cmd/shisa-de/` reproduce `shisa_de/cli.py`'s behaviour and output for doctor, ask and explain, including the text layout, the JSON forms, the exit statuses and which stream each message goes to?
- Is the flag parsing faithful to `argparse`: the `choices` on --family/--policy/--reads, the BooleanOptionalAction `--reasoning`/`--no-reasoning` and `--compound`/`--no-compound`, an absent --state differing from `--state ''`, an unknown flag, and exit status 2 for a bad invocation?
- Is `PythonJSONWith`'s `indent`/`ensure_ascii` exactly `json.dumps`: the item separator, empty containers staying inline, `indent=""` versus `indent=None`, DEL and the C1 block, and an astral code point as a surrogate pair? Is the generated fixture strong enough to catch a regression?
- Are the newly exported identifiers (`PythonJSONWith`, `PythonStr`, `PythonRepr`, `Readout.MaxLogprobs`) the right seam for a second consumer, or do they expose internals the library should keep?
- Are the command-line tests load-bearing? In particular: is the package-level `newModel` variable an acceptable test seam in production code, and does `TestExplainReportsAnUnreadableQuestion` really prove the walkthrough fails before it prints?
- Are `bestCode` and `sortedTopLogprobs` faithful to Python's `max(probabilities, key=...)` and `sorted(..., key=lambda kv: -kv[1])`, including the tie-break and the null logprob Python raises TypeError on?
- Is `stateFrom` correct about the three ways --state is read, and does the built-in state keep Python's key order (which reaches the prompt)?
- Does the `.gitignore` change actually fix the untracking of `cmd/shisa-de`, and is anything else in the tree silently ignored that should not be?

## Known non-goals

- DE-1: this port implements DE-2 only, by design, and the readout page docs/READOUT-DE2.md is the contract
- The image path (shisa_de/images.py) and the chat-render probe are not ported
- Live tests behind SHISA_DE_LIVE are M6, not this milestone
- `Decision.Values` holding `map[string]any` rather than an ordered type is recorded in PORT.md as a known output-key-order difference, not a defect to fix here
- The library's `DefaultModel` staying at the Python client's DE-1 id is a recorded decision; the CLI's differing default is what is under review

## Validation already run

- gofmt -l . (clean)
- go vet ./...
- go test ./... -count=1
- go test ./... -count=1 -race
- diff of `explain` against the Python command line on the hosted endpoint: identical apart from the sampled numbers
- diff of `doctor` and `doctor --json` against the Python command line on the hosted endpoint: identical (text), identical (json)
- diff of `doctor --probe`: the Python report has one extra `chat system` line, which PORT.md records as absent by design

## Diff summary / excerpts

M5: the command line, plus the Python-JSON options it needs.

New: cmd/shisa-de/{main.go,flags.go,state.go,doctor.go,ask.go,explain.go,main_test.go}
Modified: pyjson.go (PythonJSONWith with `indent` and `ensure_ascii`; appendPythonJSON/appendPythonObject/appendPythonString take a PythonJSONOptions), client.go (export PythonStr, PythonRepr), readout.go (export Readout.MaxLogprobs), .gitignore (anchor the built-binary rule to the repo root so it stops matching cmd/shisa-de), README.md, docs/PORT.md, AGENTS.md
New data/scripts: scripts/gen_pyjson_dumps.py, testdata/pyjson_dumps.json (264 cases), testdata/pyjson_dumps.meta.json

The reference is `shisa_de/cli.py` at /home/morpheus/data/dev/shisa/shisa-de (Python client 0.4.0 at d172d4977d133fefb9b8c82ffcc0e3d007554551). `cmd/shisa-de/` is the port of it. `pyjson.go` is the port of `json.dumps`; the new options are `json.dumps`' `indent` and `ensure_ascii`.

Deliberate absences, each documented in docs/PORT.md: no `--image`/`--image-top-logprobs` (no image read is ported), no DE-1 branch in `explain` (no DE-1 read is ported), no `chat system` line in `doctor` (Health has no chat-render probe). The command line's `--model` defaults to shisa-ai/shisa-de-2 where the Python one defaults to the library's DE-1 id, because this port cannot read DE-1.

Also deliberate and documented: a caller-visible Go map prints its keys sorted where Python's dict prints insertion order, so `ask --confidence` prints `confidence` before `label`; `doctor --json` restores the report's order because the report's key set is fixed.

Measured: `explain`, `doctor` and `doctor --json` were each diffed against the Python command line against the hosted endpoint (https://api.shisa.ai/openai, model shisa-ai/shisa-de-2) and are identical apart from the numbers a fresh request changes and the one `chat system` line.
