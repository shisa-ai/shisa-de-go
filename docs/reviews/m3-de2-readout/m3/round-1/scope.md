# Review Scope: m3-de2-readout

- Mode: implementation
- Stage: m3
- Target: M3: the DE-2 readout (commits d0955e7, df422d3, dc2b74a on top of 853305b)

## Summary

M3 of the Go port of the Python `shisa-de` client. The Python repository at /home/morpheus/data/dev/shisa/shisa-de is the reference implementation and the spec; docs/READOUT-DE2.md is copied from it verbatim and is the contract. This port implements DE-2 only.

New code, all in package `shisade` at /home/morpheus/data/dev/shisa/shisa-de-go:

- `readout.go` (827 lines): `Readout`, `ReadoutConfig`, `NewReadout`, `SetTokenizer`, `Slot`, `CodeSlots`, `AnswerPrefixID`, `CheckCodeBoundary`, `ReadCodes`, `ReadCodesAfterThought`, `Think`, `ListModels`, `Softmax`, `CodeRead`, `Thought`, `Slot`, the scaffold constants (`DirectSystem`, `ThinkSystem`, `InputRepeat`, `AnswerPrefix`), the readout identities (`DE1ReadoutVersion`, `DE2ReadoutVersion`, `ReadoutVersionFor`, `calibrationCompatible`, `CalibrationCompatibleWith`), `CandidateIDLimit` (128), `DefaultMaxLogprobs` (20).
- `endpoint.go` (189 lines): `ReadoutHTTPError`, the `completionRequest` / `completionResponse` wire shapes, `postJSON`/`getJSON`/`doJSON`, `firstChoice`, `maxResponseBytes`.
- `render.go` +6 lines: `RenderOptions.SystemPrompt`, mirroring the Python `Readout(system_prompt=...)`; a thinking render always uses `ThinkSystem`.
- `readout_test.go` (621 lines), `scripts/gen_readout_parity.py` (406 lines), `testdata/readout_parity.json` + `.meta.json`.

What the readout does, and what should be checked for fidelity to the Python client (`shisa_de/readout.py`, functions `read_codes`, `think`, `_letter_logprob`, `_check_prompt_tokens`, `check_code_boundary`, `_post`, `softmax`, and `Readout.__init__`):

1. `ReadCodes` sends one request naming every candidate in `logprob_token_ids`, in blocks of 128, with `logprobs: 20` and `return_tokens_as_token_ids: true`, `max_tokens: 1`, `temperature: 0`. A server that rejects the field with HTTP 400/422 mentioning `logprob_token_ids` is asked again without it, and the capability stays downgraded for the process. With the capability off, all remaining codes go in one request and any code absent from the top-k costs one `prompt_logprobs: 0` request per code.
2. The top-k lookup tries `token_id:<id>` then the decoded code text. `prompt_logprobs: 0` returns the prompt's own last token; this port errors when that position holds more than one entry, where the Python client takes the first.
3. `Think` sends `return_token_ids: true` and `stop_token_ids: [<channel|>]` with a positive budget, requires `token_ids` in the response, and truncates at the first closing token.
4. `ReadCodesAfterThought` takes token ids so the thought continues token for token; the fallback appends the code's token id rather than its text.
5. `CheckCodeBoundary` requires the prompt to end on `<channel|>` and each code appended to that terminator to add exactly its own token; results are cached per code. `checkPromptTokens` compares the server's `usage.prompt_tokens` to the client's tokenization and tolerates 0.
6. `Softmax` runs over the option codes only, summing in a fixed order (shortest key first, then lexicographic).

The gate is `TestReadoutReplay`: `scripts/gen_readout_parity.py` runs the Python client against a vLLM-shaped stub and records every request body and response body in order plus the Python client's result; the Go test replays them through an `httptest` server, comparing each request body after JSON decoding and each result field by field. 19 scenarios, 57 exchanges. It was checked against deliberate mutations of the request body, the rank recording, the softmax, and the text-keyed lookup, each of which fails it.

Validation already run: `gofmt -l .` clean, `go build ./...`, `go vet ./...`, `go test ./...` all pass (package `shisade` 2.4s, `shisade/tokenizer` 7.5s).

## Primary files / regions

- `readout.go` — reason: the readout: code reads, blocks, fallback, boundary and prompt-token checks, thought generation, softmax, readout identities
- `endpoint.go` — reason: the HTTP layer: request/response shapes, JSON helpers, error conventions
- `render.go` — reason: the added SystemPrompt option and its interaction with the thinking render
- `readout_test.go` — reason: the replay gate and the Go-only unit tests
- `scripts/gen_readout_parity.py` — reason: the fixture generator: the stub server and the scenarios
- `docs/PORT.md` — reason: the claimed differences from the Python client, which must be true and complete

## Review questions

- Does `readout.go` reproduce `shisa_de/readout.py`'s `read_codes`, `think`, `_letter_logprob`, `_check_prompt_tokens` and `check_code_boundary` on the wire and in the results? Name any request field, response field, or result field that differs, and say which side is wrong.
- Are the three differences docs/PORT.md claims (the multi-entry prompt_logprobs fallback, the 8 MB body cap, `CodeRead.Sampled` as a string) the only deliberate behavioural differences? Find any unclaimed one.
- Is the replay gate in readout_test.go actually able to fail? Look for comparisons that cannot fail, fixture fields that are decoded but never checked, scenarios that would pass with the Go readout broken, and any place the test's stub tokenizer could agree with the Go client while disagreeing with the Python client.
- Is `Readout` safe for concurrent use as its doc comment claims? Check the mutex coverage over `tokenizer`, `slots`, `boundaryChecked` and `candidateIDs`, and any field read without the lock.
- Do the readout identities and the `calibrationCompatible` table in readout.go match the Python `readout.py` (`READOUT_VERSIONS`, `CALIBRATION_COMPATIBLE`), and does the added `SystemPrompt` option change any answer-producing contract in a way that would require a version bump?
- Does the Go `Softmax` reproduce the Python `softmax`'s arithmetic closely enough that an argmax or a 0.7 gate cannot move, given Python 3.12+ sums floats with Neumaier compensation? Is the fixed key order the same order the Python client sums in?

## Known non-goals

- DE-1 is not implemented and must not be: a model that resolves to DE-1 is an error. Reviewing the missing DE-1 readout is out of scope.
- `policy.go`, `calibration.go`, `client.go`, `cmd/shisa-de/` and the live test suite do not exist yet (M4/M5/M6).
- The tokenizer package (`tokenizer/`), `pyjson.go`, `questions.go`, `family.go` and `codebook.go` were reviewed in M1 and M2 and are unchanged here except where readout.go calls them.
- No live requests were made and no number here is a measurement against a served model. Do not ask for one.
- `testdata/readout_parity.json` is a 420 KB generated fixture; review the generator and the test, not the fixture bytes.

## Validation already run

- gofmt -l . (clean)
- go build ./... (clean)
- go vet ./... (clean)
- go test ./... -count=1 (shisade 2.4s, shisade/tokenizer 7.5s, both ok)
- python3 scripts/gen_readout_parity.py (regenerates 19 scenarios, 57 exchanges, digest 814b9801a44f -> ead5925a30ba)
